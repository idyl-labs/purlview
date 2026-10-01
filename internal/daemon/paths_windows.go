package daemon

import (
	"github.com/idyl-labs/purlview/internal/ipc"
)

// endpointAddress names the pipe. The named-pipe namespace is machine-wide
// and shared by every logon session, so the name carries the user's SID (one
// daemon per user, reachable from all of that user's sessions) and a hash of
// the runtime directory (so an overridden state directory gets its own
// daemon). Access is enforced by the pipe's security descriptor, not by the
// name.
func endpointAddress(runtimeDir string, _ func(string) string) (string, error) {
	sid, err := ipc.CurrentUserSID()
	if err != nil {
		return "", err
	}
	return `\\.\pipe\purlview-` + sid + "-" + shortHash(runtimeDir, 8), nil
}

func ownedByCurrentUser(string) bool { return true }

func ensurePrivateRuntime(string) error { return nil }

func ensureSocketDir(string) error { return nil }
