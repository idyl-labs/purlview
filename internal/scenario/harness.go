package scenario

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/command"
)

// Real-time bounds for the harness itself (the product's own waits run on
// the fake clock).
const (
	outputWait = 10 * time.Second
	exitWait   = 20 * time.Second
)

type entryKind int

const (
	kindCommand entryKind = iota
	kindStdout
	kindStderr
	kindNote
	kindClock
	kindExit
)

type entry struct {
	kind entryKind
	text string
	// d is the advance for kindClock entries; adjacent ones merge when the
	// transcript is rendered.
	d time.Duration
	// inv is the command an output or exit line belongs to. Two commands
	// can run at once (a foreground share and the unshare that ends it);
	// the renderer keeps each command's lines together so the transcript
	// does not depend on goroutine interleaving.
	inv *Invocation
}

// recorder keeps the ordered transcript of one scenario.
type recorder struct {
	mu      sync.Mutex
	entries []entry
}

func (r *recorder) add(e entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

func (r *recorder) snapshot() []entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]entry(nil), r.entries...)
}

// streamWriter records one stream of one invocation.
type streamWriter struct {
	inv  *Invocation
	kind entryKind
	buf  *strings.Builder
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.inv.mu.Lock()
	defer w.inv.mu.Unlock()
	w.buf.Write(p)
	w.inv.all.Write(p)
	if !w.inv.abandoned {
		w.inv.w.rec.add(entry{kind: w.kind, text: string(p), inv: w.inv})
	}
	return len(p), nil
}

type invocationKey struct{}

// Invocation is one running or finished command.
type Invocation struct {
	w      *World
	args   []string
	cancel context.CancelFunc
	done   chan struct{}
	code   int

	mu        sync.Mutex
	stdout    strings.Builder
	stderr    strings.Builder
	all       strings.Builder
	abandoned bool
}

// Result is a finished command.
type Result struct {
	Args   []string
	Code   int
	Stdout string
	Stderr string
}

// Run starts a command asynchronously through the real command handlers.
func (w *World) Run(args ...string) *Invocation {
	inv := &Invocation{w: w, args: args, done: make(chan struct{})}
	w.rec.add(entry{kind: kindCommand, text: "purlview " + strings.Join(args, " "), inv: inv})
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), invocationKey{}, inv))
	inv.cancel = cancel
	streams := command.Streams{
		In:  strings.NewReader(""),
		Out: &streamWriter{inv: inv, kind: kindStdout, buf: &inv.stdout},
		Err: &streamWriter{inv: inv, kind: kindStderr, buf: &inv.stderr},
	}
	go func() {
		defer close(inv.done)
		inv.code = command.RunWithDeps(ctx, args, streams, w.Info, w.Deps())
		cancel()
		inv.mu.Lock()
		if !inv.abandoned {
			w.rec.add(entry{kind: kindExit, text: fmt.Sprintf("exit %d", inv.code), inv: inv})
		}
		inv.mu.Unlock()
	}()
	return inv
}

// Exec runs a command to completion.
func (w *World) Exec(args ...string) Result {
	return w.Run(args...).Wait()
}

// Output returns everything the command has written so far.
func (inv *Invocation) Output() string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.all.String()
}

// WaitFor blocks until the command's output contains text or the
// real-time bound passes; it reports whether the text appeared.
func (inv *Invocation) WaitFor(text string) bool {
	deadline := time.Now().Add(outputWait)
	for {
		if strings.Contains(inv.Output(), text) {
			return true
		}
		select {
		case <-inv.done:
			return strings.Contains(inv.Output(), text)
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// WaitForCount blocks until the command's output contains text n times or
// the real-time bound passes; it reports whether it did.
func (inv *Invocation) WaitForCount(text string, n int) bool {
	deadline := time.Now().Add(outputWait)
	for {
		if strings.Count(inv.Output(), text) >= n {
			return true
		}
		select {
		case <-inv.done:
			return strings.Count(inv.Output(), text) >= n
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Interrupt sends the command Ctrl-C.
func (inv *Invocation) Interrupt() {
	inv.w.Note("Ctrl-C")
	inv.cancel()
}

// Wait blocks until the command finishes (bounded) and returns its result.
func (inv *Invocation) Wait() Result {
	select {
	case <-inv.done:
	case <-time.After(exitWait):
		inv.w.Note("HARNESS: command did not finish within %s; cancelling", exitWait)
		inv.cancel()
		<-inv.done
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return Result{Args: inv.args, Code: inv.code, Stdout: inv.stdout.String(), Stderr: inv.stderr.String()}
}

// Done reports whether the command has finished.
func (inv *Invocation) Done() bool {
	select {
	case <-inv.done:
		return true
	default:
		return false
	}
}

// KillCLI models the CLI process dying: its daemon connections close and
// nothing it prints afterwards is part of the transcript.
func (w *World) KillCLI(inv *Invocation) {
	w.Note("the CLI process running this command is killed (its daemon connection closes)")
	inv.mu.Lock()
	inv.abandoned = true
	inv.mu.Unlock()
	w.Daemon.killCLI(inv)
	inv.cancel()
	<-inv.done
}

// AdvanceUntil moves the fake clock in steps until the command has printed
// text (or a bound is reached). One note records the advance before it
// happens, without the exact total: a goroutine may arm its timer after
// the first step on a slow machine, so the total is timing-dependent while
// the command's output is not.
func (w *World) AdvanceUntil(inv *Invocation, text string, step, limit time.Duration) bool {
	w.rec.add(entry{kind: kindNote, text: fmt.Sprintf("clock advances (%s steps) until: %s", formatAdvance(step), text)})
	settle()
	var total time.Duration
	for total < limit {
		if strings.Contains(inv.Output(), text) {
			break
		}
		w.Clock.Advance(step)
		w.Platform.expireShares()
		total += step
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) && !strings.Contains(inv.Output(), text) && !inv.Done() {
			time.Sleep(2 * time.Millisecond)
		}
	}
	return strings.Contains(inv.Output(), text)
}

// AdvanceUntilDone moves the fake clock in steps until the command exits.
func (w *World) AdvanceUntilDone(inv *Invocation, step, limit time.Duration) bool {
	w.rec.add(entry{kind: kindNote, text: fmt.Sprintf("clock advances (%s steps) until the command exits", formatAdvance(step))})
	settle()
	var total time.Duration
	for total < limit && !inv.Done() {
		w.Clock.Advance(step)
		w.Platform.expireShares()
		total += step
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) && !inv.Done() {
			time.Sleep(2 * time.Millisecond)
		}
	}
	return inv.Done()
}

// settle gives goroutines that react to the last output a moment to arm
// their fake-clock timers before the clock moves.
func settle() { time.Sleep(10 * time.Millisecond) }

// Check collects assertion failures so scenarios run identically under go
// test and under the development runner.
type Check struct {
	Failures []string
}

// Failf records a failure.
func (c *Check) Failf(format string, args ...any) {
	c.Failures = append(c.Failures, fmt.Sprintf(format, args...))
}

// Exit asserts the exit status.
func (c *Check) Exit(r Result, want int) {
	if r.Code != want {
		c.Failf("%s: exit %d, want %d\nstdout: %sstderr: %s", strings.Join(r.Args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
}

// Stdout asserts that stdout contains text.
func (c *Check) Stdout(r Result, text string) {
	if !strings.Contains(r.Stdout, text) {
		c.Failf("%s: stdout lacks %q:\n%s", strings.Join(r.Args, " "), text, r.Stdout)
	}
}

// Stderr asserts that stderr contains text.
func (c *Check) Stderr(r Result, text string) {
	if !strings.Contains(r.Stderr, text) {
		c.Failf("%s: stderr lacks %q:\n%s", strings.Join(r.Args, " "), text, r.Stderr)
	}
}

// NoStdout asserts that nothing was written to stdout.
func (c *Check) NoStdout(r Result) {
	if r.Stdout != "" {
		c.Failf("%s: stdout must be empty, got:\n%s", strings.Join(r.Args, " "), r.Stdout)
	}
}

// NotStderr asserts that stderr does not contain text.
func (c *Check) NotStderr(r Result, text string) {
	if strings.Contains(r.Stderr, text) {
		c.Failf("%s: stderr must not contain %q:\n%s", strings.Join(r.Args, " "), text, r.Stderr)
	}
}

// StdoutExactly asserts stdout is exactly text.
func (c *Check) StdoutExactly(r Result, text string) {
	if r.Stdout != text {
		c.Failf("%s: stdout %q, want %q", strings.Join(r.Args, " "), r.Stdout, text)
	}
}

// True asserts a condition.
func (c *Check) True(cond bool, format string, args ...any) {
	if !cond {
		c.Failf(format, args...)
	}
}

// Seen asserts that WaitFor found its text.
func (c *Check) Seen(ok bool, text string) {
	if !ok {
		c.Failf("did not see %q in time", text)
	}
}

// WaitDaemonActive blocks (bounded, real time) until the daemon serves
// exactly n shares. Ends that the platform initiates reach the daemon
// asynchronously, so assertions on the daemon's view wait for them.
func (w *World) WaitDaemonActive(n int) bool {
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		if w.Daemon.Active() == n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return w.Daemon.Active() == n
}

// WaitShareEnded blocks (bounded, real time) until the platform records
// the share as ended, for ends the daemon reports asynchronously.
func (w *World) WaitShareEnded(id string) bool {
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		if _, ended, _, _ := w.Platform.ShareState(id); ended {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, ended, _, _ := w.Platform.ShareState(id)
	return ended
}
