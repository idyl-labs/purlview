package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// CurrentProcess returns this process's identity.
func CurrentProcess() (ProcessIdentity, error) {
	return LookupProcess(os.Getpid())
}

// LookupProcess reads the process start time (clock ticks since boot, field
// 22 of /proc/<pid>/stat), which stays constant for the life of the process.
func LookupProcess(pid int) (ProcessIdentity, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProcessIdentity{}, ErrNoProcess
		}
		return ProcessIdentity{}, err
	}
	// The command name is in parentheses and may contain spaces; parse
	// after the closing parenthesis.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ProcessIdentity{}, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is state (field 3), so starttime (field 22) is fields[19].
	if len(fields) < 20 {
		return ProcessIdentity{}, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{PID: pid, StartTime: start}, nil
}

func killProcess(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return ErrNoProcess
	}
	return err
}
