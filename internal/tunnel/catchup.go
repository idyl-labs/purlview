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
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The parts of the catch-up (observer.go) that touch requests and responses:
// the page's script and its stamp, the daemon's answer to the page's check,
// and the replay of the app's error to new visitor sockets.

// revisionHeader marks the page's check. A request that carries it is
// answered by the daemon, for the target its path selects, and never reaches
// the app. The value is the check's version, "1"; the daemon accepts any.
//
// The answer is 200 with {"g":"<generation>","r":<revision>} after a
// successful barrier; 503 when the observer is not connected, the barrier
// timed out, or it is not yet known whether the target is supported, which
// the page retries; and 404 when the target has no catch-up, which ends the
// page's checks. None may be cached.
const revisionHeader = "Purlview-Revision"

// catchupScript is the minified form of catchup.js: a function expression
// the inserted tag calls with the page's stamp. Edit both together;
// catchup_script_test.go runs the same cases against each and holds the size
// under 2 KB.
const catchupScript = `(function(s){var W=window.WebSocket,F=window.fetch,c=document.currentScript,b=0,f=0,a=0,n=0,t;c&&c.remove();if(!W||!F)return;function o(u){try{var l=new URL(u,document.baseURI),p=l.pathname+"/";return l.host===location.host&&!p.indexOf(s.u)&&(s.u!=="/"||!!p.indexOf("/_purlview/t/"))}catch(e){return!1}}function h(u,p){p=[].concat(p===void 0?[]:p);for(var i=0;i<p.length;i++)if(s.p.indexOf(String(p[i]))>=0)return o(u);return!1}function WebSocket(u){if(!new.target)throw new TypeError("Failed to construct 'WebSocket': Please use the 'new' operator.");var w=Reflect.construct(W,arguments,new.target);if(!b&&h(u,arguments[1]))w.addEventListener("open",g);return w}Object.setPrototypeOf(WebSocket,Object.getPrototypeOf(W));["prototype","CONNECTING","OPEN","CLOSING","CLOSED"].forEach(function(k){Object.defineProperty(WebSocket,k,Object.getOwnPropertyDescriptor(W,k))});window.WebSocket=WebSocket;window.WebSocket===WebSocket&&(W.prototype.constructor=WebSocket);function g(){if(b)return;b=1;document.addEventListener("visibilitychange",function(){document.hidden||k()});k()}function k(){if(f||a)return;a=1;clearTimeout(t);F(new URL(s.u,location.href).href,{headers:{"Purlview-Revision":"1"},cache:"no-store"}).then(function(r){if(r.status===404||r.status===200&&!/json/.test(r.headers.get("Content-Type")))return null;if(r.status!==200)throw Error("unavailable");return r.json()}).then(function(r){f=1;r&&d(r)},function(){a=0;y()})}function y(){var l=n<4?1e3<<n:1e4;n++;t=setTimeout(function(){(n<=4||!document.hidden)&&k()},l)}function d(r){if(s.g!==null&&r.r>s.r)location.reload();else if((s.g===null||r.g!==s.g)&&m())location.reload()}function m(){try{var k="purlview-catch-up",v=String(Date.now()),l=Number(sessionStorage.getItem(k));if(l&&Math.abs(Date.now()-l)<3e4)return!1;sessionStorage.setItem(k,v);return sessionStorage.getItem(k)===v}catch(e){return!1}}})`

// stampArgument is the script's argument; see catchup.js.
type stampArgument struct {
	Generation *string  `json:"g"`
	Revision   uint64   `json:"r"`
	Path       string   `json:"u"`
	Protocols  []string `json:"p"`
}

// scriptTag is the element inserted into a stamped page. mount is the
// target's mount, so the check selects the same target as the page.
// json.Marshal escapes <, > and &, so the stamp cannot end the element.
func scriptTag(s stamp, mount string) []byte {
	arg := stampArgument{Revision: s.revision, Path: mount + "/", Protocols: []string{s.fw.subprotocol}}
	if s.known {
		g := strconv.FormatUint(s.generation, 10)
		arg.Generation = &g
	}
	b, _ := json.Marshal(arg)
	return []byte(`<script data-purlview="catch-up">` + catchupScript + "(" + string(b) + ")</script>")
}

// answerRevision answers a page's check with o, the observer of the target
// the check's path selects; nil when the target has no catch-up.
func answerRevision(w http.ResponseWriter, r *http.Request, o *observer) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	if o == nil || !o.applies() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	a, ok := o.check(r.Context())
	if !ok {
		h.Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"g":"`+strconv.FormatUint(a.generation, 10)+`","r":`+strconv.FormatUint(a.revision, 10)+`}`)
}

// Values the proxy passes from a request's forwarding to its response.
type (
	stampKey   struct{}
	upgradeKey struct{}
)

// hmrSocket is a visitor HMR upgrade on its way to the app.
type hmrSocket struct {
	fw  *framework
	uri string
}

// withValue sets a value on the request the proxy forwards; ModifyResponse
// finds it on the response's request.
func withValue(r *http.Request, key, value any) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), key, value))
}

// wantsHTML reports a navigation that asks for an HTML document: one the
// browser makes for a document or frame, or, without fetch metadata, one
// that accepts HTML.
func wantsHTML(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe", "frame":
		return true
	case "":
	default:
		return false
	}
	for _, accept := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(accept), "text/html") {
			return true
		}
	}
	return false
}

// stripConditionals makes the app render a stamped page in full: a 304 would
// let the browser show the copy it has, with an older stamp, and that page
// would reload again and again for the same change.
func stripConditionals(h http.Header) {
	h.Del("If-None-Match")
	h.Del("If-Modified-Since")
}

// insertable reports whether a response to a stamped navigation is an HTML
// page the script can be added to: a GET's body of HTML, uncompressed, in an
// ASCII-compatible charset, under no Content-Security-Policy header that
// forbids inline scripts or the script's request to the page's own origin.
// A <meta> policy is looked for while inserting.
func insertable(res *http.Response) bool {
	if res.Request == nil || res.Request.Method != http.MethodGet || !rewritableBody(res) {
		return false
	}
	mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || mt != "text/html" {
		return false
	}
	policies := res.Header.Values("Content-Security-Policy")
	return cspAllowsInlineScript(policies) && cspAllowsSelfConnect(policies)
}

// insertScript adds a script to a page on its way to the browser. tag is
// given the start of the page, up to where its place is known, and returns
// the element to insert, or nil to leave the page as it is. It is called
// before the response's headers are sent, so that the page's charset can be
// stated in them: a page whose <meta charset> the script pushes beyond the
// first 1024 bytes, where browsers look for it, still decodes as it did.
// The response's headers therefore wait for the start of the body, as far as
// the inserter holds it back anyway: until </head> or <body>, at most 64 KiB,
// or the end of the body. Development servers send a page's head at once,
// so this costs nothing in practice, and it is bounded when they do not.
// The page's validators go: they named the app's bytes, and a browser that
// revalidated with them could keep a page with an older stamp.
func insertScript(res *http.Response, tag func(head []byte) []byte) {
	in := newInserter(res.Body, nil)
	in.prime()
	res.Body = in
	if in.at() < 0 {
		return
	}
	if in.tag = tag(in.head()); in.tag == nil {
		return
	}
	res.Header.Del("Content-Length")
	res.ContentLength = -1
	res.Header.Del("Accept-Ranges")
	res.Header.Del("ETag")
	res.Header.Del("Last-Modified")
	if cs := in.scan.declared; cs != "" {
		declareCharset(res.Header, cs)
	}
}

// declareCharset adds the charset a page declares in a <meta> to its
// Content-Type, unless that already names one. As browsers do, a declared
// UTF-16 is read as UTF-8: the bytes before it were ASCII.
func declareCharset(h http.Header, charset string) {
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || params["charset"] != "" {
		return
	}
	cs := strings.ToLower(charset)
	if strings.HasPrefix(cs, "utf-16") {
		cs = "utf-8"
	}
	for _, c := range cs {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && !strings.ContainsRune("-_.:", c) {
			return
		}
	}
	params["charset"] = cs
	h.Set("Content-Type", mime.FormatMediaType(mt, params))
}

// cspAllowsSelfConnect reports whether every policy lets the page fetch from
// its own origin (connect-src, else default-src): without that the check
// could never be answered.
func cspAllowsSelfConnect(values []string) bool {
	for _, v := range values {
		for _, policy := range strings.Split(v, ",") {
			sources, ok := cspDirective(policy, "connect-src", "default-src")
			if !ok {
				continue
			}
			allowed := false
			for _, src := range sources {
				switch strings.ToLower(src) {
				case "'self'", "*", "https:", "http:":
					allowed = true
				}
			}
			if !allowed {
				return false
			}
		}
	}
	return true
}

// cspDirective is the source list of the first of names that a policy has.
func cspDirective(policy string, names ...string) ([]string, bool) {
	directives := map[string][]string{}
	for _, d := range strings.Split(policy, ";") {
		fields := strings.Fields(d)
		if len(fields) == 0 {
			continue
		}
		name := strings.ToLower(fields[0])
		if _, seen := directives[name]; !seen {
			directives[name] = fields[1:]
		}
	}
	for _, name := range names {
		if sources, ok := directives[name]; ok {
			return sources, true
		}
	}
	return nil, false
}

// cspAllowsInlineScript reports whether an inline <script> may run under
// every policy in the Content-Security-Policy header values. Report-only
// policies block nothing and are not passed here.
func cspAllowsInlineScript(values []string) bool {
	for _, v := range values {
		for _, policy := range strings.Split(v, ",") {
			if !policyAllowsInlineScript(policy) {
				return false
			}
		}
	}
	return true
}

// policyAllowsInlineScript applies one policy's directive for script
// elements: script-src-elem, else script-src, else default-src. An inline
// script runs when that list has 'unsafe-inline' and no nonce, hash or
// 'strict-dynamic', each of which makes browsers ignore 'unsafe-inline'.
func policyAllowsInlineScript(policy string) bool {
	if sources, ok := cspDirective(policy, "script-src-elem", "script-src", "default-src"); ok {
		inline := false
		for _, s := range sources {
			switch s = strings.ToLower(s); {
			case s == "'unsafe-inline'":
				inline = true
			case s == "'strict-dynamic'", strings.HasPrefix(s, "'nonce-"), strings.HasPrefix(s, "'sha256-"), strings.HasPrefix(s, "'sha384-"), strings.HasPrefix(s, "'sha512-"):
				return false
			}
		}
		return inline
	}
	return true
}

// visitorSocket is the app's end of a visitor's HMR socket, as the reverse
// proxy copies it to the visitor. While the app is in error it adds the
// error message after the app's first message, at a frame boundary: Vite
// would have buffered that error for a client connecting while it had none,
// but the observer means it always has one. The app's frames are never
// compressed, as the upgrade offered no extension, so the message is sent
// as it was received.
//
// The socket counts as open from the proxy's first read, which comes only
// once the upgrade has reached the visitor, until it is closed. An upgrade
// that fails before that is never counted, even if the proxy never closes
// this end.
type visitorSocket struct {
	io.ReadWriteCloser
	o        *observer
	scanning bool
	scan     messageEnd
	pending  []byte

	mu      sync.Mutex
	counted bool
	closed  bool
}

func newVisitorSocket(app io.ReadWriteCloser, o *observer) *visitorSocket {
	return &visitorSocket{ReadWriteCloser: app, o: o, scanning: true}
}

// Read is called by one goroutine, the proxy's copy to the visitor.
func (v *visitorSocket) Read(p []byte) (int, error) {
	v.mu.Lock()
	if !v.counted && !v.closed {
		v.counted = true
		v.o.socketOpened()
	}
	v.mu.Unlock()
	if len(v.pending) > 0 {
		n := copy(p, v.pending)
		v.pending = v.pending[n:]
		return n, nil
	}
	n, err := v.ReadWriteCloser.Read(p)
	if !v.scanning || n == 0 {
		return n, err
	}
	end := v.scan.scan(p[:n])
	if end < 0 {
		return n, err
	}
	v.scanning = false
	failure := v.o.failureMessage()
	if failure == nil || err != nil {
		return n, err
	}
	v.pending = appendFrame(nil, true, opText, failure, false)
	v.pending = append(v.pending, p[end:n]...)
	return end, nil
}

// Close ends the socket and, if it was counted open, counts it closed, once.
func (v *visitorSocket) Close() error {
	err := v.ReadWriteCloser.Close()
	v.mu.Lock()
	if v.counted && !v.closed {
		v.o.socketClosed()
	}
	v.closed = true
	v.mu.Unlock()
	return err
}
