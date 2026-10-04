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

package install

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Daemon-aware installer behaviour: an upgrade stops the running daemon and
// ends its work before replacing the executable, a failed replacement keeps
// the previous executable usable without restoring stopped work, and
// uninstall stops the daemon and removes only runtime state.

// daemonEnv points the CLI at a private state directory and a loopback
// metadata server so nothing touches the user's real daemon or the network.
// Every command also runs with isolatedEnv's throwaway home directory.
func daemonEnv(t *testing.T, dest string) map[string]string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	env := map[string]string{
		"PURLVIEW_INSTALL_DIR":           dest,
		"PURLVIEW_STATE_DIR":             state,
		"PURLVIEW_DAEMON_TEST_RESOURCES": "1",
		"PURLVIEW_DAEMON_IDLE_EXIT":      "60s",
		"PURLVIEW_UPDATE_API_URL":        "http://127.0.0.1:1",
	}
	if runtime.GOOS != "windows" {
		env["PATH"] = minimalPath()
	}
	// Built now, so the cleanup does not ask for the test's home directory
	// after the test has finished.
	stopEnv := isolatedEnv(t, env)
	t.Cleanup(func() {
		// Stop whatever daemon this test's state directory still has.
		for _, exe := range []string{filepath.Join(dest, exeName()), filepath.Join(dest, exeName()+".old")} {
			if _, err := os.Stat(exe); err == nil {
				cmd := exec.Command(exe, "daemon", "stop", "--force")
				cmd.Env = stopEnv
				_ = cmd.Run()
			}
		}
	})
	return env
}

func exeName() string {
	if hostOS == "windows" {
		return "purlview.exe"
	}
	return "purlview"
}

// daemonCmd runs a daemon subcommand of the installed executable.
func daemonCmd(t *testing.T, dest string, env map[string]string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(dest, exeName()), append([]string{"daemon"}, args...)...)
	cmd.Env = isolatedEnv(t, env)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run daemon %v: %v", args, err)
		}
	}
	t.Logf("$ purlview daemon %s (exit %d)\n%s", strings.Join(args, " "), code, out)
	return code, string(out)
}

// runInstaller dispatches to the host's installer.
func runInstaller(t *testing.T, srv *releaseServer, env map[string]string, version string) result {
	t.Helper()
	if hostOS == "windows" {
		hosts := powershellHosts(t)
		args := []string{}
		if version != "" {
			args = append(args, "-Version", version)
		}
		return runPs(t, hosts[0], srv, env, args...)
	}
	requireSh(t)
	args := []string{}
	if version != "" {
		args = append(args, "--version", version)
	}
	return runSh(t, srv, env, args...)
}

// runUninstall is runInstaller for the host's uninstall.
func runUninstall(t *testing.T, srv *releaseServer, env map[string]string) result {
	t.Helper()
	if hostOS == "windows" {
		return runPs(t, powershellHosts(t)[0], srv, env, "-Uninstall")
	}
	requireSh(t)
	return runSh(t, srv, env, "--uninstall")
}

func TestUpgradeStopsDaemonAndEndsWork(t *testing.T) {
	old := buildFixture(t, "0.9.0")
	newer := buildFixture(t, "0.9.1")
	srv := newReleaseServer(t, old, newer)
	srv.setLatest(newer.tag)
	dest := filepath.Join(t.TempDir(), "bin")
	env := daemonEnv(t, dest)

	if r := runInstaller(t, srv, env, "v0.9.0"); r.code != 0 {
		t.Fatalf("initial install: %d", r.code)
	}
	// A daemon with detached work, as a background share would leave.
	if code, out := daemonCmd(t, dest, env, "test-op", "--detached"); code != 0 {
		t.Fatalf("test-op: %d %s", code, out)
	}
	if code, out := daemonCmd(t, dest, env, "status"); code != 0 || !strings.Contains(out, "version 0.9.0") || !strings.Contains(out, "1 active") {
		t.Fatalf("status before upgrade: %d %s", code, out)
	}

	r := runInstaller(t, srv, env, "")
	if r.code != 0 || !r.contains("stopped the running Purlview daemon") || !r.contains("ends any active shares") {
		t.Fatalf("upgrade with a running daemon: code=%d\n%s", r.code, r.output)
	}
	if got := installedVersion(t, dest); got != "purlview 0.9.1" {
		t.Fatalf("installed %q", got)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		// The daemon released its file handle, so the old executable was
		// deleted, not merely renamed aside.
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("destination must contain only the new executable, got %v", names)
	}
	if code, out := daemonCmd(t, dest, env, "status"); code != 1 || !strings.Contains(out, "not running") || strings.Contains(out, "maintenance") {
		t.Fatalf("after upgrade the daemon must be stopped and maintenance released: %d %s", code, out)
	}
	// The next use starts the new version with no work carried over.
	if code, out := daemonCmd(t, dest, env, "start"); code != 0 || !strings.Contains(out, "daemon started") || !strings.Contains(out, "version 0.9.1") {
		t.Fatalf("start after upgrade: %d %s", code, out)
	}
	if code, out := daemonCmd(t, dest, env, "status"); code != 0 || !strings.Contains(out, "0 active") || strings.Contains(out, "owner=daemon active") {
		t.Fatalf("no work may survive the upgrade: %d %s", code, out)
	}
}

func TestFailedReplacementKeepsOldExecutableWithoutRestoringWork(t *testing.T) {
	if hostOS == "windows" {
		t.Skip("the read-only destination technique does not apply on Windows; the rename fallback is covered by TestPsReplacesRunningExecutable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	old := buildFixture(t, "0.9.0")
	newer := buildFixture(t, "0.9.1")
	srv := newReleaseServer(t, old, newer)
	srv.setLatest(newer.tag)
	dest := filepath.Join(t.TempDir(), "bin")
	env := daemonEnv(t, dest)
	if r := runInstaller(t, srv, env, "v0.9.0"); r.code != 0 {
		t.Fatalf("initial install: %d", r.code)
	}
	if code, out := daemonCmd(t, dest, env, "test-op", "--detached"); code != 0 {
		t.Fatalf("test-op: %d %s", code, out)
	}
	before, err := os.ReadFile(filepath.Join(dest, "purlview"))
	if err != nil {
		t.Fatal(err)
	}
	// Replacement fails after the daemon was stopped: the destination
	// became read-only.
	if err := os.Chmod(dest, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dest, 0o755) })
	r := runInstaller(t, srv, env, "")
	if r.code == 0 || !r.contains("cannot write to") || !r.contains("stopped the running Purlview daemon") {
		t.Fatalf("expected a failed replacement after the daemon stop: code=%d\n%s", r.code, r.output)
	}
	after, err := os.ReadFile(filepath.Join(dest, "purlview"))
	if err != nil || string(after) != string(before) {
		t.Fatal("the previous executable must be intact")
	}
	if got := installedVersion(t, dest); got != "purlview 0.9.0" {
		t.Fatalf("installed %q", got)
	}
	// Maintenance was released, the daemon is down, and its work is gone.
	if code, out := daemonCmd(t, dest, env, "status"); code != 1 || strings.Contains(out, "maintenance") {
		t.Fatalf("status after failed upgrade: %d %s", code, out)
	}
	if code, out := daemonCmd(t, dest, env, "start"); code != 0 || !strings.Contains(out, "version 0.9.0") {
		t.Fatalf("old version must still start its daemon: %d %s", code, out)
	}
	if code, out := daemonCmd(t, dest, env, "status"); code != 0 || !strings.Contains(out, "0 active") {
		t.Fatalf("stopped work must not be restored: %d %s", code, out)
	}
}

func TestUninstallStopsDaemonAndRemovesRuntimeState(t *testing.T) {
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)
	dest := filepath.Join(t.TempDir(), "bin")
	env := daemonEnv(t, dest)
	if r := runInstaller(t, srv, env, "v0.9.0"); r.code != 0 {
		t.Fatalf("install: %d", r.code)
	}
	if code, out := daemonCmd(t, dest, env, "test-op", "--detached"); code != 0 {
		t.Fatalf("test-op: %d %s", code, out)
	}
	state := env["PURLVIEW_STATE_DIR"]
	if _, err := os.Stat(filepath.Join(state, "runtime")); err != nil {
		t.Fatal("runtime state expected before uninstall")
	}
	// The account credential, where the CLI keeps it, and anything else
	// that is not runtime state must survive: inside the state directory
	// and next to it.
	keep := []string{
		filepath.Join(state, "state", "credentials.json"),
		filepath.Join(state, "notes.txt"),
		filepath.Join(filepath.Dir(state), "keep.txt"),
	}
	for _, file := range keep {
		writeSentinel(t, file)
	}
	for _, dir := range []string{"logs", "cache"} {
		writeSentinel(t, filepath.Join(state, dir, "sentinel"))
	}
	exe := filepath.Join(dest, exeName())
	r := runUninstall(t, srv, env)
	if r.code != 0 || !r.contains("stopped the running Purlview daemon") || !r.contains("removed runtime state") || !r.contains("credentials (if any) are not removed") {
		t.Fatalf("uninstall: code=%d\n%s", r.code, r.output)
	}
	if exists(exe) {
		t.Fatal("executable still present")
	}
	for _, dir := range []string{"runtime", "logs", "cache"} {
		if exists(filepath.Join(state, dir)) {
			t.Errorf("%s must be removed", filepath.Join(state, dir))
		}
	}
	for _, file := range keep {
		if got, err := os.ReadFile(file); err != nil || string(got) != "sentinel" {
			t.Errorf("uninstall removed or changed %s, which is not runtime state", file)
		}
	}
	// No daemon process survives: its lock directory is gone, and a fresh
	// status from a copy of the executable sees nothing.
	deadline := time.Now().Add(5 * time.Second)
	for exists(filepath.Join(state, "runtime", "daemon.lock")) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}
