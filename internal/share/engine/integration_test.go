package engine_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/internal/testplatform"
	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/apiserver"
	"github.com/idyl-labs/purlview/sdk/purlview"
)

type integrationTunnel struct {
	calls  atomic.Int32
	events chan engine.ConnEvent
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	served []engine.Serving // what the engine passed on each connect
}

// ConnectServing records what the engine passes with the real SDK behind it:
// the share's public origin from the create response, the target list and
// whether bodies are rewritten.
func (t *integrationTunnel) ConnectServing(_ context.Context, _ api.InstallationCredential, _ string, s engine.Serving) (engine.Connection, error) {
	t.calls.Add(1)
	t.mu.Lock()
	t.served = append(t.served, s)
	t.mu.Unlock()
	return t, nil
}
func (t *integrationTunnel) Events() <-chan engine.ConnEvent { return t.events }
func (t *integrationTunnel) Close() error                    { t.once.Do(func() { close(t.closed) }); return nil }
func integrationEngine(t *testing.T, h http.Handler, tunnel *integrationTunnel) (*engine.Engine, share.Session) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	c, err := purlview.New(purlview.Config{Endpoint: server.URL, Host: "account.example.invalid", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	e := engine.New(engine.Config{Platform: engine.ComposedPlatform{Management: engine.SDKManagement{Client: c}, TunnelClient: tunnel}, Prober: okProber{}, Logger: log.New(io.Discard, "", 0), RevokeTimeout: time.Second})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		e.Shutdown(ctx)
	})
	s, err := e.Open(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	return e, s
}
func nextHTTPEvent(t *testing.T, s share.Session) share.Event {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("engine event deadline")
		return share.Event{}
	}
}
func startHTTPShare(t *testing.T, s share.Session) {
	t.Helper()
	_, err := s.Start(context.Background(), share.StartRequest{Attempt: "one_intentional_attempt", Spec: share.Spec{Targets: []share.Target{{URL: "http://localhost:3000/demo?version=2"}}, TTL: time.Hour}, Owner: share.OwnerDetached, Credential: testplatform.Credential()})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSDKEngineLostCreateReplyReconcilesSameAttemptAndCleansUp(t *testing.T) {
	backend := testplatform.New()
	handler := apiserver.New("account.example.invalid", backend)
	var creates atomic.Int32
	var keysMu sync.Mutex
	var keys []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == api.SharesPath {
			keysMu.Lock()
			keys = append(keys, r.Header.Get(api.IdempotencyHeader))
			keysMu.Unlock()
			if creates.Add(1) == 1 {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				if rec.Code != 200 {
					t.Errorf("create %d %s", rec.Code, rec.Body.String())
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
		}
		handler.ServeHTTP(w, r)
	})
	tunnel := &integrationTunnel{events: make(chan engine.ConnEvent, 1), closed: make(chan struct{})}
	_, s := integrationEngine(t, h, tunnel)
	startHTTPShare(t, s)
	event := nextHTTPEvent(t, s)
	if event.Kind != share.EventEnded || event.Remote.Status != share.RemoteConfirmed || event.Share.EndReason != share.ReasonPlatformUnavailable || event.Share.ID == "" {
		t.Fatalf("lost-create cleanup: %+v", event)
	}
	count, revokes, records := backend.Counts()
	if count != 2 || revokes != 1 || records != 1 || tunnel.calls.Load() != 0 {
		t.Fatalf("effects %d %d %d; tunnel %d", count, revokes, records, tunnel.calls.Load())
	}
	keysMu.Lock()
	defer keysMu.Unlock()
	if len(keys) != 2 || keys[0] != "one_intentional_attempt" || keys[0] != keys[1] {
		t.Fatalf("reconciliation changed key: %v", keys)
	}
	if strings.Contains(event.Detail, "synthetic") {
		t.Fatal("secret in event diagnostic")
	}
}

func TestSDKEngineRevocationOutcomeAndReadiness(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed", true: "confirmed"}[confirmed], func(t *testing.T) {
			backend := testplatform.New()
			if !confirmed {
				backend.RevokeHook = func(ctx context.Context, token string, r api.RevokeShareRequest) (api.RevokeShareResult, error) {
					res, err := backend.Revoke(ctx, token, r)
					res.Outcome = "unconfirmed"
					return res, err
				}
			}
			tunnel := &integrationTunnel{events: make(chan engine.ConnEvent, 1), closed: make(chan struct{})}
			_, s := integrationEngine(t, apiserver.New("account.example.invalid", backend), tunnel)
			startHTTPShare(t, s)
			// There is no engine.ConnReady until this explicit fixture event. Stop-before-ready
			// in the false case proves a record never masquerades as a serving route.
			if confirmed {
				tunnel.events <- engine.ConnEvent{Kind: engine.ConnReady}
				ev := nextHTTPEvent(t, s)
				if ev.Kind != share.EventReady || ev.Share.URL == "" {
					t.Fatalf("ready: %+v", ev)
				}
				// With the real SDK behind it, the engine tells the tunnel
				// client the share's public origin from the create response,
				// the target list and that bodies are rewritten.
				tunnel.mu.Lock()
				served := append([]engine.Serving(nil), tunnel.served...)
				tunnel.mu.Unlock()
				if len(served) != 1 || served[0].Origin == "" || served[0].Origin != ev.Share.Origin || !reflect.DeepEqual(served[0].Targets, []string{"http://localhost:3000/demo?version=2"}) || !served[0].Rewrite {
					t.Fatalf("the tunnel client was told %+v for share %+v", served, ev.Share)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			res, err := s.Stop(ctx, share.StopRequest{Attempt: "one_intentional_attempt", Reason: share.ReasonStopped})
			if err != nil {
				t.Fatal(err)
			}
			want := share.RemoteUnconfirmed
			if confirmed {
				want = share.RemoteConfirmed
			}
			if res.Remote.Status != want {
				t.Fatalf("outcome %+v", res)
			}
			ev := nextHTTPEvent(t, s)
			if ev.Kind != share.EventEnded {
				t.Fatal("announced readiness without tunnel event")
			}
		})
	}
}

func TestSDKEngineCancelledHTTPCreateReleasesAndReconciles(t *testing.T) {
	backend := testplatform.New()
	entered, released := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	backend.CreateHook = func(ctx context.Context, token string, r api.CreateShareRequest) (api.ShareAccess, error) {
		result, err := backend.Create(ctx, token, r)
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			close(released)
			return api.ShareAccess{}, ctx.Err()
		}
		return result, err
	}
	tunnel := &integrationTunnel{events: make(chan engine.ConnEvent), closed: make(chan struct{})}
	_, s := integrationEngine(t, apiserver.New("account.example.invalid", backend), tunnel)
	startHTTPShare(t, s)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("create not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := s.Stop(ctx, share.StopRequest{Attempt: "one_intentional_attempt", Reason: share.ReasonStopped})
	if err != nil || r.Remote.Status != share.RemoteConfirmed {
		t.Fatalf("cancel cleanup %+v %v", r, err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("HTTP cancellation did not reach service")
	}
	creates, revokes, records := backend.Counts()
	if creates != 2 || revokes != 1 || records != 1 {
		t.Fatal("cancel did not reconcile and revoke original record")
	}
}

func TestSDKEngineNormalBackendCannotReachTunnel(t *testing.T) {
	tunnel := &integrationTunnel{events: make(chan engine.ConnEvent), closed: make(chan struct{})}
	_, s := integrationEngine(t, apiserver.New("account.example.invalid", nil), tunnel)
	startHTTPShare(t, s)
	ev := nextHTTPEvent(t, s)
	if ev.Kind != share.EventEnded || ev.Share.ID != "" || ev.Share.URL != "" || ev.Remote.Status != share.RemoteNone || tunnel.calls.Load() != 0 || !strings.Contains(ev.Detail, "not implemented") {
		t.Fatalf("normal backend claimed product action: %+v", ev)
	}
}

type okProber struct{}

func (okProber) Probe(context.Context, string) error { return nil }
