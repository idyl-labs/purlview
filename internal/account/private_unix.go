//go:build !windows

package account

import (
	"errors"
	"os"
	"syscall"
)

func privateFile(path string, fix bool) error {
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("credential path is a symlink")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || uint64(sys.Uid) != uint64(os.Getuid()) { //nolint:gosec // OS user IDs are nonnegative
		return errors.New("credential path is not owned by current user")
	}
	mode := os.FileMode(0600)
	if st.IsDir() {
		mode = 0700
	}
	if fix {
		return os.Chmod(path, mode)
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("credential permissions must be owner-only")
	}
	return nil
}
