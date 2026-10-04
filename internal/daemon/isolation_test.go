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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/daemon"
)

// Inside a test binary the default per-user paths belong to the developer
// who runs the tests. Resolve is where every caller gets its paths (the
// controller for Connect, Stop, Status and Ensure, the credential store, the
// daemon itself), so without an explicit state directory it refuses instead
// of pointing any of them at the real daemon or the real credential.
func TestDefaultPathsAreRefusedInsideATestBinary(t *testing.T) {
	t.Parallel()
	fakeHome := t.TempDir()
	environments := map[string]func(string) string{
		"nothing set": func(string) string { return "" },
		"the process environment without a state directory": func(k string) string {
			if k == daemon.EnvStateDir {
				return ""
			}
			return os.Getenv(k)
		},
		"another home directory is not a state directory": func(k string) string {
			switch k {
			case "HOME", "USERPROFILE":
				return fakeHome
			case "LOCALAPPDATA":
				return filepath.Join(fakeHome, "AppData", "Local")
			}
			return ""
		},
	}
	for name, getenv := range environments {
		if p, err := daemon.Resolve(getenv); !errors.Is(err, daemon.ErrUnisolatedTest) {
			t.Errorf("%s: Resolve returned %+v, %v", name, p, err)
		}
		if c, err := daemon.NewController(getenv, buildInfoFor("0.9.0")); !errors.Is(err, daemon.ErrUnisolatedTest) {
			t.Errorf("%s: NewController returned %+v, %v", name, c, err)
		}
	}
	if entries, _ := os.ReadDir(fakeHome); len(entries) != 0 {
		t.Errorf("a refused Resolve created %v", entries)
	}

	state := filepath.Join(t.TempDir(), "state")
	p, err := daemon.Resolve(func(k string) string {
		if k == daemon.EnvStateDir {
			return state
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Resolve with a state directory: %v", err)
	}
	for _, path := range []string{p.Runtime, p.Logs, p.Cache, p.State, p.CredentialsPath()} {
		if !strings.HasPrefix(path, state+string(filepath.Separator)) {
			t.Errorf("%s is outside the state directory %s", path, state)
		}
	}
}

// A controller hands the daemon it starts the process environment, not its
// injected getenv. Inside a test binary the start is therefore refused until
// the process environment names this controller's state directory, so no
// test can start a daemon on the developer's real per-user state, or on a
// directory the controller is not watching, by accident.
func TestInProcessStartNeedsStateDirInProcessEnvironment(t *testing.T) {
	e := newEnv(t)
	for _, item := range os.Environ() {
		if name, _, _ := strings.Cut(item, "="); strings.HasPrefix(name, "PURLVIEW_") {
			t.Setenv(name, "")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctrl := e.controller(binV1, "0.9.0")

	// The injected getenv relocates the controller's paths, but the daemon
	// would not see it.
	sess, err := ctrl.Ensure(ctx)
	if sess != nil {
		_ = sess.Close()
	}
	if !errors.Is(err, daemon.ErrUnisolatedTest) {
		t.Fatalf("Ensure without %s in the process environment: %v", daemon.EnvStateDir, err)
	}
	if _, err := os.Stat(e.paths().LogPath()); err == nil {
		t.Fatal("a daemon log exists: the refusal came after the spawn")
	}
	if daemon.IsHeld(e.paths().DaemonLockPath()) {
		t.Fatal("a daemon holds the lock after a refused start")
	}

	// A process environment that names another directory would start a
	// daemon there, one this controller would never hear from.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv(daemon.EnvStateDir, elsewhere)
	sess, err = ctrl.Ensure(ctx)
	if sess != nil {
		_ = sess.Close()
	}
	if !errors.Is(err, daemon.ErrUnisolatedTest) || !strings.Contains(err.Error(), elsewhere) {
		t.Fatalf("Ensure with another %s in the process environment: %v", daemon.EnvStateDir, err)
	}
	if _, err := os.Stat(elsewhere); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s after a refused start: %v", elsewhere, err)
	}
	if _, err := os.Stat(e.paths().LogPath()); err == nil {
		t.Fatal("a daemon log exists: the refusal came after the spawn")
	}

	// With the controller's state directory in the process environment the
	// same controller starts a daemon, and that daemon uses the private state.
	for k, v := range e.vars {
		t.Setenv(k, v)
	}
	sess, err = ctrl.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure with %s in the process environment: %v", daemon.EnvStateDir, err)
	}
	defer func() { _ = sess.Close() }()
	if !sess.Started {
		t.Fatal("expected this call to start the daemon")
	}
	if ep := e.endpoint(); ep.Instance != sess.Hello.Instance {
		t.Fatalf("endpoint in the private state names instance %s, the session %s", ep.Instance, sess.Hello.Instance)
	}
}
