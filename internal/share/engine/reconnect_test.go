package engine

import (
	"bytes"
	"context"
	"log"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/share"
)

// Rand sources that fix the jitter: the floor and the ceiling of each draw.
var (
	jitterFloor   = func(int64) int64 { return 0 }
	jitterCeiling = func(n int64) int64 { return n - 1 }
)

// Reconnect waits are drawn with full jitter from 1 s up to a ceiling that
// doubles to 30 s, and a reset starts the ceiling again from 1 s.
func TestBackoffIsFullJitterFromOneToThirtySeconds(t *testing.T) {
	t.Parallel()
	cfg := New(Config{}).cfg
	if cfg.ReconnectMin != time.Second || cfg.ReconnectMax != 30*time.Second || cfg.Rand == nil {
		t.Fatalf("defaults: %s to %s, rand %v", cfg.ReconnectMin, cfg.ReconnectMax, cfg.Rand != nil)
	}
	draw := func(r func(int64) int64, n int) []time.Duration {
		b := backoff{min: cfg.ReconnectMin, max: cfg.ReconnectMax, rand: r}
		var out []time.Duration
		for range n {
			out = append(out, b.next())
		}
		return out
	}
	s := time.Second
	if got := draw(jitterCeiling, 7); !slices.Equal(got, []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s}) {
		t.Fatalf("ceilings %v", got)
	}
	if got := draw(jitterFloor, 7); !slices.Equal(got, []time.Duration{s, s, s, s, s, s, s}) {
		t.Fatalf("floors %v", got)
	}
	src := rand.New(rand.NewPCG(1, 2))
	b := backoff{min: cfg.ReconnectMin, max: cfg.ReconnectMax, rand: src.Int64N}
	top := time.Second
	for i := range 1000 {
		if i%10 == 0 {
			b.reset()
			top = time.Second
		}
		if d := b.next(); d < time.Second || d > top {
			t.Fatalf("draw %d: %s outside 1s..%s", i, d, top)
		}
		top = min(2*top, 30*time.Second)
	}
}

// An overloaded edge's hint is waited out, plus the back-off, before the
// next attempt.
func TestOverloadedEdgeWaitsItsHintPlusJitter(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	p.connErr = &Unavailable{Detail: "the edge is busy", RetryAfter: 10 * time.Second}
	logs := &syncBuffer{}
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk, Logger: log.New(logs, "", 0), Rand: jitterFloor})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "o1", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	// The refused first attempt waits the hint plus the back-off's 1 s.
	waitFor(t, func() bool { return strings.Contains(logs.String(), "retrying in 11s") })
	time.Sleep(20 * time.Millisecond) // the wait starts right after the log line
	clk.Advance(11*time.Second - time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if n := served(p); n != 1 {
		t.Fatalf("%d attempts before the hint and the back-off passed", n)
	}
	p.mu.Lock()
	p.connErr = nil
	p.mu.Unlock()
	clk.Advance(time.Millisecond)
	next(t, sess, share.EventReady)
	if n := served(p); n != 2 {
		t.Fatalf("%d attempts", n)
	}
}

// A share whose dock ends unasked asks the platform first. Revoked, it ends
// as revoked without announcing a lost connection or redocking; expired, it
// ends as expired; with the device signed out, it ends as such.
func TestDockedShareNoLongerActiveEndsInsteadOfRedocking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(p *stubPlatform, clk *clock.Fake, id string)
		want  share.EndReason
	}{
		{"revoked", func(p *stubPlatform, _ *clock.Fake, id string) { p.revoked[id] = true }, share.ReasonRevoked},
		{"expired", func(p *stubPlatform, clk *clock.Fake, id string) {
			// The platform's clock runs ahead: it no longer lists the share
			// a moment before the local expiry fires.
			clk.Advance(time.Hour - 2*time.Second)
			p.revoked[id] = true
		}, share.ReasonExpired},
		{"signed out", func(p *stubPlatform, _ *clock.Fake, _ string) {
			p.checkErr = &Rejected{Reason: RejectAuthority, Detail: "this device's authorisation was revoked"}
		}, share.ReasonAuthorityInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := clock.NewFake(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
			p := newStub(clk)
			e, sess, id := dockedShare(t, p, clk)
			p.mu.Lock()
			tc.setup(p, clk, id)
			p.mu.Unlock()
			p.drop(id)
			ev := anyEvent(t, sess)
			if ev.Kind != share.EventEnded || ev.Share.EndReason != tc.want || ev.Remote.Status != share.RemoteNone {
				t.Fatalf("got %s %+v, want the share to end as %s", ev.Kind, ev.Share, tc.want)
			}
			o := e.find(share.StopRequest{ID: id})
			o.mu.Lock()
			lineage := o.lineage
			o.mu.Unlock()
			if lineage != nil {
				t.Fatal("the lineage outlived the share")
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.checks != 1 || len(p.served) != 1 || p.revokes != 0 {
				t.Fatalf("checks %d, docks %d, revokes %d", p.checks, len(p.served), p.revokes)
			}
		})
	}
}

// While the platform doesn't answer, the share keeps redocking. A dock that
// was up only briefly waits its back-off first.
func TestDockedShareKeepsRedockingWhileThePlatformIsUnreachable(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	p.checkErr = &Unavailable{Detail: "connection refused"}
	_, sess, id := dockedShare(t, p, clk)
	for round := 1; round <= 2; round++ {
		p.drop(id)
		if ev := anyEvent(t, sess); ev.Kind != share.EventConnectionLost || ev.Share.State != share.StateReconnecting {
			t.Fatalf("round %d: %s %+v", round, ev.Kind, ev.Share)
		}
		time.Sleep(20 * time.Millisecond)
		if n := served(p); n != round {
			t.Fatalf("round %d: redocked before the back-off (%d docks)", round, n)
		}
		advanceUntil(t, clk, sess, share.EventRestored)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.checks != 2 || len(p.served) != 3 {
		t.Fatalf("checks %d, docks %d", p.checks, len(p.served))
	}
}

// An edge drain moves an active share along: it redocks at once, on the
// same lineage, without asking the platform. So does a dock that stayed up
// and was lost, after the platform says the share is still active.
func TestDockedShareRedocksAtOnceAfterADrainOrALongDock(t *testing.T) {
	t.Parallel()
	for _, kind := range []ConnEventKind{ConnDrained, ConnLost} {
		clk := clock.NewFake(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
		p := newStub(clk)
		_, sess, id := dockedShare(t, p, clk)
		clk.Advance(DefaultReconnectMax)
		p.mu.Lock()
		p.conns[id].ch <- ConnEvent{Kind: kind}
		delete(p.conns, id)
		p.mu.Unlock()
		next(t, sess, share.EventConnectionLost)
		next(t, sess, share.EventRestored)
		p.mu.Lock()
		checks, docks := p.checks, slices.Clone(p.served)
		p.mu.Unlock()
		wantChecks := map[ConnEventKind]int{ConnDrained: 0, ConnLost: 1}[kind]
		if checks != wantChecks || len(docks) != 2 || docks[1].Lineage == nil || docks[1].Lineage != docks[0].Lineage {
			t.Fatalf("%s: checks %d, docks %+v", kind, checks, docks)
		}
	}
}

// dockedShare starts a one-hour share, with the floor of every jittered
// wait, and returns it ready.
func dockedShare(t *testing.T, p *stubPlatform, clk *clock.Fake) (*Engine, share.Session, string) {
	t.Helper()
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk, Rand: jitterFloor})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "f1", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	return e, sess, next(t, sess, share.EventReady).Share.ID
}

func served(p *stubPlatform) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.served)
}

func anyEvent(t *testing.T, sess share.Session) share.Event {
	t.Helper()
	select {
	case ev, ok := <-sess.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return share.Event{}
}

// advanceUntil moves the fake clock a second at a time until kind arrives;
// the share must not end on the way.
func advanceUntil(t *testing.T, clk *clock.Fake, sess share.Session, kind share.EventKind) share.Event {
	t.Helper()
	for range 60 {
		clk.Advance(time.Second)
		select {
		case ev := <-sess.Events():
			if ev.Kind == kind {
				return ev
			}
			if ev.Kind == share.EventEnded {
				t.Fatalf("ended while waiting for %s: %+v", kind, ev.Share)
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("no %s within a minute", kind)
	return share.Event{}
}

// syncBuffer is a log destination a test can read while the engine writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
