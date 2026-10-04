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
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

// CurrentProcess returns this process's identity.
func CurrentProcess() (ProcessIdentity, error) {
	return identityOf(windows.CurrentProcess(), int(windows.GetCurrentProcessId()))
}

// LookupProcess reads the process creation time (FILETIME) of pid.
func LookupProcess(pid int) (ProcessIdentity, error) {
	if pid <= 0 || pid > math.MaxUint32 {
		return ProcessIdentity{}, ErrNoProcess
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return ProcessIdentity{}, ErrNoProcess
		}
		return ProcessIdentity{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	// A handle can be opened for a process that has exited but is not yet
	// reaped; treat that as gone.
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != 259 { // STILL_ACTIVE
		return ProcessIdentity{}, ErrNoProcess
	}
	return identityOf(h, pid)
}

func identityOf(h windows.Handle, pid int) (ProcessIdentity, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return ProcessIdentity{}, fmt.Errorf("process times: %w", err)
	}
	return ProcessIdentity{PID: pid, StartTime: creation.Nanoseconds()}, nil
}

func killProcess(pid int) error {
	if pid <= 0 || pid > math.MaxUint32 {
		return fmt.Errorf("open process %d: %w", pid, ErrNoProcess)
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return windows.TerminateProcess(h, 137)
}
