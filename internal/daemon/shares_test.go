package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// sharePlatform admits every share and lets the test speak for its proxy.
type sharePlatform struct {
	mu      sync.Mutex
	created []api.CreateShareRequest
	conn    chan engine.ConnEvent
	refuse  string // a target that refuses its probe
}

type shareConn struct{ ch chan engine.ConnEvent }

func (c shareConn) Events() <-chan engine.ConnEvent { return c.ch }
func (shareConn) Close() error                      { return nil }

func (p *sharePlatform) CreateShare(_ context.Context, _ api.InstallationCredential, r api.CreateShareRequest) (*api.ShareAccess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, r)
	now := time.Now().UTC()
	out := &api.ShareAccess{Share: resource.Share{ID: "shr_k7m2p4qx", Origin: "https://k7m2p4qx.content.example.invalid", Recipients: r.Recipients, CreatedAt: now, ExpiresAt: now.Add(r.TTL)}, URL: "https://k7m2p4qx.entry.example.invalid/"}
	for _, email := range r.Recipients {
		out.Invites = append(out.Invites, api.InviteResult{Email: email, Status: api.InviteSent})
	}
	return out, nil
}
func (p *sharePlatform) Revoke(context.Context, api.InstallationCredential, string) error { return nil }
func (p *sharePlatform) ShareActive(context.Context, api.InstallationCredential, string) (bool, error) {
	return true, nil
}
func (p *sharePlatform) ConnectServing(context.Context, api.InstallationCredential, string, engine.Serving) (engine.Connection, error) {
	p.conn <- engine.ConnEvent{Kind: engine.ConnReady}
	return shareConn{p.conn}, nil
}
func (p *sharePlatform) Probe(_ context.Context, target string) error {
	if target == p.refuse {
		return errors.New("connection refused")
	}
	return nil
}

// serveShares runs an in-process daemon server over the real private transport.
func serveShares(t *testing.T, platform *sharePlatform) func() *ipc.Client {
	t.Helper()
	p, err := Resolve(func(k string) string {
		if k == EnvStateDir {
			return filepath.Join(t.TempDir(), "state")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err = ensureSocketDir(p.Endpoint); err != nil {
		t.Fatal(err)
	}
	l, err := ipc.Listen(p.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	shares := engine.New(engine.Config{Platform: engine.ComposedPlatform{Management: platform, TunnelClient: platform}, Prober: platform})
	srv, err := NewServer(ServerConfig{Shares: shares, Paths: p, Instance: "test", IdleExit: time.Minute, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { srv.Serve(ctx, l); close(served) }()
	t.Cleanup(func() { stop(); <-served })
	return func() *ipc.Client {
		raw, err := ipc.Dial(ctx, p.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		c := ipc.NewClient(raw)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

func startParams(attempt string, recipients []string) ipc.ShareStartParams {
	return ipc.ShareStartParams{Attempt: attempt, Owner: "detached", Target: "localhost:3000", TTLMs: 60000, Recipients: recipients, Credential: ipc.ShareCredential{Account: "creator@example.invalid", AccountID: "acc_test", Device: "dev_test", DeviceLabel: "test", Token: "synthetic-installation-secret"}}
}

// The private protocol carries several recipients, the recorded invite results
// and the visitor and app events from the engine to a subscribed CLI.
func TestShareProtocolCarriesRecipientsInvitesAndAppEvents(t *testing.T) {
	platform := &sharePlatform{conn: make(chan engine.ConnEvent, 8)}
	c := serveShares(t, platform)()
	ctx := context.Background()
	if _, err := c.Hello(ctx, "test", ipc.BuildIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, ipc.OpSubscribe, nil, nil); err != nil {
		t.Fatal(err)
	}
	var err error
	start := func(attempt string, recipients []string) (ipc.ShareRecord, error) {
		var rec ipc.ShareRecord
		e := c.Call(ctx, ipc.OpShareStart, startParams(attempt, recipients), &rec)
		return rec, e
	}
	many := make([]string, resource.MaxRecipients+1)
	for i := range many {
		many[i] = string(rune('a'+i)) + "@example.invalid"
	}
	for _, refused := range [][]string{{"Ana@example.invalid"}, {"ana@example.invalid", "ana@example.invalid"}, {"nobody"}, {""}, many} {
		if _, err = start("refused", refused); !ipc.IsCode(err, ipc.CodeBadRequest) {
			t.Fatalf("recipients %q: %v", refused, err)
		}
	}
	recipients := []string{"ana@example.invalid", "raj@example.invalid"}
	rec, err := start("attempt", recipients)
	if err != nil || !slices.Equal(rec.Recipients, recipients) {
		t.Fatalf("start: %+v %v", rec, err)
	}
	var kinds []string
	next := func() ipc.ShareEventData {
		t.Helper()
		select {
		case ev := <-c.Events():
			var data ipc.ShareEventData
			if ev.Event != ipc.EventShare || json.Unmarshal(ev.Data, &data) != nil {
				t.Fatalf("event %+v", ev)
			}
			kinds = append(kinds, data.Kind)
			return data
		case <-time.After(5 * time.Second):
			t.Fatalf("events so far: %v", kinds)
		}
		return ipc.ShareEventData{}
	}
	ready := next()
	invites := []ipc.ShareInvite{{Email: "ana@example.invalid", Status: "sent"}, {Email: "raj@example.invalid", Status: "sent"}}
	if ready.Kind != "ready" || !slices.Equal(ready.Share.Recipients, recipients) || !slices.Equal(ready.Share.Invites, invites) {
		t.Fatalf("ready record %+v", ready)
	}
	// The observations share the channel that the fake platform's Connect
	// feeds ConnReady into, so they must wait for the ready event: pushed
	// while the engine is still connecting they precede ConnReady, and the
	// engine treats anything else before ready as a lost connection and
	// retries with 1 s, 2 s and 4 s of backoff, longer than this test waits.
	platform.conn <- engine.ConnEvent{Kind: engine.ConnFirstVisitor, Detail: "localhost:3000"}
	platform.conn <- engine.ConnEvent{Kind: engine.ConnAppUnresponsive, Detail: "localhost:3000"}
	platform.conn <- engine.ConnEvent{Kind: engine.ConnAppResponding, Detail: "localhost:3000"}
	for range 3 {
		if data := next(); data.Detail != "localhost:3000" || data.Share.State != "ready" {
			t.Fatalf("event %+v", data)
		}
	}
	if want := []string{"ready", "first_visitor", "app_unresponsive", "app_responding"}; !slices.Equal(kinds, want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.created) != 1 || !slices.Equal(platform.created[0].Recipients, recipients) {
		t.Fatalf("the platform saw %+v", platform.created)
	}
}

// Version 1 named one "recipient", which this daemon would not see and would
// therefore share with anyone: a client of another version gets no share
// operations, only the means to retire the daemon.
func TestShareOperationsNeedTheSameProtocol(t *testing.T) {
	platform := &sharePlatform{conn: make(chan engine.ConnEvent, 8)}
	c := serveShares(t, platform)()
	ctx := context.Background()
	err := c.Call(ctx, ipc.OpHello, ipc.HelloParams{Protocol: 1, Client: "test"}, nil)
	if !ipc.IsCode(err, ipc.CodeProtocolMismatch) {
		t.Fatalf("hello from protocol 1: %v", err)
	}
	err = c.Call(ctx, ipc.OpShareStart, json.RawMessage(`{"attempt":"old","owner":"detached","target":"localhost:3000","ttl_ms":60000,"recipient":"ana@example.invalid","credential":{"device":"dev_test","token":"synthetic-installation-secret"}}`), nil)
	if !ipc.IsCode(err, ipc.CodeProtocolMismatch) {
		t.Fatalf("share.start from protocol 1: %v", err)
	}
	var status ipc.StatusResult
	if err = c.Call(ctx, ipc.OpStatus, nil, &status); err != nil {
		t.Fatalf("status must stay available: %v", err)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.created) != 0 {
		t.Fatal("a share was created for a client of another protocol")
	}
}

// The record carries the app that did not answer, so a CLI that learns of the
// end from a start reply or a stop result, not the ended event, names it.
func TestShareRecordNamesTheAppThatDidNotAnswer(t *testing.T) {
	platform := &sharePlatform{conn: make(chan engine.ConnEvent, 8), refuse: "http://localhost:8000"}
	c := serveShares(t, platform)()
	ctx := context.Background()
	if _, err := c.Hello(ctx, "test", ipc.BuildIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, ipc.OpSubscribe, nil, nil); err != nil {
		t.Fatal(err)
	}
	params := startParams("further", nil)
	params.Target, params.Targets = "localhost:5173", []string{"localhost:5173", "localhost:8000"}
	if err := c.Call(ctx, ipc.OpShareStart, params, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-c.Events():
		var data ipc.ShareEventData
		if ev.Event != ipc.EventShare || json.Unmarshal(ev.Data, &data) != nil || data.Kind != "ended" || data.Share.EndApp != "localhost:8000" {
			t.Fatalf("event %+v (%s)", data, ev.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the share did not end")
	}
	var res ipc.ShareStopResult
	if err := c.Call(ctx, ipc.OpShareStop, ipc.ShareStopParams{Attempt: "further"}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Share.EndReason != "target_unreachable" || res.Share.EndApp != "localhost:8000" || res.Share.Detail != "connection refused" {
		t.Fatalf("ended record %+v", res.Share)
	}
}
