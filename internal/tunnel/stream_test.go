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
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A stream gives a request's header 5s and an idle stream 120s. The tests
// below scale both down, so this one holds the values that are served.
func TestStreamTimeouts(t *testing.T) {
	s := streamServer(nil)
	if s.ReadHeaderTimeout != 5*time.Second || s.IdleTimeout != 120*time.Second {
		t.Fatalf("header timeout %v, idle timeout %v", s.ReadHeaderTimeout, s.IdleTimeout)
	}
}

// servedStream serves h on a lane-like stream with the production server,
// its header and idle timeouts scaled down to the given ones. It returns the
// gateway's end of the stream, a function that cancels serving, and a
// channel closed once serving has ended.
func servedStream(t *testing.T, h http.Handler, header, idle time.Duration) (net.Conn, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	server := streamServer(h)
	server.ReadHeaderTimeout, server.IdleTimeout = header, idle
	gateway, stream := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		serveStreamOn(ctx, withDeadlines(noDeadlines{stream}), server)
	}()
	t.Cleanup(func() {
		cancel()
		_ = gateway.Close()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Error("serving did not end")
		}
	})
	return gateway, cancel, ended
}

// echoPath answers every request with its path.
var echoPath = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, r.URL.Path)
})

// getOnStream sends a GET for path on the stream and returns the response
// body.
func getOnStream(t *testing.T, c net.Conn, r *bufio.Reader, path string) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: share\r\n\r\n"); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %v", path, resp.StatusCode, err)
	}
	return string(body)
}

// closedWithin reports whether the server closes its end of the stream
// within d, which the gateway sees as EOF. Anything the server sends first,
// such as an error response, is skipped.
func closedWithin(c net.Conn, r io.Reader, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, r)
	return err == nil
}

// A stream opened ahead of its first request and left idle for longer than
// the header timeout still serves that request.
func TestAStreamIdleBeforeItsFirstRequestIsServed(t *testing.T) {
	const header = time.Second
	gateway, _, _ := servedStream(t, echoPath, header, time.Minute)
	time.Sleep(2 * header)
	if got := getOnStream(t, gateway, bufio.NewReader(gateway), "/first"); got != "/first" {
		t.Fatalf("got %q", got)
	}
}

// A stream idle for longer than the idle timeout is closed, whether or not it
// has served a request; one closed by either end while waiting ends at once.
func TestAnIdleStreamIsClosed(t *testing.T) {
	t.Run("before its first request", func(t *testing.T) {
		gateway, _, ended := servedStream(t, echoPath, time.Minute, 100*time.Millisecond)
		if !closedWithin(gateway, gateway, 5*time.Second) {
			t.Fatal("the idle stream stayed open")
		}
		<-ended
	})
	t.Run("between requests", func(t *testing.T) {
		gateway, _, ended := servedStream(t, echoPath, time.Minute, time.Second)
		r := bufio.NewReader(gateway)
		getOnStream(t, gateway, r, "/once")
		if !closedWithin(gateway, r, 5*time.Second) {
			t.Fatal("the idle stream stayed open")
		}
		<-ended
	})
	t.Run("closed by the gateway", func(t *testing.T) {
		gateway, _, ended := servedStream(t, echoPath, time.Minute, time.Minute)
		_ = gateway.Close()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("serving outlived the stream")
		}
	})
	t.Run("closed by the daemon", func(t *testing.T) {
		gateway, cancel, ended := servedStream(t, echoPath, time.Minute, time.Minute)
		cancel()
		if !closedWithin(gateway, gateway, 5*time.Second) {
			t.Fatal("the stream stayed open")
		}
		<-ended
	})
}

// With no idle timeout, as in net/http, a stream waits for its first
// request without a limit.
func TestAStreamWithoutAnIdleTimeoutWaits(t *testing.T) {
	gateway, _, _ := servedStream(t, echoPath, time.Minute, 0)
	time.Sleep(200 * time.Millisecond)
	if got := getOnStream(t, gateway, bufio.NewReader(gateway), "/later"); got != "/later" {
		t.Fatalf("got %q", got)
	}
}

// A request whose header stops arriving after its first byte is held to the
// header timeout, not the idle timeout.
func TestASlowHeaderEndsTheStream(t *testing.T) {
	gateway, _, ended := servedStream(t, echoPath, 200*time.Millisecond, time.Minute)
	_ = gateway.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(gateway, "G"); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(gateway, gateway, 5*time.Second) {
		t.Fatal("the stream waited for the rest of the header")
	}
	<-ended
}

// One stream serves requests one after another.
func TestSequentialRequestsOnAStreamAreServed(t *testing.T) {
	gateway, _, _ := servedStream(t, echoPath, time.Minute, time.Minute)
	r := bufio.NewReader(gateway)
	for _, path := range []string{"/one", "/two"} {
		if got := getOnStream(t, gateway, r, path); got != path {
			t.Fatalf("got %q for %s", got, path)
		}
	}
}

// When the gateway closes the stream during a request, the request's context
// is cancelled, so the handler stops working for a visitor who has gone.
func TestClosingAStreamCancelsItsRequest(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	gateway, _, _ := servedStream(t, h, time.Minute, time.Minute)
	go func() { _, _ = io.WriteString(gateway, "GET /slow HTTP/1.1\r\nHost: share\r\n\r\n") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request was not served")
	}
	_ = gateway.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the request's context outlived the stream")
	}
}
