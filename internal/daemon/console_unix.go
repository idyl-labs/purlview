//go:build !windows

package daemon

import (
	"os"
	"os/signal"
	"syscall"
)

// consoleState reports whether the process has a controlling terminal.
func consoleState() string {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "none"
	}
	_ = f.Close()
	return "attached"
}

// notifyTermination registers the signals that ask the daemon to shut down.
func notifyTermination(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}
