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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVite is a development server with the HMR behaviour of Vite 8.3 that
// the catch-up depends on (vite/dist/node/chunks/node.js,
// createWebSocketServer):
//
//   - its client module at /@vite/client has its configuration substituted
//     as Vite substitutes it: directSocketHost is host, port and base, while
//     the socket's path, base joined with server.hmr.path, is the last
//     literal of socketHost;
//   - a handshake must offer the vite-hmr subprotocol at the HMR path, and
//     must carry the token only when it has an Origin; an upgrade at any
//     other path, or for another subprotocol, is left unanswered;
//   - an index.html gets its client script prepended to the head, before
//     the template's <meta charset>, and is served as text/html with no
//     charset;
//   - a new client is registered, then sent {"type":"connected"}, then the
//     buffered message if there is one;
//   - an error or full-reload sent while no client is connected is buffered
//     for the next client; anything else is sent to every client in one
//     in-order loop;
//   - a ping is answered with a pong written into the same ordered stream.
type fakeVite struct {
	*httptest.Server
	t *testing.T

	mu         sync.Mutex
	base       string // the dev server's base
	path       string // the HMR socket's path: base joined with server.hmr.path
	token      string
	modulePath string        // where the client module is served; "" for nowhere
	advertised string        // the socket path the client module names, if not path
	moduleWait time.Duration // how long the client module takes
	hmrPort    string        // the client module's hmrPort; "" for null
	acceptWait time.Duration // how long a handshake takes to be accepted
	refuse     bool          // refuse every handshake
	silent     bool          // answer no ping
	clients    map[*fakeClient]bool
	buffered   []byte
	handshakes []fakeHandshake
	requests   []string // every other request's method and path, in order
	page       string   // the HTML served for any other GET
	pageHeader http.Header
	onPage     func() // runs before a page is rendered

	hung   []net.Conn    // upgrades left unanswered
	pinged chan struct{} // receives after each pong is written
}

type fakeHandshake struct {
	at     time.Time
	header http.Header
	uri    string
}

type fakeClient struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *fakeClient) write(frame []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.conn.Write(frame)
}

// fakePage is an index.html as Vite 8.3 serves it: the client prepended to
// the head, before the template's own <meta charset>.
const fakePage = `<!doctype html>
<html lang="en">
  <head>
    <script type="module" src="/@vite/client"></script>

    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>Demo</title>
  </head>
  <body>
    <h1>Save 1</h1>
    <script type="module" src="/src/main.ts"></script>
  </body>
</html>
`

func newFakeVite(t *testing.T) *fakeVite {
	t.Helper()
	fv := &fakeVite{t: t, base: "/", path: "/", token: "SyntheticTok3n", modulePath: "/@vite/client", clients: map[*fakeClient]bool{}, page: fakePage, pinged: make(chan struct{}, 64)}
	fv.Server = httptest.NewServer(http.HandlerFunc(fv.serve))
	t.Cleanup(func() {
		fv.kick()
		fv.Close()
	})
	return fv
}

func (fv *fakeVite) serve(w http.ResponseWriter, r *http.Request) {
	fv.mu.Lock()
	base, path, token, modulePath, page := fv.base, fv.path, fv.token, fv.modulePath, fv.page
	advertised, moduleWait, hmrPort := fv.advertised, fv.moduleWait, fv.hmrPort
	if hmrPort == "" {
		hmrPort = "null"
	}
	fv.mu.Unlock()
	if advertised == "" {
		advertised = path
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		fv.upgrade(w, r, path, token)
		return
	}
	fv.mu.Lock()
	fv.requests = append(fv.requests, r.Method+" "+r.URL.RequestURI())
	onPage := fv.onPage
	fv.mu.Unlock()
	if modulePath != "" && r.URL.Path == modulePath {
		time.Sleep(moduleWait)
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = fmt.Fprintf(w, "console.debug(\"[vite] connecting...\");\nconst importMetaUrl = new URL(import.meta.url);\nconst serverHost = %q;\nconst socketProtocol = null || (importMetaUrl.protocol === \"https:\" ? \"wss\" : \"ws\");\nconst hmrPort = %s;\nconst socketHost = `${null || importMetaUrl.hostname}:${hmrPort || importMetaUrl.port}${%q}`;\nconst directSocketHost = %q;\nconst base = \"/\" || \"/\";\nconst hmrTimeout = 30000;\nconst wsToken = %q;\n",
			r.Host+base, hmrPort, advertised, r.Host+base, token)
		return
	}
	if r.URL.Path == modulePath || r.URL.Path == "/@vite/client" {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/data.json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"script":"<script>"}`)
		return
	}
	if onPage != nil {
		onPage()
	}
	fv.mu.Lock()
	fv.pageHeader = r.Header.Clone()
	fv.mu.Unlock()
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("ETag", `"page-1"`)
	w.Header().Set("Last-Modified", "Tue, 22 Sep 2026 14:47:19 GMT")
	_, _ = io.WriteString(w, page)
}

func (fv *fakeVite) upgrade(w http.ResponseWriter, r *http.Request, path, token string) {
	fv.mu.Lock()
	fv.handshakes = append(fv.handshakes, fakeHandshake{at: time.Now(), header: r.Header.Clone(), uri: r.URL.RequestURI()})
	refuse, acceptWait := fv.refuse, fv.acceptWait
	fv.mu.Unlock()
	time.Sleep(acceptWait)
	if r.Header.Get("Sec-Websocket-Protocol") != "vite-hmr" || r.URL.Path != path {
		// Vite's upgrade listener ignores it, and nothing else answers.
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			fv.mu.Lock()
			fv.hung = append(fv.hung, conn)
			fv.mu.Unlock()
		}
		return
	}
	if refuse {
		http.Error(w, "refused", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Origin") != "" && r.URL.Query().Get("token") != token {
		http.Error(w, "refused", http.StatusBadRequest)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	// As in ws, the client is registered as its handshake is answered, before
	// anything else can happen on the server.
	c := &fakeClient{conn: conn}
	fv.mu.Lock()
	fv.clients[c] = true
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: vite-hmr\r\n\r\n", acceptKey(r.Header.Get("Sec-Websocket-Key")))
	_ = rw.Flush()
	c.write(appendFrame(nil, true, opText, []byte(`{"type":"connected"}`), false))
	if fv.buffered != nil {
		c.write(appendFrame(nil, true, opText, fv.buffered, false))
		fv.buffered = nil
	}
	fv.mu.Unlock()
	go fv.read(c, rw.Reader)
}

// read answers the client's pings until it goes.
func (fv *fakeVite) read(c *fakeClient, br *bufio.Reader) {
	defer func() {
		fv.mu.Lock()
		delete(fv.clients, c)
		fv.mu.Unlock()
		_ = c.conn.Close()
	}()
	for {
		h, err := readFrameHeader(br)
		if err != nil || !h.masked {
			return
		}
		payload := make([]byte, h.length)
		if _, err := io.ReadFull(br, payload); err != nil {
			return
		}
		maskBytes(h.mask, 0, payload)
		switch h.opcode {
		case opPing:
			fv.mu.Lock()
			silent := fv.silent
			fv.mu.Unlock()
			if !silent {
				c.write(appendFrame(nil, true, opPong, payload, false))
				select {
				case fv.pinged <- struct{}{}:
				default:
				}
			}
		case opClose:
			c.write(appendFrame(nil, true, opClose, nil, false))
			return
		}
	}
}

// send is Vite's hot channel send.
func (fv *fakeVite) send(payload string) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	typ := messageType([]byte(payload))
	if (typ == "error" || typ == "full-reload") && len(fv.clients) == 0 {
		fv.buffered = []byte(payload)
		return
	}
	frame := appendFrame(nil, true, opText, []byte(payload), false)
	for c := range fv.clients {
		c.write(frame)
	}
}

// sendFragmented sends payload to every client as two fragments, calling
// between after the first: the server may send control frames in between.
func (fv *fakeVite) sendFragmented(payload string, cut int, between func()) {
	fv.mu.Lock()
	clients := make([]*fakeClient, 0, len(fv.clients))
	for c := range fv.clients {
		clients = append(clients, c)
	}
	fv.mu.Unlock()
	for _, c := range clients {
		c.write(appendFrame(nil, false, opText, []byte(payload[:cut]), false))
	}
	between()
	for _, c := range clients {
		c.write(appendFrame(nil, true, opContinuation, []byte(payload[cut:]), false))
	}
}

// kick drops every client, as a restarting dev server does.
func (fv *fakeVite) kick() {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	for c := range fv.clients {
		_ = c.conn.Close()
	}
	for _, c := range fv.hung {
		_ = c.Close()
	}
	fv.hung = nil
}

func (fv *fakeVite) clientCount() int {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	return len(fv.clients)
}

func (fv *fakeVite) set(f func(*fakeVite)) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	f(fv)
}

func (fv *fakeVite) handshakesSoFar() []fakeHandshake {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	return append([]fakeHandshake(nil), fv.handshakes...)
}

func (fv *fakeVite) requestsSoFar() []string {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	return append([]string(nil), fv.requests...)
}

// eventually waits up to 5 s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
