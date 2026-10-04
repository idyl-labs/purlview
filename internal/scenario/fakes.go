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

package scenario

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
)

// Prober is the fake target probe: every target answers unless marked.
type Prober struct {
	mu          sync.Mutex
	unreachable map[string]string
	probes      int
}

// Unreachable makes a target fail its probe with the given detail.
func (p *Prober) Unreachable(target, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreachable[target] = detail
}

// Probe implements engine.Prober.
func (p *Prober) Probe(_ context.Context, target string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes++
	if d, ok := p.unreachable[target]; ok {
		return errors.New(d)
	}
	return nil
}

// Updates is the fake release metadata source. It behaves like the daemon's
// checker: a call returns what the cache holds after waiting at most the
// given time for an in-flight fetch, and a fetch (taking latency on the
// fake clock) runs in the background whenever none is running.
type Updates struct {
	clock       *clock.Fake
	mu          sync.Mutex
	latest      *share.Release
	latency     time.Duration
	unavailable bool
	cached      *share.Release
	inflight    chan struct{}
	calls       int
}

// Available publishes a newer version; a fetch takes latency on the fake
// clock to learn it (zero is immediate).
func (u *Updates) Available(version string, latency time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.latest = &share.Release{Version: version, Tag: "v" + version, URL: "https://releases.purlview.invalid/v" + version}
	u.latency = latency
}

// Unavailable makes every fetch fail after latency; the cache stays as it is.
func (u *Updates) Unavailable(latency time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.unavailable = true
	u.latency = latency
}

// Check implements engine.UpdateSource.
func (u *Updates) Check(ctx context.Context, _ string, wait time.Duration) (*share.Release, error) {
	u.mu.Lock()
	u.calls++
	if u.inflight == nil {
		done := make(chan struct{})
		u.inflight = done
		// The latency timer is created here, before Check returns, so a
		// clock advance made after the command finished always reaches it.
		var latency <-chan time.Time
		if u.latency > 0 {
			latency = u.clock.NewTimer(u.latency).C()
		}
		go u.fetch(done, latency)
	}
	done, cached := u.inflight, u.cached
	u.mu.Unlock()
	if wait <= 0 {
		return cached, nil
	}
	t := u.clock.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C():
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cached, nil
}

// WaitIdle blocks (bounded, real time) until no fetch is in flight, so a
// scenario can order a command after the background fetch has landed.
func (u *Updates) WaitIdle() bool {
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		idle := u.inflight == nil
		u.mu.Unlock()
		if idle {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func (u *Updates) fetch(done chan struct{}, latency <-chan time.Time) {
	if latency != nil {
		<-latency
	}
	u.mu.Lock()
	if !u.unavailable {
		u.cached = u.latest
	}
	u.inflight = nil
	u.mu.Unlock()
	close(done)
}

// Browser is the fake browser: it records what would have opened.
type Browser struct {
	mu     sync.Mutex
	fail   error
	Opened []string
}

// Fail makes Open fail with err (no browser on this machine).
func (b *Browser) Fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = err
}

// Open implements command.Browser.
func (b *Browser) Open(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.Opened = append(b.Opened, url)
	return nil
}

// Daemon is the in-process stand-in for the per-user daemon process: a
// share.Runner over the real share engine. It models the daemon being
// absent, failing to start, stopping, and a lost start reply. Process and
// socket guarantees are not modelled here; the lifecycle tests prove them.
type Daemon struct {
	w    *World
	mu   sync.Mutex
	logs diagnosticBuffer

	engine        *engine.Engine
	running       bool
	startErr      error
	loseNextReply bool
	replyAfterEnd bool
	sessions      map[*Invocation][]share.Session
	starts        int
	events        []share.EventKind // every share event the daemon has reported
}

func newDaemon(w *World) *Daemon {
	return &Daemon{w: w, sessions: map[*Invocation][]share.Session{}}
}

func (d *Daemon) newEngine() *engine.Engine {
	return engine.New(engine.Config{
		Logger: log.New(&d.logs, "", 0), Platform: d.w.Platform.Engine(), Prober: d.w.Prober, Clock: d.w.Clock, Updates: d.w.Updates,
		ProbeTimeout: 3 * time.Second, StartupTimeout: 30 * time.Second, RevokeTimeout: 5 * time.Second,
		ReconnectMin: time.Second, ReconnectMax: 30 * time.Second,
		// The top of each jittered wait: 1 s doubling to 30 s, as in the
		// transcripts.
		Rand: func(n int64) int64 { return n - 1 },
	})
}

// StartFails makes the next daemon start fail (for example, a runtime
// directory that cannot be created).
func (d *Daemon) StartFails(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.startErr = err
}

// LoseNextStartReply makes the next share start reach the daemon but lose
// its reply: the CLI sees a broken connection.
func (d *Daemon) LoseNextStartReply() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loseNextReply = true
}

// ReplyToNextStartAfterItEnds holds the next share start's reply until the
// share has ended, as a real daemon's reply does when the probes finish
// before it answers: the CLI learns the end from the reply, not an event.
func (d *Daemon) ReplyToNextStartAfterItEnds() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.replyAfterEnd = true
}

// Stop stops the daemon as `purlview daemon stop` or an upgrade would:
// every share ends, and a later start begins with none.
func (d *Daemon) Stop() {
	d.mu.Lock()
	eng := d.engine
	d.engine = nil
	d.running = false
	d.mu.Unlock()
	if eng != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		eng.Shutdown(ctx)
		cancel()
	}
}

// Running reports whether a daemon is up.
func (d *Daemon) Running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// Starts reports how many times a daemon was started.
func (d *Daemon) Starts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.starts
}

// Active reports the number of shares the daemon serves.
func (d *Daemon) Active() int {
	d.mu.Lock()
	eng := d.engine
	d.mu.Unlock()
	if eng == nil {
		return 0
	}
	return eng.Active()
}

// WaitShareEvents blocks (bounded, real time) until the daemon has reported
// these share events to its clients, in this order.
func (d *Daemon) WaitShareEvents(kinds ...share.EventKind) bool {
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		n := 0
		for _, kind := range d.events {
			if n < len(kinds) && kind == kinds[n] {
				n++
			}
		}
		d.mu.Unlock()
		if n == len(kinds) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// Open implements share.Runner.
func (d *Daemon) Open(ctx context.Context, start bool) (share.Session, error) {
	d.mu.Lock()
	if !d.running {
		if !start {
			d.mu.Unlock()
			return nil, share.ErrDaemonNotRunning
		}
		if d.startErr != nil {
			err := d.startErr
			d.mu.Unlock()
			return nil, err
		}
		d.engine = d.newEngine()
		d.running = true
		d.starts++
		if observer, err := d.engine.Open(ctx, false); err == nil {
			go func() {
				for ev := range observer.Events() {
					d.mu.Lock()
					d.events = append(d.events, ev.Kind)
					d.mu.Unlock()
				}
			}()
		}
	}
	eng := d.engine
	d.mu.Unlock()
	inner, err := eng.Open(ctx, start)
	if err != nil {
		return nil, err
	}
	s := &daemonSession{Session: inner, d: d}
	if inv, ok := ctx.Value(invocationKey{}).(*Invocation); ok {
		d.mu.Lock()
		d.sessions[inv] = append(d.sessions[inv], inner)
		d.mu.Unlock()
	}
	return s, nil
}

// killCLI closes every daemon connection an invocation opened, as the
// daemon would observe when that CLI process dies.
func (d *Daemon) killCLI(inv *Invocation) {
	d.mu.Lock()
	sessions := d.sessions[inv]
	delete(d.sessions, inv)
	d.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

type daemonSession struct {
	share.Session
	d *Daemon
}

func (s *daemonSession) Start(ctx context.Context, req share.StartRequest) (share.Share, error) {
	s.d.mu.Lock()
	lose := s.d.loseNextReply
	s.d.loseNextReply = false
	late := s.d.replyAfterEnd
	s.d.replyAfterEnd = false
	s.d.mu.Unlock()
	snap, err := s.Session.Start(ctx, req)
	if late && err == nil {
		snap = s.ended(ctx, snap)
	}
	if lose && err == nil {
		// The daemon registered the start, but this connection broke
		// before the reply arrived.
		_ = s.Close()
		return share.Share{}, fmt.Errorf("ipc: %s: connection reset by peer", "share.start")
	}
	return snap, err
}

// ended waits (bounded, real time) for the share to end and returns the
// ended record; on time out it returns the reply it was given.
func (s *daemonSession) ended(ctx context.Context, snap share.Share) share.Share {
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		shares, err := s.Shares(ctx)
		if err != nil {
			return snap
		}
		for _, sh := range shares {
			if sh.Attempt == snap.Attempt && !sh.Active() {
				return sh
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return snap
}

// diagnosticBuffer records operational engine diagnostics separately from
// explicitly labelled scenario transcripts. It is safe while shares run.
type diagnosticBuffer struct {
	mu   sync.Mutex
	text string
}

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text += string(p)
	return len(p), nil
}

func (b *diagnosticBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text
}
