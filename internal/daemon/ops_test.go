package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/ipc"
)

func TestRegistryEndsExactlyOnceWithFirstReason(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var ended []ipc.Operation
	r := newRegistry(func(op ipc.Operation) { mu.Lock(); ended = append(ended, op); mu.Unlock() })
	op := r.start("t", false, "conn-1", 0)
	if op.Owner != OwnerClient || op.State != "active" {
		t.Fatalf("unexpected start snapshot: %+v", op)
	}
	// Race stop, revoke and disconnect: exactly one wins, cleanup runs once.
	var wg sync.WaitGroup
	firsts := make(chan bool, 3)
	for _, reason := range []string{ReasonStopped, ReasonRevoked, ReasonDisconnected} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, found, first := r.end(op.ID, reason)
			if !found {
				t.Errorf("operation must stay known after ending")
			}
			firsts <- first
		}()
	}
	wg.Wait()
	close(firsts)
	n := 0
	for f := range firsts {
		if f {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("exactly one end must win, got %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 1 || ended[0].State != "ended" || ended[0].Reason == "" {
		t.Fatalf("onEnded must run once with the winning reason: %+v", ended)
	}
	if r.activeCount() != 0 {
		t.Fatalf("ended operation still active")
	}
	// A later stop reports already ended, never restarts anything.
	again, found, first := r.end(op.ID, ReasonStopped)
	if !found || first || again.Reason != ended[0].Reason {
		t.Fatalf("repeated stop: found=%v first=%v reason=%q", found, first, again.Reason)
	}
}

func TestRegistryExpiryAndOwnership(t *testing.T) {
	t.Parallel()
	endedCh := make(chan ipc.Operation, 8)
	r := newRegistry(func(op ipc.Operation) { endedCh <- op })
	attachedA := r.start("a", false, "conn-A", 0)
	attachedB := r.start("b", false, "conn-B", 0)
	detached := r.start("d", true, "conn-A", 0)
	expiring := r.start("e", true, "", 60*time.Millisecond)
	if detached.Owner != OwnerDaemon {
		t.Fatalf("detached operation must be daemon-owned: %+v", detached)
	}

	// Losing connection A ends only A's attached work.
	if n := r.endOwnedBy("conn-A", ReasonDisconnected); n != 1 {
		t.Fatalf("disconnect of A ended %d operations, want 1", n)
	}
	got := <-endedCh
	if got.ID != attachedA.ID || got.Reason != ReasonDisconnected {
		t.Fatalf("wrong operation ended on disconnect: %+v", got)
	}

	// Expiry fires on its own and cannot be undone.
	select {
	case got = <-endedCh:
		if got.ID != expiring.ID || got.Reason != ReasonExpired {
			t.Fatalf("expected expiry of %s, got %+v", expiring.ID, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expiry did not fire")
	}
	if _, _, first := r.end(expiring.ID, ReasonStopped); first {
		t.Fatal("stop after expiry must not be the first end")
	}

	// B and the detached operation are untouched.
	active := 0
	for _, op := range r.list() {
		if op.State == "active" {
			active++
			if op.ID != attachedB.ID && op.ID != detached.ID {
				t.Fatalf("unexpected active operation %+v", op)
			}
		}
	}
	if active != 2 {
		t.Fatalf("active=%d, want 2", active)
	}
	if n := r.endAll(ReasonShutdown); n != 2 {
		t.Fatalf("endAll ended %d, want 2", n)
	}
	if r.activeCount() != 0 {
		t.Fatal("operations survived shutdown")
	}
}

func TestRegistryKeepsBoundedHistory(t *testing.T) {
	t.Parallel()
	r := newRegistry(nil)
	r.keepEnded = 3
	var ids []string
	for i := 0; i < 5; i++ {
		op := r.start("h", true, "", 0)
		ids = append(ids, op.ID)
		r.end(op.ID, ReasonStopped)
	}
	if _, found, _ := r.end(ids[0], ReasonStopped); found {
		t.Fatal("oldest ended operation should have been forgotten")
	}
	if _, found, first := r.end(ids[4], ReasonStopped); !found || first {
		t.Fatal("recent ended operation must be remembered as already ended")
	}
	if n := len(r.list()); n != 3 {
		t.Fatalf("list has %d entries, want 3", n)
	}
}
