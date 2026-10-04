// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package clock abstracts time so that bounded waits, expiry and reconnect
// back-off can be driven deterministically in tests and scenarios.
//
// Production code uses Real. The scenario runner uses Fake, whose time only
// moves when a test advances it, so a one-hour share expiry never needs a
// one-hour test.
package clock

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Clock is the subset of package time that the CLI and the share engine use.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed.
	After(d time.Duration) <-chan time.Time
	// NewTimer returns a timer that fires once after d.
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer.
type Timer interface {
	C() <-chan time.Time
	// Stop prevents the timer from firing; it reports whether it was still
	// pending.
	Stop() bool
}

// Real is the wall clock.
type Real struct{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTimer returns a real timer.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.t.C }
func (t realTimer) Stop() bool          { return t.t.Stop() }

// WithTimeout derives a context that is cancelled when d elapses on c.
// The returned cancel function must be called to release the waiter.
func WithTimeout(ctx context.Context, c Clock, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	timer := c.NewTimer(d)
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
			cancel()
		case <-done:
			timer.Stop()
		}
	}()
	return ctx, func() {
		select {
		case <-done:
		default:
			close(done)
		}
		cancel()
	}
}

// Fake is a clock whose time only advances when Advance is called. Timers
// fire, in deadline order, as the fake time passes them.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	seq    uint64
}

// NewFake returns a fake clock starting at start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start}
}

type fakeTimer struct {
	clock    *Fake
	deadline time.Time
	seq      uint64
	ch       chan time.Time
	fired    bool
	stopped  bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	t.clock.remove(t)
	return true
}

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After registers a timer and returns its channel.
func (f *Fake) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

// NewTimer registers a timer that fires when the fake time reaches now+d.
// A non-positive d fires on the next Advance (or immediately, see Advance).
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	t := &fakeTimer{clock: f, deadline: f.now.Add(d), seq: f.seq, ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.fired = true
		t.ch <- f.now
		return t
	}
	f.timers = append(f.timers, t)
	return t
}

func (f *Fake) remove(t *fakeTimer) {
	for i, x := range f.timers {
		if x == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			return
		}
	}
}

// Advance moves the fake time forward by d, firing every timer whose
// deadline is reached, in deadline order. Each timer observes the time at
// which it fires.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	for {
		sort.SliceStable(f.timers, func(i, j int) bool {
			if f.timers[i].deadline.Equal(f.timers[j].deadline) {
				return f.timers[i].seq < f.timers[j].seq
			}
			return f.timers[i].deadline.Before(f.timers[j].deadline)
		})
		if len(f.timers) == 0 || f.timers[0].deadline.After(target) {
			break
		}
		t := f.timers[0]
		f.timers = f.timers[1:]
		f.now = t.deadline
		t.fired = true
		t.ch <- f.now
	}
	f.now = target
	f.mu.Unlock()
}

// Pending reports how many timers are waiting.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}
