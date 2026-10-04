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
	"strings"
	"testing"
	"time"
)

// noDeadlines is a connection whose deadlines do nothing, as a lane
// stream's are.
type noDeadlines struct{ net.Conn }

func (noDeadlines) SetDeadline(time.Time) error      { return nil }
func (noDeadlines) SetReadDeadline(time.Time) error  { return nil }
func (noDeadlines) SetWriteDeadline(time.Time) error { return nil }

// A handler that hijacks the connection, as the proxy does for a WebSocket
// upgrade, gets it and can talk both ways over a lane-like stream.
func TestHijackOverAStreamWithoutDeadlines(t *testing.T) {
	visitor, stream := net.Pipe()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = io.WriteString(c, "echo "+line)
		_ = c.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveStream(ctx, withDeadlines(noDeadlines{stream}), h)

	done := make(chan string, 1)
	go func() {
		_, _ = io.WriteString(visitor, "GET /ws HTTP/1.1\r\nHost: share\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		r := bufio.NewReader(visitor)
		resp, err := http.ReadResponse(r, nil)
		if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
			done <- "no upgrade"
			return
		}
		_, _ = io.WriteString(visitor, "hello\n")
		line, _ := r.ReadString('\n')
		done <- line
	}()
	select {
	case got := <-done:
		if strings.TrimSpace(got) != "echo hello" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade hung")
	}
}
