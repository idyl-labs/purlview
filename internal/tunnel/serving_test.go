package tunnel

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// The path the live daemon takes, end to end but for the transport: the
// engine creates the share through its management boundary, calls the tunnel
// client with what it passes, and the proxy is built exactly as
// ConnectServing builds it from that and from the first target. The
// header translation of a one-target share must survive all of it, on a
// public origin with an explicit port.

// recordingManagement is the platform's create as the engine sees it: a
// share whose origin is the public content origin, with an explicit port.
type recordingManagement struct{ origin, target string }

func (m recordingManagement) CreateShare(_ context.Context, _ api.InstallationCredential, req api.CreateShareRequest) (*api.ShareAccess, error) {
	now := time.Now()
	return &api.ShareAccess{Share: resource.Share{ID: "shr_qvrvm4qb", Origin: m.origin, EntryOrigin: "https://qvrvm4qb.purlview.test:8443", Target: req.Target, Targets: req.Targets, Device: "dev_test", DeviceLabel: "test", State: "starting", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, URL: "https://qvrvm4qb.purlview.test:8443/?token=SyntheticToken00000000"}, nil
}

func (recordingManagement) Revoke(context.Context, api.InstallationCredential, string) error {
	return nil
}

func (recordingManagement) ShareActive(context.Context, api.InstallationCredential, string) (bool, error) {
	return true, nil
}

// recordingTunnel is the tunnel client as the engine calls it; it records
// the Serving it was given and admits at once.
type recordingTunnel struct {
	served chan engine.Serving
}

func (t *recordingTunnel) ConnectServing(_ context.Context, _ api.InstallationCredential, _ string, s engine.Serving) (engine.Connection, error) {
	t.served <- s
	c := &recordedConn{events: make(chan engine.ConnEvent, 1)}
	c.events <- engine.ConnEvent{Kind: engine.ConnReady}
	return c, nil
}

type recordedConn struct{ events chan engine.ConnEvent }

func (c *recordedConn) Events() <-chan engine.ConnEvent { return c.events }
func (c *recordedConn) Close() error                    { return nil }

type okProber struct{}

func (okProber) Probe(context.Context, string) error { return nil }

func TestServingFromTheEngineTranslatesAOneTargetShare(t *testing.T) {
	seen := make(chan http.Header, 4)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		h.Set("Host", r.Host)
		seen <- h
		w.Header().Set("Access-Control-Allow-Origin", "http://"+r.Host)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<a href='http://"+r.Host+"/x'>self</a> and http://localhost:9999/")
	}))
	defer app.Close()
	const public = "https://qvrvm4qb.purlview-content.test:8443"
	targetURL := app.URL // http://127.0.0.1:<port>, as ParseTarget resolves "127.0.0.1:<port>"
	tunnel := &recordingTunnel{served: make(chan engine.Serving, 2)}
	e := engine.New(engine.Config{Platform: engine.ComposedPlatform{Management: recordingManagement{origin: public, target: targetURL}, TunnelClient: tunnel}, Prober: okProber{}, Logger: log.New(io.Discard, "", 0)})
	sess, err := e.Open(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := share.ParseTargets([]string{strings.TrimPrefix(targetURL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	cred := api.InstallationCredential{Identity: resource.Identity{Account: "dev@example.invalid", Device: "dev_test", DeviceLabel: "test"}, Token: "t"}
	if _, err = sess.Start(context.Background(), share.StartRequest{Attempt: "a1", Spec: share.Spec{Targets: targets, TTL: time.Hour}, Owner: share.OwnerDetached, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	var s engine.Serving
	select {
	case s = <-tunnel.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the engine never connected")
	}
	if s.Origin != public || !slices.Equal(s.Targets, []string{targetURL}) || !s.Rewrite {
		t.Fatalf("the engine passed %+v", s)
	}
	// What ConnectServing does with it, from the first target: the
	// target's origin.
	admitted, _ := url.Parse(targetURL)
	front := httptest.NewServer((&Client{}).proxyFor("shr_qvrvm4qb", admitted, s))
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/seen", nil)
	req.Header.Set("Origin", public)
	req.Header.Set("Referer", public+"/form?a=1")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	got := <-seen
	if got.Get("Origin") != targetURL || got.Get("Referer") != targetURL+"/form?a=1" || got.Get("Host") != admitted.Host {
		t.Fatalf("the app saw Origin %q Referer %q Host %q", got.Get("Origin"), got.Get("Referer"), got.Get("Host"))
	}
	if res.Header.Get("Access-Control-Allow-Origin") != public || string(body) != "<a href='"+public+"/x'>self</a> and http://localhost:9999/" {
		t.Fatalf("to the browser: %v %q", res.Header, body)
	}
	// A Serving without an origin translates nothing and rewrites nothing.
	plain := httptest.NewServer((&Client{}).proxyFor("shr_qvrvm4qb", admitted, engine.Serving{}))
	defer plain.Close()
	req, _ = http.NewRequest(http.MethodPost, plain.URL+"/seen", nil)
	req.Header.Set("Origin", public)
	if res, err = http.DefaultTransport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if got = <-seen; got.Get("Origin") != public {
		t.Fatalf("without a public origin the app saw Origin %q", got.Get("Origin"))
	}
}
