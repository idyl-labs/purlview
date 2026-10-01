// Package install holds the fallback installers for Purlview and their tests.
//
// install.sh (macOS, Linux) and install.ps1 (Windows) are the scripts; the Go
// tests in this directory build fixture releases, serve them from a loopback
// HTTP server and exercise the installers end to end, including failure cases.
package install
