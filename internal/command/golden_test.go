package command

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/updatecheck"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

var updateGoldens = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// Every output surface of the CLI, rendered through the same functions the
// commands use. Each has a golden without colour and one with; the two
// differ only by the escapes.
type surface struct {
	name   string
	render func(p *output.Printer, d Deps)
}

const (
	goldenLink       = "https://k7m2p4qx.purlview.link/?token=Zk3vQ9x7Lm2Np5RtYw8AbC"
	goldenEntry      = "https://k7m2p4qx.purlview.link/"
	goldenExpiry     = "3:42 PM"
	goldenAccountCmd = "purlview login"
)

var goldenNow = time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)

func at(hour, minute int) time.Time {
	return time.Date(2026, 9, 19, hour, minute, 0, 0, time.UTC)
}

func goldenDeps() Deps {
	return Deps{Clock: clock.NewFake(goldenNow), Local: time.UTC, Getenv: func(string) string { return "" }}.withDefaults()
}

// say renders a failed command the way Run does.
func say(p *output.Printer, d Deps, path string, err error) {
	root := &cobra.Command{Use: "purlview"}
	cmd := root
	for _, name := range strings.Fields(path)[1:] {
		sub := &cobra.Command{Use: name}
		cmd.AddCommand(sub)
		cmd = sub
	}
	report(p, d, cmd, err)
}

func goldenShares() ([]share.Share, *api.InstallationCredential) {
	cred := &api.InstallationCredential{Identity: resource.Identity{Account: "sam@example.com", Device: "dev_macbook", DeviceLabel: "macbook"}}
	return []share.Share{
		{ID: "shr_k7m2p4qx", Target: "http://localhost:5173", Targets: []string{"http://localhost:5173", "http://localhost:8000"}, Device: "dev_macbook", DeviceLabel: "macbook", State: share.StateReady, ExpiresAt: goldenNow.Add(43 * time.Minute), URL: goldenLink},
		{ID: "shr_p4q9x2bd", Target: "http://localhost:3000", Device: "dev_laptop", DeviceLabel: "laptop", Recipients: []string{"ana@example.com", "raj@example.com"}, State: share.StateReconnecting, ExpiresAt: goldenNow.Add(12 * time.Minute), URL: "https://p4q9x2bd.purlview.link/"},
	}, cred
}

func goldenDevices() []api.Installation {
	return []api.Installation{
		{ID: "dev_macbook", Label: "macbook", CreatedAt: goldenNow, LastUsedAt: goldenNow, ActiveShares: 1, Current: true},
		{ID: "dev_laptop", Label: "laptop", CreatedAt: goldenNow.Add(-17 * 24 * time.Hour), LastUsedAt: goldenNow.Add(-time.Hour), ActiveShares: 1},
	}
}

func endedBeforeReady(d Deps, reason share.EndReason, detail string, remote share.RemoteOutcome) error {
	w := &shareWatch{d: d, share: share.Share{Target: "http://localhost:3000", ExpiresAt: goldenNow.Add(time.Hour)}}
	return w.ended(share.Event{Kind: share.EventEnded, Share: share.Share{Target: "http://localhost:3000", EndReason: reason, Detail: detail}, Remote: remote})
}

var surfaces = []surface{
	{"share-foreground", func(p *output.Printer, _ Deps) {
		p.Share(output.ShareBlock{App: "localhost:3000", Link: goldenLink, ExpiresIn: "1h", ExpiresAt: goldenExpiry})
	}},
	{"share-recipients", func(p *output.Printer, _ Deps) {
		p.Share(output.ShareBlock{App: "localhost:3000", Link: goldenEntry, Recipients: []string{"ana@example.com", "raj@example.com"}, InvitesSent: []string{"ana@example.com", "raj@example.com"}, ExpiresIn: "1h", ExpiresAt: goldenExpiry})
	}},
	{"share-one-recipient-invite-not-sent", func(p *output.Printer, _ Deps) {
		p.Share(output.ShareBlock{App: "localhost:3000", Link: goldenEntry, Recipients: []string{"ana@example.com", "raj@example.com"}, InvitesSent: []string{"ana@example.com"}, InvitesNotSent: []string{"raj@example.com"}, ExpiresIn: "1h", ExpiresAt: goldenExpiry})
	}},
	{"share-background-opens", func(p *output.Printer, _ Deps) {
		p.Share(output.ShareBlock{App: "localhost:3000", Link: goldenLink, Opens: "/dashboard?tab=2", ExpiresIn: "1h", ExpiresAt: goldenExpiry, StopID: "k7m2p4qx"})
	}},
	{"share-several", func(p *output.Printer, _ Deps) {
		p.Share(output.ShareBlock{App: "localhost:5173", More: []string{"localhost:8000"}, Link: goldenLink, ExpiresIn: "1h", ExpiresAt: goldenExpiry})
	}},
	{"session-events-several", func(p *output.Printer, _ Deps) {
		p.Event(at(14, 51), appUnresponsive("localhost:8000"))
		p.Event(at(14, 51), appResponding("localhost:8000"))
	}},
	{"session-events", func(p *output.Printer, _ Deps) {
		p.Event(at(14, 44), firstVisitor())
		p.Event(at(14, 51), appUnresponsive("localhost:3000"))
		p.Event(at(14, 51), appResponding("localhost:3000"))
		p.Event(at(14, 58), connectionLost())
		p.Event(at(14, 58), reconnected(goldenExpiry))
		p.Event(at(15, 37), expiresSoon())
		p.Event(at(15, 42), shareExpired())
		p.Event(at(15, 10), stopped())
		p.Event(at(15, 10), stoppedElsewhere())
		p.Event(at(15, 10), deviceSignedOut())
		p.Event(at(15, 10), stoppedHere(goldenExpiry))
	}},
	{"session-ends-badly", func(p *output.Printer, _ Deps) {
		p.Event(at(15, 10), backgroundStopped(false))
		p.Event(at(15, 10), purlviewStoppedShare())
	}},
	{"list", func(p *output.Printer, d Deps) {
		shares, cred := goldenShares()
		p.Table(shareTable(d, cred, shares))
	}},
	{"list-empty", func(p *output.Printer, _ Deps) { p.Empty("No active shares", "purlview share 3000") }},
	{"list-this-device-only", func(p *output.Printer, d Deps) {
		shares, cred := goldenShares()
		p.Status(output.Msg(output.Attend, "Showing this device only").Remedy("couldn't reach Purlview"))
		p.Table(shareTable(d, cred, shares[:1]))
	}},
	{"link", func(p *output.Printer, _ Deps) { p.Link(goldenLink) }},
	{"stop", func(p *output.Printer, _ Deps) { p.Status(stoppedShare("k7m2p4qx")) }},
	{"stop-already-stopped", func(p *output.Printer, _ Deps) { p.Status(alreadyStopped("k7m2p4qx")) }},
	{"devices", func(p *output.Printer, d Deps) { p.Table(deviceTable(d, goldenDevices(), false)) }},
	{"devices-signout", func(p *output.Printer, _ Deps) {
		p.Confirm("Sign out laptop? Its 1 share will stop.")
		p.Status(output.Msg(output.Done, "Signed out ", output.Name("laptop")))
	}},
	{"devices-signout-refusals", func(p *output.Printer, d Deps) {
		say(p, d, "purlview devices signout", fail(output.Name("macbook"), " is this device").then("purlview logout"))
		list := goldenDevices()
		list[0].Label = "laptop"
		_, _ = pickDevice(p, d, list, "laptop")
		say(p, d, "purlview devices signout", fail("Signing out ", output.Name("laptop"), " needs a yes").remedy("add ", bright("--yes")))
	}},
	{"whoami", func(p *output.Printer, _ Deps) {
		p.Whoami(output.Identity{Account: "sam@example.com", Device: "macbook", Status: output.Working("Signed in")})
	}},
	{"whoami-offline", func(p *output.Printer, _ Deps) {
		p.Whoami(output.Identity{Account: "sam@example.com", Device: "macbook", Status: output.Attention("Signed in (not checked, offline)")})
	}},
	{"whoami-signed-out-elsewhere", func(p *output.Printer, _ Deps) {
		p.Whoami(output.Identity{Account: "sam@example.com", Device: "macbook", Status: output.Failed("Signed out from another device")})
		p.Hint(goldenAccountCmd)
	}},
	{"logout", func(p *output.Printer, _ Deps) { p.Status(signedOutHere()) }},
	{"sign-in", func(p *output.Printer, _ Deps) {
		// As rendered when input is not a terminal, which ends each prompt
		// line itself; a terminal's echo of the answer does that instead.
		p.SignIn()
		p.Prompt("Email:")
		p.PromptEnd()
		p.Note("Code sent. Check your inbox.")
		p.Prompt("Code:")
		p.PromptEnd()
		p.Blank()
		p.Status(signedInAs("sam@example.com"))
	}},
	{"sign-in-wrong-code", func(p *output.Printer, _ Deps) {
		p.Prompt("Code:")
		p.PromptEnd()
		p.Status(output.Msg(output.Failure, "That code didn't match").Remedy(output.Count(4, "attempt") + " left"))
		p.Prompt("Code:")
		p.PromptEnd()
		p.Status(output.Msg(output.Failure, "That code didn't match").Remedy("codes have 6 digits"))
		p.Prompt("Code:")
		p.PromptEnd()
		p.Status(output.Msg(output.Attend, "Wait 40s before asking for another code"))
		p.Prompt("Code:")
		p.PromptEnd()
		p.Status(output.Msg(output.Attend, "No more codes for this sign-in").Remedy("run ", bright(goldenAccountCmd), " to start over"))
		p.Prompt("Code:")
		p.PromptEnd()
		p.Status(codeExpired())
	}},
	{"sign-in-already", func(p *output.Printer, _ Deps) {
		p.Status(output.Msg(output.Attend, "This device was signed out").Remedy("sign in again"))
	}},
	{"errors", func(p *output.Printer, d Deps) {
		say(p, d, "purlview share", endedBeforeReady(d, share.ReasonTargetUnreachable, "connection refused", share.RemoteOutcome{}))
		say(p, d, "purlview share", unreachable(errors.New("connection refused")))
		say(p, d, "purlview share", notSignedIn())
		say(p, d, "purlview share", invalid(must(share.ParseTTL("2h"))))
		say(p, d, "purlview share", invalid(must(share.ParseRecipient("reviewer"))))
		say(p, d, "purlview share", invalid(must(share.ParseTarget("70000"))))
		say(p, d, "purlview share", invalid(must(share.ParseTarget("localhost"))))
		say(p, d, "purlview share", invalid(must(share.ParseTarget("http://"+strings.Repeat("a", 250)+".example:3000"))))
		say(p, d, "purlview stop", noShare(share.Ref{ID: "shr_k7m2p4qx"}, nil))
		say(p, d, "purlview share", endedBeforeReady(d, share.ReasonStartupTimeout, "not ready within 30s", share.RemoteOutcome{}))
		say(p, d, "purlview share", cancelled(share.RemoteOutcome{}))
		say(p, d, "purlview share", cancelled(share.RemoteOutcome{Status: share.RemoteUnconfirmed, Detail: "connection refused"}))
		say(p, d, "purlview share", backgroundFailed(errors.New("runtime directory is not writable")))
		say(p, d, "purlview share", endedBeforeReady(d, share.ReasonDaemonShutdown, "daemon stopped", share.RemoteOutcome{}))
		say(p, d, "purlview share", signedOutElsewhere(errors.New("authority_invalid")))
		say(p, d, "purlview stop", fail("Couldn't confirm ", output.Name("k7m2p4qx"), " stopped").remedy("try again, or see ", bright("purlview list")))
		say(p, d, "purlview devices signout", noDevice("laptop", nil))
		say(p, d, "purlview share", invalid(must(share.ParseTargets(strings.Fields("5000 5001 5002 5003 5004 5005 5006 5007 5008 5009 5010")))))
		say(p, d, "purlview share", invalid(must(share.ParseTargets([]string{"5173", "127.0.0.1:5173"}))))
		say(p, d, "purlview share", invalid(must(share.ParseTargets([]string{}))))
		say(p, d, "purlview", unknownCommand(newHelpRoot(), "shar"))
		say(p, d, "purlview link", notLinkID(share.Ref{URL: "https://k7m2p4qx.purlview.link"}))
		say(p, d, "purlview login", fail("Sign-in was not finished").remedy("run ", bright(goldenAccountCmd), " again"))
		say(p, d, "purlview share", limitReached("running_shares"))
		say(p, d, "purlview share", limitReached("shares_per_hour"))
		say(p, d, "purlview share", limitReached("a_future_limit"))
		say(p, d, "purlview share", updateRequired(updatecheck.Advice{Method: "homebrew", Command: "brew upgrade --cask purlview"}))
		say(p, d, "purlview share", updateRequired(updatecheck.Advice{Method: "deb", Link: "https://github.com/idyl-labs/purlview-releases/releases/latest"}))
	}},
	{"errors-with-debug", func(p *output.Printer, d Deps) {
		d.Getenv = func(k string) string {
			if k == EnvDebug {
				return "1"
			}
			return ""
		}
		say(p, d, "purlview share", unreachable(errors.New("dial tcp 203.0.113.9:443: connect: connection refused")))
	}},
	{"update-notice", func(p *output.Printer, _ Deps) {
		p.UpdateNotice(output.Update{Latest: "0.9.1", Current: "0.9.0", Command: "brew upgrade --cask purlview"})
	}},
	{"update-notice-download", func(p *output.Printer, _ Deps) {
		p.UpdateNotice(output.Update{Latest: "0.9.1", Current: "0.9.0", Page: "https://github.com/idyl-labs/purlview-releases/releases/latest"})
	}},
	{"updated-daemon", func(p *output.Printer, _ Deps) {
		p.Status(output.Msg(output.Attend, "Purlview was updated").Remedy("shares started before the update have stopped"))
	}},
}

// must turns a parse result into the error it produced (or nil).
func must[T any](_ T, err error) error { return err }

func newHelpRoot() *cobra.Command {
	return NewWithDeps(testInfo, Streams{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, goldenDeps())
}

// helpSurfaces are the help pages, rendered by the command tree itself.
var helpSurfaces = [][]string{nil, {"share"}, {"list"}, {"link"}, {"stop"}, {"devices"}, {"devices", "signout"}, {"login"}, {"logout"}, {"whoami"}}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func render(t *testing.T, s surface, colour bool) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	s.render(output.New(&out, &errOut, colour), goldenDeps())
	return out.String(), errOut.String()
}

func renderHelp(t *testing.T, args []string, colour bool) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	d := goldenDeps()
	d.Color = colour
	if code := RunWithDeps(context.Background(), append(append([]string{}, args...), "--help"), Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, testInfo, d); code != ExitOK {
		t.Fatalf("help %v: exit %d %s", args, code, errOut.String())
	}
	return out.String(), errOut.String()
}

func golden(t *testing.T, name string, out, errOut string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	got := "[stdout]\n" + out + "[stderr]\n" + errOut
	if *updateGoldens {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (regenerate with go test ./internal/command -run TestGoldens -update)", path, err)
	}
	// A Windows checkout may convert the goldens to CRLF; the output never has one.
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("%s differs from the golden; regenerate with -update if the change is intended\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// TestGoldens pins every surface with and without colour. Without colour
// no escape is written; with colour the bytes minus the escapes are the
// same; and stdout carries data only: the link alone for share and link,
// never a status line, never an escape on a link.
func TestGoldens(t *testing.T) {
	t.Parallel()
	all := map[string]func(t *testing.T, colour bool) (string, string){}
	for _, s := range surfaces {
		all[s.name] = func(t *testing.T, colour bool) (string, string) { return render(t, s, colour) }
	}
	for _, args := range helpSurfaces {
		name := "help-" + strings.Join(append([]string{"purlview"}, args...), "-")
		all[name] = func(t *testing.T, colour bool) (string, string) { return renderHelp(t, args, colour) }
	}
	for name, fn := range all {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, errOut := fn(t, false)
			cout, cerr := fn(t, true)
			golden(t, name, out, errOut)
			golden(t, name+".color", cout, cerr)
			if strings.Contains(out+errOut, "\x1b") {
				t.Errorf("escapes without colour:\n%q", out+errOut)
			}
			if ansi.ReplaceAllString(cout, "") != out || ansi.ReplaceAllString(cerr, "") != errOut {
				t.Errorf("colour changes more than the escapes:\n%q\n%q", cout, cerr)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "✓") || strings.HasPrefix(line, "✗") || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "→") || strings.HasPrefix(line, "Run '") {
					t.Errorf("status on stdout: %q", line)
				}
			}
			if strings.HasPrefix(name, "share") || name == "link" {
				if cout != out || !strings.HasPrefix(out, "https://") || strings.Count(out, "\n") != 1 {
					t.Errorf("stdout must be exactly the link: %q %q", out, cout)
				}
			}
			if strings.Contains(out+errOut, "_purlview") {
				t.Errorf("_purlview must never be printed:\n%s", out+errOut)
			}
		})
	}
}
