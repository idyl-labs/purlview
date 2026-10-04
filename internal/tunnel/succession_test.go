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

package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idyl-labs/hyperplane-go/dock"
	"github.com/idyl-labs/hyperplane-go/generation"

	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/sdk/api"
)

// fakeEdge stands in for dock.Open: it records every dock's config
// and admits it with the next generation, unless it is set to refuse.
type fakeEdge struct {
	mu      sync.Mutex
	configs []dock.Config
	docks   []*fakeDock
	refuse  error
}

func (f *fakeEdge) open(_ context.Context, cfg dock.Config) (shareDock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs = append(f.configs, cfg)
	if f.refuse != nil {
		return nil, f.refuse
	}
	n := len(f.docks) + 1
	ctx, end := context.WithCancel(context.Background())
	d := &fakeDock{gen: generation.DockGen{Slot: uint64(n), Epoch: 1, Nonce: fmt.Sprintf("secret-nonce-%d", n)}, ctx: ctx, end: end, drained: make(chan struct{})}
	f.docks = append(f.docks, d)
	return d, nil
}

func (f *fakeEdge) dock(i int) *fakeDock {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.docks[i]
}

type fakeDock struct {
	gen     generation.DockGen
	ctx     context.Context
	end     context.CancelFunc
	drained chan struct{}
}

func (d *fakeDock) Gen() generation.DockGen  { return d.gen }
func (d *fakeDock) Context() context.Context { return d.ctx }
func (d *fakeDock) Drained() <-chan struct{} { return d.drained }
func (d *fakeDock) Close() error             { d.end(); return nil }
func (d *fakeDock) drain()                   { close(d.drained) }
func (d *fakeDock) AcceptLane(ctx context.Context) (*dock.Lane, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.ctx.Done():
		return nil, d.ctx.Err()
	}
}

// dockServing is a share's serving material for the edge, with
// one self-signed certificate standing in for its SVID and the bundle.
func dockServing(t *testing.T) engine.Serving {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := url.Parse("spiffe://example.invalid/share/shr_q7m2p4xk")
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{id}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return engine.Serving{
		Origin:  "https://q7m2p4xk.content.invalid",
		Targets: []string{"http://127.0.0.1:9"},
		Tunnel: &api.Tunnel{Endpoint: "edge.invalid:443", Edge: "spiffe://example.invalid/edge", Gateway: "spiffe://example.invalid/gateway",
			SVID: [][]byte{der}, Lease: []byte("lease"), Bundle: [][]byte{der}},
		Key:     key,
		Lineage: engine.NewLineage(),
	}
}

func dockOnce(t *testing.T, c *Client, s engine.Serving) engine.Connection {
	t.Helper()
	conn, err := c.ConnectServing(context.Background(), api.InstallationCredential{}, "shr_q7m2p4xk", s)
	if err != nil {
		t.Fatal(err)
	}
	if ev := <-conn.Events(); ev.Kind != engine.ConnReady {
		t.Fatalf("first event %s", ev.Kind)
	}
	return conn
}

func endedAs(t *testing.T, conn engine.Connection) engine.ConnEventKind {
	t.Helper()
	select {
	case ev := <-conn.Events():
		return ev.Kind
	case <-time.After(5 * time.Second):
		t.Fatal("the connection did not end")
	}
	return ""
}

// Every redock names the share's previous dock as its predecessor, and all
// of a share's docks use its one session cache. A dock the network drops
// is lost; one the edge drains is drained. The generation is never logged.
func TestRedocksNameThePredecessorAndShareOneSessionCache(t *testing.T) {
	t.Parallel()
	edge := &fakeEdge{}
	var logs bytes.Buffer
	c := &Client{Logger: log.New(&logs, "", 0), openDock: edge.open}
	s := dockServing(t)

	conn := dockOnce(t, c, s)
	edge.dock(0).end() // the network drops the dock
	if kind := endedAs(t, conn); kind != engine.ConnLost {
		t.Fatalf("a dropped dock ended as %s", kind)
	}
	_ = conn.Close()
	conn = dockOnce(t, c, s)
	edge.dock(1).drain() // the edge moves the share along
	if kind := endedAs(t, conn); kind != engine.ConnDrained {
		t.Fatalf("a drained dock ended as %s", kind)
	}
	_ = conn.Close()
	_ = dockOnce(t, c, s).Close()

	edge.mu.Lock()
	defer edge.mu.Unlock()
	if len(edge.configs) != 3 {
		t.Fatalf("%d docks", len(edge.configs))
	}
	for i, cfg := range edge.configs {
		if cfg.SessionCache == nil || cfg.SessionCache != s.Lineage.Sessions() {
			t.Fatalf("dock %d: not the share's session cache", i)
		}
		if cfg.MaxIncomingStreams != 256 {
			t.Fatalf("dock %d accepts %d lane streams, want 256", i, cfg.MaxIncomingStreams)
		}
		if i == 0 {
			if cfg.Predecessor != nil {
				t.Fatal("the first dock names a predecessor")
			}
			continue
		}
		if cfg.Predecessor == nil || !reflect.DeepEqual(*cfg.Predecessor, edge.docks[i-1].gen) {
			t.Fatalf("dock %d names %v, want the generation of dock %d", i, cfg.Predecessor != nil, i-1)
		}
	}
	if strings.Contains(logs.String(), "secret-nonce") {
		t.Fatal("a generation reached the log")
	}
}

// An overloaded edge's hint reaches the engine; any other refusal is plain
// unavailability. Neither changes the lineage.
func TestRefusedDockIsUnavailable(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{dock.ErrOverloaded{RetryAfter: 7 * time.Second}, errors.New("dock: dial: timeout")} {
		edge := &fakeEdge{refuse: refusal}
		c := &Client{openDock: edge.open}
		s := dockServing(t)
		_, err := c.ConnectServing(context.Background(), api.InstallationCredential{}, "shr_q7m2p4xk", s)
		var u *engine.Unavailable
		if !errors.As(err, &u) {
			t.Fatalf("%v: got %v", refusal, err)
		}
		var want time.Duration
		if errors.As(refusal, new(dock.ErrOverloaded)) {
			want = 7 * time.Second
		}
		if u.RetryAfter != want || s.Lineage.Predecessor() != nil {
			t.Fatalf("%v: retry after %s, predecessor %v", refusal, u.RetryAfter, s.Lineage.Predecessor() != nil)
		}
	}
}
