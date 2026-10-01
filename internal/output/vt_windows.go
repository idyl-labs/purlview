package output

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnableColor switches a Windows console to ANSI escape processing, which
// Windows Terminal and conhost support but leave off until a process asks.
// It reports false when a stream is a console that refuses, so the caller
// falls back to plain text instead of printing raw escapes. Streams that are
// not consoles (pipes, files) need nothing.
func EnableColor(streams ...any) bool {
	for _, s := range streams {
		f, ok := s.(*os.File)
		if !ok || f == nil {
			continue
		}
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) != nil {
			continue
		}
		if windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
			return false
		}
	}
	return true
}
