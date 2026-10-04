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
	"time"

	"github.com/idyl-labs/purlview/internal/share"
)

const (
	signInBlock = "\nSign in to Purlview\n\n  Email: "
	codeSent    = "  Code sent. Check your inbox.\n"
	signedIn    = "✓ Signed in as " + AccountEmail + "\n"
	wrongCode   = "000000"
)

func init() {
	register(
		Scenario{Name: "login/email-code", Title: "login asks for the email address and the emailed 6-digit code, typed as hidden text, and ends at the signed-in line.", Setup: "signed out; an email code arrives.", Run: func(c *Check, w *World) {
			w.Typed()
			inv := w.Run("login")
			c.Seen(inv.WaitFor("Email:"), "asking for the email")
			w.Type(AccountEmail)
			c.Seen(inv.WaitFor("Code:"), "asking for the code")
			code := w.Platform.LoginCode()
			w.TypeHidden(code[:3] + " " + code[3:])
			r := inv.Wait()
			c.Exit(r, 0)
			c.NoStdout(r)
			c.True(r.Stderr == signInBlock+codeSent+"  Code: \n\n"+signedIn, "the sign-in block: %q", r.Stderr)
			c.NotStderr(r, code)
			c.Exit(w.Exec("whoami"), 0)
		}},
		Scenario{
			Name:  "login/wrong-code",
			Title: "A wrong code asks again with the attempts left; the fifth miss ends the sign-in (exit 1).",
			Setup: "signed out; the user mistypes the code.",
			Run: func(c *Check, w *World) {
				w.Typed()
				inv := w.Run("login")
				c.Seen(inv.WaitFor("Email:"), "asking for the email")
				w.Type(AccountEmail)
				c.Seen(inv.WaitFor("Code:"), "asking for the code")
				w.TypeHidden(wrongCode)
				c.Seen(inv.WaitFor("4 attempts left"), "asking again")
				w.TypeHidden("12345")
				c.Seen(inv.WaitFor("codes have 6 digits"), "a code of the wrong length costs no attempt")
				w.TypeHidden(w.Platform.LoginCode())
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, "  Code: \n✗ That code didn't match — 4 attempts left\n  Code: \n✗ That code didn't match — codes have 6 digits\n  Code: \n\n"+signedIn)
				c.NotStderr(r, wrongCode)
				c.Exit(w.Exec("logout"), 0)

				inv = w.Run("login")
				c.Seen(inv.WaitFor("Email:"), "asking for the email")
				w.Type(AccountEmail)
				for miss := 1; miss <= 5; miss++ {
					c.Seen(inv.WaitForCount("  Code: ", miss), "asking for the code")
					w.TypeHidden(wrongCode)
				}
				r = inv.Wait()
				c.Exit(r, 1)
				c.Stderr(r, "✗ That code didn't match — 1 attempt left\n  Code: \n✗ That code has expired — run purlview login to get a new one\n")
				cred, _ := w.Store.Load()
				c.True(cred == nil, "five wrong codes must not sign in")
			},
		},
		Scenario{
			Name:  "login/resend-code",
			Title: "Typing resend sends a new code after a minute, twice per sign-in; asking earlier says how long to wait.",
			Setup: "signed out; the first email never arrives.",
			Run: func(c *Check, w *World) {
				w.Typed()
				inv := w.Run("login")
				c.Seen(inv.WaitFor("Email:"), "asking for the email")
				w.Type(AccountEmail)
				c.Seen(inv.WaitFor("Code:"), "waiting for the code")
				first := w.Platform.LoginCode()
				w.Advance(20 * time.Second)
				w.TypeHidden("resend")
				c.Seen(inv.WaitFor("! Wait 40s before asking for another code"), "too early")
				w.Advance(40 * time.Second)
				w.TypeHidden("resend")
				c.Seen(inv.WaitForCount(codeSent, 2), "the second email")
				second := w.Platform.LoginCode()
				c.True(len(second) == 6 && second != first && w.Platform.LoginSends() == 2, "a resend replaces the code: %q then %q", first, second)
				w.Advance(time.Minute)
				w.TypeHidden("resend")
				c.Seen(inv.WaitForCount(codeSent, 3), "the third email")
				w.Advance(time.Minute)
				w.TypeHidden("resend")
				c.Seen(inv.WaitFor("! No more codes for this sign-in — run purlview login to start over"), "two resends per sign-in")
				c.True(w.Platform.LoginSends() == 3, "the platform must not be asked again")
				w.TypeHidden(w.Platform.LoginCode())
				r := inv.Wait()
				c.Exit(r, 0)
				c.Stderr(r, signedIn)
			},
		},
		Scenario{
			Name:  "login/without-a-terminal",
			Title: "Without a terminal, login reads the email address and the code as two lines of standard input.",
			Setup: "signed out; standard input is a pipe carrying the address and the code.",
			Run: func(c *Check, w *World) {
				w.Interactive = false
				r := w.Exec("login")
				c.Exit(r, 0)
				c.NoStdout(r)
				c.True(r.Stderr == signInBlock+"\n"+codeSent+"  Code: \n\n"+signedIn, "every prompt line is ended: %q", r.Stderr)
			},
		},
		Scenario{Name: "login/no-browser-flag-rejected", Title: "--no-browser is not a login flag; it is rejected before sign-in.", Setup: "signed out.", Run: func(c *Check, w *World) {
			r := w.Exec("login", "--no-browser")
			c.Exit(r, 2)
			c.True(r.Stderr == "✗ Unknown flag: --no-browser\nRun 'purlview login --help' for usage.\n", "two lines: %q", r.Stderr)
			cred, _ := w.Store.Load()
			c.True(cred == nil, "still signed out")
			c.True(len(w.Browser.Opened) == 0, "no browser opens")
		}},
		Scenario{
			Name:  "login/cancelled",
			Title: "Ctrl-C at a prompt ends its line, prints nothing more and exits 130.",
			Setup: "signed out; the user presses Ctrl-C at the code prompt.",
			Run: func(c *Check, w *World) {
				w.Typed()
				inv := w.Run("login")
				c.Seen(inv.WaitFor("Email:"), "asking for the email")
				w.Type(AccountEmail)
				c.Seen(inv.WaitFor("Code:"), "asking for the code")
				inv.Interrupt()
				r := inv.Wait()
				c.Exit(r, 130)
				c.NoStdout(r)
				c.True(r.Stderr == signInBlock+codeSent+"  Code: \n", "nothing after the prompt: %q", r.Stderr)
				cred, _ := w.Store.Load()
				c.True(cred == nil, "nothing may be remembered")
			},
		},
		Scenario{
			Name:  "login/timeout",
			Title: "A code that is never entered expires after five minutes (exit 1).",
			Setup: "signed out; no code is entered; the fake clock passes the five-minute lifetime.",
			Run: func(c *Check, w *World) {
				w.Typed()
				inv := w.Run("login")
				c.Seen(inv.WaitFor("Email:"), "asking for the email")
				w.Type(AccountEmail)
				c.Seen(inv.WaitFor("Code:"), "asking for the code")
				c.Seen(w.AdvanceUntil(inv, "has expired", time.Minute, 10*time.Minute), "timeout")
				r := inv.Wait()
				c.Exit(r, 1)
				c.Stderr(r, "  Code: \n✗ That code has expired — run purlview login to get a new one\n")
			},
		},
		Scenario{
			Name:  "login/repeated-and-signed-out-elsewhere",
			Title: "A repeated login confirms who is signed in; only a device that was signed out signs in again.",
			Setup: "signed in; later, this device is signed out from the console.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("login")
				c.Exit(r, 0)
				c.True(r.Stderr == signedIn, "one line: %q", r.Stderr)
				w.Note("this device is signed out from the console")
				w.Platform.RevokeDevice(ThisDevice)
				r = w.Exec("login")
				c.Exit(r, 0)
				c.Stderr(r, "! This device was signed out — sign in again\n"+signInBlock)
				c.Stderr(r, signedIn)
				cred, _ := w.Store.Load()
				c.True(cred != nil && cred.Device != ThisDevice, "a new sign-in must replace the one that was signed out")
			},
		},
		Scenario{
			Name:  "login/credential-save-fails",
			Title: "When the sign-in cannot be saved on this device, login says whether Purlview still lists the device.",
			Setup: "signed out; the credential store refuses to save; the second attempt happens while the platform is unreachable.",
			Run: func(c *Check, w *World) {
				w.Store.SaveFails(errors.New("disk full"))
				r := w.Exec("login")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.Stderr(r, "✗ Couldn't save your sign-in on this device — nothing stays signed in\n")
				c.NotStderr(r, "disk full")
				c.True(w.Platform.DeviceRevoked(ThisDevice), "the unusable sign-in must be signed out on the platform")
				w.Note("platform unreachable right after the code is accepted")
				w.Platform.LoginMode("wait")
				inv := w.Run("login")
				c.Seen(inv.WaitFor("Code:"), "waiting")
				w.Platform.UnreachableAfterApproval()
				w.Platform.Approve()
				r = inv.Wait()
				c.Exit(r, 1)
				c.Stderr(r, "✗ Couldn't save your sign-in on this device — Purlview still lists it; sign it out with purlview devices\n")
				cred, _ := w.Store.Load()
				c.True(cred == nil, "nothing may be remembered")
			},
		},
		Scenario{
			Name:  "login/platform-unavailable",
			Title: "login fails clearly when Purlview cannot be reached.",
			Setup: "signed out; the platform is unreachable.",
			Run: func(c *Check, w *World) {
				w.Platform.Unreachable()
				r := w.Exec("login")
				c.Exit(r, 1)
				c.Stderr(r, noPurlview+"\n")
				c.NotStderr(r, "connection refused")
			},
		},
	)

	register(
		Scenario{
			Name:  "list/multiple-devices",
			Title: "list shows the account's shares on every device without their links; a dot marks this device's rows.",
			Setup: "signed in; the laptop shares an app with two people and has lost its connection; this device shares one.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.SeedRemoteShare("https://internal.example", 12*time.Minute, false, "ana@example.invalid", "raj@example.invalid")
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				r = w.Exec("list")
				c.Exit(r, 0)
				c.True(r.Stderr == "", "the table is all there is: %q", r.Stderr)
				c.StdoutExactly(r, ""+
					"  ID        APP               ACCESS                  DEVICE       STATUS        EXPIRES\n"+
					"  "+label1+"  internal.example  ana@example.invalid +1  laptop       Reconnecting  in 12m\n"+
					"● "+label2+"  localhost:3000    Anyone with the link    this device  Sharing       in 1h\n")
				c.True(!containsLine(r.Stdout, "token=") && !containsLine(r.Stdout, "https://"), "no link may be listed:\n%s", r.Stdout)
			},
		},
		Scenario{
			Name:  "list/empty-account",
			Title: "An account with no active shares says so, names the command that starts one, and exits 0.",
			Setup: "signed in; no shares.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("list")
				c.Exit(r, 0)
				c.NoStdout(r)
				c.True(r.Stderr == "No active shares\n→ purlview share 3000\n", "the empty state: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "list/no-local-daemon",
			Title: "list works with no daemon running on this machine and does not start one.",
			Setup: "signed in; no daemon; the laptop serves a share.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.SeedRemoteShare("https://internal.example", 30*time.Minute, true)
				r := w.Exec("list")
				c.Exit(r, 0)
				c.Stdout(r, "laptop  Sharing  in 30m")
				c.True(w.Daemon.Starts() == 0 && !w.Daemon.Running(), "list must not start a daemon")
			},
		},
		Scenario{
			Name:  "list/signed-out",
			Title: "list while signed out says how to sign in.",
			Setup: "signed out.",
			Run: func(c *Check, w *World) {
				r := w.Exec("list")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == notSigned+"\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "list/purlview-unreachable",
			Title: "When Purlview can't be reached, list shows this device's shares, says so, and exits 1; a failure never reads as an empty account.",
			Setup: "signed in; one background share on this device; the platform becomes unreachable.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("share", target, "--background")
				c.Exit(r, 0)
				w.Note("platform unreachable")
				w.Platform.Unreachable()
				r = w.Exec("list")
				c.Exit(r, 1)
				c.True(r.Stderr == "! Showing this device only — couldn't reach Purlview\n", "one line: %q", r.Stderr)
				c.Stdout(r, "● "+label1+"  localhost:3000  Anyone with the link  this device  Sharing  in 1h")
				w.Note("purlview daemon stop")
				w.Daemon.Stop()
				r = w.Exec("list")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == noPurlview+"\n", "one line: %q", r.Stderr)
			},
		},
	)

	register(
		Scenario{
			Name:  "link/prints-only-the-link",
			Title: "link prints a share's link, and only that, for a share of any of your devices.",
			Setup: "signed in; one background share here and one on the laptop.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				c.Exit(w.Exec("share", target, "--background"), 0)
				remote := w.Platform.SeedRemoteShare("https://internal.example", 30*time.Minute, true, "ana@example.invalid")
				r := w.Exec("link", label1)
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				c.True(r.Stderr == "", "nothing but the link: %q", r.Stderr)
				r = w.Exec("link", remote)
				c.Exit(r, 0)
				c.StdoutExactly(r, entry2+accessPath+"\n")
				r = w.Exec("link", "zzzzzzzz")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ No share zzzzzzzz in your account — see purlview list\n", "one line: %q", r.Stderr)
				r = w.Exec("link")
				c.Exit(r, 2)
				c.Stderr(r, "✗ link needs a share id — see purlview list\nRun 'purlview link --help' for usage.\n")
				w.Note("platform unreachable")
				w.Platform.Unreachable()
				r = w.Exec("link", label1)
				c.Exit(r, 0)
				c.StdoutExactly(r, url1+"\n")
				r = w.Exec("link", share.Label(remote))
				c.Exit(r, 1)
				c.NoStdout(r)
				c.Stderr(r, noPurlview)
			},
		},
	)

	register(
		Scenario{
			Name:  "stop/by-id-and-by-link",
			Title: "stop takes the share's id, its link or the address of a page opened through it; the hidden former name unshare still works.",
			Setup: "signed in; three background shares.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				c.Exit(w.Exec("share", target, "--background"), 0)
				c.Exit(w.Exec("share", "https://internal.example/demo", "--background"), 0)
				c.Exit(w.Exec("share", "8080", "--background"), 0)
				r := w.Exec("stop", label1)
				c.Exit(r, 0)
				c.NoStdout(r)
				c.True(r.Stderr == "✓ Stopped "+label1+" — the link no longer works\n", "one line: %q", r.Stderr)
				r = w.Exec("stop", origin2+"/some/page?x=1")
				c.Exit(r, 0)
				c.Stderr(r, "✓ Stopped "+label2+" — the link no longer works")
				r = w.Exec("unshare", "shr_f8q3w6hn")
				c.Exit(r, 0)
				c.Stderr(r, "✓ Stopped f8q3w6hn — the link no longer works")
				r = w.Exec("list")
				c.Stderr(r, "No active shares")
				c.True(w.WaitDaemonActive(0), "the daemon must stop all three on the platform's notice")
			},
		},
		Scenario{
			Name:  "stop/local-foreground-share",
			Title: "stop from another terminal stops a foreground share; that terminal says it was stopped elsewhere and exits 0.",
			Setup: "signed in; a foreground share runs in another terminal.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				fg := w.Run("share", target)
				c.Seen(fg.WaitFor(readyLine), "ready")
				r := w.Exec("stop", label1)
				c.Exit(r, 0)
				c.Stderr(r, "✓ Stopped "+label1)
				fr := fg.Wait()
				c.Exit(fr, 0)
				c.Stderr(fr, "12:00 PM  Stopped from another device\n")
			},
		},
		Scenario{
			Name:  "stop/remote-device-share",
			Title: "stop stops a share of another device, connected or not; nothing waits for that device.",
			Setup: "signed in; the laptop shares two apps; one has lost its connection.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				id := w.Platform.SeedRemoteShare("https://internal.example", 30*time.Minute, true)
				off := w.Platform.SeedRemoteShare("https://other.example", 30*time.Minute, false)
				r := w.Exec("stop", share.Label(id))
				c.Exit(r, 0)
				c.True(r.Stderr == "✓ Stopped "+share.Label(id)+" — the link no longer works\n", "one line: %q", r.Stderr)
				_, ended, _, conns := w.Platform.ShareState(id)
				c.True(ended && conns == 0, "the platform must stop the share and its streams, got ended=%v conns=%d", ended, conns)
				r = w.Exec("stop", share.Label(off))
				c.Exit(r, 0)
				c.True(r.Stderr == "✓ Stopped "+share.Label(off)+" — the link no longer works\n", "one line: %q", r.Stderr)
				_, ended, _, _ = w.Platform.ShareState(off)
				c.True(ended, "the platform must record the stop")
			},
		},
		Scenario{
			Name:  "stop/already-stopped",
			Title: "Stopping a share that was already stopped succeeds and says so.",
			Setup: "signed in; a background share was stopped once already.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				c.Exit(w.Exec("share", target, "--background"), 0)
				c.Exit(w.Exec("stop", label1), 0)
				r := w.Exec("stop", label1)
				c.Exit(r, 0)
				c.NoStdout(r)
				c.True(r.Stderr == "✓ "+label1+" was already stopped\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "stop/unknown-share",
			Title: "An unknown or someone else's share gets one line that tells them apart for nobody; what is not an id or a link is a usage error.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("stop", "zzzzzzzz")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ No share zzzzzzzz in your account — see purlview list\n", "one line: %q", r.Stderr)
				r = w.Exec("stop", "7K2M")
				c.Exit(r, 2)
				c.True(r.Stderr == "✗ 7K2M isn't a share id or link — see purlview list\nRun 'purlview stop --help' for usage.\n", "two lines: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "stop/uncertain-confirmation",
			Title: "A stop whose confirmation is lost is reported as unconfirmed (exit 1); repeating it is safe.",
			Setup: "signed in; one background share; the platform's acknowledgement is lost once.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				c.Exit(w.Exec("share", target, "--background"), 0)
				w.Note("the platform's acknowledgement will be lost")
				w.Platform.RevokeUncertain()
				r := w.Exec("stop", label1)
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "✗ Couldn't confirm "+label1+" stopped — try again, or see purlview list\n", "one line: %q", r.Stderr)
				r = w.Exec("stop", label1)
				c.Exit(r, 0)
				c.Stderr(r, "✓ "+label1+" was already stopped")
			},
		},
		Scenario{
			Name:  "stop/purlview-unreachable-local-share",
			Title: "With Purlview unreachable, a share of this device stops here and the line says by when the link stops working (exit 1).",
			Setup: "signed in; one background share; the platform is unreachable.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				c.Exit(w.Exec("share", target, "--background"), 0)
				w.Note("platform unreachable")
				w.Platform.Unreachable()
				r := w.Exec("stop", label1)
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == "! Stopped here, but Purlview couldn't confirm — the link stops working by 1:00 PM\n", "one line: %q", r.Stderr)
				c.True(w.Daemon.Active() == 0, "this device must have stopped serving it")
			},
		},
		Scenario{
			Name:  "stop/purlview-unreachable-remote-share",
			Title: "With Purlview unreachable, another device's share cannot be stopped and nothing changes.",
			Setup: "signed in; the laptop's share; the platform is unreachable.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				id := w.Platform.SeedRemoteShare("https://internal.example", 30*time.Minute, true)
				w.Platform.Unreachable()
				r := w.Exec("stop", share.Label(id))
				c.Exit(r, 1)
				c.True(r.Stderr == noPurlview+"\n", "one line: %q", r.Stderr)
			},
		},
	)

	register(
		Scenario{
			Name:  "devices/list",
			Title: "devices lists the devices signed in to the account; a dot marks this one.",
			Setup: "signed in on this device today; the laptop signed in two weeks ago and shares one app.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.SeedRemoteShare("https://internal.example", 30*time.Minute, true)
				r := w.Exec("devices")
				c.Exit(r, 0)
				c.True(r.Stderr == "", "the table is all there is: %q", r.Stderr)
				c.StdoutExactly(r, ""+
					"  NAME    SIGNED IN  LAST USED  SHARES\n"+
					"  laptop  Sep 2      today           1\n"+
					"● studio  Sep 16     today           0\n")
			},
		},
		Scenario{
			Name:  "devices/signout",
			Title: "devices signout asks first, naming what stops, and defaults to No; --yes skips the question.",
			Setup: "signed in; the laptop shares one app.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				id := w.Platform.SeedRemoteShare("https://internal.example", 30*time.Minute, true)
				w.Typed()
				inv := w.Run("devices", "signout", "laptop")
				c.Seen(inv.WaitFor("[y/N]"), "the question")
				w.Type("")
				r := inv.Wait()
				c.Exit(r, 0)
				c.True(r.Stderr == "Sign out laptop? Its 1 share will stop. [y/N] ", "Enter means No: %q", r.Stderr)
				c.True(!w.Platform.DeviceRevoked(OtherDevice), "No must change nothing")
				inv = w.Run("devices", "signout", "laptop")
				c.Seen(inv.WaitFor("[y/N]"), "the question")
				w.Type("y")
				r = inv.Wait()
				c.Exit(r, 0)
				c.NoStdout(r)
				c.True(r.Stderr == "Sign out laptop? Its 1 share will stop. [y/N] ✓ Signed out laptop\n", "the question and the result: %q", r.Stderr)
				_, ended, _, _ := w.Platform.ShareState(id)
				c.True(ended && w.Platform.DeviceRevoked(OtherDevice), "the laptop and its share must be stopped")
				r = w.Exec("devices", "signout", "laptop", "--yes")
				c.Exit(r, 1)
				c.True(r.Stderr == "✗ No device laptop in your account — see purlview devices\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "devices/signout-refusals",
			Title: "devices signout refuses this device, a name two devices share, and a question nobody can answer.",
			Setup: "signed in; two other devices are both named laptop.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("devices", "signout", ThisLabel, "--yes")
				c.Exit(r, 1)
				c.True(r.Stderr == "✗ studio is this device\n→ purlview logout\n", "the line and its hint: %q", r.Stderr)
				c.True(!w.Platform.DeviceRevoked(ThisDevice), "this device must stay signed in")
				w.Platform.AddDevice("dev_laptop2", OtherLabel, Epoch.Add(-24*time.Hour))
				r = w.Exec("devices", "signout", "laptop", "--yes")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == ""+
					"✗ 2 devices are named laptop — sign one out by its id\n"+
					"  ID           SIGNED IN  LAST USED  SHARES\n"+
					"  dev_laptop   Sep 2      today           0\n"+
					"  dev_laptop2  Sep 15     Sep 15          0\n", "the line and the ids: %q", r.Stderr)
				c.True(!w.Platform.DeviceRevoked(OtherDevice), "neither may be signed out")
				w.Interactive = false
				r = w.Exec("devices", "signout", "dev_laptop2")
				c.Exit(r, 1)
				c.True(r.Stderr == "✗ Signing out laptop needs a yes — add --yes\n", "one line: %q", r.Stderr)
				r = w.Exec("devices", "signout", "dev_laptop2", "--yes")
				c.Exit(r, 0)
				c.True(r.Stderr == "✓ Signed out laptop\n", "one line: %q", r.Stderr)
				c.True(w.Platform.DeviceRevoked("dev_laptop2") && !w.Platform.DeviceRevoked(OtherDevice), "only the named id is signed out")
			},
		},
	)

	register(
		Scenario{Name: "logout/with-foreground-share", Title: "logout signs out on this device only; a running foreground share continues until it is stopped.", Setup: "signed in with a foreground share.", Run: func(c *Check, w *World) {
			w.SignIn()
			fg := w.Run("share", target)
			c.Seen(fg.WaitFor(readyLine), "ready")
			r := w.Exec("logout")
			c.Exit(r, 0)
			c.NoStdout(r)
			c.True(r.Stderr == "✓ Signed out on this device — running shares continue until stopped or expired\n", "one line: %q", r.Stderr)
			c.True(w.Daemon.Active() == 1, "foreground survives logout")
			c.True(!w.Platform.DeviceRevoked(ThisDevice), "the device stays signed in on the platform")
			cred, _ := w.Store.Load()
			c.True(cred == nil, "local credential cleared")
			fg.Interrupt()
			c.Exit(fg.Wait(), 0)
		}},
		Scenario{Name: "logout/with-background-shares-other-devices-unaffected", Title: "logout leaves background shares running, here and on other devices.", Setup: "two local background shares and one remote share.", Run: func(c *Check, w *World) {
			w.SignIn()
			remote := w.Platform.SeedRemoteShare("https://internal.example", 30*time.Minute, true)
			c.Exit(w.Exec("share", target, "--background"), 0)
			c.Exit(w.Exec("share", "localhost:8080", "--background"), 0)
			c.Exit(w.Exec("logout"), 0)
			c.True(w.Daemon.Active() == 2, "background shares survive")
			_, ended, _, conns := w.Platform.ShareState(remote)
			c.True(!ended && conns == 1, "remote survives")
		}},
		Scenario{Name: "logout/no-daemon", Title: "logout needs no daemon and no call to Purlview.", Setup: "signed in without a daemon.", Run: func(c *Check, w *World) {
			w.SignIn()
			c.Exit(w.Exec("logout"), 0)
			c.True(w.Daemon.Starts() == 0, "no daemon started")
			c.True(!w.Platform.DeviceRevoked(ThisDevice), "no call to the platform")
		}},
		Scenario{Name: "logout/repeated", Title: "A repeated logout is harmless.", Setup: "signed in, then signed out.", Run: func(c *Check, w *World) { w.SignIn(); c.Exit(w.Exec("logout"), 0); c.Exit(w.Exec("logout"), 0) }},
		Scenario{Name: "logout/offline", Title: "logout works offline; shares keep running.", Setup: "signed in with a background share; platform offline.", Run: func(c *Check, w *World) {
			w.SignIn()
			c.Exit(w.Exec("share", target, "--background"), 0)
			w.Platform.Unreachable()
			c.Exit(w.Exec("logout"), 0)
			c.True(w.Daemon.Active() == 1, "local sharing survives")
			c.True(!w.Platform.DeviceRevoked(ThisDevice), "the device stays signed in on the platform")
			c.Exit(w.Exec("whoami"), 1)
		}},
	)

	register(
		Scenario{
			Name:  "whoami/signed-in",
			Title: "whoami shows the account, this device and the status Purlview confirmed, without starting a daemon.",
			Setup: "signed in.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				r := w.Exec("whoami")
				c.Exit(r, 0)
				c.StdoutExactly(r, "Account  "+AccountEmail+"\nDevice   "+ThisLabel+"\nStatus   Signed in\n")
				c.True(r.Stderr == "", "the block is all there is: %q", r.Stderr)
				c.True(w.Daemon.Starts() == 0, "whoami must not start a daemon")
			},
		},
		Scenario{
			Name:  "whoami/signed-out",
			Title: "whoami while signed out says how to sign in.",
			Setup: "signed out.",
			Run: func(c *Check, w *World) {
				r := w.Exec("whoami")
				c.Exit(r, 1)
				c.NoStdout(r)
				c.True(r.Stderr == notSigned+"\n", "one line: %q", r.Stderr)
			},
		},
		Scenario{
			Name:  "whoami/offline",
			Title: "With Purlview unreachable, whoami shows what this device remembers and says it was not checked.",
			Setup: "signed in; the platform is unreachable.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.Unreachable()
				r := w.Exec("whoami")
				c.Exit(r, 0)
				c.StdoutExactly(r, "Account  "+AccountEmail+"\nDevice   "+ThisLabel+"\nStatus   Signed in (not checked, offline)\n")
				c.NotStderr(r, "connection refused")
			},
		},
		Scenario{
			Name:  "whoami/signed-out-from-another-device",
			Title: "A device that was signed out from elsewhere says so, with how to sign in again (exit 1).",
			Setup: "signed in; this device was signed out from the console.",
			Run: func(c *Check, w *World) {
				w.SignIn()
				w.Platform.RevokeDevice(ThisDevice)
				r := w.Exec("whoami")
				c.Exit(r, 1)
				c.StdoutExactly(r, "Account  "+AccountEmail+"\nDevice   "+ThisLabel+"\nStatus   Signed out from another device\n")
				c.True(r.Stderr == "→ purlview login\n", "the hint: %q", r.Stderr)
			},
		},
	)
}
