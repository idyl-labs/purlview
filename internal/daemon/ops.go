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

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/ipc"
)

// End reasons. They are part of the protocol surface (Operation.Reason).
const (
	ReasonStopped      = "stopped"
	ReasonExpired      = "expired"
	ReasonRevoked      = "revoked"
	ReasonDisconnected = "client_disconnected"
	ReasonShutdown     = "daemon_shutdown"
)

// Owner values.
const (
	OwnerClient = "client"
	OwnerDaemon = "daemon"
)

// operation is one unit of owned work: a test-only resource that does
// nothing, so that the lifecycle tests can exercise ownership and
// cancellation through the real process boundary.
type operation struct {
	id        string
	name      string
	owner     string
	connID    string // owning connection for attached operations
	startedAt time.Time
	deadline  time.Time

	mu      sync.Mutex
	ended   bool
	endedAt time.Time
	reason  string
	timer   *time.Timer
	done    chan struct{}
}

func (o *operation) snapshot() ipc.Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	op := ipc.Operation{ID: o.id, Name: o.name, Owner: o.owner, State: "active", StartedAt: o.startedAt.UTC().Format(time.RFC3339Nano)}
	if !o.deadline.IsZero() {
		op.Deadline = o.deadline.UTC().Format(time.RFC3339Nano)
	}
	if o.ended {
		op.State = "ended"
		op.EndedAt = o.endedAt.UTC().Format(time.RFC3339Nano)
		op.Reason = o.reason
	}
	return op
}

// end marks the operation ended exactly once. It returns false when it had
// already ended; the first reason wins and cleanup runs once.
func (o *operation) end(reason string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return false
	}
	o.ended = true
	o.endedAt = now
	o.reason = reason
	if o.timer != nil {
		o.timer.Stop()
	}
	close(o.done)
	return true
}

// registry owns all operations of a daemon.
type registry struct {
	mu     sync.Mutex
	active map[string]*operation
	// recent keeps ended operations for a while so a late or repeated stop
	// is answered with "already ended" rather than "not found".
	recent    []*operation
	keepEnded int
	now       func() time.Time
	onEnded   func(op ipc.Operation)
}

func newRegistry(onEnded func(ipc.Operation)) *registry {
	return &registry{active: map[string]*operation{}, keepEnded: 200, now: time.Now, onEnded: onEnded}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("daemon: random id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// start registers an operation. ttl <= 0 means no deadline.
func (r *registry) start(name string, detached bool, connID string, ttl time.Duration) ipc.Operation {
	now := r.now()
	o := &operation{id: newID(), name: name, owner: OwnerClient, connID: connID, startedAt: now, done: make(chan struct{})}
	if detached {
		o.owner = OwnerDaemon
		o.connID = ""
	}
	if ttl > 0 {
		o.deadline = now.Add(ttl)
		o.timer = time.AfterFunc(ttl, func() { r.end(o.id, ReasonExpired) })
	}
	r.mu.Lock()
	r.active[o.id] = o
	r.mu.Unlock()
	return o.snapshot()
}

// end ends one operation. found reports whether the id is known at all;
// first reports whether this call ended it.
func (r *registry) end(id, reason string) (op ipc.Operation, found, first bool) {
	r.mu.Lock()
	o, ok := r.active[id]
	if !ok {
		for _, e := range r.recent {
			if e.id == id {
				r.mu.Unlock()
				return e.snapshot(), true, false
			}
		}
		r.mu.Unlock()
		return ipc.Operation{}, false, false
	}
	first = o.end(reason, r.now())
	if first {
		delete(r.active, id)
		r.recent = append(r.recent, o)
		if len(r.recent) > r.keepEnded {
			r.recent = r.recent[len(r.recent)-r.keepEnded:]
		}
	}
	r.mu.Unlock()
	op = o.snapshot()
	if first && r.onEnded != nil {
		r.onEnded(op)
	}
	return op, true, first
}

// endOwnedBy ends every attached operation of a connection.
func (r *registry) endOwnedBy(connID, reason string) int {
	r.mu.Lock()
	var ids []string
	for id, o := range r.active {
		if o.owner == OwnerClient && o.connID == connID {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, _, first := r.end(id, reason); first {
			n++
		}
	}
	return n
}

// endAll ends every active operation.
func (r *registry) endAll(reason string) int {
	r.mu.Lock()
	ids := make([]string, 0, len(r.active))
	for id := range r.active {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, _, first := r.end(id, reason); first {
			n++
		}
	}
	return n
}

func (r *registry) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

// list returns active operations followed by recently ended ones, oldest
// first.
func (r *registry) list() []ipc.Operation {
	r.mu.Lock()
	all := make([]*operation, 0, len(r.active)+len(r.recent))
	for _, o := range r.active {
		all = append(all, o)
	}
	all = append(all, r.recent...)
	r.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].startedAt.Before(all[j].startedAt) })
	out := make([]ipc.Operation, 0, len(all))
	for _, o := range all {
		out = append(out, o.snapshot())
	}
	return out
}
