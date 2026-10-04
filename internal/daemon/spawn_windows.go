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

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configureDetached creates the child without a console (DETACHED_PROCESS)
// and in its own process group, so it is not attached to the launching
// console and does not receive that console's Ctrl-C or close events. Its
// standard handles are the log file, not the console's.
func configureDetached(cmd *exec.Cmd, logOut *os.File) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
		HideWindow:    true,
	}
	cmd.Stdin = nil
	cmd.Stdout = logOut
	cmd.Stderr = logOut
}

// startDetached first tries to break the child out of the caller's job
// object, so a job configured to kill its members on close (some terminal
// hosts and CI runners) does not take the daemon with it. When the job
// forbids breakaway, CreateProcess fails with access denied and the child is
// started inside the job instead; docs/daemon.md records this limit.
func startDetached(build func() *exec.Cmd) (*exec.Cmd, error) {
	cmd := build()
	err := cmd.Start()
	if err != nil && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		cmd = build()
		cmd.SysProcAttr.CreationFlags &^= windows.CREATE_BREAKAWAY_FROM_JOB
		err = cmd.Start()
	}
	if err != nil {
		return nil, err
	}
	return cmd, nil
}
