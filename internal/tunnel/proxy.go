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
// and app events and answers for an app that does not. ModifyResponse must
// never return an error: the proxy would hand the request to the error
// handler, which marks the app unresponsive although it answered.
func newProxy(public origin, targets []*target, rewrite bool, logger *log.Logger) *proxy {
	set := newNeedleSet(public, targets)
	if !rewrite {
		set = newNeedleSet(origin{}, nil)
	}
	for _, t := range targets {
		t.rp = newTargetProxy(public, t, targets, set, rewrite, logger)
	}
	return &proxy{targets: targets}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n, _, ok := cutMount(r.URL.EscapedPath())
	if !ok {
		p.targets[0].rp.ServeHTTP(w, r)
		return
	}
	if n > len(p.targets) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p.targets[n-1].rp.ServeHTTP(w, r)
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
		return nil
	}
	return rp
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
