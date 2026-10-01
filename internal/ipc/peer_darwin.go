package ipc

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the effective uid of the process at the other end of a
// Unix-domain connection (LOCAL_PEERCRED).
func peerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			cerr = err
			return
		}
		uid = int(cred.Uid)
	})
	if err != nil {
		return -1, err
	}
	return uid, cerr
}
