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

package engine

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/share"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// stubPlatform is the smallest platform that can admit a share. The full
// stateful platform lives in package scenario.
type stubPlatform struct {
	mu       sync.Mutex
	clock    *clock.Fake
	created  map[string]*api.ShareAccess
	revoked  map[string]bool
	creates  int
	revokes  int
	conns    map[string]*stubConn
	holdCall chan struct{} // when set, CreateShare blocks until closed
	held     chan struct{} // receives once CreateShare is blocked on holdCall
	down     bool
	// invite makes create report invite results: sent, then not sent.
	invite     bool
	recipients []string
	origins    []string // what each admitted connection was told
	served     []Serving
	admits     string                            // the target admission names, when set
	requests   map[string]api.CreateShareRequest // by share id
	checks     int                               // ShareActive calls
	checkErr   error                             // ShareActive's answer, when set
	connErr    error                             // ConnectServing's answer, when set
}

type stubConn struct {
	ch chan ConnEvent
}

func (c *stubConn) Events() <-chan ConnEvent { return c.ch }
func (c *stubConn) Close() error             { return nil }

func newStub(clk *clock.Fake) *stubPlatform {
	return &stubPlatform{clock: clk, created: map[string]*api.ShareAccess{}, revoked: map[string]bool{}, conns: map[string]*stubConn{}}
}

func (p *stubPlatform) CreateShare(ctx context.Context, _ api.InstallationCredential, req api.CreateShareRequest) (*api.ShareAccess, error) {
	p.mu.Lock()
	hold := p.holdCall
	p.mu.Unlock()
	if hold != nil {
		select {
		case p.held <- struct{}{}:
		default:
		}
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return nil, &Unavailable{Detail: "connection refused"}
	}
	p.creates++
	if rec, ok := p.created[req.Key]; ok {
		return rec, nil
	}
	now := p.clock.Now()
	rec := &api.ShareAccess{Share: resource.Share{ID: "shr_" + req.Key, Origin: "https://" + req.Key + ".content.invalid", CreatedAt: now, ExpiresAt: now.Add(req.TTL)}, URL: "https://" + req.Key + ".share.invalid"}
	p.recipients = req.Recipients
	for i, email := range req.Recipients {
		if p.invite {
			rec.Invites = append(rec.Invites, api.InviteResult{Email: email, Status: []string{api.InviteSent, api.InviteNotSent}[i%2]})
		}
	}
	if req.PublicKey != nil {
		rec.Tunnel = &api.Tunnel{Endpoint: "edge.invalid:443", Edge: "spiffe://example.invalid/edge", Gateway: "spiffe://example.invalid/gateway",
			SVID: [][]byte{[]byte("svid")}, Lease: []byte("lease"), Bundle: [][]byte{[]byte("root")}}
	}
	p.created[req.Key] = rec
	if p.requests == nil {
		p.requests = map[string]api.CreateShareRequest{}
	}
	p.requests[rec.Share.ID] = req
	return rec, nil
}

func (p *stubPlatform) ConnectServing(_ context.Context, _ api.InstallationCredential, id string, s Serving) (Connection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return nil, &Unavailable{}
	}
	if p.connErr != nil {
		p.served = append(p.served, s)
		return nil, p.connErr
	}
	if p.revoked[id] {
		return nil, &Rejected{Reason: RejectRevoked}
	}
	p.origins = append(p.origins, s.Origin)
	p.served = append(p.served, s)
	c := &stubConn{ch: make(chan ConnEvent, 4)}
	c.ch <- ConnEvent{Kind: ConnReady, Detail: p.admits}
	p.conns[id] = c
	return c, nil
}

func (p *stubPlatform) Revoke(_ context.Context, _ api.InstallationCredential, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return &Unavailable{Detail: "connection refused"}
	}
	p.revokes++
	p.revoked[id] = true
	if c, ok := p.conns[id]; ok {
		c.ch <- ConnEvent{Kind: ConnRevoked}
	}
	return nil
}

// ShareActive answers as the platform's list would: an unrevoked share
// before its expiry. checkErr, when set, is the answer instead.
func (p *stubPlatform) ShareActive(_ context.Context, _ api.InstallationCredential, id string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	if p.checkErr != nil {
		return false, p.checkErr
	}
	for _, rec := range p.created {
		if rec.Share.ID == id {
			return !p.revoked[id] && p.clock.Now().Before(rec.Share.ExpiresAt), nil
		}
	}
	return false, nil
}

func (p *stubPlatform) drop(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[id]; ok {
		c.ch <- ConnEvent{Kind: ConnLost}
		delete(p.conns, id)
	}
}

func (p *stubPlatform) observe(id string, kinds ...ConnEventKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, kind := range kinds {
		p.conns[id].ch <- ConnEvent{Kind: kind, Detail: "localhost:3000"}
	}
}

// Each serving connection reports what it sees from scratch. The share tells
// its subscribers about the first visitor once and alternates unresponsive and
// responding, starting from responding, also across a reconnect.
func TestVisitorAndAppEventsAreReducedToTheShare(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "v1", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	id := next(t, sess, share.EventReady).Share.ID
	var got []share.EventKind
	collect := func(until int) {
		t.Helper()
		for len(got) < until {
			select {
			case ev := <-sess.Events():
				if ev.Share.ID != id || ev.Share.State == share.StateEnded {
					t.Fatalf("event %+v", ev)
				}
				if ev.Kind != share.EventConnectionLost && ev.Kind != share.EventRestored && ev.Detail != "localhost:3000" {
					t.Fatalf("event %+v does not name the app", ev)
				}
				got = append(got, ev.Kind)
			case <-time.After(5 * time.Second):
				t.Fatalf("events so far: %v", got)
			}
		}
	}
	p.observe(id, ConnFirstVisitor, ConnAppResponding, ConnAppUnresponsive, ConnAppUnresponsive)
	collect(2)
	clk.Advance(DefaultReconnectMax) // a dock that stayed up redocks at once
	p.drop(id)
	collect(4)
	p.observe(id, ConnFirstVisitor, ConnAppUnresponsive, ConnAppResponding, ConnAppResponding, ConnAppUnresponsive)
	p.mu.Lock()
	p.conns[id].ch <- ConnEvent{Kind: ConnAppResponding} // names no app: the share's target stands in
	p.mu.Unlock()
	collect(7)
	want := []share.EventKind{share.EventFirstVisitor, share.EventAppUnresponsive, share.EventConnectionLost, share.EventRestored, share.EventAppResponding, share.EventAppUnresponsive, share.EventAppResponding}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if snap := e.list()[0]; snap.State != share.StateReady {
		t.Fatalf("observations must not change the share's state: %+v", snap)
	}
}

func TestCreateCarriesRecipientsAndRecordsInviteResults(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	p.invite = true
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	s := spec(t)
	s.Recipients = []string{"ana@example.invalid", "raj@example.invalid"}
	started, err := sess.Start(context.Background(), share.StartRequest{Attempt: "i1", Spec: s, Owner: share.OwnerDetached, Credential: cred})
	if err != nil || !slices.Equal(started.Recipients, s.Recipients) {
		t.Fatalf("start: %+v %v", started, err)
	}
	ready := next(t, sess, share.EventReady).Share
	want := []share.Invite{{Email: "ana@example.invalid", Status: api.InviteSent}, {Email: "raj@example.invalid", Status: api.InviteNotSent}}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Equal(ready.Recipients, s.Recipients) || !slices.Equal(ready.Invites, want) || !slices.Equal(p.recipients, s.Recipients) {
		t.Fatalf("ready share %+v; platform saw %v", ready, p.recipients)
	}
}

type okProber struct{}

func (okProber) Probe(context.Context, string) error { return nil }

var cred = api.InstallationCredential{Identity: resource.Identity{Account: "creator@example.invalid", Device: "dev_1", DeviceLabel: "studio"}, Token: "t"}

func spec(t *testing.T) share.Spec {
	t.Helper()
	target, err := share.ParseTarget("localhost:3000")
	if err != nil {
		t.Fatal(err)
	}
	return share.Spec{Targets: []share.Target{target}, TTL: time.Hour}
}

func next(t *testing.T, sess share.Session, kind share.EventKind) share.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("events closed while waiting for %s", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event", kind)
		}
	}
}

func TestRepeatedAttemptReturnsTheSameShare(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	req := share.StartRequest{Attempt: "a1", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred}
	first, err := sess.Start(context.Background(), req)
	// The stub admits synchronously, so the first snapshot is starting or
	// already ready depending on scheduling; what matters is the repeat.
	if err != nil || (first.State != share.StateStarting && first.State != share.StateReady) {
		t.Fatalf("start: %+v %v", first, err)
	}
	ready := next(t, sess, share.EventReady)
	again, err := sess.Start(context.Background(), req)
	if err != nil || again.ID != ready.Share.ID || again.State != share.StateReady {
		t.Fatalf("repeated start must return the ready share: %+v %v", again, err)
	}
	if p.creates != 1 || e.Active() != 1 {
		t.Fatalf("creates=%d active=%d", p.creates, e.Active())
	}
	// A second intentional attempt for the same target is a second share.
	other, _ := sess.Start(context.Background(), share.StartRequest{Attempt: "a2", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred})
	next(t, sess, share.EventReady)
	if other.Attempt == first.Attempt || e.Active() != 2 {
		t.Fatal("a new attempt must create a new share")
	}
}

func TestAttachedShareEndsWithSessionAndOthersContinue(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	a, _ := e.Open(context.Background(), true)
	b, _ := e.Open(context.Background(), true)
	if _, err := a.Start(context.Background(), share.StartRequest{Attempt: "fg", Spec: spec(t), Owner: share.OwnerAttached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	fg := next(t, b, share.EventReady)
	if _, err := a.Start(context.Background(), share.StartRequest{Attempt: "bg", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	next(t, b, share.EventReady)
	_ = a.Close()
	ended := next(t, b, share.EventEnded)
	if ended.Share.ID != fg.Share.ID || ended.Share.EndReason != share.ReasonCLIDisconnected || ended.Remote.Status != share.RemoteConfirmed {
		t.Fatalf("attached share must end with cli_disconnected and confirmed revocation: %+v", ended)
	}
	shares, _ := b.Shares(context.Background())
	active := 0
	for _, s := range shares {
		if s.Active() {
			active++
			if s.Attempt != "bg" {
				t.Fatalf("unexpected active share %+v", s)
			}
		}
	}
	if active != 1 {
		t.Fatalf("active=%d, want the detached share only", active)
	}
	res, err := b.Stop(context.Background(), share.StopRequest{ID: fg.Share.ID})
	if err != nil || !res.AlreadyEnded || res.Share.EndReason != share.ReasonCLIDisconnected {
		t.Fatalf("stop after end: %+v %v", res, err)
	}
	if _, err := b.Stop(context.Background(), share.StopRequest{ID: "shr_nope"}); !share.IsKind(err, share.KindNotFound) {
		t.Fatalf("unknown share: %v", err)
	}
}

func TestReconnectKeepsIdentityAndExpiryIsLocal(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	p := newStub(clk)
	e := New(Config{Platform: ComposedPlatform{Management: p, TunnelClient: p}, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "r1", Spec: share.Spec{Targets: spec(t).Targets, TTL: 15 * time.Minute}, Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	ready := next(t, sess, share.EventReady)
	p.drop(ready.Share.ID)
	lost := next(t, sess, share.EventConnectionLost)
	if lost.Share.State != share.StateReconnecting {
		t.Fatalf("state %s", lost.Share.State)
	}
	// The first retry waits for the back-off on the fake clock.
	p.mu.Lock()
	p.down = true
	p.mu.Unlock()
	clk.Advance(time.Second)
	time.Sleep(20 * time.Millisecond)
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
	clk.Advance(2 * time.Second)
	restored := next(t, sess, share.EventRestored)
	// Every connection, also behind a composed platform, is told the origin
	// the create response named.
	p.mu.Lock()
	origins := slices.Clone(p.origins)
	p.mu.Unlock()
	if !slices.Equal(origins, []string{"https://r1.content.invalid", "https://r1.content.invalid"}) || ready.Share.Origin != origins[0] {
		t.Fatalf("connections were told %v", origins)
	}
	if restored.Share.ID != ready.Share.ID || restored.Share.URL != ready.Share.URL || !restored.Share.ExpiresAt.Equal(ready.Share.ExpiresAt) {
		t.Fatalf("restored share changed identity: %+v vs %+v", restored.Share, ready.Share)
	}
	// Local expiry fires from the fake clock even with no platform event.
	clk.Advance(15 * time.Minute)
	ended := next(t, sess, share.EventEnded)
	if ended.Share.EndReason != share.ReasonExpired || ended.Remote.Status != share.RemoteNone {
		t.Fatalf("expiry: %+v", ended)
	}
	if p.revokes != 0 {
		t.Fatal("expiry must not call revoke")
	}
}

// Several targets: every target is probed before the share exists and
// the first that refuses names the end; the create names the whole list and
// no rewrite flag; every connection is told the list and whether to rewrite;
// the app state is kept per target while the first visitor stays per share;
// and an admission that names another target than the first listed one makes
// the record name that target alone.
func TestSeveralTargets(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	probe := &recordingProber{refuse: "http://localhost:8000"}
	e := New(Config{Platform: p, Prober: probe, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	targets := []share.Target{{URL: "http://localhost:5173/app?x=1"}, {URL: "http://localhost:8000"}, {URL: "https://staging.example.invalid"}}
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "t1", Spec: share.Spec{Targets: targets, TTL: time.Hour, NoRewrite: true}, Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	ended := next(t, sess, share.EventEnded)
	if ended.Share.EndReason != share.ReasonTargetUnreachable || ended.Detail != "localhost:8000" || ended.Share.EndApp != "localhost:8000" || ended.Share.Detail != "refused" || p.creates != 0 {
		t.Fatalf("a refusing further target: %+v (creates %d)", ended, p.creates)
	}
	// A start reply or stop result that already sees the end carries only
	// the record, which names the app as the event does.
	if again, err := sess.Start(context.Background(), share.StartRequest{Attempt: "t1", Spec: share.Spec{Targets: targets, TTL: time.Hour, NoRewrite: true}, Owner: share.OwnerDetached, Credential: cred}); err != nil || again.EndApp != "localhost:8000" {
		t.Fatalf("a repeated start after the end: %+v %v", again, err)
	}
	if res, err := sess.Stop(context.Background(), share.StopRequest{Attempt: "t1"}); err != nil || res.Share.EndApp != "localhost:8000" {
		t.Fatalf("a stop after the end: %+v %v", res, err)
	}
	probe.mu.Lock()
	probed := slices.Clone(probe.probed)
	probe.mu.Unlock()
	slices.Sort(probed)
	if !slices.Equal(probed, []string{"http://localhost:5173/app?x=1", "http://localhost:8000", "https://staging.example.invalid"}) {
		t.Fatalf("probed %v", probed)
	}
	probe.mu.Lock()
	probe.refuse = ""
	probe.mu.Unlock()
	started, err := sess.Start(context.Background(), share.StartRequest{Attempt: "t2", Spec: share.Spec{Targets: targets, TTL: time.Hour, NoRewrite: true}, Owner: share.OwnerDetached, Credential: cred})
	if err != nil || started.Target != targets[0].URL || !slices.Equal(started.Targets, share.TargetURLs(targets)) || !started.NoRewrite {
		t.Fatalf("started %+v %v", started, err)
	}
	ready := next(t, sess, share.EventReady)
	p.mu.Lock()
	req, served := p.requests[ready.Share.ID], p.served
	p.mu.Unlock()
	if !slices.Equal(req.Targets, share.TargetURLs(targets)) || req.Target != targets[0].URL || req.RewriteURLs {
		t.Fatalf("create request %+v", req)
	}
	if len(served) != 1 || served[0].Origin != ready.Share.Origin || !slices.Equal(served[0].Targets, share.TargetURLs(targets)) || served[0].Rewrite {
		t.Fatalf("connection told %+v", served)
	}
	if !slices.Equal(ready.Share.Targets, share.TargetURLs(targets)) {
		t.Fatalf("ready record %+v", ready.Share)
	}
	id := ready.Share.ID
	var got []string
	collect := func(n int) {
		t.Helper()
		for len(got) < n {
			select {
			case ev := <-sess.Events():
				if ev.Share.ID == id && ev.Share.State != share.StateEnded {
					got = append(got, string(ev.Kind)+" "+ev.Detail)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("events so far: %v", got)
			}
		}
	}
	p.mu.Lock()
	for _, ev := range []ConnEvent{
		{Kind: ConnFirstVisitor, Detail: "localhost:5173"},
		{Kind: ConnAppUnresponsive, Detail: "localhost:8000"},
		{Kind: ConnFirstVisitor, Detail: "localhost:8000"},
		{Kind: ConnAppUnresponsive, Detail: "localhost:5173"},
		{Kind: ConnAppResponding, Detail: "localhost:8000"},
		{Kind: ConnAppResponding, Detail: "localhost:8000"},
		{Kind: ConnAppResponding, Detail: "localhost:5173"},
	} {
		p.conns[id].ch <- ev
	}
	p.mu.Unlock()
	collect(5)
	want := []string{"first_visitor localhost:5173", "app_unresponsive localhost:8000", "app_unresponsive localhost:5173", "app_responding localhost:8000", "app_responding localhost:5173"}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	// Admission names another target than the first listed: the record then
	// names that one alone, so the block prints no further app.
	p.mu.Lock()
	p.admits = "http://localhost:3000"
	p.mu.Unlock()
	if _, err = sess.Start(context.Background(), share.StartRequest{Attempt: "t3", Spec: share.Spec{Targets: targets, TTL: time.Hour}, Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	ready = next(t, sess, share.EventReady)
	if ready.Share.Target != "http://localhost:3000" || ready.Share.Targets != nil {
		t.Fatalf("mismatched admission: %+v", ready.Share)
	}
	// An admission naming the first target under another loopback name is
	// the same target.
	p.mu.Lock()
	p.admits = "http://127.0.0.1:5173"
	p.mu.Unlock()
	if _, err = sess.Start(context.Background(), share.StartRequest{Attempt: "t4", Spec: share.Spec{Targets: targets, TTL: time.Hour}, Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	if ready = next(t, sess, share.EventReady); !slices.Equal(ready.Share.Targets, share.TargetURLs(targets)) {
		t.Fatalf("aliased admission: %+v", ready.Share)
	}
}

type recordingProber struct {
	mu     sync.Mutex
	probed []string
	refuse string
}

func (r *recordingProber) Probe(_ context.Context, target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probed = append(r.probed, target)
	if target == r.refuse {
		return errors.New("refused")
	}
	return nil
}

func TestCancelDuringCreateRecoversAndRevokes(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	p.holdCall, p.held = make(chan struct{}), make(chan struct{}, 1)
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "c1", Spec: spec(t), Owner: share.OwnerAttached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	<-p.held // the op is blocked in CreateShare
	p.mu.Lock()
	p.holdCall = nil
	p.mu.Unlock()
	res, err := sess.Stop(context.Background(), share.StopRequest{Attempt: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Share.EndReason != share.ReasonCancelled || res.Remote.Status != share.RemoteConfirmed || res.Share.ID == "" {
		t.Fatalf("cancelled create must be recovered and revoked: %+v", res)
	}
	if p.creates != 1 || p.revokes != 1 {
		t.Fatalf("creates=%d revokes=%d", p.creates, p.revokes)
	}
}

func TestCancelDuringCreateWithPlatformDownIsUncertain(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	p.holdCall, p.held = make(chan struct{}), make(chan struct{}, 1)
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "c2", Spec: spec(t), Owner: share.OwnerAttached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	<-p.held
	p.mu.Lock()
	p.holdCall = nil
	p.down = true
	p.mu.Unlock()
	res, err := sess.Stop(context.Background(), share.StopRequest{Attempt: "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Share.EndReason != share.ReasonCancelled || res.Remote.Status != share.RemoteUnconfirmed {
		t.Fatalf("uncertain cleanup must be reported: %+v", res)
	}
}

func TestShutdownEndsEverythingAndClosesSessions(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "s1", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	next(t, sess, share.EventReady)
	e.Shutdown(context.Background())
	ended := next(t, sess, share.EventEnded)
	if ended.Share.EndReason != share.ReasonDaemonShutdown {
		t.Fatalf("reason %s", ended.Share.EndReason)
	}
	if _, ok := <-sess.Events(); ok {
		t.Fatal("events must close after shutdown")
	}
	if _, err := e.Open(context.Background(), true); !errors.Is(err, share.ErrSessionClosed) {
		t.Fatalf("open after shutdown: %v", err)
	}
}

// A denied create keeps the limit the platform named; any other refusal
// carries none, so the CLI words only a real limit as one.
func TestDeniedCreateKeepsTheNamedLimit(t *testing.T) {
	for _, tc := range []struct {
		err  *api.Error
		want string
	}{
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied, Limit: api.LimitRunningShares}, "running_shares"},
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied, Limit: api.LimitSharesPerHour}, "shares_per_hour"},
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied}, ""},
		{&api.Error{Code: api.InvalidRequest, Outcome: api.NotApplied, Limit: api.LimitRunningShares}, ""},
	} {
		rej := AsRejected(managementError(tc.err))
		if rej == nil || rej.Reason != RejectRequest || rej.Limit != tc.want {
			t.Fatalf("%+v: got %+v, want limit %q", tc.err, rej, tc.want)
		}
	}
}

// update_required is its own refusal, so the share ends as update_required
// and the CLI can say how to update rather than "try again".
func TestUpdateRequiredIsItsOwnRefusal(t *testing.T) {
	rej := AsRejected(managementError(&api.Error{Code: api.UpdateRequired, Outcome: api.NotApplied}))
	if rej == nil || rej.Reason != RejectUpdate || rej.Limit != "" {
		t.Fatalf("got %+v, want an update refusal", rej)
	}
}

// Each share has its own key: every create of the attempt sends its public
// half, and every dock, redocks included, gets the tunnel material with the
// private half and the one lineage its docks hand on.
func TestShareKeysReachCreateAndServing(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	p := newStub(clk)
	e := New(Config{Platform: p, Prober: okProber{}, Clock: clk})
	sess, _ := e.Open(context.Background(), true)
	if _, err := sess.Start(context.Background(), share.StartRequest{Attempt: "k1", Spec: spec(t), Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	ready := next(t, sess, share.EventReady).Share
	clk.Advance(DefaultReconnectMax) // a dock that stayed up redocks at once
	p.drop(ready.ID)
	next(t, sess, share.EventConnectionLost)
	next(t, sess, share.EventRestored)
	p.mu.Lock()
	sent := p.requests[ready.ID].PublicKey
	served := slices.Clone(p.served)
	p.mu.Unlock()
	if len(served) != 2 {
		t.Fatalf("%d docks", len(served))
	}
	for _, s := range served {
		if len(sent) != ed25519.PublicKeySize || s.Tunnel == nil || s.Key == nil || !s.Key.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(sent)) {
			t.Fatalf("sent %x, served %+v", sent, s)
		}
		if s.Lineage == nil || s.Lineage != served[0].Lineage || s.Lineage.Sessions() == nil {
			t.Fatalf("every dock of a share must get its one lineage: %p, first %p", s.Lineage, served[0].Lineage)
		}
	}
}
