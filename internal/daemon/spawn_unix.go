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
