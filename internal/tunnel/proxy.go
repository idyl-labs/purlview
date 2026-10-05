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
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// proxy forwards gateway requests to the share's targets. The mount index in
// the raw path is the only thing that selects a target: everything else goes
// to the first, and an index the share does not have is answered here with a
// bare 404 that no watcher counts. Each target has its own reverse proxy and
// transport, which dials that target's host and port and nothing else, uses
// no proxy and follows no redirect.
type proxy struct {
	targets []*target
}

// newProxy builds the share's proxy. public is the share's origin; without
// one nothing is translated or rewritten, as without targets nothing is
// mounted. Each target's watch observes its requests for the share's visitor
// and app events and answers for an app that does not. With rewriting, each
// target also has a catch-up observer (observer.go), which runs only while
// visitors are present and ends with stop. ModifyResponse must never return
// an error: the proxy would hand the request to the error handler, which
// marks the app unresponsive although it answered.
func newProxy(public origin, targets []*target, rewrite bool, logger *log.Logger) *proxy {
	set := newNeedleSet(public, targets)
	if !rewrite {
		set = newNeedleSet(origin{}, nil)
	}
	for _, t := range targets {
		t.rp = newTargetProxy(public, t, targets, set, rewrite, logger)
		if rewrite {
			t.catchup = newObserver(t.url, t.rp.Transport, t.name, logger)
		}
	}
	return &proxy{targets: targets}
}

// ServeHTTP selects the target by the mount alone. A page's catch-up check
// is answered here for that target and never reaches the app.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := p.targets[0]
	if n, _, ok := cutMount(r.URL.EscapedPath()); ok {
		if n > len(p.targets) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t = p.targets[n-1]
	}
	if r.Header.Get(revisionHeader) != "" {
		answerRevision(w, r, t.catchup)
		return
	}
	t.rp.ServeHTTP(w, r)
}

// stop ends the targets' catch-up observers and waits for them.
func (p *proxy) stop() {
	for _, t := range p.targets {
		if t.catchup != nil {
			t.catchup.stop()
		}
	}
}

// closeIdle releases every target's idle connections.
func (p *proxy) closeIdle() {
	for _, t := range p.targets {
		t.rp.Transport.(*http.Transport).CloseIdleConnections()
	}
}

// watches lists the targets' watchers, which share one wake channel.
func (p *proxy) watches() []*appWatch {
	out := make([]*appWatch, len(p.targets))
	for i, t := range p.targets {
		out[i] = t.watch
	}
	return out
}

// appIdleConnTimeout is how long an idle connection to an app is kept for
// reuse. It must stay below the idle timeout of every development server we
// forward to (Node's http.Server, so Next.js, Vite and Express, and uvicorn
// close after 5s; gunicorn after 2s). A connection reused just as the app
// closes it fails, and Go retries only requests it can replay, so a visitor's
// POST would get the unresponsive 502. Connections to the app are local and
// cheap to open again.
const appIdleConnTimeout = time.Second

func newTargetProxy(public origin, t *target, targets []*target, set *needleSet, rewrite bool, logger *log.Logger) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0), ErrorHandler: t.watch.unanswered}
	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		t.watch.request(pr.In)
		if t.mount != "" {
			// The mount decides the host; what follows it is the path, kept
			// as the browser spelled it.
			_, rest, _ := cutMount(pr.In.URL.EscapedPath())
			if rest == "" {
				rest = "/"
			}
			pr.Out.URL.RawPath = rest
			pr.Out.URL.Path, _ = url.PathUnescape(rest)
		}
		pr.SetURL(t.url)
		pr.Out.Host = t.url.Host
		translateRequest(pr.Out.Header, public, t, targets)
		if rewrite {
			// The rewrite reads bodies as the app wrote them; identity is the
			// one encoding every development server honours.
			pr.Out.Header.Set("Accept-Encoding", "identity")
		}
		if t.catchup != nil {
			catchupRequest(pr, t)
		}
	}
	rp.Transport = &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 32, IdleConnTimeout: appIdleConnTimeout, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	rp.ModifyResponse = func(r *http.Response) error {
		t.watch.answered(r)
		translateResponse(r.Header, public, t, targets)
		cookies := r.Header.Values("Set-Cookie")
		r.Header.Del("Set-Cookie")
		for _, value := range cookies {
			if kept, ok := hostOnlyCookie(value); ok {
				r.Header.Add("Set-Cookie", kept)
			}
		}
		rewriteResponse(r, set, logger)
		if t.catchup != nil {
			catchupResponse(r, t)
		}
		return nil
	}
	return rp
}

// catchupRequest prepares a request for the catch-up as it is forwarded,
// once its address is the target's. A navigation starts the observer and is
// stamped with the target's state now, before the app renders the page. A
// visitor HMR upgrade starts the observer too, and offers no extension, so
// the app's frames reach the proxy uncompressed and an error can be
// replayed between them.
func catchupRequest(pr *httputil.ProxyRequest, t *target) {
	if navigation(pr.In) {
		t.catchup.navigated()
		if s := t.catchup.stamp(pr.In.Context()); s.script || s.pending {
			if wantsHTML(pr.In) {
				stripConditionals(pr.Out.Header)
			}
			pr.Out = withValue(pr.Out, stampKey{}, s)
		}
		return
	}
	if fw := hmrUpgrade(pr.In); fw != nil {
		pr.Out.Header.Del("Sec-Websocket-Extensions")
		pr.Out = withValue(pr.Out, upgradeKey{}, hmrSocket{fw: fw, uri: pr.Out.URL.RequestURI()})
	}
}

// catchupResponse stamps a navigation's HTML page, or follows a visitor HMR
// socket the app accepted. A page stamped before the target's framework was
// known gets an unknown stamp if the framework is known by now, or if the
// page loads the framework's client.
func catchupResponse(r *http.Response, t *target) {
	if r.Request == nil {
		return
	}
	ctx := r.Request.Context()
	if s, ok := ctx.Value(stampKey{}).(stamp); ok && insertable(r) {
		insertScript(r, func(head []byte) []byte {
			if s.pending {
				fw := t.catchup.supportedFramework()
				if fw == nil && t.catchup.undecided() {
					fw = loadsClient(head)
				}
				if fw == nil {
					return nil
				}
				s = stamp{script: true, fw: fw}
			}
			return scriptTag(s, t.mount)
		})
		return
	}
	if u, ok := ctx.Value(upgradeKey{}).(hmrSocket); ok && r.StatusCode == http.StatusSwitchingProtocols && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		if app, ok := r.Body.(io.ReadWriteCloser); ok {
			t.catchup.learn(u.fw, u.uri)
			r.Body = newVisitorSocket(app, t.catchup)
		}
	}
}

// hostOnlyCookie keeps an application cookie on the share's own host: it drops
// every Domain attribute, however it is spelled, and refuses Purlview's
// reserved names, which cannot replace the gate. A browser reads an
// attribute's name as the text before its first "=", without the spaces
// around it and in any case (RFC 6265 section 5.2), so this does too. Path is
// left alone: locally the cookie jar keys on host, not port, and double-submit
// CSRF patterns read an API's cookie from the front end's page.
func hostOnlyCookie(value string) (string, bool) {
	parts := strings.Split(value, ";")
	name, _, _ := strings.Cut(parts[0], "=")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "__host-purlview-") {
		return "", false
	}
	kept := parts[:1]
	for _, p := range parts[1:] {
		attribute, _, _ := strings.Cut(p, "=")
		if !strings.EqualFold(strings.TrimSpace(attribute), "domain") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ";"), true
}
