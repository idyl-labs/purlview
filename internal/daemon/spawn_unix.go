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

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

// configureDetached makes the child a new session leader so it has no
// controlling terminal: closing the launching terminal (SIGHUP to its
// session) does not reach it, and it receives no job-control signals.
// Standard input is /dev/null and output goes to the log file, so no pipe
// of the launching shell stays open.
func configureDetached(cmd *exec.Cmd, logOut *os.File) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = logOut
	cmd.Stderr = logOut
}

// startDetached starts the command; on Unix a single attempt suffices.
func startDetached(build func() *exec.Cmd) (*exec.Cmd, error) {
	cmd := build()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
