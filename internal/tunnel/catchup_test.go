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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// catchupShare is a share of a Vite app and an API behind a real proxy,
// with the observers' timings shortened for tests.
func catchupShare(t *testing.T, fv *fakeVite, rewrite bool) (front *httptest.Server, p *proxy, api *httptest.Server, apiSeen chan string) {
	t.Helper()
	apiSeen = make(chan string, 64)
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiSeen <- r.Method + " " + r.URL.RequestURI()
		if r.URL.Path == "/@vite/client" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><head><script src="/api.js"></script></head></html>`)
	}))
	t.Cleanup(api.Close)
	admitted, _ := url.Parse(fv.URL)
	targets, mismatch := newTargets(admitted, []string{fv.URL, api.URL}, nil)
	if mismatch != "" {
		t.Fatal(mismatch)
	}
	p = newProxy(mustOrigin(t, livePublic), targets, rewrite, nil)
	for _, tg := range p.targets {
		if o := tg.catchup; o != nil {
			o.idle = time.Hour
			o.stampWait = 5 * time.Second
			o.barrierWait = 5 * time.Second
			o.backoffMin, o.backoffMax = 10*time.Millisecond, 40*time.Millisecond
		}
	}
	front = httptest.NewServer(p)
	t.Cleanup(func() {
		front.Close()
		p.stop()
		p.closeIdle()
	})
	return front, p, api, apiSeen
}

var navigate = http.Header{"Sec-Fetch-Mode": {"navigate"}, "Accept": {"text/html"}}

// stampOf reads the stamp from a page's inserted script, or nil.
func stampOf(t *testing.T, body string) *stampArgument {
	t.Helper()
	const open = `<script data-purlview="catch-up">` + catchupScript + "("
	i := strings.Index(body, open)
	if i < 0 {
		return nil
	}
	rest := body[i+len(open):]
	end := strings.Index(rest, ")</script>")
	var s stampArgument
	if end < 0 || json.Unmarshal([]byte(rest[:end]), &s) != nil {
		t.Fatalf("unreadable stamp in %q", body)
	}
	return &s
}

// check asks the share for a target's revision as the script does.
func check(t *testing.T, front *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	return get(t, front, path, http.Header{revisionHeader: {"1"}, "Sec-Fetch-Mode": {"cors"}, "Accept": {"*/*"}})
}

// A navigation to a supported app gets the script in its head, stamped with
// the target's generation and revision. In a page as Vite serves it, the
// script goes right after the template's <meta charset>, which follows
// Vite's client module: a module runs only once the page is parsed, so the
// script still runs first. The charset the page declares is stated in the
// response, as the script pushes the declaration past the bytes browsers
// look at for it. The page loses the validators that named the app's bytes,
// and the app is asked for the whole page.
func TestNavigationsAreStampedInTheHead(t *testing.T) {
	fv := newFakeVite(t)
	front, p, _, _ := catchupShare(t, fv, true)
	res, body := get(t, front, "/", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "If-None-Match": {`"page-1"`}, "If-Modified-Since": {"Tue, 22 Sep 2026 14:47:19 GMT"}})
	tag := `<script data-purlview="catch-up">`
	if !strings.Contains(body, `<meta charset="UTF-8" />`+tag) {
		t.Fatalf("script not after the charset: %q", body)
	}
	if strings.Index(body, `<meta charset`) >= 1024 || res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("the charset moved to %d, Content-Type %q", strings.Index(body, `<meta charset`), res.Header.Get("Content-Type"))
	}
	s := stampOf(t, body)
	o := p.targets[0].catchup
	o.mu.Lock()
	gen, rev := strconv.FormatUint(o.generation, 10), o.revision
	o.mu.Unlock()
	if s.Generation == nil || *s.Generation != gen || s.Revision != rev || s.Path != "/" || len(s.Protocols) != 1 || s.Protocols[0] != "vite-hmr" {
		t.Fatalf("stamp %+v, observer at %s/%d", s, gen, rev)
	}
	if res.StatusCode != 200 || res.Header.Get("ETag") != "" || res.Header.Get("Last-Modified") != "" || res.Header.Get("Content-Length") != "" {
		t.Fatalf("headers %v", res.Header)
	}
	fv.mu.Lock()
	seen := fv.pageHeader
	fv.mu.Unlock()
	if seen.Get("If-None-Match") != "" || seen.Get("If-Modified-Since") != "" {
		t.Fatalf("the app was asked conditionally: %v", seen)
	}
	if len(body) != len(fakePage)+len(scriptTag(stamp{known: true, fw: vite, generation: o.generation, revision: rev}, "")) {
		t.Fatalf("the page changed beyond the script: %q", body)
	}
}

// A page whose head runs a script before its <meta charset> gets the script
// before that one. The declaration then moves past the first 1024 bytes, so
// the response states the charset instead; one the response already states
// is kept.
func TestACharsetPushedBackIsStatedInTheResponse(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) {
		fv.page = strings.Replace(fakePage, `<script type="module"`, `<script>window.early = 1</script><script type="module"`, 1)
	})
	front, _, _, _ := catchupShare(t, fv, true)
	res, body := get(t, front, "/", navigate)
	if !strings.Contains(body, `<script data-purlview="catch-up">`) || strings.Index(body, `data-purlview`) > strings.Index(body, `window.early`) {
		t.Fatalf("script not before the early one: %q", body)
	}
	if strings.Index(body, `<meta charset`) < 1024 || res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("charset at %d, Content-Type %q", strings.Index(body, `<meta charset`), res.Header.Get("Content-Type"))
	}
	h := http.Header{"Content-Type": {"text/html; charset=iso-8859-1"}}
	declareCharset(h, "utf-8")
	if h.Get("Content-Type") != "text/html; charset=iso-8859-1" {
		t.Fatalf("a stated charset was replaced: %q", h.Get("Content-Type"))
	}
	for declared, want := range map[string]string{"UTF-16LE": "text/html; charset=utf-8", "Shift_JIS": "text/html; charset=shift_jis", `x"; y`: "text/html"} {
		h := http.Header{"Content-Type": {"text/html"}}
		declareCharset(h, declared)
		if h.Get("Content-Type") != want {
			t.Errorf("%q: %q", declared, h.Get("Content-Type"))
		}
	}
}

// The stamp is read when the request is forwarded, before the app renders:
// a change the app makes while it renders the page is newer than the
// stamp, so the page's check tells it to reload.
func TestTheStampIsTakenWhenTheRequestIsForwarded(t *testing.T) {
	fv := newFakeVite(t)
	front, p, _, _ := catchupShare(t, fv, true)
	o := p.targets[0].catchup
	get(t, front, "/", navigate)
	before := mustCheck(t, o)
	fv.set(func(fv *fakeVite) {
		fv.onPage = func() {
			fv.send(`{"type":"full-reload","path":"*"}`)
			eventually(t, "the observer counts the change", func() bool {
				o.mu.Lock()
				defer o.mu.Unlock()
				return o.revision == before.revision+1
			})
		}
	})
	_, body := get(t, front, "/", navigate)
	s := stampOf(t, body)
	if s == nil || s.Revision != before.revision {
		t.Fatalf("stamped %+v, revision before the request %d", s, before.revision)
	}
	res, answer := check(t, front, "/")
	if res.StatusCode != 200 || answer != fmt.Sprintf(`{"g":"%d","r":%d}`, before.generation, before.revision+1) {
		t.Fatalf("check: %d %q", res.StatusCode, answer)
	}
}

// An observer that cannot connect within the stamp's bound leaves the stamp
// unknown; the page still gets the script, and its check is unavailable.
func TestAnObserverThatCannotConnectStampsUnknown(t *testing.T) {
	fv := newFakeVite(t)
	front, p, _, _ := catchupShare(t, fv, true)
	o := p.targets[0].catchup
	connected(t, o) // the framework is known
	fv.set(func(fv *fakeVite) { fv.refuse = true })
	fv.kick()
	eventually(t, "the observer is down", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.conn == nil
	})
	o.stampWait = 100 * time.Millisecond
	start := time.Now()
	_, body := get(t, front, "/", navigate)
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Fatalf("the navigation waited %v", took)
	}
	if s := stampOf(t, body); s == nil || s.Generation != nil || !strings.Contains(body, `"g":null`) {
		t.Fatalf("stamp %+v in %q", s, body)
	}
	if res, _ := check(t, front, "/"); res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("check while down: %d %v", res.StatusCode, res.Header)
	}
}

// A check is answered by the daemon for the target its path selects, and
// never reaches an app, whatever the method or path.
func TestTheRevisionCheckNeverReachesTheApp(t *testing.T) {
	fv := newFakeVite(t)
	front, p, _, apiSeen := catchupShare(t, fv, true)
	get(t, front, "/", navigate)
	get(t, front, "/_purlview/t/2/", navigate) // the API is found unsupported
	eventually(t, "the API is found unsupported", func() bool { return !p.targets[1].catchup.applies() })
	for len(apiSeen) > 0 {
		<-apiSeen // the navigation and the observer's look for a client module
	}
	before := len(fv.requestsSoFar())
	a := mustCheck(t, p.targets[0].catchup)
	for _, path := range []string{"/", "/deep/page?q=1", "/_purlview/t/2/", "/_purlview/t/2/x", "/_purlview/t/9/"} {
		res, body := check(t, front, path)
		switch {
		case path == "/_purlview/t/9/":
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("%s: %d", path, res.StatusCode)
			}
		case strings.HasPrefix(path, "/_purlview/t/2/"):
			// The API has no catch-up: it is no supported framework.
			if res.StatusCode != http.StatusNotFound || res.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s: %d %v", path, res.StatusCode, res.Header)
			}
		default:
			if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/json" || res.Header.Get("Cache-Control") != "no-store" ||
				body != fmt.Sprintf(`{"g":"%d","r":%d}`, a.generation, a.revision) {
				t.Errorf("%s: %d %v %q", path, res.StatusCode, res.Header, body)
			}
		}
	}
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/", strings.NewReader("x"))
	req.Header.Set(revisionHeader, "1")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("POST check: %v %v", res, err)
	} else {
		_ = res.Body.Close()
	}
	if got := fv.requestsSoFar()[before:]; len(got) != 0 {
		t.Fatalf("checks reached the app: %v", got)
	}
	select {
	case got := <-apiSeen:
		t.Fatalf("a check reached the API: %s", got)
	default:
	}
}

// Nothing is inserted with --no-rewrite, into what is not HTML, into a
// response to anything but a navigation, or under a Content-Security-Policy
// that forbids inline scripts; with --no-rewrite a check is still answered
// by the daemon, as having no catch-up.
func TestWhatIsNotStamped(t *testing.T) {
	fv := newFakeVite(t)
	front, _, _, _ := catchupShare(t, fv, true)
	for name, c := range map[string]struct {
		path   string
		header http.Header
	}{
		"not HTML":          {"/data.json", navigate},
		"a fetch of a page": {"/", http.Header{"Sec-Fetch-Mode": {"cors"}, "Accept": {"text/html"}}},
		"a script":          {"/", http.Header{"Sec-Fetch-Mode": {"no-cors"}, "Sec-Fetch-Dest": {"script"}}},
	} {
		if _, body := get(t, front, c.path, c.header); strings.Contains(body, "data-purlview") {
			t.Errorf("%s: stamped %q", name, body)
		}
	}
	req, _ := http.NewRequest(http.MethodHead, front.URL+"/", nil)
	req.Header = navigate.Clone()
	if res, err := http.DefaultClient.Do(req); err != nil || res.ContentLength > 0 {
		t.Errorf("HEAD: %v %v", res, err)
	}
	// An API page that is no supported framework.
	if _, body := get(t, front, "/_purlview/t/2/", navigate); strings.Contains(body, "data-purlview") {
		t.Errorf("unsupported target stamped: %q", body)
	}
	// A Content-Security-Policy header that forbids inline scripts.
	for policy, stamped := range map[string]bool{
		"default-src 'self'":                                 false,
		"script-src 'self' 'unsafe-inline'":                  true,
		"script-src 'unsafe-inline' 'nonce-abc'":             false,
		"img-src *":                                          true,
		"default-src 'self'; script-src 'unsafe-inline' *":   true,
		"script-src 'unsafe-inline', script-src 'self'":      false,
		"script-src-elem 'self'; script-src 'unsafe-inline'": false,
		"script-src 'unsafe-inline'; connect-src 'none'":     false,
	} {
		csp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				fv.serve(w, r)
				return
			}
			w.Header().Set("Content-Security-Policy", policy)
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, fakePage)
		}))
		u, _ := url.Parse(csp.URL)
		targets, _ := newTargets(u, nil, nil)
		pp := newProxy(mustOrigin(t, livePublic), targets, true, nil)
		pp.targets[0].catchup.learn(vite, "/")
		srv := httptest.NewServer(pp)
		_, body := get(t, srv, "/", navigate)
		if got := strings.Contains(body, "data-purlview"); got != stamped {
			t.Errorf("CSP %q: stamped %v", policy, got)
		}
		srv.Close()
		pp.stop()
		pp.closeIdle()
		csp.Close()
	}
	// A <meta> policy in the head.
	fv.set(func(fv *fakeVite) {
		fv.page = strings.Replace(fakePage, `<title>`, `<meta http-equiv="Content-Security-Policy" content="script-src 'self' 'unsafe-inline'"><title>`, 1)
	})
	if _, body := get(t, front, "/", navigate); strings.Contains(body, "data-purlview") {
		t.Errorf("a page with a <meta> policy was stamped")
	}
	// --no-rewrite: no observer, no script, and the check stays here.
	plainVite := newFakeVite(t)
	plain, pp, _, _ := catchupShare(t, plainVite, false)
	if pp.targets[0].catchup != nil {
		t.Fatal("an observer without rewriting")
	}
	if _, body := get(t, plain, "/", navigate); strings.Contains(body, "data-purlview") || body != fakePage {
		t.Errorf("--no-rewrite stamped %q", body)
	}
	before := len(plainVite.requestsSoFar())
	if res, _ := check(t, plain, "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("--no-rewrite check: %d", res.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	if got := plainVite.requestsSoFar(); len(got) != before || len(plainVite.handshakesSoFar()) != 0 {
		t.Errorf("--no-rewrite: the app saw %v and %d handshakes", got[before:], len(plainVite.handshakesSoFar()))
	}
}

// openVisitorSocket opens a visitor's HMR socket through the share as a browser
// does, offering compression, and returns its reader.
func openVisitorSocket(t *testing.T, front *httptest.Server, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: k7m2p4qx.purlview.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: vite-hmr\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n\r\n", path, livePublic)
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	return conn, br
}

// nextMessage reads one unfragmented server message.
func nextMessage(t *testing.T, conn net.Conn, br *bufio.Reader) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	h, err := readFrameHeader(br)
	if err != nil || !h.fin || h.masked {
		t.Fatalf("frame %+v: %v", h, err)
	}
	payload := make([]byte, h.length)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// While the app is in error, a new visitor socket gets the error right after
// the app's first message, as Vite would have sent its buffered error to a
// client connecting while it had none. A change ends the replay. Visitor
// HMR sockets offer no compression to the app.
func TestTheAppsErrorIsReplayedToNewSocketsUntilAChange(t *testing.T) {
	// The fake reproduces Vite's buffering: with no client connected, an
	// error waits for the next one.
	alone := newFakeVite(t)
	const failure = `{"type":"error","err":{"message":"Unexpected token (3:4)","stack":"","id":"/src/pages/index.astro"}}`
	alone.send(failure)
	aloneShare, _, _, _ := catchupShare(t, alone, false)
	conn, br := openVisitorSocket(t, aloneShare, "/?token=SyntheticTok3n")
	if got := []string{nextMessage(t, conn, br), nextMessage(t, conn, br)}; got[0] != `{"type":"connected"}` || got[1] != failure {
		t.Fatalf("Vite's own buffering: %q", got)
	}

	fv := newFakeVite(t)
	front, p, _, _ := catchupShare(t, fv, true)
	o := p.targets[0].catchup
	get(t, front, "/", navigate)
	connected(t, o)
	fv.send(failure) // the observer is connected: Vite buffers nothing
	eventually(t, "the observer sees the error", func() bool { return o.failureMessage() != nil })
	first, firstReader := openVisitorSocket(t, front, "/?token=SyntheticTok3n")
	if got := []string{nextMessage(t, first, firstReader), nextMessage(t, first, firstReader)}; got[0] != `{"type":"connected"}` || got[1] != failure {
		t.Fatalf("replay: %q", got)
	}
	for _, h := range fv.handshakesSoFar() {
		if h.header.Get("Sec-Websocket-Extensions") != "" {
			t.Fatalf("an HMR handshake offered %q", h.header.Get("Sec-Websocket-Extensions"))
		}
	}
	const update = `{"type":"update","updates":[]}`
	fv.send(update)
	if got := nextMessage(t, first, firstReader); got != update {
		t.Fatalf("after the replay: %q", got)
	}
	eventually(t, "the observer sees the change", func() bool { return o.failureMessage() == nil })
	second, secondReader := openVisitorSocket(t, front, "/?token=SyntheticTok3n")
	const marker = `{"type":"custom","event":"marker"}`
	fv.send(marker)
	if got := []string{nextMessage(t, second, secondReader), nextMessage(t, second, secondReader)}; got[0] != `{"type":"connected"}` || got[1] != marker {
		t.Fatalf("after a change: %q", got)
	}
	// Each visitor socket is counted while it is open.
	o.mu.Lock()
	open := o.sockets
	o.mu.Unlock()
	if open != 2 {
		t.Fatalf("%d sockets open", open)
	}
	_ = first.Close()
	_ = second.Close()
	eventually(t, "the sockets are counted closed", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.sockets == 0
	})
}

// The replay goes in at a frame boundary however the app's bytes arrive:
// after a first message split across reads and fragments, with a control
// frame between the fragments, and before whatever came with it.
func TestTheReplayGoesInAtAMessageBoundary(t *testing.T) {
	o := newObserver(&url.URL{Scheme: "http", Host: "localhost:5173"}, nil, "localhost:5173", nil)
	o.failure = []byte(`{"type":"error"}`)
	var stream []byte
	stream = appendFrame(stream, false, opText, []byte(`{"type":`), false)
	stream = appendFrame(stream, true, opPing, []byte("p"), false)
	stream = appendFrame(stream, true, opContinuation, []byte(`"connected"}`), false)
	after := appendFrame(nil, true, opText, []byte(strings.Repeat("u", 300)), false)
	stream = append(stream, after...)
	want := append(append(append([]byte{}, stream[:len(stream)-len(after)]...), appendFrame(nil, true, opText, o.failure, false)...), after...)
	for size := 1; size <= len(stream); size++ {
		app := &fakeConn{r: &chunkReader{}}
		for rest := stream; len(rest) > 0; {
			n := min(size, len(rest))
			app.r.chunks, rest = append(app.r.chunks, rest[:n]), rest[n:]
		}
		v := newVisitorSocket(app, o)
		got, _ := io.ReadAll(v)
		if string(got) != string(want) {
			t.Fatalf("reads of %d: got %q\nwant %q", size, got, want)
		}
		if o.sockets != 1 {
			t.Fatalf("a socket being read counted %d", o.sockets)
		}
		_ = v.Close()
		_ = v.Close()
		if o.sockets != 0 {
			t.Fatalf("closing twice counted %d", o.sockets)
		}
	}
}

// An upgrade that fails after the app accepted it, before the proxy copies
// anything, never counts as an open visitor socket, whether or not this end
// is closed.
func TestAFailedUpgradeIsNotCountedOpen(t *testing.T) {
	o := newObserver(&url.URL{Scheme: "http", Host: "localhost:5173"}, nil, "localhost:5173", nil)
	never := newVisitorSocket(&fakeConn{r: &chunkReader{}}, o)
	if o.sockets != 0 {
		t.Fatalf("counted before a read: %d", o.sockets)
	}
	closed := newVisitorSocket(&fakeConn{r: &chunkReader{}}, o)
	_ = closed.Close()
	if _, err := closed.Read(make([]byte, 8)); err == nil || o.sockets != 0 {
		t.Fatalf("closed before a read: %d, %v", o.sockets, err)
	}
	_ = never
}

type fakeConn struct {
	r *chunkReader
	io.Writer
}

func (c *fakeConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *fakeConn) Close() error               { return nil }

func TestContentSecurityPolicies(t *testing.T) {
	for policy, want := range map[string]bool{
		"":                                                    true,
		"img-src 'self'; style-src 'self'":                    true,
		"default-src 'self'":                                  false,
		"default-src *":                                       false,
		"default-src 'unsafe-inline'":                         true,
		"script-src 'none'":                                   false,
		"SCRIPT-SRC 'UNSAFE-INLINE'":                          true,
		"script-src 'unsafe-inline' 'sha256-x'":               false,
		"script-src 'unsafe-inline' 'nonce-a'":                false,
		"script-src 'unsafe-inline' 'strict-dynamic'":         false,
		"script-src-elem 'unsafe-inline'; default-src 'none'": true,
		"script-src 'unsafe-inline'; script-src 'self'":       true, // the first directive of a name wins
		"default-src 'none'; script-src 'unsafe-inline'":      true,
		"script-src 'unsafe-inline', default-src 'none'":      false, // two policies; both apply
	} {
		if got := cspAllowsInlineScript([]string{policy}); got != want {
			t.Errorf("%q: %v", policy, got)
		}
	}
	if cspAllowsInlineScript([]string{"script-src 'unsafe-inline'", "script-src 'self'"}) {
		t.Error("every header's policy applies")
	}
	// The script's one request goes to the page's own origin.
	for policy, want := range map[string]bool{
		"":                           true,
		"script-src 'unsafe-inline'": true,
		"connect-src 'self'":         true,
		"connect-src *":              true,
		"connect-src https:":         true,
		"connect-src 'none'":         false,
		"connect-src https://api.example.invalid": false,
		"default-src 'none'":                      false,
		"default-src 'none'; connect-src 'self'":  true,
	} {
		if got := cspAllowsSelfConnect([]string{policy}); got != want {
			t.Errorf("connect %q: %v", policy, got)
		}
	}
}

// A Vite app mounted as a further target is stamped with its mount, so its
// page's check selects it, and a visitor socket through the mount shows the
// observer the socket's address on the app itself.
func TestAMountedAppIsCaughtUpUnderItsMount(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.modulePath = "" }) // learned from the visitor's socket
	api := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(api.Close)
	admitted, _ := url.Parse(api.URL)
	targets, mismatch := newTargets(admitted, []string{api.URL, fv.URL}, nil)
	if mismatch != "" {
		t.Fatal(mismatch)
	}
	p := newProxy(mustOrigin(t, livePublic), targets, true, nil)
	front := httptest.NewServer(p)
	t.Cleanup(func() {
		front.Close()
		p.stop()
		p.closeIdle()
	})
	o := p.targets[1].catchup
	o.stampWait = 5 * time.Second
	get(t, front, "/_purlview/t/2/", navigate)
	openVisitorSocket(t, front, "/_purlview/t/2/?token=SyntheticTok3n")
	eventually(t, "the observer connects", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.conn != nil
	})
	o.mu.Lock()
	learned := *o.learned
	o.mu.Unlock()
	if learned.uri != "/?token=SyntheticTok3n" || learned.fw != vite {
		t.Fatalf("learned %+v", learned)
	}
	_, body := get(t, front, "/_purlview/t/2/", navigate)
	if s := stampOf(t, body); s == nil || s.Path != "/_purlview/t/2/" || s.Generation == nil {
		t.Fatalf("stamp %+v", s)
	}
	if res, _ := check(t, front, "/_purlview/t/2/"); res.StatusCode != 200 {
		t.Fatalf("check under the mount: %d", res.StatusCode)
	}
	get(t, front, "/", navigate) // the root's app is found unsupported
	if res, _ := check(t, front, "/"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("check at the root, whose app is not supported: %d", res.StatusCode)
	}
}

// A page requested before it is known whether the app is supported, as on
// the first visit to a cold dev server, gets an unknown stamp if it loads the
// framework's client, or if the app has been found supported by the time it
// answers; otherwise it gets nothing.
func TestAPageStampedBeforeTheAppIsKnownGetsAnUnknownStamp(t *testing.T) {
	for name, c := range map[string]struct {
		page    string
		render  time.Duration
		stamped bool
	}{
		"loads the client":             {fakePage, 0, true},
		"found supported meanwhile":    {`<html><head><title>x</title></head></html>`, 600 * time.Millisecond, true},
		"neither, before the decision": {`<html><head><title>x</title></head></html>`, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			fv := newFakeVite(t)
			fv.set(func(fv *fakeVite) {
				fv.moduleWait = 300 * time.Millisecond
				fv.page = c.page
				if c.render > 0 {
					fv.onPage = func() { time.Sleep(c.render) }
				}
			})
			front, p, _, _ := catchupShare(t, fv, true)
			p.targets[0].catchup.stampWait = 50 * time.Millisecond
			_, body := get(t, front, "/", navigate)
			s := stampOf(t, body)
			if (s != nil) != c.stamped || s != nil && (s.Generation != nil || s.Protocols[0] != "vite-hmr") {
				t.Fatalf("stamp %+v in %q", s, body)
			}
		})
	}
}

// A change saved while the observer is down reaches a page stamped before
// it: the observer comes back with another generation, which tells the page
// to reload, and it counts the change it is sent as it connects.
func TestAChangeWhileTheObserverIsDownChangesTheGeneration(t *testing.T) {
	fv := newFakeVite(t)
	front, _, _, _ := catchupShare(t, fv, true)
	_, body := get(t, front, "/", navigate)
	s := stampOf(t, body)
	if s == nil || s.Generation == nil {
		t.Fatalf("stamp %+v", s)
	}
	fv.set(func(fv *fakeVite) { fv.refuse = true })
	fv.kick()
	eventually(t, "the observer is down", func() bool { return fv.clientCount() == 0 })
	fv.send(`{"type":"full-reload","path":"*"}`) // no client: Vite buffers it
	fv.set(func(fv *fakeVite) { fv.refuse = false })
	var answer string
	eventually(t, "the check is answered", func() bool {
		res, a := check(t, front, "/")
		answer = a
		return res.StatusCode == 200
	})
	var got struct {
		G string `json:"g"`
		R uint64 `json:"r"`
	}
	if err := json.Unmarshal([]byte(answer), &got); err != nil || got.G == *s.Generation || got.R != s.Revision+1 {
		t.Fatalf("stamped %s/%d, answered %q", *s.Generation, s.Revision, answer)
	}
}

// Only a navigation that asks for an HTML document loses its conditional
// headers; any other is forwarded as the browser sent it.
func TestOnlyDocumentNavigationsAreAskedInFull(t *testing.T) {
	fv := newFakeVite(t)
	front, _, _, _ := catchupShare(t, fv, true)
	for dest, stripped := range map[string]bool{"document": true, "iframe": true, "embed": false, "object": false, "": true} {
		h := http.Header{"Sec-Fetch-Mode": {"navigate"}, "If-None-Match": {`"page-1"`}}
		if dest != "" {
			h.Set("Sec-Fetch-Dest", dest)
		} else {
			h.Set("Accept", "text/html")
			h.Del("Sec-Fetch-Mode")
		}
		get(t, front, "/", h)
		fv.mu.Lock()
		seen := fv.pageHeader.Get("If-None-Match")
		fv.mu.Unlock()
		if (seen == "") != stripped {
			t.Errorf("Sec-Fetch-Dest %q: the app saw If-None-Match %q", dest, seen)
		}
	}
}

// A visitor's HMR socket is served on a lane stream like any request; once
// upgraded it belongs to the proxy, not to the stream's HTTP server, so it
// outlives the stream's idle timeout, and the replay and later messages
// still reach the visitor.
func TestAVisitorSocketOutlivesTheStreamIdleTimeout(t *testing.T) {
	fv := newFakeVite(t)
	front, p, _, _ := catchupShare(t, fv, true)
	o := p.targets[0].catchup
	get(t, front, "/", navigate)
	connected(t, o)
	const failure = `{"type":"error","err":{"message":"x"}}`
	fv.send(failure)
	eventually(t, "the observer sees the error", func() bool { return o.failureMessage() != nil })
	const idle = 100 * time.Millisecond
	gateway, _, _ := servedStream(t, p, time.Second, idle)
	_ = gateway.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(gateway, "GET /?token=SyntheticTok3n HTTP/1.1\r\nHost: k7m2p4qx.purlview.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: vite-hmr\r\n\r\n", livePublic)
	br := bufio.NewReader(gateway)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	if got := []string{nextMessage(t, gateway, br), nextMessage(t, gateway, br)}; got[0] != `{"type":"connected"}` || got[1] != failure {
		t.Fatalf("on the stream: %q", got)
	}
	time.Sleep(5 * idle)
	const update = `{"type":"update","updates":[]}`
	fv.send(update)
	if got := nextMessage(t, gateway, br); got != update {
		t.Fatalf("after the idle timeout: %q", got)
	}
}
