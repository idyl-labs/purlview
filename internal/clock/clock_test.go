package clock

import (
	"context"
	"testing"
	"time"
)

func TestFakeFiresTimersInDeadlineOrder(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)
	later := f.NewTimer(2 * time.Second)
	sooner := f.NewTimer(time.Second)
	stopped := f.NewTimer(1500 * time.Millisecond)
	if !stopped.Stop() {
		t.Fatal("stop must report a pending timer")
	}
	if f.Pending() != 2 {
		t.Fatalf("pending %d, want 2", f.Pending())
	}
	f.Advance(3 * time.Second)
	if got := <-sooner.C(); !got.Equal(start.Add(time.Second)) {
		t.Fatalf("sooner fired at %s", got)
	}
	if got := <-later.C(); !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("later fired at %s", got)
	}
	select {
	case <-stopped.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if !f.Now().Equal(start.Add(3 * time.Second)) {
		t.Fatalf("now %s", f.Now())
	}
	if later.Stop() {
		t.Fatal("a fired timer is not pending")
	}
}

func TestFakeZeroTimerFiresImmediately(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Unix(0, 0))
	select {
	case <-f.After(0):
	default:
		t.Fatal("zero-duration timer must be ready")
	}
}

func TestWithTimeoutCancelsOnClock(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Unix(0, 0))
	ctx, cancel := WithTimeout(context.Background(), f, time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("cancelled early")
	default:
	}
	f.Advance(time.Minute)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context not cancelled after the clock passed the deadline")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("err %v", ctx.Err())
	}
}

func TestWithTimeoutCancelReleasesTimer(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Unix(0, 0))
	_, cancel := WithTimeout(context.Background(), f, time.Minute)
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for f.Pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.Pending() != 0 {
		t.Fatal("timer leaked after cancel")
	}
	cancel() // idempotent
}

func TestRealClock(t *testing.T) {
	t.Parallel()
	var c Clock = Real{}
	before := c.Now()
	tm := c.NewTimer(time.Millisecond)
	<-tm.C()
	if c.Now().Before(before) {
		t.Fatal("time went backwards")
	}
}
