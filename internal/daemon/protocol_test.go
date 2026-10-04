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

package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/ipc"
)

// oldDaemon stands in for a running daemon of private protocol 1: it holds the
// daemon lock, publishes an endpoint record and answers as that release does.
// hello from another protocol is a mismatch, shutdown is always served, and
// everything else is refused. It never names this test process as killable.
type oldDaemon struct {
	mu       sync.Mutex
	hello    ipc.HelloParams
	refused  []string
	shutdown string
	release  func()
}

func startOldDaemon(t *testing.T, e *testEnv) *oldDaemon {
	t.Helper()
	p := e.paths()
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := daemon.EnsureSocketDir(p.Endpoint); err != nil {
		t.Fatal(err)
	}
	lock, err := daemon.TryLock(p.DaemonLockPath())
	if err != nil {
		t.Fatal(err)
	}
	l, err := ipc.Listen(p.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := &daemon.Endpoint{Protocol: 1, Instance: "old", Process: daemon.ProcessIdentity{PID: os.Getpid(), StartTime: 1}, Endpoint: p.Endpoint,
		Executable: "/opt/old/purlview", Build: ipc.BuildIdentity{Version: "0.1.0-alpha.6", Commit: "a6055df"}, StartedAt: time.Now().UTC()}
	if err = daemon.WriteEndpoint(p, record); err != nil {
		t.Fatal(err)
	}
	d := &oldDaemon{}
	var once sync.Once
	d.release = func() {
		once.Do(func() {
			_ = l.Close()
			_ = daemon.RemoveEndpoint(p, "old")
			_ = lock.Unlock()
		})
	}
	t.Cleanup(d.release)
	go func() {
		for {
			raw, err := l.Accept()
			if err != nil {
				return
			}
			go d.serve(ipc.NewConn(raw))
		}
	}()
	return d
}

func (d *oldDaemon) serve(c *ipc.Conn) {
	defer func() { _ = c.Close() }()
	for {
		req, err := c.ReadRequest(time.Now().Add(10 * time.Second))
		if err != nil {
			return
		}
		resp := &ipc.Response{ID: req.ID, Error: &ipc.Error{Code: ipc.CodeProtocolMismatch, Message: "protocol version mismatch; only status and shutdown are available"}}
		d.mu.Lock()
		switch req.Op {
		case ipc.OpHello:
			_ = json.Unmarshal(req.Params, &d.hello)
			resp.Error.Message = "daemon speaks protocol 1, client protocol 2"
		case ipc.OpShutdown:
			var p ipc.ShutdownParams
			_ = json.Unmarshal(req.Params, &p)
			d.shutdown = p.Reason
			resp = &ipc.Response{ID: req.ID, OK: true, Result: ipc.Marshal(ipc.ShutdownResult{})}
		default:
			d.refused = append(d.refused, req.Op)
		}
		stop := d.shutdown != ""
		d.mu.Unlock()
		if c.WriteResponse(resp, time.Now().Add(5*time.Second)) != nil {
			return
		}
		if stop {
			d.release()
			return
		}
	}
}

// A CLI of private protocol 2 that meets a running protocol 1 daemon keeps the
// connection, is refused share operations, retires that daemon and starts its
// own (docs/daemon.md, "Versioning"): it never sends it a share to serve.
func TestNewerProtocolRetiresAnOlderDaemon(t *testing.T) {
	e := newEnv(t)
	old := startOldDaemon(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	probe, err := e.controller(binV1, "0.9.0").Connect(ctx)
	if err != nil {
		t.Fatalf("a protocol mismatch must leave a usable connection: %v", err)
	}
	if probe.Hello.Protocol == ipc.ProtocolVersion || probe.Hello.Build.Version != "0.1.0-alpha.6" || probe.Hello.Instance != "old" {
		t.Fatalf("hello %+v", probe.Hello)
	}
	if err = probe.Client.Call(ctx, ipc.OpShareList, nil, nil); !ipc.IsCode(err, ipc.CodeProtocolMismatch) {
		t.Fatalf("share.list on the old daemon: %v", err)
	}
	_ = probe.Close()

	// The replacement runs as the CLI does, in a child process that carries this
	// test's environment. Controller.start gives the daemon the process
	// environment, so an in-process Ensure here would start one on the real
	// per-user state.
	r := e.run(binV1, "daemon", "start")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon started") || !strings.Contains(r.stderr, "replacing the local daemon (it runs 0.1.0-alpha.6") {
		t.Fatalf("replacement: %+v", r)
	}
	old.mu.Lock()
	defer old.mu.Unlock()
	if old.hello.Protocol != ipc.ProtocolVersion || old.shutdown != "outdated build" {
		t.Fatalf("the old daemon saw hello %+v and shutdown %q", old.hello, old.shutdown)
	}
	for _, op := range old.refused {
		if strings.HasPrefix(op, "share.") && op != ipc.OpShareList {
			t.Fatalf("the old daemon was sent %s", op)
		}
	}
	if ep := e.endpoint(); ep == nil || ep.Instance == "old" || ep.Protocol != ipc.ProtocolVersion {
		t.Fatalf("endpoint record after replacement: %+v", ep)
	}
}
