// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/updatecheck"
)

// Environment knobs read by the controller. They exist for tests and for
// manual diagnosis; docs/daemon.md lists them.
const (
	EnvStartTimeout  = "PURLVIEW_DAEMON_START_TIMEOUT"
	EnvIdleExit      = "PURLVIEW_DAEMON_IDLE_EXIT"
	EnvTestResources = "PURLVIEW_DAEMON_TEST_RESOURCES"
	// EnvTestStartupDelay delays the daemon's listen step; honoured only
	// together with EnvTestResources.
	EnvTestStartupDelay = "PURLVIEW_DAEMON_TEST_STARTUP_DELAY"
)

// DefaultStartTimeout bounds one daemon start, from spawn to handshake.
const DefaultStartTimeout = 10 * time.Second

// Sentinel errors reported by the controller.
var (
	ErrNotRunning   = errors.New("daemon is not running")
	ErrMaintenance  = errors.New("Purlview is being upgraded or removed; try again in a moment") //nolint:staticcheck // Purlview is a proper product name.
	ErrUnresponsive = errors.New("daemon is running but not responding")
)

// Controller is the CLI side of the daemon lifecycle.
type Controller struct {
	Paths      Paths
	Executable string
	Build      buildinfo.Info
	// StartTimeout bounds Ensure's start; IdleExit is passed to a daemon
	// this controller starts.
	StartTimeout  time.Duration
	IdleExit      time.Duration
	TestResources bool
	// Diagnostics receives progress messages (nil discards them); Notices
	// receives the few messages a user should see even during routine use,
	// such as an outdated daemon being replaced.
	Diagnostics io.Writer
	Notices     io.Writer
	// Replaced, when set, is told instead of Notices that an outdated daemon
	// is being replaced, so share can say it in the product's own words.
	Replaced func()
	getenv   func(string) string
}

// NewController resolves paths and options from the environment.
func NewController(getenv func(string) string, info buildinfo.Info) (*Controller, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	p, err := Resolve(getenv)
	if err != nil {
		return nil, err
	}
	exe, err := executablePath()
	if err != nil {
		return nil, err
	}
	c := &Controller{Paths: p, Executable: exe, Build: info, StartTimeout: DefaultStartTimeout, IdleExit: DefaultIdleExit, getenv: getenv}
	if v := getenv(EnvStartTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.StartTimeout = d
		}
	}
	if v := getenv(EnvIdleExit); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.IdleExit = d
		}
	}
	c.TestResources = getenv(EnvTestResources) == "1"
	return c, nil
}

func (c *Controller) diag(format string, args ...any) {
	if c.Diagnostics != nil {
		fmt.Fprintf(c.Diagnostics, format+"\n", args...)
	}
}

func (c *Controller) notice(format string, args ...any) {
	if c.Notices != nil {
		fmt.Fprintf(c.Notices, "purlview: "+format+"\n", args...)
	} else {
		c.diag(format, args...)
	}
}

// Session is an open, handshaken connection to a daemon.
type Session struct {
	Client *ipc.Client
	Hello  *ipc.HelloResult
	// Started reports that this call started the daemon; Retired names the
	// build of a daemon this call stopped because it did not match the
	// installed executable.
	Started bool
	Retired string
}

// Close closes the connection.
func (s *Session) Close() error {
	if s == nil || s.Client == nil {
		return nil
	}
	return s.Client.Close()
}

// connect dials the recorded endpoint and performs the handshake. It
// distinguishes "not running" (no record, dead socket, lock free) from
// "unresponsive" (lock held, no usable answer).
func (c *Controller) connect(ctx context.Context) (*Session, error) {
	ep, err := ReadEndpoint(c.Paths)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotRunning
		}
		if errors.Is(err, fs.ErrPermission) {
			// Another user's runtime directory, or broken permissions:
			// say so rather than "not running".
			return nil, fmt.Errorf("runtime state is not accessible (is %s owned by you?): %w", c.Paths.Runtime, err)
		}
		// A corrupt record: decide by the lock.
		if !IsHeld(c.Paths.DaemonLockPath()) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("%w: %w", ErrUnresponsive, err)
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := ipc.Dial(dctx, ep.Endpoint)
	if err != nil {
		if !IsHeld(c.Paths.DaemonLockPath()) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("%w (pid %d): %w", ErrUnresponsive, ep.Process.PID, err)
	}
	client := ipc.NewClient(raw)
	hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
	defer hcancel()
	hello, err := client.Hello(hctx, "cli", buildIdentity(c.Build))
	if err != nil {
		var pe *ipc.Error
		if errors.As(err, &pe) && pe.Code == ipc.CodeProtocolMismatch {
			// Keep the connection: it can still be told to shut down.
			return &Session{Client: client, Hello: &ipc.HelloResult{Instance: ep.Instance, PID: ep.Process.PID, Build: ep.Build, Executable: ep.Executable, Protocol: -1}}, nil
		}
		_ = client.Close()
		if !IsHeld(c.Paths.DaemonLockPath()) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("%w (pid %d): %w", ErrUnresponsive, ep.Process.PID, err)
	}
	return &Session{Client: client, Hello: hello}, nil
}

// Connect returns a session with the running daemon or ErrNotRunning. It
// never starts one.
func (c *Controller) Connect(ctx context.Context) (*Session, error) {
	return c.connect(ctx)
}

// matches reports whether the daemon runs the installed executable.
func (c *Controller) matches(h *ipc.HelloResult) bool {
	if h.Protocol != ipc.ProtocolVersion {
		return false
	}
	if !samePath(h.Executable, c.Executable) {
		return false
	}
	if h.Build != buildIdentity(c.Build) {
		return false
	}
	st, err := os.Stat(c.Executable)
	if err != nil {
		return true // cannot tell; do not churn
	}
	if h.ExeSize != st.Size() {
		return false
	}
	if t, err := time.Parse(time.RFC3339Nano, h.ExeModTime); err == nil && !t.Equal(st.ModTime().UTC()) {
		return false
	}
	return true
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Ensure returns a session with a daemon that runs this executable,
// starting one when none runs and retiring one that runs a different build.
func (c *Controller) Ensure(ctx context.Context) (*Session, error) {
	if m, err := ReadMaintenance(c.Paths); err == nil && m != nil {
		return nil, fmt.Errorf("%w (%s until %s)", ErrMaintenance, m.Reason, m.Until.Local().Format(time.Kitchen))
	}
	retired := ""
	sess, err := c.connect(ctx)
	switch {
	case err == nil:
		if c.matches(sess.Hello) {
			return sess, nil
		}
		retired = describe(sess.Hello)
		if c.Replaced != nil {
			c.Replaced()
			c.diag("replacing the local daemon (it runs %s; this executable is %s); its shares have ended", retired, c.Build.Version)
		} else {
			c.notice("replacing the local daemon (it runs %s; this executable is %s); its shares have ended", retired, c.Build.Version)
		}
		if err := c.stopSession(ctx, sess, c.StartTimeout); err != nil {
			return nil, fmt.Errorf("stop outdated daemon: %w", err)
		}
	case errors.Is(err, ErrNotRunning), errors.Is(err, ErrUnresponsive):
		// Another starter may hold the daemon lock while publishing its
		// endpoint (including a transient Windows sharing violation). Wait
		// for the start lock and probe again before declaring it unresponsive.
	default:
		return nil, err
	}

	if err := c.Paths.EnsureDirs(); err != nil {
		return nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, c.StartTimeout)
	defer cancel()
	lock, err := Lock(lctx, c.Paths.StartLockPath())
	if err != nil {
		return nil, fmt.Errorf("waiting for another Purlview command to finish starting the daemon: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	if m, err := ReadMaintenance(c.Paths); err == nil && m != nil {
		return nil, fmt.Errorf("%w (%s until %s)", ErrMaintenance, m.Reason, m.Until.Local().Format(time.Kitchen))
	}
	// Another command may have started it while this one waited.
	sess, err = c.connect(ctx)
	switch {
	case err == nil:
		if c.matches(sess.Hello) {
			sess.Retired = retired
			return sess, nil
		}
		_ = sess.Close()
		return nil, fmt.Errorf("a daemon running %s appeared while starting; retry", describe(sess.Hello))
	case errors.Is(err, ErrNotRunning):
	default:
		return nil, err
	}
	if err := c.cleanStale(); err != nil {
		return nil, err
	}
	sess, err = c.start(ctx)
	if err != nil {
		return nil, err
	}
	sess.Retired = retired
	return sess, nil
}

func describe(h *ipc.HelloResult) string {
	if h == nil {
		return "unknown build"
	}
	s := h.Build.Version
	if h.Build.Commit != "" && h.Build.Commit != "unknown" {
		s += " (" + shortCommit(h.Build.Commit) + ")"
	}
	if h.Protocol >= 0 && h.Protocol != ipc.ProtocolVersion {
		s += fmt.Sprintf(" protocol %d", h.Protocol)
	}
	return s
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// cleanStale removes the endpoint record and socket of a daemon that no
// longer holds the lock. It touches only files this runtime directory
// recorded; it never signals a process.
func (c *Controller) cleanStale() error {
	lock, err := TryLock(c.Paths.DaemonLockPath())
	if errors.Is(err, ErrLocked) {
		return ErrUnresponsive
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if ep, err := ReadEndpoint(c.Paths); err == nil {
		c.diag("removing stale endpoint record of daemon pid %d (%s)", ep.Process.PID, ep.Build.Version)
		if runtime.GOOS != "windows" && ep.Endpoint != "" {
			_ = os.Remove(ep.Endpoint)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Remove(c.Paths.Endpoint); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return RemoveEndpoint(c.Paths, "")
}

// spawnEnv is the environment a daemon started by this controller inherits:
// the process environment, unchanged.
//
// The daemon resolves its paths from that environment, not from the
// controller's getenv. A test that relocates state only through an injected
// getenv (which satisfies Resolve) would therefore still start a daemon on
// the developer's real per-user state, and one whose process environment
// names another directory would start a daemon there and then wait in vain
// for its handshake, so inside a `go test` binary the start is refused unless
// the process environment names this controller's state directory. Like the
// check in Resolve, this never applies to a released executable.
func (c *Controller) spawnEnv() ([]string, error) {
	if !testing.Testing() {
		return os.Environ(), nil
	}
	if os.Getenv(EnvStateDir) == "" {
		return nil, fmt.Errorf("%w: cannot start a daemon because %s is not set in the process environment, which the daemon inherits instead of the controller's getenv (use t.Setenv)", ErrUnisolatedTest, EnvStateDir)
	}
	p, err := Resolve(os.Getenv)
	if err != nil {
		return nil, err
	}
	if p.State != c.Paths.State {
		return nil, fmt.Errorf("%w: cannot start a daemon because %s in the process environment, which the daemon inherits, names %s while this controller's state is under %s (use t.Setenv with the controller's directory)", ErrUnisolatedTest, EnvStateDir, filepath.Dir(p.State), filepath.Dir(c.Paths.State))
	}
	return os.Environ(), nil
}

// start spawns the daemon and waits for its handshake.
func (c *Controller) start(ctx context.Context) (*Session, error) {
	env, err := c.spawnEnv()
	if err != nil {
		return nil, err
	}
	instance := newID()
	logOut, err := os.OpenFile(c.Paths.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = logOut.Close() }()

	args := []string{"daemon", "run", "--instance", instance, "--idle-exit", c.IdleExit.String()}
	if c.TestResources {
		args = append(args, "--test-resources")
	}
	// The executable is this program's own resolved path and the arguments
	// are fixed; nothing here comes from user input.
	build := func() *exec.Cmd {
		cmd := exec.Command(c.Executable, args...) //nolint:gosec // see above
		cmd.Dir = c.Paths.Runtime
		cmd.Env = env
		configureDetached(cmd, logOut)
		return cmd
	}
	cmd, err := startDetached(build)
	if err != nil {
		return nil, fmt.Errorf("start daemon %s: %w", c.Executable, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	c.diag("started daemon pid %d (instance %s); waiting for readiness", cmd.Process.Pid, instance)

	deadline := time.Now().Add(c.StartTimeout)
	for {
		if ep, err := ReadEndpoint(c.Paths); err == nil && ep.Instance == instance {
			sess, err := c.connect(ctx)
			if err == nil {
				if sess.Hello.Instance == instance && sess.Hello.Ready {
					sess.Started = true
					return sess, nil
				}
				_ = sess.Close()
			}
		}
		select {
		case err := <-exited:
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() == ExitAlreadyRunning {
				// Someone else won the race after all; use their daemon.
				if sess, err := c.connect(ctx); err == nil {
					return sess, nil
				}
			}
			return nil, fmt.Errorf("daemon exited during startup (%w); last log lines from %s:\n%s", err, c.Paths.LogPath(), tailFile(c.Paths.LogPath(), 8))
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), c.abandon(cmd, instance, exited))
		case <-time.After(40 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			if err := c.abandon(cmd, instance, exited); err != nil {
				return nil, fmt.Errorf("daemon did not become ready within %s; cleanup failed: %w. Log: %s\n%s", c.StartTimeout, err, c.Paths.LogPath(), tailFile(c.Paths.LogPath(), 8))
			}
			return nil, fmt.Errorf("daemon did not become ready within %s; it was stopped. Log: %s\n%s", c.StartTimeout, c.Paths.LogPath(), tailFile(c.Paths.LogPath(), 8))
		}
	}
}

// abandon kills the daemon this controller just started (and only that one)
// and waits for its exit before removing its endpoint and releasing the start
// lock. In particular, Windows TerminateProcess returns before handles and
// locks have been released by the child.
func (c *Controller) abandon(cmd *exec.Cmd, instance string, exited <-chan error) error {
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop daemon pid %d: %w", cmd.Process.Pid, err)
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		return fmt.Errorf("daemon pid %d did not exit after being stopped", cmd.Process.Pid)
	}
	return RemoveEndpoint(c.Paths, instance)
}

// StopOptions configure Stop.
type StopOptions struct {
	Timeout time.Duration
	// Maintenance leaves a marker that blocks starts until Resume or expiry.
	Maintenance bool
	// Force kills an unresponsive daemon after verifying its identity.
	Force  bool
	Reason string
}

// StopResult reports what Stop did.
type StopResult struct {
	WasRunning bool
	Operations int
	Build      string
	PID        int
	Forced     bool
}

// Stop shuts the daemon down and waits until it has released its lock. It
// is idempotent: a daemon that is not running is a success.
func (c *Controller) Stop(ctx context.Context, opts StopOptions) (StopResult, error) {
	var res StopResult
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	if opts.Reason == "" {
		opts.Reason = "stop"
	}
	if err := c.Paths.EnsureDirs(); err != nil {
		return res, err
	}
	if opts.Maintenance {
		lctx, cancel := context.WithTimeout(ctx, opts.Timeout)
		lock, err := Lock(lctx, c.Paths.StartLockPath())
		cancel()
		if err != nil {
			return res, fmt.Errorf("waiting for a daemon start to finish: %w", err)
		}
		defer func() { _ = lock.Unlock() }()
		if err := WriteMaintenance(c.Paths, DefaultMaintenanceWindow, opts.Reason); err != nil {
			return res, err
		}
	}
	var ident *ProcessIdentity
	if ep, err := ReadEndpoint(c.Paths); err == nil {
		id := ep.Process
		ident = &id
	}
	sess, err := c.connect(ctx)
	switch {
	case err == nil:
		res.WasRunning = true
		res.Build, res.PID = describe(sess.Hello), sess.Hello.PID
		var ack ipc.ShutdownResult
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := sess.Client.Call(sctx, ipc.OpShutdown, ipc.ShutdownParams{Reason: opts.Reason}, &ack)
		cancel()
		_ = sess.Close()
		if err != nil && !ipc.IsCode(err, ipc.CodeShuttingDown) {
			if !opts.Force {
				return res, fmt.Errorf("shutdown request failed: %w", err)
			}
		}
		res.Operations = ack.Operations
		if err := c.waitStopped(ctx, opts.Timeout); err != nil {
			if !opts.Force {
				return res, err
			}
			forced, ferr := c.forceKill()
			if ferr != nil {
				return res, ferr
			}
			res.Forced = forced
		}
		// The lock is released just before the process exits; wait for the
		// process itself so its executable handle is closed (an installer
		// replaces the file right after this returns).
		if ident != nil && ident.PID == sess.Hello.PID {
			exitDeadline := time.Now().Add(5 * time.Second)
			for ident.Alive() && time.Now().Before(exitDeadline) {
				time.Sleep(20 * time.Millisecond)
			}
		}
	case errors.Is(err, ErrNotRunning):
	case errors.Is(err, ErrUnresponsive):
		res.WasRunning = true
		if !opts.Force {
			return res, err
		}
		forced, ferr := c.forceKill()
		if ferr != nil {
			return res, ferr
		}
		res.Forced = forced
	default:
		return res, err
	}
	// Whatever is left is stale by now: the lock is free.
	if err := c.cleanStale(); err != nil && !errors.Is(err, ErrUnresponsive) {
		return res, err
	}
	return res, nil
}

// waitStopped polls until the daemon lock is free.
func (c *Controller) waitStopped(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !IsHeld(c.Paths.DaemonLockPath()) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: daemon did not exit within %s", ErrUnresponsive, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// forceKill terminates the daemon recorded in the endpoint file, but only
// when the process at that PID is still the recorded one.
func (c *Controller) forceKill() (bool, error) {
	ep, err := ReadEndpoint(c.Paths)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !ep.Process.Alive() {
		return false, nil
	}
	c.diag("killing unresponsive daemon pid %d (started %s)", ep.Process.PID, ep.StartedAt.Format(time.RFC3339))
	if err := ep.Process.Kill(); err != nil && !errors.Is(err, ErrNoProcess) {
		return false, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for IsHeld(c.Paths.DaemonLockPath()) {
		if time.Now().After(deadline) {
			return true, fmt.Errorf("daemon pid %d was killed but its lock is still held", ep.Process.PID)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true, nil
}

// Resume clears the maintenance marker.
func (c *Controller) Resume() error {
	return ClearMaintenance(c.Paths)
}

// Report is a point-in-time view for `daemon status`.
type Report struct {
	Running     bool
	Endpoint    string
	Hello       *ipc.HelloResult
	Status      *ipc.StatusResult
	Maintenance *Maintenance
	// Update is a newer release the daemon already knows of, on this
	// executable's channel; nil when there is none or it is not known yet.
	Update *ipc.ReleaseInfo
	Error  string
}

// Status inspects the daemon without starting it.
func (c *Controller) Status(ctx context.Context) Report {
	r := Report{Endpoint: c.Paths.Endpoint}
	if m, err := ReadMaintenance(c.Paths); err == nil {
		r.Maintenance = m
	}
	sess, err := c.connect(ctx)
	if err != nil {
		if !errors.Is(err, ErrNotRunning) {
			r.Error = err.Error()
		}
		return r
	}
	defer func() { _ = sess.Close() }()
	r.Running = true
	r.Hello = sess.Hello
	var st ipc.StatusResult
	if err := sess.Client.Call(ctx, ipc.OpStatus, nil, &st); err == nil {
		r.Status = &st
	} else {
		r.Error = err.Error()
	}
	// From the daemon's cache file, so that status never starts a check.
	if latest := updatecheck.Cached(c.Paths.UpdateCachePath(), updatecheck.Channel(c.Build)); latest != nil && updatecheck.Newer(c.Build, latest) {
		r.Update = latest
	}
	return r
}

// stopSession shuts down the daemon behind an open session and waits.
func (c *Controller) stopSession(ctx context.Context, sess *Session, timeout time.Duration) error {
	defer func() { _ = sess.Close() }()
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := sess.Client.Call(sctx, ipc.OpShutdown, ipc.ShutdownParams{Reason: "outdated build"}, nil); err != nil && !ipc.IsCode(err, ipc.CodeShuttingDown) {
		return err
	}
	return c.waitStopped(ctx, timeout)
}

// ParseBool reads an environment-style boolean.
func ParseBool(s string) bool {
	b, err := strconv.ParseBool(s)
	return err == nil && b
}
