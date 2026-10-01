package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/updatecheck"
)

// Exit codes of the daemon process.
const (
	// ExitAlreadyRunning is returned when another daemon holds the lock.
	ExitAlreadyRunning = 3
)

// ErrAlreadyRunning reports that a daemon already owns this runtime.
var ErrAlreadyRunning = errors.New("another daemon already owns this runtime directory")

// RunOptions configure the daemon process.
type RunOptions struct {
	Paths    Paths
	Instance string
	Build    buildinfo.Info
	IdleExit time.Duration
	// TestResources enables the test-only operations.
	TestResources bool
	// StartupDelay postpones listening; honoured only with TestResources, so
	// tests can exercise the start timeout.
	StartupDelay time.Duration
	// Getenv configures the update checker (API base and repository
	// overrides).
	Getenv func(string) string
}

// Run is the daemon process: it acquires the singleton lock, listens on the
// private endpoint, publishes the endpoint record, serves until shutdown,
// and removes what it published. It blocks until the daemon has exited.
func Run(ctx context.Context, opts RunOptions) error {
	p := opts.Paths
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	logf, err := openBoundedLog(p.LogPath())
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	logger := newLogger(logf, "")

	if opts.Instance == "" {
		opts.Instance = newID()
	}
	exe, err := executablePath()
	if err != nil {
		return err
	}
	self, err := CurrentProcess()
	if err != nil {
		return err
	}

	lock, err := TryLock(p.DaemonLockPath())
	if errors.Is(err, ErrLocked) {
		logger.Printf("start refused: %v", ErrAlreadyRunning)
		return ErrAlreadyRunning
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	if opts.TestResources && opts.StartupDelay > 0 {
		logger.Printf("test hook: delaying startup by %s", opts.StartupDelay)
		select {
		case <-time.After(opts.StartupDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ensureSocketDir(p.Endpoint); err != nil {
		return err
	}
	l, err := ipc.Listen(p.Endpoint)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", p.Endpoint, err)
	}

	build := buildIdentity(opts.Build)
	var updates *updatecheck.Checker
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	updates = updatecheck.New(getenv, p.UpdateCachePath(), "purlview/"+opts.Build.Version, logger)

	shares, err := liveEngine(getenv, logger)
	if err != nil {
		_ = l.Close()
		return err
	}
	srv, err := NewServer(ServerConfig{Shares: shares,
		Paths: p, Instance: opts.Instance, Build: build, Executable: exe,
		IdleExit: opts.IdleExit, TestResources: opts.TestResources, Logger: logger, Updates: updates,
	})
	if err != nil {
		_ = l.Close()
		return err
	}

	ep := &Endpoint{
		Protocol: ipc.ProtocolVersion, Instance: opts.Instance, Process: self, Endpoint: p.Endpoint,
		Executable: exe, Build: build, StartedAt: time.Now().UTC(), TestResources: opts.TestResources,
	}
	if err := WriteEndpoint(p, ep); err != nil {
		_ = l.Close()
		return err
	}
	logger.Printf("daemon %s started: pid %d, %s %s, build %s (%s), console %s, idle exit %s, test resources %v",
		opts.Instance, os.Getpid(), ipc.EndpointKind, p.Endpoint, build.Version, build.Commit, consoleState(), srv.idleExit, opts.TestResources)

	sigCtx, stop := context.WithCancel(ctx)
	defer stop()
	sigs := make(chan os.Signal, 2)
	notifyTermination(sigs)
	defer signal.Stop(sigs)
	go func() {
		select {
		case sig := <-sigs:
			logger.Printf("received %v", sig)
			stop()
		case <-sigCtx.Done():
		}
	}()

	reason := srv.Serve(sigCtx, l)
	_ = RemoveEndpoint(p, opts.Instance)
	logger.Printf("daemon %s exited (%s)", opts.Instance, reason)
	return nil
}

// buildIdentity converts the executable's build info to the wire form.
func buildIdentity(info buildinfo.Info) ipc.BuildIdentity {
	return ipc.BuildIdentity{Version: info.Version, Commit: info.Commit, Modified: info.Modified, BuiltBy: info.BuiltBy}
}

// executablePath returns the running executable with symlinks resolved, so
// the CLI and the daemon agree on the installed file regardless of how each
// was invoked (a Homebrew symlink, a relative path).
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Clean(exe), nil
}
