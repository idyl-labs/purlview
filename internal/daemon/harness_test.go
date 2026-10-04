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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/updatecheck"
)

// The lifecycle tests drive the real executable: they build it once per
// version, start it as a CLI (which starts the daemon as a detached
// process), and talk to the daemon over the private endpoint with the same
// client the CLI uses. Every test owns a private state directory and stops
// what it started.

var (
	binV1 string // 0.9.0, stable channel
	binV2 string // 0.9.1, stable channel
	binRC string // 0.9.1-rc.1, prerelease channel
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "purlview-lifecycle-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	build := func(version, name string) string {
		out := filepath.Join(dir, name, exeName("purlview"))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ldflags := "-X github.com/idyl-labs/purlview/internal/buildinfo.version=" + version + " -X github.com/idyl-labs/purlview/internal/buildinfo.builtBy=test"
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, "../../cmd/purlview")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if o, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build %s: %v\n%s", version, err, o)
			os.Exit(1)
		}
		return out
	}
	binV1 = build("0.9.0", "v1")
	binV2 = build("0.9.1", "v2")
	binRC = build("0.9.1-rc.1", "rc")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// buildInfoFor mirrors what the built executable reports about itself.
func buildInfoFor(version string) buildinfo.Info {
	return buildinfo.Info{Version: version, Commit: "unknown", BuiltBy: "test", GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// fakeReleases serves the two GitHub release endpoints the checker uses.
type fakeReleases struct {
	*httptest.Server
	mu       sync.Mutex
	latest   string   // stable tag, "" for 404
	all      []string // tags for the list endpoint
	status   int
	delay    time.Duration
	requests atomic.Int64
	paths    []string
}

func newFakeReleases(t *testing.T) *fakeReleases {
	t.Helper()
	f := &fakeReleases{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		latest, all, status, delay := f.latest, f.all, f.status, f.delay
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("{bogus"))
			return
		}
		type release struct {
			TagName    string `json:"tag_name"`
			Prerelease bool   `json:"prerelease"`
			HTMLURL    string `json:"html_url"`
		}
		mk := func(tag string) release {
			return release{TagName: tag, Prerelease: strings.Contains(tag, "-"), HTMLURL: "https://example.test/" + tag}
		}
		etag := `"` + latest + "|" + strings.Join(all, ",") + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if latest == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(mk(latest))
		case strings.HasSuffix(r.URL.Path, "/releases"):
			var out []release
			for _, tag := range all {
				out = append(out, mk(tag))
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// cached is the version the daemon's cache file holds for a channel, or "".
func (e *testEnv) cached(channel string) string {
	if latest := updatecheck.Cached(e.paths().UpdateCachePath(), channel); latest != nil {
		return latest.Version
	}
	return ""
}

func (f *fakeReleases) set(latest string, all ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest, f.all = latest, all
}

func (f *fakeReleases) fail(status int, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.delay = status, delay
}

func (f *fakeReleases) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// testEnv is one isolated Purlview state for a test.
type testEnv struct {
	t        *testing.T
	stateDir string
	api      *fakeReleases
	vars     map[string]string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, stateDir: filepath.Join(t.TempDir(), "state"), api: newFakeReleases(t)}
	e.vars = map[string]string{
		daemon.EnvStateDir:                   e.stateDir,
		daemon.EnvIdleExit:                   "30s",
		daemon.EnvTestResources:              "1",
		daemon.EnvStartTimeout:               "15s",
		"PURLVIEW_UPDATE_API_URL":            e.api.URL,
		"PURLVIEW_RELEASE_REPOSITORY":        "acme/purlview-releases",
		"PURLVIEW_DAEMON_TEST_STARTUP_DELAY": "",
		// The built executable defaults to production; no test may reach it.
		"PURLVIEW_PLATFORM_ENDPOINT": "none",
	}
	t.Cleanup(func() {
		// Stop only this environment's daemon; never anything else.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ctrl := e.controller(binV1, "0.9.0")
		if _, err := ctrl.Stop(ctx, daemon.StopOptions{Force: true, Timeout: 10 * time.Second}); err != nil {
			t.Logf("cleanup stop: %v", err)
		}
	})
	return e
}

func (e *testEnv) set(k, v string) { e.vars[k] = v }

func (e *testEnv) getenv(k string) string {
	if v, ok := e.vars[k]; ok {
		if k == "PURLVIEW_PLATFORM_ENDPOINT" && v == "none" {
			return "" // as the executable reads it
		}
		return v
	}
	if controlled(k) {
		return ""
	}
	return os.Getenv(k)
}

// controlled names the variables a test sets or leaves unset itself, never
// inherited from the machine: Purlview's own, and those that turn the update
// check off (a CI runner sets CI).
func controlled(k string) bool {
	return strings.HasPrefix(k, "PURLVIEW_") || k == "CI" || k == "DO_NOT_TRACK"
}

func (e *testEnv) environ() []string {
	env := []string{}
	for _, item := range os.Environ() {
		if k, _, _ := strings.Cut(item, "="); !controlled(k) {
			env = append(env, item)
		}
	}
	for k, v := range e.vars {
		env = append(env, k+"="+v)
	}
	return env
}

type result struct {
	code   int
	stdout string
	stderr string
	took   time.Duration
}

// run executes the CLI with this environment and captures the outcome.
func (e *testEnv) run(bin string, args ...string) result {
	e.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = e.environ()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errOut.String(), took: time.Since(start)}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			e.t.Fatalf("run %s %v: %v", filepath.Base(bin), args, err)
		}
		r.code = ee.ExitCode()
	}
	e.t.Logf("$ %s %s (exit %d, %s)\nstdout: %sstderr: %s", filepath.Base(filepath.Dir(bin)), strings.Join(args, " "), r.code, r.took.Round(time.Millisecond), r.stdout, r.stderr)
	return r
}

// controller is an in-process controller for the given executable, with
// the build identity that executable reports.
func (e *testEnv) controller(bin, version string) *daemon.Controller {
	e.t.Helper()
	c, err := daemon.NewController(e.getenv, buildInfoFor(version))
	if err != nil {
		e.t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		e.t.Fatal(err)
	}
	c.Executable = filepath.Clean(resolved)
	return c
}

// connect returns a handshaken client to the running daemon.
func (e *testEnv) connect(bin, version string) *daemon.Session {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := e.controller(bin, version).Connect(ctx)
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	e.t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func (e *testEnv) status(bin, version string) daemon.Report {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.controller(bin, version).Status(ctx)
}

func (e *testEnv) endpoint() *daemon.Endpoint {
	e.t.Helper()
	p, err := daemon.Resolve(e.getenv)
	if err != nil {
		e.t.Fatal(err)
	}
	ep, err := daemon.ReadEndpoint(p)
	if err != nil {
		e.t.Fatalf("endpoint: %v", err)
	}
	return ep
}

func (e *testEnv) paths() daemon.Paths {
	p, err := daemon.Resolve(e.getenv)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// ops lists the daemon's operations through the protocol.
func ops(t *testing.T, c *ipc.Client) []ipc.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res ipc.TestListResult
	if err := c.Call(ctx, ipc.OpTestList, nil, &res); err != nil {
		t.Fatalf("list: %v", err)
	}
	return res.Operations
}

func activeOps(t *testing.T, c *ipc.Client) []ipc.Operation {
	t.Helper()
	var out []ipc.Operation
	for _, op := range ops(t, c) {
		if op.State == "active" {
			out = append(out, op)
		}
	}
	return out
}

func findOp(list []ipc.Operation, id string) *ipc.Operation {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func startOp(t *testing.T, c *ipc.Client, name string, detached bool, ttl time.Duration) ipc.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var op ipc.Operation
	if err := c.Call(ctx, ipc.OpTestStart, ipc.TestStartParams{Name: name, Detached: detached, TTLMs: ttl.Milliseconds()}, &op); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	return op
}

func stopOp(t *testing.T, c *ipc.Client, id string, revoke bool) ipc.TestStopResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op := ipc.OpTestStop
	if revoke {
		op = ipc.OpTestRevoke
	}
	var res ipc.TestStopResult
	if err := c.Call(ctx, op, ipc.TestStopParams{ID: id}, &res); err != nil {
		t.Fatalf("stop %s: %v", id, err)
	}
	return res
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitEvent reads events until one matches or the timeout passes.
func waitEvent(t *testing.T, c *ipc.Client, timeout time.Duration, match func(*ipc.Event) bool) *ipc.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatal("event stream closed")
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for event")
		}
	}
}

func opEnded(id string) func(*ipc.Event) bool {
	return func(ev *ipc.Event) bool {
		if ev.Event != ipc.EventOpEnded {
			return false
		}
		var d ipc.OpEndedData
		return json.Unmarshal(ev.Data, &d) == nil && d.Operation.ID == id
	}
}

func subscribe(t *testing.T, c *ipc.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, ipc.OpSubscribe, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func processAlive(pid int, start int64) bool {
	return daemon.ProcessIdentity{PID: pid, StartTime: start}.Alive()
}
