//go:build !windows

package output

// EnableColor is only needed on Windows; other terminals interpret ANSI
// escapes as they are.
func EnableColor(...any) bool { return true }
