//go:build !darwin && !linux

package command

// prettyName: Windows and the rest call a computer by its hostname.
func prettyName() string { return "" }
