// Package command builds the purlview command tree and runs it.
//
// The package has one execution boundary, Run, which takes explicit streams
// and returns an exit code instead of terminating the process. Tests use the
// same boundary, so the command tree is exercised exactly as the executable
// runs it.
//
// Exit codes:
//
//	0    success (help, version and completion output included)
//	1    the command ran and failed, or its outcome is uncertain
//	2    usage error: unknown command, unknown flag or invalid arguments
//	130  interrupted (Ctrl-C) before the command succeeded
//
// Handlers return data and never print: package output renders every line,
// and a lint rule keeps it that way. Data goes to stdout (the link of share
// and link, tables, the whoami block; help, version and completion too);
// status, prompts, hints and errors go to stderr. An error is one line,
// "✗ problem — remedy"; PURLVIEW_DEBUG=1 adds its cause on a second line, and
// a usage error adds the pointer to the command's help. Help, version,
// completion and usage errors read or write no state and never use the
// network. Product commands act through the boundaries in Deps: the
// production wiring talks to the real local daemon and platform, and reports
// honestly when no platform is configured; the scenario runner injects
// controlled fixtures.
package command

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/output"
)

// Exit codes returned by Run.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

// EnvDebug adds the underlying cause to an error, on a second line.
const EnvDebug = "PURLVIEW_DEBUG"

// Streams are the process streams a command run reads and writes.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Run executes the command tree with args and returns the exit code. It is
// the process entry point for the CLI and the daemon, and the only one that
// defaults to the production platform.
func Run(ctx context.Context, args []string, s Streams, info buildinfo.Info) int {
	return RunWithEnv(ctx, args, s, info, withProductionDefaults(osGetenv))
}

// RunWithEnv is Run with an explicit environment (tests isolate state
// through PURLVIEW_STATE_DIR).
func RunWithEnv(ctx context.Context, args []string, s Streams, info buildinfo.Info, getenv func(string) string) int {
	return RunWithDeps(ctx, args, s, info, DefaultDeps(s, info, getenv))
}

// RunWithDeps executes the command tree against injected boundaries. The
// scenario runner uses it with controlled fixtures; the command handlers
// are the same ones the installed executable runs.
func RunWithDeps(ctx context.Context, args []string, s Streams, info buildinfo.Info, deps Deps) int {
	deps = deps.withDefaults()
	root := NewWithDeps(info, s, deps)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return ExitOK
	}
	if cmd == nil {
		cmd = root
	}
	return report(output.New(s.Out, s.Err, deps.Color), deps, cmd, err)
}

// report prints a failed command's one line, once, and returns its status.
func report(p *output.Printer, d Deps, cmd *cobra.Command, err error) int {
	if errors.Is(err, daemon.ErrUnisolatedTest) {
		// A test binary that reached the real per-user state is stopped in
		// the guard's own words, whatever the command; nothing else is said.
		cause := err
		var pr *problem
		if errors.As(err, &pr) && pr.cause != nil {
			cause = pr.cause
		}
		p.Status(output.Msg(output.Failure, cause.Error()))
		return ExitError
	}
	var pr *problem
	if !errors.As(err, &pr) {
		pr = unexpected(err)
		var usage *usageError
		if errors.As(err, &usage) {
			pr = misuse(sentence(usage.Error())).then(usage.hints...)
		}
	}
	if pr.said {
		return pr.code
	}
	p.Status(pr.msg)
	if pr.cause != nil && d.Getenv(EnvDebug) == "1" {
		p.Cause(pr.cause.Error())
	}
	for _, h := range pr.hints {
		p.Hint(h)
	}
	if pr.usage {
		p.Usage(cmd.CommandPath())
	}
	return pr.code
}

// New builds the root command with all subcommands attached to s, reading
// the process environment. Building the tree touches no state.
func New(info buildinfo.Info, s Streams) *cobra.Command {
	return NewWithEnv(info, s, osGetenv)
}

// NewWithEnv builds the root command with an explicit environment lookup.
func NewWithEnv(info buildinfo.Info, s Streams, getenv func(string) string) *cobra.Command {
	return NewWithDeps(info, s, DefaultDeps(s, info, getenv))
}

// Commands are listed in reading order, not alphabetically. Cobra keeps
// this in a package variable, so it is set once, before any goroutine.
func init() { cobra.EnableCommandSorting = false }

// Command groups of the root help.
const (
	groupShare   = "share"
	groupAccount = "account"
	groupSystem  = "system"
)

// NewWithDeps builds the root command against injected boundaries.
func NewWithDeps(info buildinfo.Info, s Streams, deps Deps) *cobra.Command {
	deps = deps.withDefaults()
	p := output.New(s.Out, s.Err, deps.Color)
	root := &cobra.Command{
		Use:   "purlview",
		Short: "Share your running app. Improve it together.",
		Long:  "Share your running app. Improve it together.",
		Example: `  purlview share 3000
  purlview share 5173 8000
  purlview share 3000 --to ana@example.com`,
		Version:       info.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return unknownCommand(cmd, args[0])
		},
	}
	root.SetIn(s.In)
	root.SetOut(s.Out)
	root.SetErr(s.Err)
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: err}
	})
	root.AddGroup(
		&cobra.Group{ID: groupShare, Title: "Share"},
		&cobra.Group{ID: groupAccount, Title: "Account"},
		&cobra.Group{ID: groupSystem, Title: "System"},
	)
	root.AddCommand(
		inGroup(groupShare, newShare(info, p, deps)),
		inGroup(groupShare, newList(p, deps)),
		inGroup(groupShare, newLink(p, deps)),
		inGroup(groupShare, newStop(p, deps, "stop", false)),
		newStop(p, deps, "unshare", true),
		inGroup(groupAccount, newLogin(p, deps)),
		inGroup(groupAccount, newLogout(p, deps)),
		inGroup(groupAccount, newWhoami(p, deps)),
		inGroup(groupAccount, newDevices(p, deps)),
		newDaemon(info, p, deps.Getenv),
	)
	root.SetHelpCommandGroupID(groupSystem)
	root.SetCompletionCommandGroupID(groupSystem)

	// Cobra adds "completion" lazily, and its parent command is not runnable,
	// so an unknown shell would print help and exit 0. Add it now and make it
	// report a usage error instead.
	root.InitDefaultCompletionCmd()
	for _, c := range root.Commands() {
		if c.Name() != "completion" {
			continue
		}
		c.Args = cobra.ArbitraryArgs
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return unknownCommand(cmd, args[0])
		}
	}
	root.InitDefaultHelpCmd()
	root.Flags().BoolP("version", "v", false, "Show the version.")
	punctuate(root)
	return root
}

func inGroup(id string, c *cobra.Command) *cobra.Command {
	c.GroupID = id
	return c
}

// punctuate gives every command its help flag and ends every flag
// description with a period, Cobra's own included.
func punctuate(c *cobra.Command) {
	c.Flags().BoolP("help", "h", false, "Show help for "+c.Name()+".")
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if !strings.HasSuffix(f.Usage, ".") {
			f.Usage = sentence(f.Usage) + "."
		}
	})
	for _, sub := range c.Commands() {
		punctuate(sub)
	}
}

// usageError marks an error caused by how the command was invoked.
type usageError struct {
	err   error
	hints []string
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// unknownCommand names the unknown subcommand and offers the near ones as
// hints, as a usage error with a nonzero exit code.
func unknownCommand(parent *cobra.Command, name string) error {
	pr := misuse(name + " isn't a " + parent.CommandPath() + " command")
	for _, s := range parent.SuggestionsFor(name) {
		pr.hints = append(pr.hints, parent.CommandPath()+" "+s)
	}
	return pr
}

// usageArgs wraps a Cobra positional-argument validator so that a failure
// is reported as a usage error. An argument to a command that takes none
// is an unknown subcommand, in the tree's own words.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			if len(args) > 0 && !cmd.HasAvailableSubCommands() && cobra.NoArgs(cmd, args) != nil {
				return unknownCommand(cmd, args[0])
			}
			return &usageError{err: err}
		}
		return nil
	}
}

// oneArg requires exactly one argument and says what it is otherwise.
func oneArg(missing, extra *problem) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return missing
		case len(args) > 1:
			return extra
		}
		return nil
	}
}
