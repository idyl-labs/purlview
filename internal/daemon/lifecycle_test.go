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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/account"
	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/ipc"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// A share from a script (no terminal) without a remembered credential
// bootstraps the daemon and then stops with login instructions; with a
// remembered credential it reaches the daemon's share operation, which this
// build answers honestly.
const (
	notSignedIn    = "✗ Not signed in — run purlview login first"
	notImplemented = "✗ Sharing is not implemented in this scaffold build"
)

// 1. First share bootstraps the daemon, later commands reuse it, and share
// still stops honestly with nothing on stdout.
func TestFirstShareBootstrapsAndReusesDaemon(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.api.set("v0.9.0")
	if r := e.run(binV1, "daemon", "status"); r.code != 1 || !strings.Contains(r.stderr, "Daemon is not running") {
		t.Fatalf("status before: %+v", r)
	}
	r := e.run(binV1, "share", "localhost:3000")
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, notSignedIn) {
		t.Fatalf("first share: %+v", r)
	}
	ep := e.endpoint()
	if !processAlive(ep.Process.PID, ep.Process.StartTime) {
		t.Fatal("daemon process is not alive after share")
	}
	st := e.status(binV1, "0.9.0")
	if !st.Running || st.Hello.Instance != ep.Instance || st.Hello.Build.Version != "0.9.0" || !st.Hello.Ready {
		t.Fatalf("status: %+v", st)
	}
	if st.Hello.Console != "none" {
		t.Fatalf("daemon must run without a console/terminal, got %q", st.Hello.Console)
	}
	// The share returned without waiting for metadata; the daemon's fetch
	// finishes on its own. A share that arrives while a fetch is in flight
	// joins it, so let this one land before the next share.
	waitFor(t, "first metadata request", 5*time.Second, func() bool { return e.api.requests.Load() == 1 })
	waitFor(t, "cached answer", 5*time.Second, func() bool { _, err := os.Stat(e.paths().UpdateCachePath()); return err == nil })
	r = e.run(binV1, "share", "localhost:3000", "--background")
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, notSignedIn) {
		t.Fatalf("background share: %+v", r)
	}
	if e.endpoint().Instance != ep.Instance {
		t.Fatal("second share must reuse the daemon")
	}
	r = e.run(binV1, "daemon", "start")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon reused") {
		t.Fatalf("daemon start: %+v", r)
	}
	// The second share reuses the first answer: an answered check is asked
	// again only a day later, so GitHub sees one request.
	time.Sleep(300 * time.Millisecond)
	if n := e.api.requests.Load(); n != 1 {
		t.Fatalf("%d metadata requests for two shares within a day, want 1", n)
	}
	r = e.run(binV1, "daemon", "stop")
	if r.code != 0 || !strings.Contains(r.stdout, "stopped") {
		t.Fatalf("stop: %+v", r)
	}
	waitFor(t, "daemon exit", 5*time.Second, func() bool { return !processAlive(ep.Process.PID, ep.Process.StartTime) })
	if r := e.run(binV1, "daemon", "stop"); r.code != 0 || !strings.Contains(r.stdout, "was not running") {
		t.Fatalf("second stop must succeed: %+v", r)
	}
	if _, err := os.Stat(e.paths().EndpointPath()); err == nil {
		t.Fatal("endpoint record must be removed after a clean stop")
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(e.paths().Endpoint); err == nil {
			t.Fatal("socket must be removed after a clean stop")
		}
	}
}

// 1a. With a remembered credential the share request travels the real IPC
// to the daemon, which has no platform client and answers not_implemented;
// the CLI reports that without claiming a share. The credential file is the
// supported private file custody of package account.
func TestShareWithCredentialReachesDaemonAndFailsHonestly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.api.set("v0.9.0")
	cred := &api.InstallationCredential{Identity: resource.Identity{Account: "creator@example.invalid", AccountID: "acc_1", Device: "dev_1", DeviceLabel: "ci"}, Token: "tok_test"}
	if err := account.ScopedFileStore(e.paths().CredentialsPath(), "", "", "").Save(cred); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"share", "localhost:3000"}, {"share", "localhost:3000", "--background", "--ttl", "15m", "--to", "reviewer@example.invalid"}} {
		r := e.run(binV1, args...)
		// One line: nothing is printed before the link works.
		if r.code != 1 || r.stdout != "" || r.stderr != notImplemented+"\n" {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	st := e.status(binV1, "0.9.0")
	if !st.Running || len(st.Status.Operations) != 0 {
		t.Fatalf("the daemon must be running and own nothing: %+v", st)
	}
	// The other account commands with a credential also stop at the missing
	// platform, without starting or needing a daemon.
	if r := e.run(binV1, "daemon", "stop"); r.code != 0 {
		t.Fatalf("stop: %+v", r)
	}
	for _, tc := range []struct{ args, want []string }{
		{[]string{"list"}, []string{"✗ Listing shares is not implemented in this scaffold build"}},
		{[]string{"stop", "k7m2p4qx"}, []string{"✗ Stopping shares is not implemented in this scaffold build"}},
		{[]string{"unshare", "k7m2p4qx"}, []string{"✗ Stopping shares is not implemented in this scaffold build"}},
		{[]string{"link", "k7m2p4qx"}, []string{"✗ Listing shares is not implemented in this scaffold build"}},
		{[]string{"devices"}, []string{"✗ Listing devices is not implemented in this scaffold build"}},
	} {
		r := e.run(binV1, tc.args...)
		if r.code != 1 {
			t.Fatalf("%v: %+v", tc.args, r)
		}
		for _, w := range tc.want {
			if !strings.Contains(r.stderr, w) {
				t.Fatalf("%v: stderr %q lacks %q", tc.args, r.stderr, w)
			}
		}
	}
	if daemon.IsHeld(e.paths().DaemonLockPath()) {
		t.Fatal("list, unshare and whoami must not start a daemon")
	}
}

// 1b. A daemon that never becomes ready is stopped and cleaned up; no
// unowned process and no stale record remain, and a retry works.
func TestStartTimeoutCleansUp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.set(daemon.EnvStartTimeout, "700ms")
	e.set("PURLVIEW_DAEMON_TEST_STARTUP_DELAY", "10s")
	r := e.run(binV1, "share", "localhost:3000")
	if r.code != 1 || !strings.Contains(r.stderr, "did not become ready within 700ms") || r.stdout != "" {
		t.Fatalf("timeout: %+v", r)
	}
	if r.took > 6*time.Second {
		t.Fatalf("took %s; the start must be bounded", r.took)
	}
	if daemon.IsHeld(e.paths().DaemonLockPath()) {
		t.Fatal("daemon lock still held: the child was not stopped")
	}
	if _, err := os.Stat(e.paths().EndpointPath()); err == nil {
		t.Fatal("stale endpoint record left behind")
	}
	e.set("PURLVIEW_DAEMON_TEST_STARTUP_DELAY", "")
	e.set(daemon.EnvStartTimeout, "15s")
	if r := e.run(binV1, "daemon", "start"); r.code != 0 || !strings.Contains(r.stdout, "daemon started") {
		t.Fatalf("retry after timeout: %+v", r)
	}
}

// A starter owns the singleton before its endpoint is readable. An initial
// failed probe must wait for that starter before deciding it is unresponsive.
func TestEnsureWaitsForConcurrentStartup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.paths()
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	startLock, err := daemon.TryLock(p.StartLockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = startLock.Unlock() }()
	daemonLock, err := daemon.TryLock(p.DaemonLockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemonLock.Unlock() }()
	if err := os.WriteFile(p.EndpointPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctrl := e.controller(binV1, "0.9.0")
	result := make(chan error, 1)
	go func() {
		sess, err := ctrl.Ensure(ctx)
		if sess != nil {
			_ = sess.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("Ensure returned before the concurrent starter finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := startLock.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, daemon.ErrUnresponsive) {
			t.Fatalf("invalid endpoint after the starter finished: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Ensure did not probe again after the starter finished")
	}
}

// 2. Concurrent starts produce exactly one daemon, and concurrent clients
// do not corrupt operation state.
func TestConcurrentStartsYieldOneDaemon(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const n = 8
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(binV1, "daemon", "start")
			cmd.Env = e.environ()
			out, err := cmd.CombinedOutput()
			results[i] = result{stdout: string(out)}
			if err != nil {
				results[i].code = 1
			}
		}(i)
	}
	wg.Wait()
	instances := map[string]bool{}
	started := 0
	for i, r := range results {
		if r.code != 0 {
			t.Fatalf("start %d failed: %s", i, r.stdout)
		}
		f := strings.Fields(r.stdout)
		for j := range f {
			if f[j] == "instance" && j+1 < len(f) {
				instances[strings.TrimSuffix(f[j+1], ",")] = true
			}
		}
		if strings.Contains(r.stdout, "daemon started") {
			started++
		}
	}
	if len(instances) != 1 || started != 1 {
		t.Fatalf("instances=%v started=%d, want one daemon started once:\n%v", instances, started, results)
	}

	sess := e.connect(binV1, "0.9.0")
	const clients = 12
	var cw sync.WaitGroup
	for i := 0; i < clients; i++ {
		cw.Add(1)
		go func(i int) {
			defer cw.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, err := e.controller(binV1, "0.9.0").Connect(ctx)
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			defer func() { _ = s.Close() }()
			op := startOp(t, s.Client, "c", true, 0)
			if r := stopOp(t, s.Client, op.ID, false); r.AlreadyEnded {
				t.Errorf("client %d: fresh stop reported already ended", i)
			}
			if r := stopOp(t, s.Client, op.ID, false); !r.AlreadyEnded {
				t.Errorf("client %d: repeated stop must report already ended", i)
			}
		}(i)
	}
	cw.Wait()
	if a := activeOps(t, sess.Client); len(a) != 0 {
		t.Fatalf("active operations after concurrent clients: %+v", a)
	}
	if got := len(ops(t, sess.Client)); got != clients {
		t.Fatalf("history has %d operations, want %d", got, clients)
	}
}

// 3. A stale endpoint record is recovered without touching an unrelated
// process, even one that is a Purlview daemon whose PID the record names.
func TestStaleEndpointRecoveryNeverKillsUnrelatedProcess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	decoy := newEnv(t) // an unrelated daemon of the same name
	if r := decoy.run(binV1, "daemon", "start"); r.code != 0 {
		t.Fatalf("decoy: %+v", r)
	}
	decoyEP := decoy.endpoint()

	if r := e.run(binV1, "daemon", "start"); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	ep := e.endpoint()
	if err := ep.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "daemon death", 5*time.Second, func() bool { return !daemon.IsHeld(e.paths().DaemonLockPath()) })
	// Rewrite the record to name the decoy's PID with a start time that
	// does not match, as after PID reuse.
	stale := *ep
	stale.Process = daemon.ProcessIdentity{PID: decoyEP.Process.PID, StartTime: decoyEP.Process.StartTime + 1}
	if err := daemon.WriteEndpoint(e.paths(), &stale); err != nil {
		t.Fatal(err)
	}
	// status: not running, no complaint; stop --force: nothing to kill.
	if st := e.status(binV1, "0.9.0"); st.Running || st.Error != "" {
		t.Fatalf("stale record must read as not running: %+v", st)
	}
	if r := e.run(binV1, "daemon", "stop", "--force"); r.code != 0 || !strings.Contains(r.stdout, "was not running") {
		t.Fatalf("stop --force on stale record: %+v", r)
	}
	if !processAlive(decoyEP.Process.PID, decoyEP.Process.StartTime) {
		t.Fatal("the unrelated daemon was killed")
	}
	// A start recovers by removing the stale record and socket.
	if err := daemon.WriteEndpoint(e.paths(), &stale); err != nil {
		t.Fatal(err)
	}
	r := e.run(binV1, "daemon", "start")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon started") || !strings.Contains(r.stderr, "removing stale endpoint record") {
		t.Fatalf("recovery start: %+v", r)
	}
	if e.endpoint().Instance == ep.Instance {
		t.Fatal("a fresh instance was expected")
	}
	if !processAlive(decoyEP.Process.PID, decoyEP.Process.StartTime) {
		t.Fatal("the unrelated daemon was killed during recovery")
	}
	if !decoy.status(binV1, "0.9.0").Running {
		t.Fatal("the unrelated daemon stopped serving")
	}
}

// 4. The endpoint is private to the user (Unix: modes; the Windows
// descriptor is checked in the ipc package).
func TestEndpointIsPrivateToUser(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("pipe security is covered by ipc.TestPipeSecurityDescriptorIsOwnerOnly")
	}
	e := newEnv(t)
	if r := e.run(binV1, "daemon", "start"); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	p := e.paths()
	for _, dir := range []string{p.Runtime, filepath.Dir(p.Endpoint), p.Logs, p.Cache} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is accessible to others: %o", dir, st.Mode().Perm())
		}
	}
	st, err := os.Lstat(p.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o, want 0600", st.Mode().Perm())
	}
	if len(p.Endpoint) > 107 {
		t.Fatalf("socket path too long for the kernel: %d bytes", len(p.Endpoint))
	}
	// A socket that became world-accessible is refused by the client.
	if err := os.Chmod(p.Endpoint, 0o666); err != nil {
		t.Fatal(err)
	}
	if r := e.run(binV1, "daemon", "status"); r.code == 0 || !strings.Contains(r.stderr, "accessible to other users") {
		t.Fatalf("status must refuse a shared socket: %+v", r)
	}
	_ = os.Chmod(p.Endpoint, 0o600)
	if st := e.status(binV1, "0.9.0"); !st.Running {
		t.Fatalf("status after restoring mode: %+v", st)
	}
	for _, f := range []string{p.EndpointPath(), p.LogPath(), p.UpdateCachePath()} {
		st, err := os.Stat(f)
		if err != nil {
			if f == p.UpdateCachePath() {
				continue // written only after the first check
			}
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is accessible to others: %o", f, st.Mode().Perm())
		}
	}
}

// 5. Attached work ends with its connection or process; detached work and
// unrelated work survive; stop, expiry and revocation are idempotent.
func TestAttachedAndDetachedOwnership(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if r := e.run(binV1, "daemon", "start"); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	a := e.connect(binV1, "0.9.0")
	b := e.connect(binV1, "0.9.0")
	subscribe(t, b.Client)
	attachedA := startOp(t, a.Client, "attached-a", false, 0)
	detachedA := startOp(t, a.Client, "detached-a", true, 0)
	attachedB := startOp(t, b.Client, "attached-b", false, 0)
	if attachedA.Owner != daemon.OwnerClient || detachedA.Owner != daemon.OwnerDaemon {
		t.Fatalf("owners: %+v %+v", attachedA, detachedA)
	}

	_ = a.Close() // the controlling CLI of attached-a goes away
	ev := waitEvent(t, b.Client, 5*time.Second, opEnded(attachedA.ID))
	var ended ipc.OpEndedData
	_ = jsonUnmarshal(ev.Data, &ended)
	if ended.Operation.Reason != daemon.ReasonDisconnected {
		t.Fatalf("attached-a ended with %q, want client_disconnected", ended.Operation.Reason)
	}
	active := activeOps(t, b.Client)
	if findOp(active, detachedA.ID) == nil || findOp(active, attachedB.ID) == nil || len(active) != 2 {
		t.Fatalf("detached and unrelated work must survive: %+v", active)
	}

	// Explicit stop is idempotent; the first reason sticks.
	if r := stopOp(t, b.Client, detachedA.ID, false); r.AlreadyEnded || r.Operation.Reason != daemon.ReasonStopped {
		t.Fatalf("stop: %+v", r)
	}
	if r := stopOp(t, b.Client, detachedA.ID, true); !r.AlreadyEnded || r.Operation.Reason != daemon.ReasonStopped {
		t.Fatalf("revoke after stop must be a no-op: %+v", r)
	}
	// Injected remote revocation ends attached-b for its own client.
	if r := stopOp(t, b.Client, attachedB.ID, true); r.AlreadyEnded || r.Operation.Reason != daemon.ReasonRevoked {
		t.Fatalf("revoke: %+v", r)
	}
	waitEvent(t, b.Client, 5*time.Second, opEnded(attachedB.ID))
	if r := stopOp(t, b.Client, attachedB.ID, false); !r.AlreadyEnded || r.Operation.Reason != daemon.ReasonRevoked {
		t.Fatalf("stop after revoke: %+v", r)
	}
	// Expiry fires on its own and cannot be extended or undone.
	expiring := startOp(t, b.Client, "expiring", true, 150*time.Millisecond)
	ev = waitEvent(t, b.Client, 5*time.Second, opEnded(expiring.ID))
	_ = jsonUnmarshal(ev.Data, &ended)
	if ended.Operation.Reason != daemon.ReasonExpired {
		t.Fatalf("expiry reason %q", ended.Operation.Reason)
	}
	if r := stopOp(t, b.Client, expiring.ID, false); !r.AlreadyEnded {
		t.Fatal("stop after expiry must report already ended")
	}
	if a := activeOps(t, b.Client); len(a) != 0 {
		t.Fatalf("active: %+v", a)
	}

	// The same through the process boundary: an attached test-op ends when
	// its CLI process is killed; a detached one outlives its CLI.
	r := e.run(binV1, "daemon", "test-op", "--detached")
	if r.code != 0 {
		t.Fatalf("detached test-op: %+v", r)
	}
	detachedID := strings.Fields(r.stdout)[0]
	cmd := exec.Command(binV1, "daemon", "test-op", "--name", "held")
	cmd.Env = e.environ()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := stdout.Read(buf)
	heldID := strings.Fields(string(buf[:n]))[0]
	if findOp(activeOps(t, b.Client), heldID) == nil {
		t.Fatal("held operation not active")
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	ev = waitEvent(t, b.Client, 5*time.Second, opEnded(heldID))
	_ = jsonUnmarshal(ev.Data, &ended)
	if ended.Operation.Reason != daemon.ReasonDisconnected {
		t.Fatalf("killed CLI: reason %q", ended.Operation.Reason)
	}
	if findOp(activeOps(t, b.Client), detachedID) == nil {
		t.Fatal("detached operation must survive its CLI")
	}
}

// 6. Closing the launching terminal does not end detached work: the daemon
// has no controlling terminal or console.
func TestDetachedWorkSurvivesTerminalClose(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	launcher := startInTerminal(t, e, binV1)
	waitFor(t, "daemon started from the terminal", 20*time.Second, func() bool { return e.status(binV1, "0.9.0").Running })
	sess := e.connect(binV1, "0.9.0")
	waitFor(t, "detached operation", 10*time.Second, func() bool { return len(activeOps(t, sess.Client)) == 1 })
	ep := e.endpoint()
	if h := sess.Hello; h.Console != "none" {
		t.Fatalf("daemon reports console %q; it must be detached", h.Console)
	}
	closeTerminal(t, launcher)
	time.Sleep(500 * time.Millisecond)
	if !processAlive(ep.Process.PID, ep.Process.StartTime) {
		t.Fatal("daemon died with its launching terminal")
	}
	st := e.status(binV1, "0.9.0")
	if !st.Running || st.Hello.Instance != ep.Instance {
		t.Fatalf("daemon not serving after terminal close: %+v", st)
	}
	if a := activeOps(t, sess.Client); len(a) != 1 {
		t.Fatalf("detached work lost: %+v", a)
	}
}

// 7. A crashed daemon is not reported as ready; the next start begins with
// no operations.
func TestCrashRestartResurrectsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if r := e.run(binV1, "daemon", "test-op", "--detached"); r.code != 0 {
		t.Fatalf("start with work: %+v", r)
	}
	ep := e.endpoint()
	if err := ep.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "crash", 5*time.Second, func() bool { return !daemon.IsHeld(e.paths().DaemonLockPath()) })
	if _, err := os.Stat(e.paths().EndpointPath()); err != nil {
		t.Fatal("a crashed daemon leaves its record; that is the case under test")
	}
	r := e.run(binV1, "daemon", "status")
	if r.code != 1 || !strings.Contains(r.stderr, "Daemon is not running") || strings.Contains(r.stderr, "not responding") {
		t.Fatalf("crashed daemon must read as not running: %+v", r)
	}
	r = e.run(binV1, "daemon", "start")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon started") {
		t.Fatalf("restart: %+v", r)
	}
	if e.endpoint().Instance == ep.Instance {
		t.Fatal("new instance expected")
	}
	sess := e.connect(binV1, "0.9.0")
	if list := ops(t, sess.Client); len(list) != 0 {
		t.Fatalf("restart must not resurrect operations: %+v", list)
	}
}

// 8. The next use after an executable replacement retires the old daemon
// and starts the installed version; replacing the file in place with the
// same version is detected too.
func TestNextUseRetiresOutdatedDaemon(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	install := filepath.Join(t.TempDir(), "bin", exeName("purlview"))
	copyFile(t, binV1, install)
	if r := e.run(install, "daemon", "test-op", "--detached"); r.code != 0 {
		t.Fatalf("v1 with work: %+v", r)
	}
	old := e.endpoint()

	copyFile(t, binV2, install) // an external upgrade replaced the file
	r := e.run(install, "daemon", "start")
	if r.code != 0 || !strings.Contains(r.stdout, "daemon started (retired 0.9.0") || !strings.Contains(r.stdout, "version 0.9.1") {
		t.Fatalf("next use after upgrade: %+v", r)
	}
	waitFor(t, "old daemon exit", 5*time.Second, func() bool { return !processAlive(old.Process.PID, old.Process.StartTime) })
	sess := e.connect(install, "0.9.1")
	if list := ops(t, sess.Client); len(list) != 0 {
		t.Fatalf("work from the old daemon must be gone: %+v", list)
	}
	_ = sess.Close()

	// share does the same on its own path.
	copyFile(t, binV1, install)
	r = e.run(install, "share", "localhost:3000")
	if r.code != 1 || !strings.Contains(r.stderr, "! Purlview was updated — shares started before the update have stopped") || !strings.Contains(r.stderr, notSignedIn) {
		t.Fatalf("share after downgrade: %+v", r)
	}
	if e.endpoint().Build.Version != "0.9.0" {
		t.Fatal("share did not start the installed version")
	}

	// Same version, rebuilt file (development flow): size or mtime changed.
	cur := e.endpoint()
	copyFile(t, binV1, install)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(install, future, future); err != nil {
		t.Fatal(err)
	}
	r = e.run(install, "daemon", "start")
	if r.code != 0 || !strings.Contains(r.stdout, "retired") || e.endpoint().Instance == cur.Instance {
		t.Fatalf("rebuilt executable must retire the daemon: %+v", r)
	}
	// The daemon runs an executable inside this test's temporary directory;
	// stop it before the directory is removed.
	if r := e.run(install, "daemon", "stop"); r.code != 0 {
		t.Fatalf("final stop: %+v", r)
	}
}

// 9. Maintenance blocks starts (also racing ones) until resumed or expired;
// stop --maintenance ends work.
func TestMaintenanceBlocksStartsUntilResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if r := e.run(binV1, "daemon", "test-op", "--detached"); r.code != 0 {
		t.Fatalf("start with work: %+v", r)
	}
	ep := e.endpoint()
	r := e.run(binV1, "daemon", "stop", "--maintenance", "--timeout", "10s")
	if r.code != 0 || !strings.Contains(r.stdout, "1 operation(s) ended") || !strings.Contains(r.stdout, "starts are blocked") {
		t.Fatalf("stop --maintenance: %+v", r)
	}
	waitFor(t, "daemon exit", 5*time.Second, func() bool { return !processAlive(ep.Process.PID, ep.Process.StartTime) })
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(binV1, "share", "localhost:3000")
			cmd.Env = e.environ()
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "✗ Purlview is being updated — try again in a moment") {
				t.Errorf("share during maintenance must be refused: %v %s", err, out)
			}
		}()
	}
	wg.Wait()
	if r := e.run(binV1, "daemon", "start"); r.code == 0 || !strings.Contains(r.stderr, "being upgraded") {
		t.Fatalf("start during maintenance: %+v", r)
	}
	if daemon.IsHeld(e.paths().DaemonLockPath()) {
		t.Fatal("a daemon started during maintenance")
	}
	if r := e.run(binV1, "daemon", "status"); r.code != 1 || !strings.Contains(r.stdout, "maintenance: upgrade") {
		t.Fatalf("status must show maintenance: %+v", r)
	}
	if r := e.run(binV1, "daemon", "resume"); r.code != 0 {
		t.Fatalf("resume: %+v", r)
	}
	if r := e.run(binV1, "daemon", "start"); r.code != 0 || !strings.Contains(r.stdout, "daemon started") {
		t.Fatalf("start after resume: %+v", r)
	}
	// Stop with maintenance while a start is racing: the start either
	// finishes first and its daemon is stopped, or it is refused.
	r = e.run(binV1, "daemon", "stop", "--maintenance")
	if r.code != 0 {
		t.Fatalf("second maintenance stop: %+v", r)
	}
	done := make(chan result, 1)
	go func() { done <- e.run(binV1, "daemon", "start") }()
	res := <-done
	if res.code == 0 {
		t.Fatalf("start must be refused during maintenance: %+v", res)
	}
	// An expired marker no longer blocks.
	if err := daemon.WriteMaintenance(e.paths(), -time.Second, "expired"); err != nil {
		t.Fatal(err)
	}
	if r := e.run(binV1, "daemon", "start"); r.code != 0 {
		t.Fatalf("start after expiry: %+v", r)
	}
}

// 10. Update checks: newer, equal, older, channel handling, repeated
// invocations, failures, no downloads. A share starts the daemon's check
// and never waits for it; what the daemon then knows is read back through
// daemon status. The notice itself is a share-block matter, pinned by the
// scenarios and goldens: a share that fails prints its one line only.
func TestShareUpdateNotices(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.api.set("v0.9.1", "v0.9.0", "v0.9.1", "v0.9.2-rc.1")
	r := e.run(binV1, "share", "localhost:3000")
	if r.code != 1 || r.stdout != "" || r.stderr != notSignedIn+"\n" {
		t.Fatalf("share: %+v", r)
	}
	waitFor(t, "first metadata request", 5*time.Second, func() bool { return e.api.requests.Load() >= 1 })
	waitFor(t, "cached answer", 5*time.Second, func() bool { return e.cached("stable") == "0.9.1" })
	r = e.run(binV1, "share", "localhost:3000")
	if r.code != 1 || r.stdout != "" || strings.Contains(r.stderr, "newer") {
		t.Fatalf("second share: %+v", r)
	}
	if r := e.run(binV1, "daemon", "status"); r.code != 0 || !strings.Contains(r.stdout, "update:     0.9.1 available (") {
		t.Fatalf("the daemon must hold the newer version: %+v", r)
	}
	// Repeated invocation asks again (conditional request, cached).
	r = e.run(binV1, "share", "localhost:3000")
	if r.code != 1 || strings.Contains(r.stderr, "newer") {
		t.Fatalf("third share: %q", r.stderr)
	}
	if r := e.run(binV1, "daemon", "status"); !strings.Contains(r.stdout, "0.9.1 available") {
		t.Fatalf("third status: %q", r.stdout)
	}
	// Equal and older: silent.
	e.api.set("v0.9.0")
	waitQuiet(t, e, binV1)
	e.api.set("v0.8.0")
	waitQuiet(t, e, binV1)
	// Prerelease build follows the prerelease channel; stable does not.
	e.api.set("v0.9.0", "v0.9.0", "v0.9.2-rc.1")
	waitQuiet(t, e, binV1)
	before := e.api.requests.Load()
	r = e.run(binRC, "share", "localhost:3000") // retires the 0.9.0 daemon, starts 0.9.1-rc.1
	if r.code != 1 || r.stdout != "" {
		t.Fatalf("prerelease share: %+v", r)
	}
	waitFor(t, "prerelease metadata request", 5*time.Second, func() bool { return e.api.requests.Load() > before })
	// The stable entry's ETag names every tag, so only the parsed
	// prerelease entry says the rc daemon's own fetch has landed.
	waitFor(t, "prerelease cached answer", 5*time.Second, func() bool { return e.cached("prerelease") == "0.9.2-rc.1" })
	if r := e.run(binRC, "daemon", "status"); !strings.Contains(r.stdout, "update:     0.9.2-rc.1 available (") {
		t.Fatalf("prerelease channel: %q", r.stdout)
	}
	if r := e.run(binV1, "daemon", "status"); strings.Contains(r.stdout, "update:") {
		t.Fatalf("the stable channel must not follow a prerelease: %q", r.stdout)
	}
	// Failures stay silent and never slow sharing down: the command does
	// not wait for metadata at all.
	for _, tc := range []struct {
		name   string
		status int
		delay  time.Duration
	}{{"rate limited", 429, 0}, {"malformed", 200, 0}, {"server error", 500, 0}, {"slow", 0, 6 * time.Second}} {
		e.api.fail(tc.status, tc.delay)
		clearBackoff(t, e)
		before := e.api.requests.Load()
		for i := 0; i < 2; i++ {
			r = e.run(binRC, "share", "localhost:3000")
			if r.code != 1 || strings.Contains(r.stderr, "newer version") || r.stdout != "" || !strings.Contains(r.stderr, notSignedIn) {
				t.Fatalf("%s: %+v", tc.name, r)
			}
			if r.took > 3*time.Second {
				t.Fatalf("%s: share took %s; the command must not wait for metadata", tc.name, r.took)
			}
			waitFor(t, tc.name+" request", 5*time.Second, func() bool { return e.api.requests.Load() > before })
		}
	}
	e.api.fail(0, 0)
	// Offline: the API is unreachable; nothing changes for the user.
	e2 := newEnv(t)
	e2.set("PURLVIEW_UPDATE_API_URL", "http://127.0.0.1:1")
	r = e2.run(binV1, "share", "localhost:3000")
	if r.code != 1 || strings.Contains(r.stderr, "newer") || !strings.Contains(r.stderr, notSignedIn) || r.took > 3*time.Second {
		t.Fatalf("offline: %+v", r)
	}
	// Only metadata endpoints were ever requested; no asset, no download.
	for _, p := range e.api.seen() {
		if !strings.HasPrefix(p, "/repos/acme/purlview-releases/releases") || strings.Contains(p, "download") {
			t.Fatalf("unexpected request %s", p)
		}
	}
	// The cache is saved through a temporary file renamed into place; a check
	// still landing (the slow case above) may hold one for a moment. Only a
	// file that stays is unexpected.
	waitFor(t, "a cache holding only update-check.json", 5*time.Second, func() bool {
		entries, _ := os.ReadDir(e.paths().Cache)
		for _, en := range entries {
			if en.Name() != "update-check.json" {
				return false
			}
		}
		return true
	})
	// Help, version and completion make no request even with a daemon up.
	before = e.api.requests.Load()
	for _, args := range [][]string{{"--help"}, {"--version"}, {"completion", "bash"}, {"share", "--help"}} {
		if r := e.run(binV1, args...); r.code != 0 || r.stderr != "" {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if e.api.requests.Load() != before {
		t.Fatal("help/version/completion must not check for updates")
	}
}

// waitQuiet proves a metadata state produces no notice: a share starts the
// check, and once the daemon holds the answer a second share stays silent.
func waitQuiet(t *testing.T, e *testEnv, bin string) {
	t.Helper()
	clearBackoff(t, e)
	before := e.api.requests.Load()
	r := e.run(bin, "share", "localhost:3000")
	if r.code != 1 || strings.Contains(r.stderr, "newer") {
		t.Fatalf("expected no notice: %+v", r)
	}
	waitFor(t, "metadata request", 5*time.Second, func() bool { return e.api.requests.Load() > before })
	waitFor(t, "cached answer", 5*time.Second, func() bool { _, err := os.Stat(e.paths().UpdateCachePath()); return err == nil })
	if r := e.run(bin, "daemon", "status"); r.code != 0 || strings.Contains(r.stdout, "update:") {
		t.Fatalf("expected no newer version from the cached answer: %+v", r)
	}
}

// clearBackoff restarts the daemon so it drops the in-memory back-off and
// starts without a cache; the metadata server state changed. The daemon is
// stopped before the cache file is removed: a fetch still in flight would
// otherwise rewrite the file after the removal and hand the next daemon a
// stale answer.
func clearBackoff(t *testing.T, e *testEnv) {
	t.Helper()
	if r := e.run(binV1, "daemon", "stop"); r.code != 0 {
		t.Fatalf("stop: %+v", r)
	}
	_ = os.Remove(e.paths().UpdateCachePath())
}

// 11. Help, version, completion and usage errors create no state, start no
// process and use no network.
func TestHelpVersionCompletionHaveNoSideEffects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, args := range [][]string{nil, {"--help"}, {"--version"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"completion", "powershell"}, {"share", "--help"}, {"help", "share"}, {"daemon", "--help"}, {"bogus"}, {"share"}, {"share", "--bogus", "x"}, {"list"}, {"login"}} {
		r := e.run(binV1, args...)
		if len(args) > 0 && (args[0] == "bogus" || args[0] == "list" || args[0] == "login" || len(args) == 1 && args[0] == "share" || len(args) == 3) {
			if r.code == 0 {
				t.Fatalf("%v must fail", args)
			}
		} else if r.code != 0 {
			t.Fatalf("%v: %+v", args, r)
		}
		if _, err := os.Stat(e.stateDir); err == nil {
			t.Fatalf("%v created state at %s", args, e.stateDir)
		}
	}
	if e.api.requests.Load() != 0 {
		t.Fatal("offline commands made a network request")
	}
	if daemon.IsHeld(e.paths().DaemonLockPath()) {
		t.Fatal("a daemon is running")
	}
	if r := e.run(binV1, "daemon", "status"); r.code != 1 {
		t.Fatalf("status: %+v", r)
	}
	if _, err := os.Stat(e.stateDir); err == nil {
		t.Fatal("status must not create state either")
	}
}

// 12. An unresponsive daemon is reported, not killed by name; --force kills
// exactly that process after checking its identity.
func TestUnresponsiveDaemonIsReportedAndForceStopped(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("needs a way to freeze a process; documented as a manual check on Windows")
	}
	e := newEnv(t)
	if r := e.run(binV1, "daemon", "start"); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	ep := e.endpoint()
	proc, err := os.FindProcess(ep.Process.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(sigstop()); err != nil {
		t.Fatal(err)
	}
	r := e.run(binV1, "daemon", "status")
	if r.code != 1 || !strings.Contains(r.stderr, "not responding") {
		t.Fatalf("frozen daemon must be reported as not responding: %+v", r)
	}
	r = e.run(binV1, "daemon", "stop", "--timeout", "1s")
	if r.code == 0 || !strings.Contains(r.stderr, "not responding") {
		t.Fatalf("stop without --force must refuse: %+v", r)
	}
	if !processAlive(ep.Process.PID, ep.Process.StartTime) {
		t.Fatal("stop without --force must not kill")
	}
	r = e.run(binV1, "daemon", "stop", "--force", "--timeout", "1s")
	if r.code != 0 || !strings.Contains(r.stdout, "was killed") {
		t.Fatalf("stop --force: %+v", r)
	}
	if processAlive(ep.Process.PID, ep.Process.StartTime) {
		t.Fatal("daemon survived --force")
	}
	if r := e.run(binV1, "daemon", "start"); r.code != 0 || !strings.Contains(r.stdout, "daemon started") {
		t.Fatalf("start after force: %+v", r)
	}
}

// 13. Idle exit: the daemon leaves when nothing keeps it, stays while it
// owns work.
func TestIdleExitRespectsOwnedWork(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.set(daemon.EnvIdleExit, "400ms")
	if r := e.run(binV1, "daemon", "test-op", "--detached"); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	ep := e.endpoint()
	time.Sleep(1200 * time.Millisecond)
	if !processAlive(ep.Process.PID, ep.Process.StartTime) {
		t.Fatal("daemon exited while it owned work")
	}
	sess := e.connect(binV1, "0.9.0")
	for _, op := range activeOps(t, sess.Client) {
		stopOp(t, sess.Client, op.ID, false)
	}
	_ = sess.Close()
	waitFor(t, "idle exit", 5*time.Second, func() bool { return !processAlive(ep.Process.PID, ep.Process.StartTime) })
	if _, err := os.Stat(e.paths().EndpointPath()); err == nil {
		t.Fatal("endpoint record left after idle exit")
	}
	if r := e.run(binV1, "daemon", "status"); r.code != 1 {
		t.Fatalf("status after idle exit: %+v", r)
	}
}

// copyFile replaces dst with src's content (a new file, then rename).
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// A running executable cannot be replaced in place, but it can be
		// renamed aside (what the Windows installer does).
		_ = os.Remove(dst + ".old")
		if _, err := os.Stat(dst); err == nil {
			if err := os.Rename(dst, dst+".old"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}
