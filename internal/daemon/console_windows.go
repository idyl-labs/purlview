package daemon

import (
	"os"
	"os/signal"

	"golang.org/x/sys/windows"
)

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	getConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

// consoleState reports whether the process is attached to a console.
func consoleState() string {
	if err := getConsoleWindow.Find(); err != nil {
		return "unknown"
	}
	h, _, _ := getConsoleWindow.Call()
	if h == 0 {
		return "none"
	}
	return "attached"
}

// notifyTermination registers Ctrl-C/Ctrl-Break (only delivered when a
// console exists).
func notifyTermination(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
