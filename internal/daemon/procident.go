package daemon

import "errors"

// ErrNoProcess reports that no process with the given identity exists.
var ErrNoProcess = errors.New("no such process")

// ProcessIdentity identifies a live process: its PID together with the
// kernel's record of when it started. PIDs are reused; the pair is not.
type ProcessIdentity struct {
	PID       int   `json:"pid"`
	StartTime int64 `json:"start_time"`
}

// Alive reports whether the identified process still exists, meaning a
// process with that PID exists and started at the recorded time.
func (p ProcessIdentity) Alive() bool {
	cur, err := LookupProcess(p.PID)
	return err == nil && cur.StartTime == p.StartTime
}

// Kill terminates the process only if it is still the identified one.
func (p ProcessIdentity) Kill() error {
	if !p.Alive() {
		return ErrNoProcess
	}
	return killProcess(p.PID)
}
