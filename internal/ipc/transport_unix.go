//go:build !windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// EndpointKind names the transport for documentation and status output.
const EndpointKind = "unix socket"

// Listen creates the daemon's Unix-domain socket at path. The parent
// directory must already be private to the current user (see
// CheckPrivateDir); the socket file itself is created with mode 0600 and a
// leftover file at path is removed first (the caller has established that no
// live daemon owns it).
func Listen(path string) (net.Listener, error) {
	if err := CheckPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	// The directory is private (checked above), so the socket is unreachable
	// by other users even before its own mode is tightened.
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return &peerCheckedListener{Listener: l}, nil
}

// peerCheckedListener rejects connections from other users. The directory
// mode already keeps them out; this is defence in depth and covers a
// misconfigured directory.
type peerCheckedListener struct {
	net.Listener
}

func (l *peerCheckedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err != nil || uid != os.Geteuid() {
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

// Dial connects to the daemon socket at path after verifying that the socket
// and its directory belong to the current user and are not accessible to
// others.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	if err := CheckPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode()&fs.ModeSocket == 0 {
		return nil, fmt.Errorf("%s is not a socket", path)
	}
	if err := checkOwnerAndMode(path, st, 0o077); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	return d.DialContext(ctx, "unix", path)
}

// CheckPrivateDir verifies that dir exists, is owned by the current user and
// grants no permissions to group or others.
func CheckPrivateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return checkOwnerAndMode(dir, st, 0o077)
}

func checkOwnerAndMode(path string, st fs.FileInfo, forbidden fs.FileMode) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read ownership", path)
	}
	if int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not the current user (uid %d)", path, sys.Uid, os.Geteuid())
	}
	if st.Mode().Perm()&forbidden != 0 {
		return fmt.Errorf("%s is accessible to other users (mode %04o)", path, st.Mode().Perm())
	}
	return nil
}
