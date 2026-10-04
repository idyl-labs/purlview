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
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func sigstop() os.Signal { return os.Interrupt } // unused: the test skips on Windows

// startInTerminal launches a command processor in a new console that starts
// a detached test operation and then waits, so the daemon is created from a
// process attached to that console. A batch file avoids command-line
// quoting rules; the CLI's output is kept for diagnosis.
func startInTerminal(t *testing.T, e *testEnv, bin string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "launch.log")
	readyPath := filepath.Join(dir, "ready")
	batch := filepath.Join(dir, "launch.cmd")
	script := "@echo off\r\n\"" + bin + "\" daemon test-op --detached > \"" + logPath + "\" 2>&1\r\nif errorlevel 1 exit /b 1\r\necho ready>\"" + readyPath + "\"\r\nping -n 60 127.0.0.1 >nul\r\n"
	if err := os.WriteFile(batch, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cmd.exe", "/d", "/c", batch)
	cmd.Env = e.environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot create a console session: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
			_ = cmd.Wait()
		}
		if t.Failed() {
			out, _ := os.ReadFile(logPath)
			t.Logf("launcher output:\n%s", out)
		}
	})
	// The CLI must have exited before capturing the console's process tree:
	// its detached daemon is intentionally no longer part of that live tree.
	waitFor(t, "terminal launch command completed", 20*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	})
	return cmd
}

// closeTerminal ends every process attached to the console (the command
// processor and its ping child); the console then closes. A process that
// had inherited it would receive CTRL_CLOSE_EVENT and be terminated.
func closeTerminal(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	// taskkill can stop the console host and ping before reaching cmd.exe.
	// The command processor then exits itself, making taskkill report an
	// already-gone PID as an error. Hold handles to the original process tree
	// and verify its actual termination, including children, instead of using
	// taskkill's exit code as a proxy for whether the console closed.
	handles := terminalProcessHandles(t, uint32(cmd.Process.Pid))
	defer func() {
		for _, h := range handles {
			_ = windows.CloseHandle(h)
		}
	}()
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).CombinedOutput()
	deadline := time.Now().Add(10 * time.Second)
	for pid, h := range handles {
		remaining := max(time.Until(deadline).Milliseconds(), 0)
		state, waitErr := windows.WaitForSingleObject(h, uint32(remaining))
		if waitErr != nil || state != windows.WAIT_OBJECT_0 {
			t.Fatalf("terminal process %d did not exit: wait=%d err=%v; taskkill: %v %s", pid, state, waitErr, err, out)
		}
	}
	if err != nil {
		t.Logf("terminal process tree exited despite taskkill's error: %v %s", err, out)
	}
	_ = cmd.Wait()
}

func terminalProcessHandles(t *testing.T, root uint32) map[uint32]windows.Handle {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var entries []windows.ProcessEntry32
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		entries = append(entries, entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		t.Fatal(err)
	}
	rootHandle, rootCreated, err := terminalProcessHandle(root)
	if err != nil {
		t.Fatalf("open terminal launcher %d: %v", root, err)
	}
	handles := map[uint32]windows.Handle{root: rootHandle}
	created := map[uint32]int64{root: rootCreated}
	closeHandles := func() {
		for _, h := range handles {
			_ = windows.CloseHandle(h)
		}
	}
	for changed := true; changed; {
		changed = false
		for _, entry := range entries {
			parent, knownParent := handles[entry.ParentProcessID]
			if !knownParent || handles[entry.ProcessID] != 0 {
				continue
			}
			// An exited CLI is no longer an ancestor taskkill can traverse.
			// Its detached daemon must remain alive after the console closes.
			state, err := windows.WaitForSingleObject(parent, 0)
			if err != nil {
				closeHandles()
				t.Fatal(err)
			}
			if state != uint32(windows.WAIT_TIMEOUT) {
				continue
			}
			h, born, err := terminalProcessHandle(entry.ProcessID)
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				continue // the child exited after the snapshot
			}
			if err != nil {
				closeHandles()
				t.Fatalf("open terminal process %d: %v", entry.ProcessID, err)
			}
			if born < created[entry.ParentProcessID] {
				// Parent PIDs can be reused; an older process is not a child
				// of this incarnation of the purported parent.
				_ = windows.CloseHandle(h)
				continue
			}
			handles[entry.ProcessID] = h
			created[entry.ProcessID] = born
			changed = true
		}
	}
	return handles
}

func terminalProcessHandle(pid uint32) (windows.Handle, int64, error) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid) //nolint:misspell // Windows API constant spelling.
	if err != nil {
		return 0, 0, err
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		_ = windows.CloseHandle(h)
		return 0, 0, err
	}
	return h, creation.Nanoseconds(), nil
}
