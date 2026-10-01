// Package tunnel docks the daemon's shares on Purlview's edge and serves the
// requests the gateway forwards to each share's targets.
package tunnel

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/idyl-labs/hyperplane-go/dock"

	"github.com/idyl-labs/purlview/internal/share/engine"
)

// Client docks shares on the edge. Logger, when set, receives the
// daemon's diagnostics about a share's targets and rewriting.
type Client struct {
	Logger *log.Logger
	// ConnectIP, when set, is dialled in place of the edge's resolved
	// address; the edge is still authenticated by its SPIFFE ID.
	ConnectIP string

	// openDock, when set, docks in place of dock.Open; tests use it.
	openDock func(context.Context, dock.Config) (shareDock, error)
}

// New validates deployment configuration without opening a connection. The
// edge to dock on comes with each share from the platform.
func New(getenv func(string) string) (*Client, error) {
	ip := getenv("PURLVIEW_CONNECT_IP")
	if ip != "" && net.ParseIP(ip) == nil {
		return nil, errors.New("invalid connect IP")
	}
	return &Client{ConnectIP: ip}, nil
}

type connection struct {
	events chan engine.ConnEvent
	cancel context.CancelFunc
	close  func() error
	once   sync.Once
}

func (c *connection) Events() <-chan engine.ConnEvent { return c.events }

// Close ends the dock and its active requests.
func (c *connection) Close() error { c.once.Do(func() { c.cancel(); _ = c.close() }); return nil }

// proxyFor builds the proxy of one docked share: its first target, its
// further targets under their mounts, and the public origin from Serving for
// header translation and body rewriting. Without a public origin nothing is
// translated or rewritten. The daemon's log says what was built.
func (c *Client) proxyFor(id string, first *url.URL, s engine.Serving) *proxy {
	targets, mismatch := newTargets(first, s.Targets, nil)
	public, ok := parseOrigin(s.Origin)
	rewrite := s.Rewrite && mismatch == "" && ok
	if c.Logger != nil {
		if mismatch != "" {
			c.Logger.Printf("share %s: %s; serving the first target alone", id, mismatch)
		}
		if !ok {
			c.Logger.Printf("share %s: no public origin in %q; headers and bodies are not translated", id, s.Origin)
		}
		c.Logger.Printf("share %s: serving %d target(s) as %s, rewriting %v", id, len(targets), public.String(), rewrite)
	}
	return newProxy(public, targets, rewrite, c.Logger)
}

type oneListener struct {
	c        net.Conn
	accepted bool
	done     chan struct{}
	once     sync.Once
}

func (l *oneListener) Accept() (net.Conn, error) {
	if l.accepted {
		<-l.done
		return nil, net.ErrClosed
	}
	l.accepted = true
	return &trackedConn{Conn: l.c, listener: l}, nil
}

// Close ends the stream and its active requests.
func (l *oneListener) Close() error   { l.once.Do(func() { close(l.done) }); return l.c.Close() }
func (l *oneListener) Addr() net.Addr { return l.c.LocalAddr() }

type trackedConn struct {
	net.Conn
	listener *oneListener
}

// Close ends the stream and its active requests.
func (c *trackedConn) Close() error { return c.listener.Close() }

// serveStream serves HTTP on one end-to-end stream until it ends or ctx is
// done.
func serveStream(ctx context.Context, c net.Conn, h http.Handler) {
	l := &oneListener{c: c, done: make(chan struct{})}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-l.done:
		}
	}()
	_ = server.Serve(l)
	_ = l.Close()
}
