package tunnel

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func mustOrigin(t *testing.T, value string) origin {
	t.Helper()
	o, ok := parseOrigin(value)
	if !ok {
		t.Fatalf("%q is no origin", value)
	}
	return o
}

// An origin is scheme, host and port: default ports and host case do not
// distinguish two origins, anything else does.
func TestOriginComparisonAndSerialisation(t *testing.T) {
	same := [][2]string{
		{"https://k7m2p4qx.purlview.invalid", "https://k7m2p4qx.purlview.invalid:443"},
		{"https://k7m2p4qx.purlview.invalid", "HTTPS://K7M2P4QX.Purlview.INVALID"},
		{"http://localhost", "http://localhost:80"},
		{"http://[::1]", "HTTP://[::1]:80"},
	}
	for _, pair := range same {
		if mustOrigin(t, pair[0]) != mustOrigin(t, pair[1]) {
			t.Errorf("%s and %s are one origin", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"https://k7m2p4qx.purlview.invalid", "http://k7m2p4qx.purlview.invalid"},
		{"https://k7m2p4qx.purlview.invalid", "https://k7m2p4qx.purlview.invalid:8443"},
		{"https://k7m2p4qx.purlview.invalid", "https://other.purlview.invalid"},
		{"https://k7m2p4qx.purlview.invalid", "https://k7m2p4qx.purlview.invalid.example"},
		{"http://localhost:443", "https://localhost"},
	}
	for _, pair := range different {
		if mustOrigin(t, pair[0]) == mustOrigin(t, pair[1]) {
			t.Errorf("%s and %s are different origins", pair[0], pair[1])
		}
	}
	for value, want := range map[string]string{
		"https://K7M2P4QX.purlview.invalid:443": "https://k7m2p4qx.purlview.invalid",
		"https://k7m2p4qx.purlview.test:8443":   "https://k7m2p4qx.purlview.test:8443",
		"http://localhost:80":                   "http://localhost",
		"http://127.0.0.1:3000":                 "http://127.0.0.1:3000",
		"http://[::1]:3000":                     "http://[::1]:3000",
		"https://[::1]":                         "https://[::1]",
	} {
		if got := mustOrigin(t, value).String(); got != want {
			t.Errorf("%s serialised as %s, want %s", value, got, want)
		}
	}
	for _, value := range []string{"", "null", "*", "localhost:3000", "//localhost:3000", "/path", "ws://localhost:3000", "file:///x", "http://", "http://user@localhost:3000", "http://localhost:port", "http://local host", "http://%6cocalhost:3000", "https://k7m2p4qx.purlview.invalid/", "https://k7m2p4qx.purlview.invalid?x"} {
		if o, ok := parseOrigin(value); ok {
			t.Errorf("%q parsed as origin %v", value, o)
		}
	}
	if _, rest, ok := cutOrigin("HTTP://LocalHost:3000/a%2Fb?next=http://localhost:3000/#f"); !ok || rest != "/a%2Fb?next=http://localhost:3000/#f" {
		t.Errorf("rest %q %v", rest, ok)
	}
}

const (
	livePublic = "https://k7m2p4qx.purlview.invalid"
	liveTarget = "http://localhost:3000"
)

// To the app: Origin and Referer naming the share name the target instead.
// Every other value, foreign sites included, reaches the app as it was sent.
func TestRequestHeadersNamingTheShareNameTheTarget(t *testing.T) {
	cases := []struct{ header, value, want string }{
		{"Origin", "https://k7m2p4qx.purlview.invalid", "http://localhost:3000"},
		{"Origin", "https://k7m2p4qx.purlview.invalid:443", "http://localhost:3000"},
		{"Origin", "https://K7M2P4QX.PURLVIEW.INVALID", "http://localhost:3000"},
		{"Origin", "https://evil.example", "https://evil.example"},
		{"Origin", "https://other.purlview.invalid", "https://other.purlview.invalid"},
		{"Origin", "https://k7m2p4qx.purlview.link", "https://k7m2p4qx.purlview.link"},
		{"Origin", "http://k7m2p4qx.purlview.invalid", "http://k7m2p4qx.purlview.invalid"},
		{"Origin", "https://k7m2p4qx.purlview.invalid:8443", "https://k7m2p4qx.purlview.invalid:8443"},
		{"Origin", "https://k7m2p4qx.purlview.invalid.evil.example", "https://k7m2p4qx.purlview.invalid.evil.example"},
		{"Origin", "http://localhost:3000", "http://localhost:3000"},
		{"Origin", "null", "null"},
		{"Origin", "", ""},
		{"Origin", "https://k7m2p4qx.purlview.invalid/", "https://k7m2p4qx.purlview.invalid/"},
		{"Origin", "https://k7m2p4qx.purlview.invalid https://evil.example", "https://k7m2p4qx.purlview.invalid https://evil.example"},
		{"Origin", "https://user@k7m2p4qx.purlview.invalid", "https://user@k7m2p4qx.purlview.invalid"},
		{"Origin", "k7m2p4qx.purlview.invalid", "k7m2p4qx.purlview.invalid"},
		{"Referer", "https://k7m2p4qx.purlview.invalid/review/a%2Fb?round=1&next=https://k7m2p4qx.purlview.invalid/", "http://localhost:3000/review/a%2Fb?round=1&next=https://k7m2p4qx.purlview.invalid/"},
		{"Referer", "https://k7m2p4qx.purlview.invalid:443/", "http://localhost:3000/"},
		{"Referer", "https://k7m2p4qx.purlview.invalid", "http://localhost:3000"},
		{"Referer", "https://k7m2p4qx.purlview.invalid?x=1", "http://localhost:3000?x=1"},
		{"Referer", "https://evil.example/k7m2p4qx.purlview.invalid/", "https://evil.example/k7m2p4qx.purlview.invalid/"},
		{"Referer", "https://evil.example/?u=https://k7m2p4qx.purlview.invalid/", "https://evil.example/?u=https://k7m2p4qx.purlview.invalid/"},
		{"Referer", "about:client", "about:client"},
		{"Referer", "/relative", "/relative"},
	}
	public := mustOrigin(t, livePublic)
	target, targets := single(mustOrigin(t, liveTarget))
	for _, c := range cases {
		h := http.Header{c.header: {c.value}, "Cookie": {"a=https://k7m2p4qx.purlview.invalid"}}
		translateRequest(h, public, target, targets)
		if got := h.Get(c.header); got != c.want || h.Get("Cookie") != "a=https://k7m2p4qx.purlview.invalid" {
			t.Errorf("%s: %q became %q, want %q", c.header, c.value, got, c.want)
		}
	}
	several := http.Header{"Origin": {"https://evil.example", "https://k7m2p4qx.purlview.invalid"}, "Referer": {"https://k7m2p4qx.purlview.invalid/a", "https://evil.example/b"}}
	translateRequest(several, public, target, targets)
	if !slices.Equal(several["Origin"], []string{"https://evil.example", "http://localhost:3000"}) || !slices.Equal(several["Referer"], []string{"http://localhost:3000/a", "https://evil.example/b"}) {
		t.Errorf("several values: %v", several)
	}
	// A target on its scheme's default port is named the way a browser would.
	h := http.Header{"Origin": {livePublic}}
	internal, internals := single(mustOrigin(t, "https://Internal.Example:443"))
	translateRequest(h, public, internal, internals)
	if h.Get("Origin") != "https://internal.example" {
		t.Errorf("default port: %q", h.Get("Origin"))
	}
}

// To the browser: an absolute Location at the target and an
// Access-Control-Allow-Origin that is the target name the share instead.
// Relative and foreign values reach the browser as the app sent them.
func TestResponseHeadersNamingTheTargetNameTheShare(t *testing.T) {
	const acao = "Access-Control-Allow-Origin"
	cases := []struct{ target, header, value, want string }{
		{liveTarget, "Location", "http://localhost:3000/login?next=%2Fa&next=http://localhost:3000/#top", "https://k7m2p4qx.purlview.invalid/login?next=%2Fa&next=http://localhost:3000/#top"},
		{liveTarget, "Location", "http://localhost:3000", "https://k7m2p4qx.purlview.invalid"},
		{liveTarget, "Location", "HTTP://LOCALHOST:3000/x", "https://k7m2p4qx.purlview.invalid/x"},
		// The loopback names of a loopback target are the target.
		{liveTarget, "Location", "http://127.0.0.1:3000/x", "https://k7m2p4qx.purlview.invalid/x"},
		{liveTarget, "Location", "http://[::1]:3000/x", "https://k7m2p4qx.purlview.invalid/x"},
		{"http://127.0.0.1:3000", "Location", "http://localhost:3000/x", "https://k7m2p4qx.purlview.invalid/x"},
		{"http://[::1]:3000", "Location", "http://127.0.0.1:3000/x", "https://k7m2p4qx.purlview.invalid/x"},
		{"http://localhost", "Location", "http://127.0.0.1:80/x", "https://k7m2p4qx.purlview.invalid/x"},
		// Another port, scheme or host is another app.
		{liveTarget, "Location", "http://localhost:8000/x", "http://localhost:8000/x"},
		{liveTarget, "Location", "http://127.0.0.1:8000/x", "http://127.0.0.1:8000/x"},
		{liveTarget, "Location", "https://localhost:3000/x", "https://localhost:3000/x"},
		{liveTarget, "Location", "http://localhost/x", "http://localhost/x"},
		{liveTarget, "Location", "http://localhost.example:3000/x", "http://localhost.example:3000/x"},
		{liveTarget, "Location", "http://127.0.0.2:3000/x", "http://127.0.0.2:3000/x"},
		{liveTarget, "Location", "https://accounts.example/o/oauth2?redirect_uri=http://localhost:3000/cb", "https://accounts.example/o/oauth2?redirect_uri=http://localhost:3000/cb"},
		// A target that is not loopback has no other names.
		{"http://internal.example:3000", "Location", "http://localhost:3000/x", "http://localhost:3000/x"},
		{"http://internal.example:3000", "Location", "http://Internal.Example:3000/x", "https://k7m2p4qx.purlview.invalid/x"},
		{"https://internal.example", "Location", "https://internal.example:443/x", "https://k7m2p4qx.purlview.invalid/x"},
		// Relative and protocol-relative references are the browser's to resolve.
		{liveTarget, "Location", "/login", "/login"},
		{liveTarget, "Location", "login?next=http://localhost:3000/", "login?next=http://localhost:3000/"},
		{liveTarget, "Location", "../up", "../up"},
		{liveTarget, "Location", "?x=1", "?x=1"},
		{liveTarget, "Location", "//localhost:3000/x", "//localhost:3000/x"},
		{liveTarget, "Location", "", ""},
		{liveTarget, "Location", "http://user@localhost:3000/x", "http://user@localhost:3000/x"},
		{liveTarget, "Location", "ws://localhost:3000/x", "ws://localhost:3000/x"},
		{liveTarget, acao, "http://localhost:3000", "https://k7m2p4qx.purlview.invalid"},
		{liveTarget, acao, "http://127.0.0.1:3000", "https://k7m2p4qx.purlview.invalid"},
		{liveTarget, acao, "*", "*"},
		{liveTarget, acao, "null", "null"},
		{liveTarget, acao, "https://app.example", "https://app.example"},
		{liveTarget, acao, "http://localhost:8000", "http://localhost:8000"},
		{liveTarget, acao, "http://localhost:3000/", "http://localhost:3000/"},
	}
	public := mustOrigin(t, livePublic)
	target, targets := single(mustOrigin(t, liveTarget))
	for _, c := range cases {
		h := http.Header{c.header: {c.value}, "Vary": {"Origin"}, "Content-Location": {"http://localhost:3000/x"}}
		from, all := single(mustOrigin(t, c.target))
		translateResponse(h, public, from, all)
		if got := h.Get(c.header); got != c.want || h.Get("Vary") != "Origin" || h.Get("Content-Location") != "http://localhost:3000/x" {
			t.Errorf("%s for %s: %q became %q, want %q (%v)", c.header, c.target, c.value, got, c.want, h)
		}
	}
	several := http.Header{"Location": {"/a", "http://localhost:3000/b"}, acao: {"https://app.example", "http://localhost:3000"}}
	translateResponse(several, public, target, targets)
	if !slices.Equal(several["Location"], []string{"/a", livePublic + "/b"}) || !slices.Equal(several[acao], []string{"https://app.example", livePublic}) {
		t.Errorf("several values: %v", several)
	}
	// An origin with an explicit port keeps it.
	h := http.Header{"Location": {"http://localhost:3000/x"}}
	translateResponse(h, mustOrigin(t, "https://k7m2p4qx.purlview-content.test:8443"), target, targets)
	if h.Get("Location") != "https://k7m2p4qx.purlview-content.test:8443/x" {
		t.Errorf("port: %q", h.Get("Location"))
	}
}

// Without both addresses nothing is translated, in either direction.
func TestNoTranslationWithoutBothOrigins(t *testing.T) {
	for _, pair := range [][2]origin{{{}, mustOrigin(t, liveTarget)}, {mustOrigin(t, livePublic), {}}, {}} {
		h := http.Header{"Origin": {livePublic}, "Referer": {livePublic + "/"}, "Location": {liveTarget + "/x"}, "Access-Control-Allow-Origin": {liveTarget}}
		want := h.Clone()
		one, all := single(pair[1])
		translateRequest(h, pair[0], one, all)
		translateResponse(h, pair[0], one, all)
		for k := range want {
			if !slices.Equal(h[k], want[k]) {
				t.Errorf("%v: %s became %v", pair, k, h[k])
			}
		}
	}
}

// The proxy applies both directions to ordinary requests, and the app sees no
// X-Forwarded-* header that would make it generate public URLs itself.
func TestProxyTranslatesBothDirections(t *testing.T) {
	requests := make(chan http.Header, 1)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := r.Header.Clone()
		seen.Set("Host", r.Host)
		requests <- seen
		w.Header().Set("Access-Control-Allow-Origin", "http://"+r.Host)
		w.Header().Set("Vary", "Origin")
		w.Header().Add("Set-Cookie", "sid=1; Domain =localhost; Path=/")
		switch r.URL.Path {
		case "/login": // under another loopback name than the one shared
			http.Redirect(w, r, "http://"+strings.Replace(r.Host, "127.0.0.1", "localhost", 1)+"/somewhere?from=login", http.StatusFound)
		case "/relative":
			http.Redirect(w, r, "/somewhere", http.StatusSeeOther)
		case "/away":
			http.Redirect(w, r, "https://accounts.example/signin", http.StatusFound)
		}
	}))
	defer app.Close()
	target, _ := url.Parse(app.URL)
	proxy := testProxy(mustOrigin(t, livePublic), target, newAppWatch("localhost:3000", nil))
	header := http.Header{"Origin": {livePublic}, "Referer": {livePublic + "/form?a=1"}, "X-Forwarded-Host": {"k7m2p4qx.purlview.invalid"}, "X-Forwarded-Proto": {"https"}}
	for path, location := range map[string]string{"/login": livePublic + "/somewhere?from=login", "/relative": "/somewhere", "/away": "https://accounts.example/signin", "/": ""} {
		res := forward(t, proxy, "POST", path, header)
		if got := res.Header().Get("Location"); got != location {
			t.Errorf("%s: Location %q, want %q", path, got, location)
		}
		if res.Header().Get("Access-Control-Allow-Origin") != livePublic || res.Header().Get("Vary") != "Origin" || res.Header().Get("Set-Cookie") != "sid=1; Path=/" {
			t.Errorf("%s: %v", path, res.Header())
		}
		seen := <-requests
		if seen.Get("Origin") != app.URL || seen.Get("Referer") != app.URL+"/form?a=1" || seen.Get("Host") != target.Host {
			t.Errorf("%s: the app saw %v", path, seen)
		}
		for name := range seen {
			if strings.HasPrefix(name, "X-Forwarded-") || name == "Forwarded" {
				t.Errorf("%s: the app saw %s", path, name)
			}
		}
	}
	foreign := forward(t, proxy, "POST", "/", http.Header{"Origin": {"https://evil.example"}, "Referer": {"https://evil.example/attack"}})
	if seen := <-requests; foreign.Code != http.StatusOK || seen.Get("Origin") != "https://evil.example" || seen.Get("Referer") != "https://evil.example/attack" {
		t.Errorf("a foreign site must stay foreign to the app: %v", seen)
	}
}

// An application cookie stays on the share's own host however the app spells
// Domain: browsers read the attribute's name without the spaces around it and
// in any case, and a Domain on a share would reach every other share under
// the same content domain. Purlview's reserved names never pass in any case.
func TestCookiesStayHostOnly(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"sid=1; Domain=purlview.invalid; Path=/", "sid=1; Path=/"},
		{"sid=1; domain =purlview.invalid", "sid=1"},
		{"sid=1; Domain =purlview.invalid; Secure", "sid=1; Secure"},
		{"sid=1;  DOMAIN = purlview.invalid ;HttpOnly", "sid=1;HttpOnly"},
		{"sid=1; Domain", "sid=1"},
		{"sid=1; Domain=", "sid=1"},
		{"sid=1;\tdoMain\t=.purlview.invalid", "sid=1"},
		{"sid=1; Domain=a; Domain=b; Path=/", "sid=1; Path=/"},
		{"sid=1; Path=/domain=x; SameSite=Lax", "sid=1; Path=/domain=x; SameSite=Lax"},
		{"sid=domain=x; Path=/", "sid=domain=x; Path=/"},
		{"domain=x; Path=/", "domain=x; Path=/"},
		{"sid=1; Domains=x; Max-Age=60", "sid=1; Domains=x; Max-Age=60"},
		{"sid=1", "sid=1"},
	} {
		if got, ok := hostOnlyCookie(c.in); !ok || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.in, got, ok, c.want)
		}
	}
	for _, reserved := range []string{
		"__Host-purlview-session=x; Secure; Path=/",
		" __Host-purlview-session=x",
		"__HOST-purlview-session=x; Secure; Path=/",
		"__host-purlview-warned=1",
	} {
		if got, ok := hostOnlyCookie(reserved); ok {
			t.Errorf("%q passed as %q", reserved, got)
		}
	}
}

// Development servers (webpack-dev-server, Next.js) refuse a hot-reload socket
// whose Origin is not their own host. The upgrade request is translated like
// any other, and the 101 passes through ModifyResponse without an error.
func TestWebSocketUpgradeCarriesTheTranslatedOrigin(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o, err := url.Parse(r.Header.Get("Origin")); err != nil || o.Host != r.Host {
			http.Error(w, "Invalid Host/Origin header", http.StatusForbidden)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = rw.WriteString("app heard " + line)
		_ = rw.Flush()
	}))
	defer app.Close()
	target, _ := url.Parse(app.URL)
	watch := newAppWatch("localhost:3000", nil)
	front := httptest.NewServer(testProxy(mustOrigin(t, livePublic), target, watch))
	defer front.Close()
	for origin, want := range map[string]int{livePublic: http.StatusSwitchingProtocols, "https://evil.example": http.StatusForbidden} {
		conn, err := net.Dial("tcp", front.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: k7m2p4qx.purlview.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: %s\r\n\r\n", origin)
		r := bufio.NewReader(conn)
		res, err := http.ReadResponse(r, nil)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s: %v %v", origin, res, err)
		}
		if want == http.StatusSwitchingProtocols {
			_, _ = fmt.Fprint(conn, "hello\n")
			if line, _ := r.ReadString('\n'); line != "app heard hello\n" {
				t.Fatalf("upgraded stream: %q", line)
			}
		}
		_ = conn.Close()
	}
	if got := kinds(settled(watch)); len(got) != 1 || got[0] != "app_responding" {
		t.Fatalf("an upgrade is an answer: %v", got)
	}
}
