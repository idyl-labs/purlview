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

package scenario

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/sdk/api"
)

const (
	target    = "localhost:3000"
	targetURL = "http://localhost:3000"
	label1    = "k7m2p4qx"
	label2    = "p4q9x2bd"
	origin1   = "https://" + label1 + "." + ShareDomain
	origin2   = "https://" + label2 + "." + ShareDomain
	entry1    = "https://" + label1 + "." + EntryDomain
	entry2    = "https://" + label2 + "." + EntryDomain
	url1      = entry1 + accessPath + "?token=synthetic-secret-01"
	url2      = entry2 + accessPath + "?token=synthetic-secret-02"
	id1       = "shr_" + label1
	id2       = "shr_" + label2

	// readyLine ends a foreground share block; the clock starts at noon.
	readyLine  = "Stop     Ctrl-C"
	sharing    = "✓ Sharing " + target
	anyone     = "Access   Anyone with this link"
	expires1h  = "Expires  in 1h · 1:00 PM"
	stopped    = "✓ Stopped — the link no longer works"
	expired    = "1:00 PM  Share expired"
	notSigned  = "✗ Not signed in — run purlview login first"
	noPurlview = "✗ Couldn't reach Purlview — check your connection and try again"
)

func init() {
	register(
		Scenario{
			Name:  "first-use/interactive-share-signs-in",
			Title: "An interactive first share signs the user in with an email code, then shares.",
			Setup: "signed out; interactive terminal; an email code is verified.",
			Run: func(c *Check, w *World) {
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Advance(time.Hour)
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "\nSign in to Purlview\n\n  Email: ")
				c.Stderr(r, "  Code sent. Check your inbox.\n  Code: \n\n✓ Signed in as "+AccountEmail+"\n\n"+sharing+"\n")
				c.StdoutExactly(r, url1+"\n")
				c.Stderr(r, anyone+"\n"+expires1h+"\n"+readyLine+"\n")
				c.Stderr(r, expired)
				c.NotStderr(r, w.Platform.LoginCode())
				c.True(len(w.Browser.Opened) == 0, "email login must not open a browser, opened %v", w.Browser.Opened)
				cred, _ := w.Store.Load()
				c.True(cred != nil && cred.Account == AccountEmail, "the credential must be remembered")
			},
		},
		Scenario{
			Name:  "first-use/existing-identity-skips-sign-in",
			Title: "A signed-in installation shares without any sign-in step.",
			Setup: "signed in as the creator on this device.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.NotStderr(r, "Sign in")
				c.NotStderr(r, "browser")
				c.True(len(w.Browser.Opened) == 0, "no browser must open")
				r = w.Exec("whoami")
				c.Exit(r, 0)
				c.Stdout(r, "Account  "+AccountEmail)
			},
		},
		Scenario{
			Name:  "first-use/noninteractive-unauthenticated",
			Title: "Without a terminal, a share that is not signed in says how to sign in and asks nothing.",
			Setup: "signed out; standard input and error are not terminals (a script or a pipe).",
			Run: func(c *Check, w *World) {
				w.Interactive = false
				r := w.Exec("share", target, "--background")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == notSigned+"\n", "one line and no prompt: %q", r.Stderr)
				c.True(len(w.Browser.Opened) == 0, "no browser must open")
				creates, _ := w.Platform.Calls()
				c.True(creates == 0, "no share may be created")
				r = w.Exec("list")
				c.Exit(r, 1)
				c.Stderr(r, notSigned)
			},
		},
	)

	register(
		Scenario{
			Name:  "foreground/ready-url-and-expiry",
			Title: "A foreground share prints its block once the link works, warns five minutes before the end, and finishes normally at expiry.",
			Setup: "signed in; localhost:3000 answers.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Advance(55 * time.Minute)
				c.Seen(inv.WaitFor("Expires in 5 minutes"), "the five-minute warning")
				w.Advance(5 * time.Minute)
				r := inv.Wait()
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.True(r.Stderr == sharing+"\n\n\n"+anyone+"\n"+expires1h+"\n"+readyLine+"\n\n12:55 PM  ! Expires in 5 minutes\n"+expired+"\n", "the whole of stderr: %q", r.Stderr)
				c.True(w.Daemon.Active() == 0, "the daemon must own nothing after expiry")
			},
		},
		Scenario{
			Name:  "foreground/visitor-and-app-events",
			Title: "The first visitor, and an app that stops and resumes answering, each append one line with the local time.",
			Setup: "signed in; a foreground share is ready; a visitor opens the app, which stops answering and recovers.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				before := inv.Output()
				w.Advance(44 * time.Minute)
				w.Note("a visitor opens the app; the app stops answering, then answers again")
				w.Platform.Observe(id1, engine.ConnFirstVisitor, engine.ConnAppUnresponsive, engine.ConnAppResponding)
				c.True(w.Daemon.WaitShareEvents(share.EventFirstVisitor, share.EventAppUnresponsive, share.EventAppResponding), "the daemon must report the three events to its clients")
				c.Seen(inv.WaitFor("is responding again"), "the three lines")
				inv.Interrupt()
				r := inv.Wait()
				c.Exit(r, 0)
				c.True(inv.Output() == before+"\n12:44 PM  ● First visitor opened the app\n12:44 PM  ! "+target+" stopped responding — visitors see a waiting page\n12:44 PM  ✓ "+target+" is responding again\n12:44 PM  "+stopped+"\n", "one appended line per event: %q", inv.Output())
			},
		},
		Scenario{
			Name:  "foreground/ctrl-c-stops-and-confirms",
			Title: "Ctrl-C ends the foreground share; success is reported only with the platform's confirmation.",
			Setup: "signed in; the platform confirms the revocation.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				inv.Interrupt()
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "12:00 PM  "+stopped)
				_, ended, reason, _ := w.Platform.ShareState(id1)
				c.True(ended && reason == "revoked", "the platform must record the revocation, got ended=%v reason=%q", ended, reason)
			},
		},
		Scenario{
			Name:  "foreground/ctrl-c-while-offline-is-unconfirmed",
			Title: "Ctrl-C while Purlview is unreachable stops here and says by when the link stops working (exit 1).",
			Setup: "signed in; the platform becomes unreachable after the share is ready.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Note("platform unreachable")
				w.Platform.Unreachable()
				inv.Interrupt()
				r := inv.Wait()
				c.Exit(r, 1)
				c.Stderr(r, "12:00 PM  ! Stopped here, but Purlview couldn't confirm — the link stops working by 1:00 PM")
				c.NotStderr(r, "connection refused")
				_, ended, _, conns := w.Platform.ShareState(id1)
				c.True(!ended && conns == 0, "the record stands but nothing serves it, got ended=%v conns=%d", ended, conns)
			},
		},
		Scenario{
			Name:  "foreground/cli-killed-ends-only-its-share",
			Title: "Losing the foreground CLI ends that share alone; a background share continues.",
			Setup: "signed in; one background share, then a foreground share whose CLI process dies.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				bg := w.Exec("share", "https://internal.example/demo", "--background")
				c.Exit(bg, 0)
				fg := w.Run("share", target)
				c.Seen(fg.WaitFor(readyLine), "ready")
				w.KillCLI(fg)
				c.True(w.WaitShareEnded(id2), "the daemon must revoke the attached share once its CLI is gone")
				r := w.Exec("list")
				c.Exit(r, 0)
				c.Stdout(r, label1)
				c.True(!containsLine(r.Stdout, label2), "the killed CLI's share must be gone:\n%s", r.Stdout)
				_, _, reason, _ := w.Platform.ShareState(id2)
				c.True(reason == "revoked", "the attached share must end by revocation, got %q", reason)
				c.True(w.WaitDaemonActive(1), "the background share must continue")
			},
		},
	)

	register(
		Scenario{
			Name:  "background/returns-after-ready-and-is-manageable",
			Title: "A background share returns after the daemon owns it and the route is ready; later commands see and end it.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.True(r.Stderr == sharing+"\n\n\n"+anyone+"\n"+expires1h+"\nStop     purlview stop "+label1+"\n", "the whole of stderr: %q", r.Stderr)
				c.True(w.Daemon.Active() == 1, "the daemon must own the share")
				r = w.Exec("list")
				c.Exit(r, 0)
				c.Stdout(r, "● "+label1+"  "+target+"  Anyone with the link  this device  Sharing  in 1h")
				r = w.Exec("link", label1)
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.True(r.Stderr == "", "link prints the link and nothing else: %q", r.Stderr)
				r = w.Exec("stop", label1)
				c.Exit(r, 0)
				c.NoStdout(r)
				c.Stderr(r, "✓ Stopped "+label1+" — the link no longer works")
				r = w.Exec("list")
				c.Exit(r, 0)
				c.NoStdout(r)
				c.True(r.Stderr == "No active shares\n→ purlview share 3000\n", "the empty state: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "background/lost-start-reply-is-reconciled",
			Title: "A lost start reply is retried with the same attempt id, so the daemon answers with the share it already owns.",
			Setup: "signed in; the daemon receives the start but its reply is lost.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Note("the daemon will receive the next start but the reply is lost")
				w.Daemon.LoseNextStartReply()
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				creates, _ := w.Platform.Calls()
				c.True(creates == 1, "exactly one platform create, got %d", creates)
				c.True(w.Daemon.Active() == 1, "exactly one share, got %d", w.Daemon.Active())
				r = w.Exec("list")
				c.Stdout(r, label1)
				c.True(!containsLine(r.Stdout, label2), "no second share may exist:\n%s", r.Stdout)
			},
		},
		Scenario{
			Name:  "background/two-shares-same-target",
			Title: "Two intentional share commands for the same target create two shares; nothing is deduplicated by target.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				a := w.Exec("share", target, "--background")
				b := w.Exec("share", target, "--background")
				c.Exit(a, 0)
				c.Exit(b, 0)
				c.StdoutExactly(a, url1+"\n")
				c.StdoutExactly(b, url2+"\n")
				r := w.Exec("list")
				c.Stdout(r, label1)
				c.Stdout(r, label2)
			},
		},
	)

	register(
		Scenario{
			Name:  "options/short-ttl",
			Title: "--ttl requests a shorter lifetime; the share expires when it elapses.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target, "--ttl", "15m")
				c.Seen(inv.WaitFor(readyLine), "ready")
				c.Seen(w.AdvanceUntil(inv, "Expires in 5 minutes", time.Minute, 15*time.Minute), "the warning")
				w.Advance(5 * time.Minute)
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "Expires  in 15m · 12:15 PM")
				c.Stderr(r, "12:10 PM  ! Expires in 5 minutes")
				c.Stderr(r, "12:15 PM  Share expired")
			},
		},
		Scenario{
			Name:  "options/invalid-ttl",
			Title: "An excessive or malformed --ttl is a usage error before anything starts.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", target, "--ttl", "2h")
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ --ttl 2h is over the 1h limit — use 1h or less\nRun 'purlview share --help' for usage.\n", "two lines: %q", r.Stderr)
				r = w.Exec("share", target, "--ttl", "abc")
				c.Exit(r, 2)
				c.Stderr(r, "✗ --ttl abc isn't a duration — use something like 15m")
				r = w.Exec("share", target, "--ttl", "0")
				c.Exit(r, 2)
				c.Stderr(r, "✗ --ttl 0 is too short — use 1s or more")
				c.True(w.Daemon.Starts() == 0, "no daemon may start for a usage error")
			},
		},
		Scenario{
			Name:  "options/invalid-target",
			Title: "What cannot be shared is a usage error that says what to type instead.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", "70000")
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ 70000 isn't a valid port — ports run from 1 to 65535\nRun 'purlview share --help' for usage.\n", "two lines: %q", r.Stderr)
				r = w.Exec("share", "localhost")
				c.Exit(r, 2)
				c.Stderr(r, "✗ localhost needs a port — try purlview share localhost:3000")
				r = w.Exec("share", "ftp://files.example:21")
				c.Exit(r, 2)
				c.Stderr(r, "✗ ftp addresses can't be shared — use http or https")
				r = w.Exec("share", "http://admin:secret@localhost:3000")
				c.Exit(r, 2)
				c.Stderr(r, "✗ An address with a username or password can't be shared")
				c.NotStderr(r, "secret")
				r = w.Exec("share", "http://localhost:3000/app#top")
				c.Exit(r, 2)
				c.Stderr(r, "✗ The part after # never reaches an app — remove it")
				r = w.Exec("share")
				c.Exit(r, 2)
				c.Stderr(r, "✗ share needs a port or an address — try purlview share 3000")
				c.True(w.Daemon.Starts() == 0, "no daemon may start for a usage error")
			},
		},
		Scenario{
			Name:  "options/several-apps",
			Title: "Several apps share one link: the block names each further app, list counts them, and an app that stops answering is named.",
			Setup: "signed in; localhost:5173 and localhost:8000 answer; the second stops answering and recovers.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", "5173", "8000", "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.Stderr(r, "✓ Sharing localhost:5173\n  + localhost:8000\n\n")
				c.NotStderr(r, "Opens")
				served, rewrite := w.Platform.Served(id1)
				c.True(rewrite && len(served) == 2 && served[1] == "http://localhost:8000", "the daemon must serve both apps and rewrite: %v %v", served, rewrite)
				r = w.Exec("list")
				c.Exit(r, 0)
				c.Stdout(r, label1+"  localhost:5173 +1  Anyone with the link")
				inv := w.Run("share", "3000", "8000/api?x=1")
				c.Seen(inv.WaitFor(readyLine), "ready")
				c.Seen(inv.WaitFor("+ localhost:8000"), "the further app, its path ignored")
				w.Advance(10 * time.Minute)
				w.Note("localhost:8000 stops answering, then answers again")
				w.Platform.ObserveApp(id2, "localhost:8000", engine.ConnAppUnresponsive, engine.ConnAppResponding)
				c.Seen(inv.WaitFor("localhost:8000 is responding again"), "the two lines")
				inv.Interrupt()
				r = inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "12:10 PM  ! localhost:8000 stopped responding — visitors see a waiting page\n12:10 PM  ✓ localhost:8000 is responding again\n")
			},
		},
		Scenario{
			Name:  "options/several-apps-refused",
			Title: "Too many apps, or one app named twice under any of its names, is a usage error before anything starts.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				args := []string{"share"}
				for port := 5000; port < 5011; port++ {
					args = append(args, strconv.Itoa(port))
				}
				r := w.Exec(args...)
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ share names 11 apps — a share takes 10 at most\nRun 'purlview share --help' for usage.\n", "two lines: %q", r.Stderr)
				r = w.Exec("share", "5173", "127.0.0.1:5173")
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ localhost:5173 is named twice — list each app once\nRun 'purlview share --help' for usage.\n", "two lines: %q", r.Stderr)
				r = w.Exec("share", "5173", "8000", "[::1]:8000")
				c.Exit(r, 2)
				c.Stderr(r, "✗ localhost:8000 is named twice")
				c.True(w.Daemon.Starts() == 0, "no daemon may start for a usage error")
			},
		},
		Scenario{
			Name:  "options/no-rewrite",
			Title: "--no-rewrite leaves the apps' addresses in what they send unchanged; everything else is the same share.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", "5173", "8000", "--no-rewrite", "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.Stderr(r, "✓ Sharing localhost:5173\n  + localhost:8000\n")
				served, rewrite := w.Platform.Served(id1)
				c.True(!rewrite && len(served) == 2, "the daemon must serve both apps without rewriting: %v %v", served, rewrite)
			},
		},
		Scenario{
			Name:  "startup/unreachable-further-app",
			Title: "A further app that does not answer is named, and no share exists.",
			Setup: "signed in; localhost:5173 answers, localhost:8000 refuses connections.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Prober.Unreachable("http://localhost:8000", "connection refused")
				r := w.Exec("share", "5173", "8000")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ Nothing is answering at localhost:8000 — start your app, then share again\n", "one line: %q", r.Stderr)
				creates, _ := w.Platform.Calls()
				c.True(creates == 0, "no platform create may happen, got %d", creates)
			},
		},
		Scenario{
			Name:  "options/recipient-restriction",
			Title: "--to lets in one email address, says so, and reports the invite Purlview sent.",
			Setup: "signed in; the platform sends invites.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.SendInvites()
				r := w.Exec("share", target, "--to", "Reviewer@Example.Invalid", "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, entry1+accessPath+"\n")
				c.Stderr(r, sharing+"\n✓ Invite sent to reviewer@example.invalid\n\n")
				c.Stderr(r, "Access   Only reviewer@example.invalid\n")
				c.NotStderr(r, "Anyone with")
				r = w.Exec("share", target, "--to", "a@example.invalid, b@example.invalid")
				c.Exit(r, 2)
				c.Stderr(r, "✗ a@example.invalid, b@example.invalid isn't one email address — repeat --to for each")
				r = w.Exec("share", target, "--to", "reviewer")
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ reviewer isn't an email address\nRun 'purlview share --help' for usage.\n", "two lines: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "options/several-recipients",
			Title: "--to may be repeated; any listed address can get in, a repeated address counts once, and an invite that was not sent is named.",
			Setup: "signed in; the platform sends invites, but not to raj.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.SendInvites("raj@example.invalid")
				r := w.Exec("share", target, "--to", "ana@example.invalid", "--to", "Raj@Example.Invalid", "--to", "ANA@example.invalid", "--to", "kim@example.invalid", "--background")
				c.Exit(r, 0)
				c.StdoutExactly(r, entry1+accessPath+"\n")
				c.Stderr(r, sharing+"\n✓ Invites sent to ana@example.invalid and kim@example.invalid\n! Invite not sent to raj@example.invalid — send them the link yourself\n\n")
				c.Stderr(r, "Access   Only ana@example.invalid, raj@example.invalid and kim@example.invalid\n")
				c.NotStderr(r, "Anyone with")
				args := []string{"share", target}
				for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"} {
					args = append(args, "--to", name+"@example.invalid")
				}
				r = w.Exec(args...)
				c.Exit(r, 2)
				c.Stderr(r, "✗ --to names 11 addresses — a share takes 10 at most")
				creates, _ := w.Platform.Calls()
				c.True(creates == 1, "a refused option must not reach the platform")
			},
		},
		Scenario{
			Name:  "options/rewrite-urls-withdrawn",
			Title: "The withdrawn --rewrite-urls flag is refused before anything starts.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", target, "--rewrite-urls", "--background")
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ Unknown flag: --rewrite-urls\nRun 'purlview share --help' for usage.\n", "two lines: %q", r.Stderr)
				c.True(w.Daemon.Starts() == 0, "no daemon started")
			},
		},
		Scenario{
			Name:  "options/target-path-and-query",
			Title: "A path or query is the page the link opens; the block says so, and that the whole app is reachable.",
			Setup: "signed in. (Accepted whole-origin target scope.)",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", "3000/dashboard?tab=2", "--background")
				c.Exit(r, 0)
				c.Stderr(r, sharing+"\n")
				c.Stderr(r, anyone+"\nOpens    /dashboard?tab=2 · the whole app is reachable\n"+expires1h+"\nStop     purlview stop "+label1+"\n")
				r = w.Exec("share", "https://internal.example/demo?version=2", "--background")
				c.Exit(r, 0)
				c.Stderr(r, "✓ Sharing internal.example\n")
				c.Stderr(r, "Opens    /demo?version=2 · the whole app is reachable")
				r = w.Exec("share", target, "--background")
				c.NotStderr(r, "Opens")
				r = w.Exec("list")
				c.Stdout(r, label1+"  localhost:3000")
				c.Stdout(r, label2+"  internal.example")
			},
		},
	)

	register(
		Scenario{
			Name:  "connectivity/temporary-loss-and-reconnect",
			Title: "A dropped connection is announced, redocked after its back-off, and the same link and expiry continue.",
			Setup: "signed in; the serving connection drops once.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Note("network: the serving connection drops")
				w.Platform.DropConnection(id1)
				c.Seen(w.AdvanceUntil(inv, "Reconnected", time.Second, time.Minute), "restored")
				w.Advance(time.Hour)
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "12:00 PM  ! Connection lost — reconnecting\n")
				c.Stderr(r, "12:00 PM  ✓ Reconnected — same link, still expires 1:00 PM\n")
				c.StdoutExactly(r, url1+"\n")
				c.Stderr(r, expired)
				creates, connects := w.Platform.Calls()
				c.True(creates == 1 && connects == 2, "one create and two admissions, got %d/%d", creates, connects)
			},
		},
		Scenario{
			Name:  "connectivity/platform-restart",
			Title: "A platform restart follows the same reconnect path with back-off; the share resumes with its original expiry.",
			Setup: "signed in; the platform restarts (all connections drop, it is unreachable for a few seconds).",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Note("platform restarts: connections drop, unreachable while it comes back")
				w.Platform.Restart()
				c.Seen(inv.WaitFor("Connection lost"), "lost")
				w.Note("platform reachable again")
				w.Platform.Reachable()
				c.Seen(w.AdvanceUntil(inv, "Reconnected", time.Second, time.Minute), "restored")
				inv.Interrupt()
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "✓ Reconnected — same link, still expires 1:00 PM")
				c.StdoutExactly(r, url1+"\n")
			},
		},
		Scenario{
			Name:  "connectivity/expired-while-disconnected",
			Title: "A share the platform already expired is rejected on reconnect and ends as expired.",
			Setup: "signed in; the connection drops and the platform expires the share before it reconnects.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target, "--ttl", "15m")
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Note("platform unreachable; the connection drops")
				w.Platform.Unreachable()
				w.Platform.DropConnection(id1)
				c.Seen(inv.WaitFor("Connection lost"), "lost")
				w.Note("the platform records the share as expired, then is reachable again")
				w.Platform.ExpireNow(id1)
				w.Platform.Reachable()
				c.Seen(w.AdvanceUntil(inv, "Share expired", time.Second, time.Minute), "expired")
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "  Share expired\n")
			},
		},
		Scenario{
			Name:  "connectivity/revoked-while-disconnected",
			Title: "A share stopped elsewhere while disconnected is refused on reconnect and finishes as stopped from another device.",
			Setup: "signed in; the connection drops; the share is stopped from the console.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Note("platform unreachable; the connection drops")
				w.Platform.Unreachable()
				w.Platform.DropConnection(id1)
				c.Seen(inv.WaitFor("Connection lost"), "lost")
				w.Note("the share is stopped from the console; the platform is reachable again")
				w.Platform.RevokeElsewhere(id1)
				w.Platform.Reachable()
				c.Seen(w.AdvanceUntil(inv, "Stopped from another device", time.Second, time.Minute), "stopped elsewhere")
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "  Stopped from another device\n")
			},
		},
		Scenario{
			Name:  "connectivity/invalid-authority-on-reconnect",
			Title: "A device that was signed out from elsewhere stops retrying and is told to sign in again (exit 1).",
			Setup: "signed in; this device is signed out from the console while the connection is down.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Note("platform unreachable; the connection drops")
				w.Platform.Unreachable()
				w.Platform.DropConnection(id1)
				c.Seen(inv.WaitFor("Connection lost"), "lost")
				w.Note("this device is signed out from the console; the platform is reachable again")
				w.Platform.RevokeDevice(ThisDevice)
				w.Platform.Reachable()
				c.Seen(w.AdvanceUntil(inv, "was signed out", time.Second, time.Minute), "signed out")
				r := inv.Wait()
				c.Exit(r, 1)
				c.Stderr(r, "  ✗ This device was signed out — run purlview login\n")
				_, connects := w.Platform.Calls()
				c.True(connects <= 4, "the daemon must stop retrying with invalid credentials, got %d admissions", connects)
			},
		},
	)

	register(
		Scenario{
			Name:  "startup/running-share-limit",
			Title: "At the account's limit of running shares, share says so without naming a number, and nothing runs.",
			Setup: "signed in; the platform refuses new shares at the running-share limit.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.RefuseShares(api.LimitRunningShares)
				r := w.Exec("share", "3000")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ You've reached your limit of running shares — stop one with purlview stop, then share again\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "startup/hourly-share-limit",
			Title: "At the account's limit of new shares in an hour, share says so and asks to try later.",
			Setup: "signed in; the platform refuses new shares at the hourly limit.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.RefuseShares(api.LimitSharesPerHour)
				r := w.Exec("share", "3000", "--background")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ You've reached your limit of new shares this hour — try again later\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "startup/update-required",
			Title: "When the platform no longer serves this version, share says so with the update command for how it was installed.",
			Setup: "signed in with a Homebrew install; the platform refuses this CLI version.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.RequireUpdate()
				r := w.Exec("share", "3000")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ This version of Purlview is no longer supported — update with brew upgrade --cask purlview, then share again\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "startup/unreachable-target",
			Title: "An app that does not answer is reported before any share exists, without the raw cause.",
			Setup: "signed in; localhost:3000 refuses connections.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Prober.Unreachable(targetURL, "connection refused")
				r := w.Exec("share", target)
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ Nothing is answering at "+target+" — start your app, then share again\n", "one line: %q", r.Stderr)
				creates, _ := w.Platform.Calls()
				c.True(creates == 0, "no platform create may happen, got %d", creates)
			},
		},
		Scenario{
			Name:  "startup/daemon-unavailable",
			Title: "A daemon that cannot start fails the share before sign-in or any remote change.",
			Setup: "signed out; the daemon start fails.",
			Run: func(c *Check, w *World) {
				w.Daemon.StartFails(errors.New("runtime directory is not writable"))
				r := w.Exec("share", target)
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ Couldn't start Purlview's background process — runtime directory is not writable\n→ purlview daemon status\n", "the line and its hint: %q", r.Stderr)
				c.True(len(w.Browser.Opened) == 0, "no sign-in may start")
			},
		},
		Scenario{
			Name:  "startup/platform-unavailable",
			Title: "An unreachable platform fails the share with no share created.",
			Setup: "signed in; the platform is unreachable.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.Unreachable()
				r := w.Exec("share", target)
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == noPurlview+"\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "startup/cancel-during-create-is-cleaned-up",
			Title: "Ctrl-C while the create call is in flight recovers the outcome with the same idempotency key and revokes the share.",
			Setup: "signed in; the platform holds the create call.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.HoldCreate()
				inv := w.Run("share", target)
				c.Seen(w.Platform.WaitHeldCreates(1), "the create call is in flight")
				inv.Interrupt()
				c.Seen(w.Platform.WaitHeldCreates(2), "the daemon retries the create with the same key to learn the outcome")
				w.Note("the daemon repeats the create with the same idempotency key; the platform answers")
				w.Platform.ReleaseCreate()
				r := inv.Wait()
				c.Exit(r, 130)
				c.NoStdout(r)
				c.True(r.Stderr == "Cancelled — nothing was left running\n", "one line: %q", r.Stderr)
				_, ended, reason, _ := w.Platform.ShareState(id1)
				c.True(ended && reason == "revoked", "the created share must be revoked, got ended=%v reason=%q", ended, reason)
			},
		},
		Scenario{
			Name:  "startup/cancel-with-uncertain-cleanup",
			Title: "Ctrl-C during creation while the platform is unreachable reports an uncertain outcome and how to check.",
			Setup: "signed in; the create call is in flight when the platform becomes unreachable.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.HoldCreate()
				inv := w.Run("share", target)
				c.Seen(w.Platform.WaitHeldCreates(1), "the create call is in flight")
				inv.Interrupt()
				c.Seen(w.Platform.WaitHeldCreates(2), "the daemon retries the create with the same key to learn the outcome")
				w.Note("platform unreachable when the daemon repeats the create to learn the outcome")
				w.Platform.Unreachable()
				w.Platform.ReleaseCreate()
				r := inv.Wait()
				c.Exit(r, 130)
				c.NoStdout(r)
				c.True(r.Stderr == "Cancelled — Purlview couldn't confirm; see purlview list\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "startup/never-ready-times-out",
			Title: "A share whose route never becomes ready is cleaned up after the startup bound.",
			Setup: "signed in; the platform admits the connection but never reports the route ready.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.HoldReady(true)
				inv := w.Run("share", target)
				c.Seen(w.Platform.WaitConnects(1), "the share is starting")
				c.Seen(w.AdvanceUntil(inv, "didn't start", 5*time.Second, 2*time.Minute), "timeout")
				r := inv.Wait()
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ The share didn't start within 30s — nothing was left running; try again\n", "one line: %q", r.Stderr)
				_, ended, _, _ := w.Platform.ShareState(id1)
				c.True(ended, "nothing may be left running")
			},
		},
		Scenario{
			Name:  "startup/daemon-stopped-before-ready",
			Title: "Stopping the daemon while a share is starting ends it and cleans up.",
			Setup: "signed in; the route never becomes ready; the daemon is stopped (an upgrade would do this).",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.HoldReady(true)
				inv := w.Run("share", target)
				c.Seen(w.Platform.WaitConnects(1), "the share is starting")
				w.Note("purlview daemon stop")
				w.Daemon.Stop()
				r := inv.Wait()
				c.Exit(r, 1)
				c.NoStdout(r)
				c.Stderr(r, "✗ Purlview's background process stopped — nothing was left running; try again")
			},
		},
		Scenario{
			Name:  "startup/daemon-stopped-while-sharing",
			Title: "Stopping the daemon ends a running foreground share with a clear reason.",
			Setup: "signed in; the daemon is stopped while the share is ready.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				inv := w.Run("share", target)
				c.Seen(inv.WaitFor(readyLine), "ready")
				w.Note("purlview daemon stop")
				w.Daemon.Stop()
				r := inv.Wait()
				c.Exit(r, 1)
				c.Stderr(r, "12:00 PM  ✗ Purlview's background process stopped — the link no longer works")
				_, ended, _, _ := w.Platform.ShareState(id1)
				c.True(ended, "the daemon must revoke its shares when it stops")
			},
		},
	)
}

func containsLine(text, needle string) bool { return strings.Contains(text, needle) }
