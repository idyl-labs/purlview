package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/output"
)

// newDaemon builds the hidden `daemon` command group. It is private
// plumbing for the CLI itself, the installers and tests; it is not part of
// the customer command tree, so it is hidden from help and completion.
func newDaemon(info buildinfo.Info, p *output.Printer, getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Control the local Purlview daemon (maintenance use)",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return unknownCommand(cmd, args[0])
		},
	}
	cmd.AddCommand(newDaemonRun(info, getenv), newDaemonStart(info, p, getenv), newDaemonStatus(info, p, getenv), newDaemonStop(info, p, getenv), newDaemonResume(info, getenv), newDaemonTestOp(info, p, getenv))
	return cmd
}

func newController(info buildinfo.Info, p *output.Printer, getenv func(string) string, verbose bool) (*daemon.Controller, error) {
	c, err := daemon.NewController(getenv, info)
	if err != nil {
		return nil, err
	}
	c.Notices = p.Err()
	if verbose {
		c.Diagnostics = p.Err()
	}
	return c, nil
}

// newDaemonRun is the daemon process entry point. The controller starts the
// installed executable with exactly these arguments; running it by hand in
// a terminal is supported for diagnosis (Ctrl-C stops it).
func newDaemonRun(info buildinfo.Info, getenv func(string) string) *cobra.Command {
	var instance string
	var idle time.Duration
	var testRes bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in this process (started automatically by share)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := daemon.Resolve(getenv)
			if err != nil {
				return err
			}
			opts := daemon.RunOptions{Paths: paths, Instance: instance, Build: info, IdleExit: idle, TestResources: testRes, Getenv: getenv}
			if testRes {
				opts.StartupDelay, _ = time.ParseDuration(getenv(daemon.EnvTestStartupDelay))
			}
			err = daemon.Run(cmd.Context(), opts)
			if errors.Is(err, daemon.ErrAlreadyRunning) {
				return &problem{msg: output.Msg(output.Failure, sentence(err.Error())), code: daemon.ExitAlreadyRunning}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", "instance id assigned by the starting command")
	cmd.Flags().DurationVar(&idle, "idle-exit", daemon.DefaultIdleExit, "exit after this long with no clients and no work")
	cmd.Flags().BoolVar(&testRes, "test-resources", false, "enable test-only operations (never set in ordinary use)")
	return cmd
}

func newDaemonStart(info buildinfo.Info, p *output.Printer, getenv func(string) string) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon if it is not running (share does this itself)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl, err := newController(info, p, getenv, true)
			if err != nil {
				return err
			}
			sess, err := ctrl.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = sess.Close() }()
			state := "reused"
			if sess.Started {
				state = "started"
			}
			if sess.Retired != "" {
				state += " (retired " + sess.Retired + ")"
			}
			p.Data(fmt.Sprintf("daemon %s: pid %d, version %s, instance %s, %s %s", state, sess.Hello.PID, sess.Hello.Build.Version, sess.Hello.Instance, ipc.EndpointKind, ctrl.Paths.Endpoint))
			return nil
		},
	}
}

func newDaemonStatus(info buildinfo.Info, p *output.Printer, getenv func(string) string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the daemon is running (exit 1 when it is not)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl, err := newController(info, p, getenv, false)
			if err != nil {
				return err
			}
			r := ctrl.Status(cmd.Context())
			if asJSON {
				enc := json.NewEncoder(p.Out())
				enc.SetIndent("", "  ")
				if err := enc.Encode(r); err != nil {
					return err
				}
			} else {
				printReport(p, r)
			}
			if !r.Running {
				if r.Error != "" {
					return errors.New(r.Error)
				}
				return daemon.ErrNotRunning
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

func printReport(p *output.Printer, r daemon.Report) {
	if r.Maintenance != nil {
		p.Data(fmt.Sprintf("maintenance: %s until %s", r.Maintenance.Reason, r.Maintenance.Until.Local().Format(time.RFC3339)))
	}
	if !r.Running {
		p.Data(fmt.Sprintf("daemon: not running (%s %s)", ipc.EndpointKind, r.Endpoint))
		return
	}
	h := r.Hello
	p.Data(fmt.Sprintf("daemon: running, pid %d, version %s (%s), instance %s", h.PID, h.Build.Version, h.Build.Commit, h.Instance))
	p.Data(fmt.Sprintf("  executable: %s\n  endpoint:   %s (%s)\n  console:    %s", h.Executable, r.Endpoint, ipc.EndpointKind, h.Console))
	if h.TestResources {
		p.Data("  test resources: enabled")
	}
	if r.Update != nil {
		p.Data(fmt.Sprintf("  update:     %s available (%s)", r.Update.Version, r.Update.URL))
	}
	if st := r.Status; st != nil {
		p.Data(fmt.Sprintf("  uptime:     %.0fs, clients: %d", st.UptimeSeconds, st.Clients))
		if st.IdleExitAt != "" {
			p.Data(fmt.Sprintf("  idle exit:  %s", st.IdleExitAt))
		}
		active := 0
		for _, op := range st.Operations {
			if op.State == "active" {
				active++
			}
		}
		p.Data(fmt.Sprintf("  operations: %d active", active))
		for _, op := range st.Operations {
			line := fmt.Sprintf("    %s %s owner=%s %s", op.ID, op.Name, op.Owner, op.State)
			if op.Reason != "" {
				line += " (" + op.Reason + ")"
			}
			p.Data(line)
		}
	}
}

func newDaemonStop(info buildinfo.Info, p *output.Printer, getenv func(string) string) *cobra.Command {
	var timeout time.Duration
	var maintenance, force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon and end every share it owns",
		Long: `Stop the daemon. Every share it owns ends; a later share starts a new daemon.

--maintenance additionally blocks new daemon starts for a few minutes so an
installer can replace the executable; run 'purlview daemon resume' (or wait)
to allow starts again. Stopping a daemon that is not running succeeds.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl, err := newController(info, p, getenv, true)
			if err != nil {
				return err
			}
			res, err := ctrl.Stop(cmd.Context(), daemon.StopOptions{Timeout: timeout, Maintenance: maintenance, Force: force, Reason: stopReason(maintenance)})
			if err != nil {
				return err
			}
			switch {
			case !res.WasRunning:
				p.Data("daemon was not running")
			case res.Forced:
				p.Data(fmt.Sprintf("daemon pid %d was not responding and was killed", res.PID))
			default:
				p.Data(fmt.Sprintf("daemon pid %d (%s) stopped; %d operation(s) ended", res.PID, res.Build, res.Operations))
			}
			if maintenance {
				p.Data(fmt.Sprintf("daemon starts are blocked for up to %s; run 'purlview daemon resume' when the upgrade is done", daemon.DefaultMaintenanceWindow))
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "how long to wait for the daemon to exit")
	cmd.Flags().BoolVar(&maintenance, "maintenance", false, "block daemon starts until 'daemon resume' or expiry")
	cmd.Flags().BoolVar(&force, "force", false, "kill the daemon if it does not respond (identity is verified first)")
	return cmd
}

func stopReason(maintenance bool) string {
	if maintenance {
		return "upgrade"
	}
	return "stop"
}

func newDaemonResume(info buildinfo.Info, getenv func(string) string) *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Allow daemon starts again after 'daemon stop --maintenance'",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl, err := daemon.NewController(getenv, info)
			if err != nil {
				return err
			}
			return ctrl.Resume()
		},
	}
}

// newDaemonTestOp drives a test-only operation through the real process
// boundary: an attached operation ends when this command stops (Ctrl-C or
// loss of the process), a detached one survives it. It requires a daemon
// started with test resources and is used by the lifecycle tests.
func newDaemonTestOp(info buildinfo.Info, p *output.Printer, getenv func(string) string) *cobra.Command {
	var detached bool
	var ttl time.Duration
	var name string
	cmd := &cobra.Command{
		Use:    "test-op",
		Short:  "Hold a test-only operation (lifecycle tests)",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl, err := newController(info, p, getenv, false)
			if err != nil {
				return err
			}
			sess, err := ctrl.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = sess.Close() }()
			ctx := cmd.Context()
			if err := sess.Client.Call(ctx, ipc.OpSubscribe, nil, nil); err != nil {
				return err
			}
			var op ipc.Operation
			if err := sess.Client.Call(ctx, ipc.OpTestStart, ipc.TestStartParams{Name: name, Detached: detached, TTLMs: ttl.Milliseconds()}, &op); err != nil {
				return err
			}
			p.Data(fmt.Sprintf("%s %s", op.ID, op.Owner))
			if detached {
				return nil // ownership transferred; the daemon keeps it
			}
			// Attached: stay until the operation ends or this command is
			// interrupted; then stop it explicitly (best effort) and leave.
			for {
				select {
				case <-ctx.Done():
					sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					_ = sess.Client.Call(sctx, ipc.OpTestStop, ipc.TestStopParams{ID: op.ID}, nil)
					cancel()
					p.Diagnostic(fmt.Sprintf("purlview: operation %s stopped", op.ID))
					return nil
				case ev, okc := <-sess.Client.Events():
					if !okc {
						return errors.New("connection to the daemon was lost")
					}
					if ev.Event == ipc.EventOpEnded {
						var data ipc.OpEndedData
						if json.Unmarshal(ev.Data, &data) == nil && data.Operation.ID == op.ID {
							p.Diagnostic(fmt.Sprintf("purlview: operation %s ended: %s", op.ID, data.Operation.Reason))
							return nil
						}
					}
					if ev.Event == ipc.EventDaemonShutdown {
						p.Diagnostic(fmt.Sprintf("purlview: daemon shut down; operation %s ended", op.ID))
						return nil
					}
				}
			}
		},
	}
	cmd.Flags().BoolVar(&detached, "detached", false, "transfer ownership to the daemon and return")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "end the operation after this long")
	cmd.Flags().StringVar(&name, "name", "test", "operation name")
	return cmd
}

var _ = os.Getenv
