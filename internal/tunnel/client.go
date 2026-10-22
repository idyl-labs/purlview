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

// How long a stream may take to send a request's header, and how long it
// may sit idle before its first request or between requests. A peer that
// holds a stream open for later requests must close it before
// streamIdleTimeout, so that it never sends a request just as the daemon
// closes the stream.
const (
	streamHeaderTimeout = 5 * time.Second
	streamIdleTimeout   = 120 * time.Second
)

// streamServer is the HTTP server for one end-to-end stream.
func streamServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: streamHeaderTimeout, IdleTimeout: streamIdleTimeout, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
}

// serveStream serves HTTP on one end-to-end stream until it ends, it is idle
// for streamIdleTimeout or ctx is done. c must honour read deadlines, as
// withDeadlines provides. Closing the stream during a request cancels the
// request's context.
func serveStream(ctx context.Context, c net.Conn, h http.Handler) {
	serveStreamOn(ctx, c, streamServer(h))
}

// serveStreamOn serves c with server, holding c to server's IdleTimeout
// before its first request as well as between requests.
func serveStreamOn(ctx context.Context, c net.Conn, server *http.Server) {
	rc := &replayConn{Conn: c}
	l := &oneListener{c: rc, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-l.done:
		}
	}()
	// The server would start the header timeout as soon as it is handed the
	// stream, which closes a stream opened ahead of its first request.
	// Waiting for that request's first byte holds the stream to the idle
	// timeout instead, as between requests.
	if rc.awaitFirstByte(server.IdleTimeout) {
		_ = server.Serve(l)
	}
	_ = l.Close()
}

// replayConn returns the bytes awaitFirstByte read ahead of the server before
// reading on. Only the reading goroutine touches pending; the server reads
// from one goroutine at a time.
type replayConn struct {
	net.Conn
	pending []byte
}

// awaitFirstByte waits at most d for the connection's first byte and keeps
// it for Read. It reports false when none came: the wait timed out, the peer
// closed the stream or the stream was closed locally. If a byte arrives, it
// is served; if the wait ends first, the stream is closed without reading a
// request. A zero d waits without a limit, as a zero IdleTimeout does in
// net/http when there is no ReadTimeout.
func (c *replayConn) awaitFirstByte(d time.Duration) bool {
	if d > 0 {
		_ = c.SetReadDeadline(time.Now().Add(d))
	}
	var b [1]byte
	var n int
	var err error
	for n == 0 && err == nil {
		n, err = c.Conn.Read(b[:])
	}
	_ = c.SetReadDeadline(time.Time{})
	if n == 0 {
		return false
	}
	c.pending = b[:n]
	return true
}

func (c *replayConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}
