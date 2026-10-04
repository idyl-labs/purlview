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
	"syscall"

	"golang.org/x/sys/unix"
)

// CurrentProcess returns this process's identity.
func CurrentProcess() (ProcessIdentity, error) {
	return LookupProcess(os.Getpid())
}

// LookupProcess reads the process start time from the kernel's process
// table (kern.proc.pid), in microseconds since the epoch.
func LookupProcess(pid int) (ProcessIdentity, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return ProcessIdentity{}, ErrNoProcess
		}
		return ProcessIdentity{}, err
	}
	if int(kp.Proc.P_pid) != pid {
		// The sysctl returns an empty record rather than an error for a
		// PID that does not exist.
		return ProcessIdentity{}, ErrNoProcess
	}
	tv := kp.Proc.P_starttime
	return ProcessIdentity{PID: pid, StartTime: tv.Sec*1_000_000 + int64(tv.Usec)}, nil
}

func killProcess(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return ErrNoProcess
	}
	return err
}
