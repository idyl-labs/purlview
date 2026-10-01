// Package daemon implements the per-user local daemon: its process
// lifecycle (singleton lock, detached start, readiness, idle exit), the
// server side of the private control protocol, the ownership and
// cancellation primitives behind shares, and the CLI-side controller
// that starts, reuses, retires and stops the daemon.
//
// docs/daemon.md describes the observable behaviour and its limits.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/idyl-labs/purlview/internal/production"
)

// File names inside the runtime directory.
const (
	endpointFile    = "endpoint.json"
	daemonLockFile  = "daemon.lock"
	startLockFile   = "start.lock"
	maintenanceFile = "maintenance.json"
	socketFile      = "daemon.sock"
	logFile         = "daemon.log"
	updateCacheFile = "update-check.json"
	updateNoteFile  = "update-notice.json"
	credentialsFile = "credentials.json"
)

// EnvStateDir relocates every runtime, log and cache path under one
// directory. It exists for tests and for isolating experiments; ordinary
// installations do not set it.
const EnvStateDir = "PURLVIEW_STATE_DIR"

// ErrUnisolatedTest is returned only inside a `go test` binary, where the
// default per-user paths are those of the developer who runs the tests: their
// daemon, their shares, their credential. Resolve refuses to compute them and
// a controller refuses to start a daemon that would (see spawnEnv), so a test
// that forgets its private state directory fails instead of touching them.
// testing.Testing is false in every executable made by `go build`; released
// binaries never return this error.
var ErrUnisolatedTest = errors.New("refusing to use the real per-user Purlview state from a test")

// Paths locates the daemon's private state for the current user.
type Paths struct {
	// Runtime holds the endpoint record, the locks, the maintenance marker
	// and (when the path is short enough) the Unix socket.
	Runtime string
	// Logs holds the bounded daemon log.
	Logs string
	// Cache holds the update-check cache.
	Cache string
	// State holds durable per-user product state that must outlive a
	// login session: the installation credential. It is never under a
	// runtime (tmpfs) directory.
	State string
	// Endpoint is the transport address: the socket path on macOS and Linux
	// (possibly outside Runtime, see socketPath), the pipe name on Windows.
	Endpoint string
}

// Resolve computes the paths from the environment without creating anything.
func Resolve(getenv func(string) string) (Paths, error) {
	var p Paths
	if root := getenv(EnvStateDir); root != "" {
		root, err := filepath.Abs(root)
		if err != nil {
			return p, err
		}
		p.Runtime = filepath.Join(root, "runtime")
		p.Logs = filepath.Join(root, "logs")
		p.Cache = filepath.Join(root, "cache")
		p.State = filepath.Join(root, "state")
	} else {
		if testing.Testing() {
			return p, fmt.Errorf("%w: %s is not set in the environment given to daemon.Resolve", ErrUnisolatedTest, EnvStateDir)
		}
		var err error
		p, err = defaultPaths(getenv)
		if err != nil {
			return p, err
		}
	}
	// Production, the installed default, keeps the plain paths; any other
	// configured platform (another deployment, a local stack) gets its own
	// directories so its daemon, logs and cache never mix with production's.
	if endpoint := getenv("PURLVIEW_PLATFORM_ENDPOINT"); endpoint != "" && !production.Is(getenv) {
		scope := shortHash(getenv("PURLVIEW_ENVIRONMENT")+"\x00"+endpoint+"\x00"+getenv("PURLVIEW_PLATFORM_HOST"), 8)
		p.Runtime = filepath.Join(p.Runtime, scope)
		p.Logs = filepath.Join(p.Logs, scope)
		p.Cache = filepath.Join(p.Cache, scope)
	}
	ep, err := endpointAddress(p.Runtime, getenv)
	if err != nil {
		return p, err
	}
	p.Endpoint = ep
	return p, nil
}

func defaultPaths(getenv func(string) string) (Paths, error) {
	var p Paths
	switch runtime.GOOS {
	case "darwin":
		home, err := homeDir(getenv)
		if err != nil {
			return p, err
		}
		p.Runtime = filepath.Join(home, "Library", "Application Support", "purlview", "runtime")
		p.Logs = filepath.Join(home, "Library", "Logs", "purlview")
		p.Cache = filepath.Join(home, "Library", "Caches", "purlview")
		p.State = filepath.Join(home, "Library", "Application Support", "purlview")
	case "windows":
		local := getenv("LOCALAPPDATA")
		if local == "" {
			return p, errors.New("LOCALAPPDATA is not set")
		}
		p.Runtime = filepath.Join(local, "purlview", "runtime")
		p.Logs = filepath.Join(local, "purlview", "logs")
		p.Cache = filepath.Join(local, "purlview", "cache")
		p.State = filepath.Join(local, "purlview")
	default:
		home, err := homeDir(getenv)
		if err != nil {
			return p, err
		}
		state := getenv("XDG_STATE_HOME")
		if state == "" {
			state = filepath.Join(home, ".local", "state")
		}
		cache := getenv("XDG_CACHE_HOME")
		if cache == "" {
			cache = filepath.Join(home, ".cache")
		}
		// XDG_RUNTIME_DIR is per user, but `sudo -E` and similar carry
		// another user's value along; only use it when it is ours.
		if rt := getenv("XDG_RUNTIME_DIR"); rt != "" && ownedByCurrentUser(rt) {
			p.Runtime = filepath.Join(rt, "purlview")
		} else {
			p.Runtime = filepath.Join(state, "purlview", "runtime")
		}
		p.Logs = filepath.Join(state, "purlview", "logs")
		p.Cache = filepath.Join(cache, "purlview")
		p.State = filepath.Join(state, "purlview")
	}
	return p, nil
}

func homeDir(getenv func(string) string) (string, error) {
	if h := getenv("HOME"); h != "" {
		return h, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return h, nil
}

// EnsureDirs creates the directories with private permissions.
func (p Paths) EnsureDirs() error {
	for _, dir := range []string{p.Runtime, p.Logs, p.Cache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return ensurePrivateRuntime(p.Runtime)
}

// EndpointPath is the endpoint record written by a running daemon.
func (p Paths) EndpointPath() string { return filepath.Join(p.Runtime, endpointFile) }

// DaemonLockPath is the lock a running daemon holds for its lifetime.
func (p Paths) DaemonLockPath() string { return filepath.Join(p.Runtime, daemonLockFile) }

// StartLockPath serialises daemon starts and maintenance.
func (p Paths) StartLockPath() string { return filepath.Join(p.Runtime, startLockFile) }

// MaintenancePath is the marker that blocks starts during an upgrade.
func (p Paths) MaintenancePath() string { return filepath.Join(p.Runtime, maintenanceFile) }

// LogPath is the bounded daemon log.
func (p Paths) LogPath() string { return filepath.Join(p.Logs, logFile) }

// UpdateCachePath is the update checker's cache file.
func (p Paths) UpdateCachePath() string { return filepath.Join(p.Cache, updateCacheFile) }

// UpdateNoticePath records when the CLI last showed an update notice.
func (p Paths) UpdateNoticePath() string { return filepath.Join(p.Cache, updateNoteFile) }

// CredentialsPath is the installation credential file (see package
// account). Only the CLI reads and writes it.
func (p Paths) CredentialsPath() string { return filepath.Join(p.State, credentialsFile) }

// shortHash derives a stable short identifier from a path.
func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(s))))
	return hex.EncodeToString(sum[:])[:n]
}
