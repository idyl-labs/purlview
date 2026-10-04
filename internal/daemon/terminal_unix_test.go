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

//go:build !windows

package daemon_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
)

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func sigstop() os.Signal { return syscall.SIGSTOP }

// startInTerminal runs a shell inside a pseudo-terminal (script(1)) that
// starts a detached test operation through the CLI and then lingers, so the
// daemon is launched from a session with a controlling terminal.
func startInTerminal(t *testing.T, e *testEnv, bin string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) is not available to allocate a pseudo-terminal")
	}
	shell := `"$0" daemon test-op --detached >/dev/null 2>&1; sleep 60`
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("script", "-q", "/dev/null", "sh", "-c", shell, bin)
	} else {
		cmd = exec.Command("script", "-q", "-c", "sh -c '"+shell+"' "+bin, "/dev/null")
	}
	cmd.Env = e.environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a pseudo-terminal session: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// closeTerminal kills the pty owner; the kernel hangs up the session
// (SIGHUP to the shell and its foreground group), which is what closing a
// terminal window does.
func closeTerminal(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
}
