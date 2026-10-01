package scenario

import (
	"strings"
	"time"

	"github.com/idyl-labs/purlview/internal/updatecheck"
)

// updateNotice is the notice for this copy, which came from Homebrew.
const updateNotice = "A newer Purlview is available: 0.9.1 (you have 0.9.0)\nUpdate: brew upgrade --cask purlview — updating stops running shares\n"

func init() {
	register(
		Scenario{
			Name:  "updates/notice-during-share",
			Title: "A newer release is announced on stderr below the share block, with the command that updates this copy; the link is not delayed.",
			Setup: "signed in at a terminal; this copy came from Homebrew; release metadata answers after one second with a newer version.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Updates.Available("0.9.1", time.Second)
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				c.Seen(w.AdvanceUntil(inv, "A newer Purlview", 500*time.Millisecond, 3*time.Second), "notice")
				inv.Interrupt()
				r := inv.Wait()
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.Stderr(r, readyLine+"\n\n"+updateNotice)
			},
		},
		Scenario{
			Name:  "updates/slow-metadata-does-not-delay",
			Title: "Slow release metadata delays neither readiness nor completion; the answer lands in the cache for later.",
			Setup: "signed in; release metadata takes ten seconds to answer.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Updates.Available("0.9.1", 10*time.Second)
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.NotStderr(r, "newer version")
				c.True(w.Clock.Now().Equal(Epoch), "the command must finish without any wait on the clock")
			},
		},
		Scenario{
			Name:  "updates/cached-notice-on-next-share",
			Title: "A fresh answer that arrived after a share finished is announced by the next share, after its share block.",
			Setup: "signed in at a terminal; metadata answers after ten seconds; the second share runs later.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Updates.Available("0.9.1", 10*time.Second)
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				c.NotStderr(r, "newer")
				w.Advance(time.Minute)
				c.Seen(w.Updates.WaitIdle(), "the daemon's fetch lands in its cache")
				r = w.Exec("share", "localhost:8080", "--background")
				c.Exit(r, 0)
				c.Stderr(r, "Stop     purlview stop "+label2+"\n\n"+updateNotice)
				c.True(strings.Index(r.Stderr, "A newer") > strings.Index(r.Stderr, "Stop     "), "the notice never precedes the share block")
			},
		},
		Scenario{
			Name:  "updates/notice-once-a-day-and-only-at-a-terminal",
			Title: "One version is announced at most once a day, and never when output is redirected.",
			Setup: "signed in; output is redirected for the first two shares; release metadata answers at once; this copy was installed by a script.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Advice = updatecheck.Advice{Method: "installer-script", Command: "curl -fsSLo install.sh https://purlview.com/install.sh && sh install.sh"}
				w.Updates.Available("0.9.1", 0)
				w.Interactive = false
				c.Exit(w.Exec("share", target, "--background"), 0)
				c.Seen(w.Updates.WaitIdle(), "the daemon's fetch lands in its cache")
				r := w.Exec("share", "localhost:8080", "--background")
				c.Exit(r, 0)
				c.NotStderr(r, "newer")
				w.Interactive = true
				r = w.Exec("share", "localhost:8081", "--background")
				c.Exit(r, 0)
				c.Stderr(r, "A newer Purlview is available: 0.9.1 (you have 0.9.0)\nUpdate: curl -fsSLo install.sh https://purlview.com/install.sh && sh install.sh — updating stops running shares\n")
				r = w.Exec("share", "localhost:8082", "--background")
				c.Exit(r, 0)
				c.NotStderr(r, "newer")
				w.Advance(24 * time.Hour)
				r = w.Exec("share", "localhost:8083", "--background")
				c.Exit(r, 0)
				c.Stderr(r, "A newer Purlview is available: 0.9.1 (you have 0.9.0)")
			},
		},
		Scenario{
			Name:  "updates/unavailable-metadata-is-silent",
			Title: "Unavailable release metadata is silent and changes nothing about the share.",
			Setup: "signed in; the metadata source fails.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Updates.Unavailable(0)
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.NotStderr(r, "newer")
			},
		},
	)

	register(
		Scenario{
			Name:  "output/stream-separation-when-redirected",
			Title: "With output redirected, stdout carries only returned data, the link and the table; every status line goes to stderr, without colour.",
			Setup: "signed in; not an interactive terminal.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Interactive = false
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				r = w.Exec("list")
				c.Exit(r, 0)
				c.Stdout(r, "  ID        APP")
				c.True(r.Stderr == "", "a table has no status: %q", r.Stderr)
				r = w.Exec("stop", label1)
				c.NoStdout(r)
				c.Stderr(r, "✓ Stopped "+label1)
				r = w.Exec("list")
				c.NoStdout(r)
				c.True(r.Stderr == "No active shares\n→ purlview share 3000\n", "the empty state: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "output/exit-status-classes",
			Title: "Exit status: 0 success, 2 invalid usage, 130 interrupted before success, 1 operational failure.",
			Setup: "signed in; the platform becomes unreachable for the last command.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				c.Exit(w.Exec("whoami"), 0)
				r := w.Exec("share")
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ share needs a port or an address — try purlview share 3000\nRun 'purlview share --help' for usage.\n", "two lines: %q", r.Stderr)
				w.Platform.HoldReady(true)
				inv := w.Run("share", target)
				c.Seen(w.Platform.WaitConnects(1), "the share is starting")
				inv.Interrupt()
				r = inv.Wait()
				c.Exit(r, 130)
				w.Platform.Unreachable()
				r = w.Exec("list")
				c.Exit(r, 1)
			},
		},
	)
}
