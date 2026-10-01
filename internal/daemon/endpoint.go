package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/idyl-labs/purlview/internal/ipc"
)

// Endpoint is the record a running daemon writes once it is listening. It
// tells a CLI where to connect and which process and build it will find.
// Its presence alone proves nothing: liveness is established by the daemon
// lock and by a handshake that returns the same instance id.
type Endpoint struct {
	Protocol   int               `json:"protocol"`
	Instance   string            `json:"instance"`
	Process    ProcessIdentity   `json:"process"`
	Endpoint   string            `json:"endpoint"`
	Executable string            `json:"executable"`
	Build      ipc.BuildIdentity `json:"build"`
	StartedAt  time.Time         `json:"started_at"`
	// TestResources records that test-only operations are enabled.
	TestResources bool `json:"test_resources,omitempty"`
}

// ReadEndpoint loads the record, or returns fs.ErrNotExist.
func ReadEndpoint(p Paths) (*Endpoint, error) {
	data, err := os.ReadFile(p.EndpointPath())
	if err != nil {
		return nil, err
	}
	var e Endpoint
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", p.EndpointPath(), err)
	}
	if e.Instance == "" || e.Endpoint == "" {
		return nil, fmt.Errorf("%s: incomplete endpoint record", p.EndpointPath())
	}
	return &e, nil
}

// WriteEndpoint stores the record atomically with private permissions.
func WriteEndpoint(p Paths, e *Endpoint) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.EndpointPath(), data, 0o600)
}

// RemoveEndpoint deletes the record if it still names instance (an empty
// instance removes unconditionally).
func RemoveEndpoint(p Paths, instance string) error {
	if instance != "" {
		e, err := ReadEndpoint(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			// Unreadable: it is not ours to keep.
		} else if e.Instance != instance {
			return nil
		}
	}
	err := os.Remove(p.EndpointPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Maintenance marks an upgrade or uninstall in progress. While it is active
// no CLI starts a daemon; the marker expires on its own so a crashed
// installer cannot block sharing indefinitely.
type Maintenance struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
	PID    int       `json:"pid"`
}

// DefaultMaintenanceWindow bounds how long the marker blocks starts. The
// installer enters maintenance only after downloading and verifying the new
// release, so the window covers stop and replace, which take seconds.
const DefaultMaintenanceWindow = 3 * time.Minute

// WriteMaintenance creates the marker.
func WriteMaintenance(p Paths, d time.Duration, reason string) error {
	m := Maintenance{Until: time.Now().Add(d), Reason: reason, PID: os.Getpid()}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(p.MaintenancePath(), data, 0o600)
}

// ReadMaintenance returns the marker if it exists and has not expired.
func ReadMaintenance(p Paths) (*Maintenance, error) {
	data, err := os.ReadFile(p.MaintenancePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var m Maintenance
	if err := json.Unmarshal(data, &m); err != nil {
		// A corrupt marker must not block starts forever; treat it as absent.
		return nil, nil
	}
	if !time.Now().Before(m.Until) {
		return nil, nil
	}
	return &m, nil
}

// ClearMaintenance removes the marker.
func ClearMaintenance(p Paths) error {
	err := os.Remove(p.MaintenancePath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// writeFileAtomic writes data to a temporary file next to path and renames
// it into place, so readers never see a partial file.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
