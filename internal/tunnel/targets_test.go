package tunnel

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/share/engine"
)

// single is a one-target share as the proxy sees it: the target at the root.
func single(o origin) (*target, []*target) {
	t := &target{origin: o, name: o.host + ":" + o.port, url: &url.URL{Scheme: o.scheme, Host: o.String()[len(o.scheme)+3:]}}
	return t, []*target{t}
}

// testProxy serves one target with the given watch, without rewriting.
func testProxy(public origin, u *url.URL, watch *appWatch) http.Handler {
	t := &target{url: &url.URL{Scheme: u.Scheme, Host: u.Host}, name: "localhost:3000", watch: watch}
	t.origin, _ = parseOrigin(t.url.String())
	return newProxy(public, []*target{t}, false, nil)
}

// seenRequest is what a fixture app records about one request.
type seenRequest struct {
	app, method, path, host, origin, referer, encoding string
}

// twoApps is a typical share of two targets behind a real proxy: a front
// end at the root and an API at /_purlview/t/2, each recording what it was
// sent.
func twoApps(t *testing.T, public origin, rewrite bool) (front *httptest.Server, seen chan seenRequest, apps [2]*httptest.Server) {
	t.Helper()
	seen = make(chan seenRequest, 64)
	app := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen <- seenRequest{name, r.Method, r.URL.RequestURI(), r.Host, r.Header.Get("Origin"), r.Header.Get("Referer"), r.Header.Get("Accept-Encoding")}
			self := "http://" + r.Host
			switch r.URL.Path {
			case "/page":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("ETag", `"page-1"`)
				_, _ = fmt.Fprintf(w, `<script>fetch("%s/api")</script><a href="%s/x">%s</a> unlisted http://localhost:9999/`, apps[1].URL, self, self)
			case "/redirect-root":
				http.Redirect(w, r, "/dashboard", http.StatusFound)
			case "/redirect-front":
				http.Redirect(w, r, apps[0].URL+"/app", http.StatusFound)
			case "/redirect-self":
				http.Redirect(w, r, self+"/self", http.StatusFound)
			case "/redirect-unlisted":
				http.Redirect(w, r, "http://localhost:9999/", http.StatusFound)
			case "/cors":
				w.Header().Set("Access-Control-Allow-Origin", apps[0].URL)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			case "/cookie":
				w.Header().Add("Set-Cookie", "sid=1; Path=/; Domain=localhost; HttpOnly")
				w.WriteHeader(http.StatusNoContent)
			case "/sse":
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				for i := range 3 {
					_, _ = fmt.Fprintf(w, "data: %d %s/e\n\n", i, self)
					w.(http.Flusher).Flush()
					time.Sleep(60 * time.Millisecond)
				}
			case "/ws":
				conn, rw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nContent-Type: text/html\r\n\r\n")
				_ = rw.Flush()
				line, _ := rw.ReadString('\n')
				_, _ = rw.WriteString(name + " heard " + line)
				_ = rw.Flush()
			default:
				w.Header().Set("Content-Type", "text/plain")
				_, _ = fmt.Fprintf(w, "%s %s", name, r.URL.RequestURI())
			}
		}))
	}
	apps[0], apps[1] = app("front"), app("api")
	t.Cleanup(apps[0].Close)
	t.Cleanup(apps[1].Close)
	admitted, _ := url.Parse(apps[0].URL)
	targets, mismatch := newTargets(admitted, []string{apps[0].URL, apps[1].URL + "/ignored?path"}, nil)
	if mismatch != "" {
		t.Fatal(mismatch)
	}
	front = httptest.NewServer(newProxy(public, targets, rewrite, nil))
	t.Cleanup(front.Close)
	return front, seen, apps
}

func get(t *testing.T, front *httptest.Server, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res, string(body)
}

// The mount index alone selects the target; what follows it is the path as
// the browser spelled it; an index the share does not have is a bare 404 that
// no watcher counts; and nothing else selects a further target.
func TestProxyRoutesByMountIndexOnly(t *testing.T) {
	front, seen, apps := twoApps(t, mustOrigin(t, livePublic), true)
	for path, want := range map[string]seenRequest{
		"/x?q=1":                              {app: "front", path: "/x?q=1"},
		"/_purlview/t/2":                      {app: "api", path: "/"},
		"/_purlview/t/2/":                     {app: "api", path: "/"},
		"/_purlview/t/2/x?q=1":                {app: "api", path: "/x?q=1"},
		"/_purlview/t/2/a%2Fb/c":              {app: "api", path: "/a%2Fb/c"},
		"/_purlview/t/2/../../secret":         {app: "api", path: "/../../secret"}, // the mount chose the host; the path is the API's business
		"/_purlview/t/2/_purlview/t/3/x":      {app: "api", path: "/_purlview/t/3/x"},
		"/_purlview/t/2x/echo":                {app: "front", path: "/_purlview/t/2x/echo"},
		"/%5Fpurlview/t/2/x":                  {app: "front", path: "/%5Fpurlview/t/2/x"},
		"/_purlview/t/02/x":                   {app: "front", path: "/_purlview/t/02/x"},
		"/_purlview/redeem?code=x":            {app: "front", path: "/_purlview/redeem?code=x"},
		"/_purlview/t/2/_purlview/redeem?c=1": {app: "api", path: "/_purlview/redeem?c=1"},
	} {
		res, body := get(t, front, path, nil)
		got := <-seen
		if res.StatusCode != 200 || got.app != want.app || got.path != want.path || body != want.app+" "+want.path {
			t.Errorf("%s: %d %q went to %+v", path, res.StatusCode, body, got)
		}
		wantHost := apps[0].Listener.Addr().String()
		if want.app == "api" {
			wantHost = apps[1].Listener.Addr().String()
		}
		if got.host != wantHost || got.encoding != "identity" {
			t.Errorf("%s: the app saw Host %q, Accept-Encoding %q", path, got.host, got.encoding)
		}
	}
	for _, path := range []string{"/_purlview/t/3/", "/_purlview/t/3", "/_purlview/t/99/x"} {
		res, body := get(t, front, path, nil)
		if res.StatusCode != http.StatusNotFound || body != "" || res.Header.Get(appStatusHeader) != "" {
			t.Errorf("%s: %d %q %v", path, res.StatusCode, body, res.Header)
		}
		select {
		case got := <-seen:
			t.Errorf("%s reached %+v", path, got)
		default:
		}
	}
	// A 404 for an unknown index is neither an app answer nor a forwarded
	// navigation: on a fresh proxy no watcher counts it, however the request
	// presents itself.
	fresh, freshSeen, _ := twoApps(t, mustOrigin(t, livePublic), true)
	for _, header := range []http.Header{{"Sec-Fetch-Mode": {"navigate"}, "Accept": {"text/html"}}, {"Accept": {"text/html"}}} {
		if res, body := get(t, fresh, "/_purlview/t/3/", header); res.StatusCode != http.StatusNotFound || body != "" {
			t.Fatalf("unknown index: %d %q", res.StatusCode, body)
		}
	}
	select {
	case got := <-freshSeen:
		t.Fatalf("an unknown index reached %+v", got)
	default:
	}
	for _, tg := range fresh.Config.Handler.(*proxy).targets {
		tg.watch.mu.Lock()
		visitor, observed := tg.watch.visitor, tg.watch.observed
		tg.watch.mu.Unlock()
		if visitor || observed {
			t.Errorf("%s: the 404 was counted (visitor %v, observed %v)", tg.name, visitor, observed)
		}
	}
}

// Headers are translated per target: a Referer names the target whose page
// it is and loses that target's mount, Origin follows it, a redirect at any
// listed target's own address becomes that target's public form, a mounted
// API's root-relative redirect gains the mount, and CORS names the share.
func TestProxyTranslatesHeadersPerTarget(t *testing.T) {
	public := mustOrigin(t, livePublic)
	front, seen, apps := twoApps(t, public, true)
	frontURL, apiURL := apps[0].URL, apps[1].URL
	page := http.Header{"Origin": {livePublic}, "Referer": {livePublic + "/page?x=1"}}
	mounted := http.Header{"Origin": {livePublic}, "Referer": {livePublic + "/_purlview/t/2/docs?y=2"}}
	unknownMount := http.Header{"Origin": {livePublic}, "Referer": {livePublic + "/_purlview/t/9/docs"}}
	none := http.Header{"Origin": {livePublic}}
	foreign := http.Header{"Origin": {"https://evil.example"}, "Referer": {"https://evil.example/attack"}}
	for _, c := range []struct {
		path            string
		header          http.Header
		origin, referer string
	}{
		{"/api", page, frontURL, frontURL + "/page?x=1"},
		{"/_purlview/t/2/api", page, frontURL, frontURL + "/page?x=1"},
		{"/_purlview/t/2/api", mounted, apiURL, apiURL + "/docs?y=2"},
		{"/x", mounted, apiURL, apiURL + "/docs?y=2"},
		{"/_purlview/t/2/api", http.Header{"Origin": {livePublic}, "Referer": {livePublic + "/_purlview/t/2"}}, apiURL, apiURL + "/"},
		{"/_purlview/t/2/api", http.Header{"Origin": {livePublic}, "Referer": {livePublic + "/_purlview/t/2?q=1"}}, apiURL, apiURL + "?q=1"},
		{"/_purlview/t/2/api", unknownMount, apiURL, apiURL + "/_purlview/t/9/docs"},
		{"/x", unknownMount, frontURL, frontURL + "/_purlview/t/9/docs"},
		{"/_purlview/t/2/ws", none, apiURL, ""},
		{"/x", none, frontURL, ""},
		{"/_purlview/t/2/api", foreign, "https://evil.example", "https://evil.example/attack"},
	} {
		if strings.HasSuffix(c.path, "/ws") {
			c.path = strings.TrimSuffix(c.path, "/ws") + "/plain"
		}
		get(t, front, c.path, c.header)
		got := <-seen
		if got.origin != c.origin || got.referer != c.referer {
			t.Errorf("%s with %v: the app saw Origin %q Referer %q, want %q %q", c.path, c.header, got.origin, got.referer, c.origin, c.referer)
		}
	}
	for path, location := range map[string]string{
		"/redirect-root":                   "/dashboard",
		"/_purlview/t/2/redirect-root":     "/_purlview/t/2/dashboard",
		"/redirect-self":                   livePublic + "/self",
		"/_purlview/t/2/redirect-self":     livePublic + "/_purlview/t/2/self",
		"/_purlview/t/2/redirect-front":    livePublic + "/app",
		"/redirect-unlisted":               "http://localhost:9999/",
		"/_purlview/t/2/redirect-unlisted": "http://localhost:9999/",
	} {
		res, _ := get(t, front, path, nil)
		<-seen
		if got := res.Header.Get("Location"); res.StatusCode != http.StatusFound || got != location {
			t.Errorf("%s: %d Location %q, want %q", path, res.StatusCode, got, location)
		}
	}
	for _, path := range []string{"/cors", "/_purlview/t/2/cors"} {
		res, body := get(t, front, path, nil)
		<-seen
		if res.Header.Get("Access-Control-Allow-Origin") != livePublic || body != `{"ok":true}` {
			t.Errorf("%s: %v %q", path, res.Header, body)
		}
	}
	res, _ := get(t, front, "/_purlview/t/2/cookie", nil)
	<-seen
	if got := res.Header.Values("Set-Cookie"); !slices.Equal(got, []string{"sid=1; Path=/; HttpOnly"}) {
		t.Errorf("a mounted API's cookie keeps its path and loses its domain: %v", got)
	}
}

// Bodies from any target are rewritten to the public forms, and only listed
// addresses: an unlisted port stays as it was, which the browser resolves on
// the visitor's own machine. --no-rewrite leaves bodies and encodings alone.
func TestProxyRewritesBodiesOfEveryTarget(t *testing.T) {
	public := mustOrigin(t, livePublic)
	front, seen, _ := twoApps(t, public, true)
	res, body := get(t, front, "/page", nil)
	<-seen
	want := `<script>fetch("` + livePublic + `/_purlview/t/2/api")</script><a href="` + livePublic + `/x">` + livePublic + `</a> unlisted http://localhost:9999/`
	if body != want || res.Header.Get("Content-Length") != "" || res.Header.Get("ETag") != `W/"page-1"` || res.ContentLength != -1 {
		t.Errorf("front page: %q %v", body, res.Header)
	}
	_, body = get(t, front, "/_purlview/t/2/page", nil)
	<-seen
	want = `<script>fetch("` + livePublic + `/_purlview/t/2/api")</script><a href="` + livePublic + `/_purlview/t/2/x">` + livePublic + `/_purlview/t/2</a> unlisted http://localhost:9999/`
	if body != want {
		t.Errorf("API page: %q", body)
	}
	// SSE from the API streams event by event, each rewritten.
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/_purlview/t/2/sse", nil)
	sse, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sse.Body.Close() }()
	<-seen
	var arrivals []time.Duration
	scanner := bufio.NewScanner(sse.Body)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			arrivals = append(arrivals, time.Since(start))
			if line != fmt.Sprintf("data: %d %s/_purlview/t/2/e", len(arrivals)-1, livePublic) {
				t.Errorf("event %q", line)
			}
		}
	}
	if len(arrivals) != 3 || arrivals[2]-arrivals[0] < 100*time.Millisecond {
		t.Errorf("events were not streamed as they came: %v", arrivals)
	}
	// Without rewriting: the body and the visitor's Accept-Encoding pass unchanged.
	plain, seenPlain, plainApps := twoApps(t, public, false)
	res, body = get(t, plain, "/page", http.Header{"Accept-Encoding": {"gzip, br"}})
	got := <-seenPlain
	if !strings.Contains(body, plainApps[1].URL+"/api") || res.Header.Get("Content-Length") == "" || got.encoding != "gzip, br" {
		t.Errorf("--no-rewrite: %q %v saw encoding %q", body, res.Header, got.encoding)
	}
}

// The security boundary of several targets: whatever a request names, only a
// listed target is dialled.
func TestProxyDialsOnlyListedTargets(t *testing.T) {
	front, seen, apps := twoApps(t, mustOrigin(t, livePublic), true)
	raw := func(request string) string {
		t.Helper()
		conn, err := net.Dial("tcp", front.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprint(conn, request)
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		return fmt.Sprintf("%d %s", res.StatusCode, body)
	}
	frontHost := apps[0].Listener.Addr().String()
	for name, request := range map[string]string{
		"absolute form":  "GET http://evil.example:9999/echo HTTP/1.1\r\nHost: k7m2p4qx.purlview.invalid\r\n\r\n",
		"foreign Host":   "GET /echo HTTP/1.1\r\nHost: localhost:9999\r\n\r\n",
		"CONNECT":        "CONNECT evil.example:9999 HTTP/1.1\r\nHost: evil.example:9999\r\n\r\n",
		"mounted Host":   "GET /_purlview/t/2/echo HTTP/1.1\r\nHost: localhost:9999\r\n\r\n",
		"encoded prefix": "GET /%5Fpurlview/t/2/echo HTTP/1.1\r\nHost: k7m2p4qx.purlview.invalid\r\n\r\n",
	} {
		got := raw(request)
		seenBy := <-seen
		wantHost, wantApp := frontHost, "front"
		if name == "mounted Host" {
			wantHost, wantApp = apps[1].Listener.Addr().String(), "api"
		}
		if seenBy.host != wantHost || seenBy.app != wantApp || (name != "CONNECT" && !strings.HasPrefix(got, "200 "+wantApp+" ")) {
			t.Errorf("%s: %q reached %+v", name, got, seenBy)
		}
	}
	// A WebSocket to the further target goes through its mount with its own
	// origin, and its upgraded bytes are never rewritten.
	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "GET /_purlview/t/2/ws HTTP/1.1\r\nHost: k7m2p4qx.purlview.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: %s\r\n\r\n", livePublic)
	r := bufio.NewReader(conn)
	res, err := http.ReadResponse(r, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	if got := <-seen; got.app != "api" || got.origin != apps[1].URL {
		t.Errorf("the socket handshake reached %+v", got)
	}
	_, _ = fmt.Fprint(conn, "hello "+apps[1].URL+"\n")
	if line, _ := r.ReadString('\n'); line != "api heard hello "+apps[1].URL+"\n" {
		t.Fatalf("upgraded stream: %q", line)
	}
}

// Each target's watcher reports its own app, on one connection-wide channel.
func TestEachTargetHasItsOwnWatcher(t *testing.T) {
	front, seen, apps := twoApps(t, mustOrigin(t, livePublic), false)
	p := front.Config.Handler.(*proxy)
	apps[1].Close()
	// The daemon's private signal marks the 502; the gateway removes it.
	res, _ := get(t, front, "/_purlview/t/2/api", nil)
	if res.StatusCode != http.StatusBadGateway || res.Header.Get(appStatusHeader) != appUnresponsive {
		t.Fatalf("a refused further target: %d %v", res.StatusCode, res.Header)
	}
	get(t, front, "/page", http.Header{"Sec-Fetch-Mode": {"navigate"}})
	<-seen
	var got []string
	for _, w := range p.watches() {
		for _, ev := range settled(w) {
			got = append(got, string(ev.Kind)+" "+ev.Detail)
		}
	}
	slices.Sort(got)
	want := []string{"app_responding " + p.targets[0].name, "app_unresponsive " + p.targets[1].name, "first_visitor " + p.targets[0].name}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if p.targets[0].watch.wake != p.targets[1].watch.wake {
		t.Fatal("the watchers must share one wake channel")
	}
	if _, ok := p.targets[0].rp.Transport.(*http.Transport); !ok || p.targets[0].rp.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("a target's transport must use no proxy")
	}
	_ = engine.ConnReady
}
