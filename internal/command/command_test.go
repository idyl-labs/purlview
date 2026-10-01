package command

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/daemon"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

var testInfo = buildinfo.Info{
	Version:    "9.8.7",
	Commit:     "0123456789abcdef",
	CommitDate: "2026-09-16T00:00:00Z",
	BuiltBy:    "test",
	GoVersion:  "go1.27.1",
	OS:         "testos",
	Arch:       "testarch",
}

// run executes args through the same boundary the executable uses, with
// the production wiring pointed at a private, empty state directory so no
// test reads or writes the developer's own Purlview state.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runIn(t, t.TempDir(), args...)
}

func runIn(t *testing.T, stateDir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == daemon.EnvStateDir {
			return stateDir
		}
		return ""
	}
	code = RunWithEnv(context.Background(), args, Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, testInfo, getenv)
	return code, out.String(), errOut.String()
}

func TestHelpAndVersionSucceedOnStdout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
		not  []string
	}{
		{"no arguments", nil, []string{"Share your running app. Improve it together.", "Usage:", "purlview [command]", "Share\n", "  share ", "  list ", "  link ", "  stop ", "Account\n", "  login ", "  logout ", "  whoami ", "  devices ", "System\n", "  completion "}, []string{"unshare", "endpoint", "daemon "}},
		{"--help", []string{"--help"}, []string{"Usage:", "  purlview share 3000\n", "  purlview share 3000 --to ana@example.com\n"}, []string{"Links last at most one hour", "--rewrite-urls", "browser", "endpoint"}},
		{"help command", []string{"help"}, []string{"Usage:"}, nil},
		{"share --help", []string{"share", "--help"}, []string{"purlview share <target>", "  purlview share 3000\n", "--background", "--ttl duration", "--to email", "sent an invite", "standard output"}, []string{"--rewrite-urls", "browser", "endpoint", "localhost:3000 --rewrite"}},
		{"help share", []string{"help", "share"}, []string{"purlview share <target>"}, nil},
		{"login --help", []string{"login", "--help"}, []string{"email", "6-digit", "hidden"}, []string{"endpoint", "browser"}},
		{"logout --help", []string{"logout", "--help"}, []string{"Running shares continue", "offline"}, []string{"credential"}},
		{"list --help", []string{"list", "--help"}, []string{"on all of your devices", "purlview link"}, []string{"never as an empty list", "URL"}},
		{"link --help", []string{"link", "--help"}, []string{"purlview link <id>", "clipboard"}, nil},
		{"stop --help", []string{"stop", "--help"}, []string{"purlview stop <id or link>", "offline"}, []string{"revoke", "unshare"}},
		{"unshare --help", []string{"unshare", "--help"}, []string{"purlview unshare <id or link>", "Stop"}, nil},
		{"devices --help", []string{"devices", "--help"}, []string{"purlview devices", "signout"}, []string{"installation"}},
		{"devices signout --help", []string{"devices", "signout", "--help"}, []string{"purlview devices signout <name>", "--yes", "purlview logout"}, []string{"installation", "revoke"}},
		{"--version", []string{"--version"}, []string{"purlview 9.8.7\n", "0123456789abcdef", "go1.27.1 testos/testarch", "built by: test"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := run(t, tc.args...)
			if code != ExitOK {
				t.Fatalf("exit %d, want %d; stderr: %s", code, ExitOK, stderr)
			}
			if stderr != "" {
				t.Fatalf("stderr must be empty, got: %s", stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(stdout, w) {
					t.Errorf("stdout missing %q:\n%s", w, stdout)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(stdout, w) {
					t.Errorf("stdout must not contain %q:\n%s", w, stdout)
				}
			}
			if strings.Contains(stdout, "\x1b") {
				t.Errorf("help is never coloured when stdout is not a terminal:\n%q", stdout)
			}
		})
	}
}

// Colour is decided once from the environment and the terminal, never
// leaking into a pipe: without FORCE_COLOR a buffer gets plain bytes,
// NO_COLOR wins over FORCE_COLOR, and FORCE_COLOR alone colours status.
func TestColourIsDecidedOnce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env   map[string]string
		color bool
	}{
		"not a terminal":         {nil, false},
		"forced":                 {map[string]string{"FORCE_COLOR": "1"}, true},
		"forced off":             {map[string]string{"FORCE_COLOR": "0"}, false},
		"no colour wins":         {map[string]string{"FORCE_COLOR": "1", "NO_COLOR": "1"}, false},
		"no colour, any value":   {map[string]string{"NO_COLOR": "0"}, false},
		"forced, help unchanged": {map[string]string{"FORCE_COLOR": "true"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			dir := t.TempDir()
			getenv := func(k string) string {
				if k == daemon.EnvStateDir {
					return dir
				}
				return tc.env[k]
			}
			code := RunWithEnv(context.Background(), []string{"share", "--ttl", "2h", "3000"}, Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, testInfo, getenv)
			if code != ExitUsage || out.Len() != 0 {
				t.Fatalf("code %d stdout %q", code, out.String())
			}
			plain := "✗ --ttl 2h is over the 1h limit — use 1h or less\nRun 'purlview share --help' for usage.\n"
			coloured := "\x1b[31m✗\x1b[0m --ttl 2h is over the 1h limit \x1b[90m— use 1h or less\x1b[0m\n\x1b[90mRun 'purlview share --help' for usage.\x1b[0m\n"
			want := plain
			if tc.color {
				want = coloured
			}
			if errOut.String() != want {
				t.Fatalf("stderr %q, want %q", errOut.String(), want)
			}
			out.Reset()
			code = RunWithEnv(context.Background(), []string{"--help"}, Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, testInfo, getenv)
			if code != ExitOK || strings.Contains(out.String(), "\x1b") {
				t.Fatalf("help: code %d, escapes in %q", code, out.String())
			}
		})
	}
}

func TestVersionFirstLineIsNameAndVersion(t *testing.T) {
	t.Parallel()
	_, stdout, _ := run(t, "--version")
	if first := strings.SplitN(stdout, "\n", 2)[0]; first != "purlview 9.8.7" {
		t.Fatalf("first line %q, want %q", first, "purlview 9.8.7")
	}
}

// Signed out, the production wiring gives login instructions for the
// commands that need an account, without touching a daemon or the network.
func TestSignedOutCommandsGiveInstructions(t *testing.T) {
	t.Parallel()
	const notSignedIn = "✗ Not signed in — run purlview login first\n"
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"list"}, ExitError, notSignedIn},
		{[]string{"stop", "k7m2p4qx"}, ExitError, notSignedIn},
		{[]string{"unshare", "k7m2p4qx"}, ExitError, notSignedIn},
		{[]string{"link", "k7m2p4qx"}, ExitError, notSignedIn},
		{[]string{"whoami"}, ExitError, notSignedIn},
		{[]string{"devices"}, ExitError, notSignedIn},
		{[]string{"devices", "signout", "laptop"}, ExitError, notSignedIn},
		// login reads its answers from standard input when that is not a
		// terminal; an input that ends at once ends the sign-in.
		{[]string{"login"}, ExitError, "\nSign in to Purlview\n\n  Email: \n✗ Sign-in was not finished — run purlview login again\n"},
		{[]string{"login", "--no-browser"}, ExitUsage, "✗ Unknown flag: --no-browser\nRun 'purlview login --help' for usage.\n"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			code, stdout, stderr := runIn(t, dir, tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stderr: %s", code, tc.code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty, got: %s", stdout)
			}
			if stderr != tc.want {
				t.Fatalf("stderr %q, want %q", stderr, tc.want)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("state was created: %v", entries)
			}
		})
	}
}

// With a remembered credential, the production wiring reaches the missing
// platform and reports that honestly, never claiming a result.
func TestSignedInCommandsReportMissingPlatform(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		store := &envFileStore{getenv: func(k string) string {
			if k == daemon.EnvStateDir {
				return dir
			}
			return ""
		}}

		cred := &api.InstallationCredential{Identity: resource.Identity{Account: "creator@example.invalid", AccountID: "acc_1", Device: "dev_1", DeviceLabel: "studio"}, Token: "tok"}
		if err := store.Save(cred); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{[]string{"list"}, ExitError, "", "✗ Listing shares is not implemented in this scaffold build\n"},
		{[]string{"stop", "k7m2p4qx"}, ExitError, "", "✗ Stopping shares is not implemented in this scaffold build\n"},
		{[]string{"link", "k7m2p4qx"}, ExitError, "", "✗ Listing shares is not implemented in this scaffold build\n"},
		{[]string{"devices"}, ExitError, "", "✗ Listing devices is not implemented in this scaffold build\n"},
		// A sign-in that cannot be checked is shown as what this device
		// remembers, not checked.
		{[]string{"whoami"}, ExitOK, "Account  creator@example.invalid\nDevice   studio\nStatus   Signed in (not checked, offline)\n", ""},
		{[]string{"login"}, ExitError, "", "✗ Signing in is not implemented in this scaffold build\n"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runIn(t, seed(t), tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stderr: %s", code, tc.code, stderr)
			}
			if stdout != tc.stdout {
				t.Fatalf("stdout %q, want %q", stdout, tc.stdout)
			}
			if stderr != tc.stderr {
				t.Fatalf("stderr %q, want %q", stderr, tc.stderr)
			}
		})
	}
	// Logout clears the credential locally and calls nothing remote.
	dir := seed(t)
	code, stdout, stderr := runIn(t, dir, "logout")
	if code != ExitOK || stdout != "" || stderr != "✓ Signed out on this device — running shares continue until stopped or expired\n" {
		t.Fatalf("logout: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "state", "credentials.json.*")); len(files) != 0 {
		t.Fatal("logout must remove the credential file")
	}
	if code, _, stderr := runIn(t, dir, "whoami"); code != ExitError || !strings.Contains(stderr, "Not signed in") {
		t.Fatalf("after logout: %d %q", code, stderr)
	}
	// PURLVIEW_DEBUG=1 adds the cause on a second, dim line.
	var out, errOut bytes.Buffer
	dir = seed(t)
	getenv := func(k string) string {
		switch k {
		case daemon.EnvStateDir:
			return dir
		case EnvDebug:
			return "1"
		}
		return ""
	}
	code = RunWithEnv(context.Background(), []string{"list"}, Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, testInfo, getenv)
	if code != ExitError || errOut.String() != "✗ Listing shares is not implemented in this scaffold build\n  not implemented in this scaffold build\n" {
		t.Fatalf("debug: %d %q", code, errOut.String())
	}
}

func TestUsageErrorsExitTwoWithOneHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"unknown command", []string{"bogus"}, []string{"✗ bogus isn't a purlview command", "Run 'purlview --help' for usage."}},
		{"unknown command suggestion", []string{"shar"}, []string{"→ purlview share"}},
		{"unknown root flag", []string{"--bogus"}, []string{"✗ Unknown flag: --bogus", "Run 'purlview --help' for usage."}},
		{"unknown subcommand flag", []string{"share", "--bogus", "x"}, []string{"✗ Unknown flag: --bogus", "Run 'purlview share --help' for usage."}},
		{"withdrawn rewrite flag", []string{"share", "3000", "--rewrite-urls"}, []string{"✗ Unknown flag: --rewrite-urls", "Run 'purlview share --help' for usage."}},
		{"share without target", []string{"share"}, []string{"✗ share needs a port or an address — try purlview share 3000", "Run 'purlview share --help' for usage."}},
		{"share with an app named twice", []string{"share", "3000", "127.0.0.1:3000"}, []string{"✗ localhost:3000 is named twice — list each app once", "Run 'purlview share --help' for usage."}},
		{"share with too many apps", []string{"share", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}, []string{"✗ share names 11 apps — a share takes 10 at most"}},
		{"share bad port", []string{"share", "70000"}, []string{"✗ 70000 isn't a valid port — ports run from 1 to 65535"}},
		{"share host without port", []string{"share", "localhost"}, []string{"✗ localhost needs a port — try purlview share localhost:3000"}},
		{"share bad scheme", []string{"share", "ftp://host:21"}, []string{"✗ ftp addresses can't be shared — use http or https"}},
		{"share credentials in target", []string{"share", "http://u:p@localhost:3000"}, []string{"✗ An address with a username or password can't be shared — remove them; your app's own login still applies"}},
		{"share too long an address", []string{"share", "http://" + strings.Repeat("a", 250) + ".example:3000"}, []string{"✗ That address is too long — addresses are up to 256 characters", "Run 'purlview share --help' for usage."}},
		{"share excessive ttl", []string{"share", "localhost:3000", "--ttl", "2h"}, []string{"✗ --ttl 2h is over the 1h limit — use 1h or less"}},
		{"share bad ttl", []string{"share", "localhost:3000", "--ttl", "soon"}, []string{"✗ --ttl soon isn't a duration — use something like 15m"}},
		{"share zero ttl", []string{"share", "localhost:3000", "--ttl", "0s"}, []string{"✗ --ttl 0s is too short — use 1s or more"}},
		{"share bad recipient", []string{"share", "localhost:3000", "--to", "nobody"}, []string{"✗ nobody isn't an email address"}},
		{"share two recipients in one flag", []string{"share", "localhost:3000", "--to", "a@x.invalid,b@x.invalid"}, []string{"✗ a@x.invalid,b@x.invalid isn't one email address — repeat --to for each"}},
		{"share empty recipient", []string{"share", "localhost:3000", "--to", ""}, []string{"✗ --to needs an email address"}},
		{"share empty recipient beside a good one", []string{"share", "localhost:3000", "--to", "a@x.invalid", "--to", " "}, []string{"✗ --to needs an email address"}},
		{"share bad second recipient", []string{"share", "localhost:3000", "--to", "a@x.invalid", "--to", "nobody"}, []string{"✗ nobody isn't an email address"}},
		{"stop without id", []string{"stop"}, []string{"✗ stop needs a share id or link — see purlview list", "Run 'purlview stop --help' for usage."}},
		{"stop malformed", []string{"stop", "7K2M"}, []string{"✗ 7K2M isn't a share id or link — see purlview list"}},
		{"stop with a secret-length word", []string{"stop", "Zk3vQ9x7Lm2Np5RtYw8AbC"}, []string{"✗ That isn't a share id or link — see purlview list"}},
		{"unshare alias", []string{"unshare"}, []string{"✗ unshare needs a share id or link", "Run 'purlview unshare --help' for usage."}},
		{"link without id", []string{"link"}, []string{"✗ link needs a share id — see purlview list", "Run 'purlview link --help' for usage."}},
		{"link with a link", []string{"link", "https://k7m2p4qx.purlview.link/?token=Zk3vQ9x7Lm2Np5RtYw8AbC"}, []string{"✗ link takes a share id, not a link — try purlview link k7m2p4qx"}},
		{"signout without name", []string{"devices", "signout"}, []string{"✗ signout needs a device name — see purlview devices", "Run 'purlview devices signout --help' for usage."}},
		{"list with argument", []string{"list", "extra"}, []string{"✗ extra isn't a purlview list command", "Run 'purlview list --help' for usage."}},
		{"whoami with argument", []string{"whoami", "me"}, []string{"✗ me isn't a purlview whoami command"}},
		{"login with argument", []string{"login", "now"}, []string{"✗ now isn't a purlview login command"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			code, stdout, stderr := runIn(t, dir, tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit %d, want %d; stderr: %s", code, ExitUsage, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty for a usage error, got: %s", stdout)
			}
			if !strings.HasPrefix(stderr, "✗ ") {
				t.Fatalf("a usage error is one ✗ line: %q", stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr missing %q:\n%s", w, stderr)
				}
			}
			if n := strings.Count(stderr, "Run '"); n != 1 {
				t.Errorf("expected exactly one help hint, got %d:\n%s", n, stderr)
			}
			if strings.Contains(stderr, "token=") || strings.Contains(stderr, "Zk3vQ9x7Lm2Np5RtYw8AbC") {
				t.Errorf("a secret was echoed:\n%s", stderr)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("a usage error created state: %v", entries)
			}
		})
	}
}

func TestCompletionGeneratesEveryShellOnStdout(t *testing.T) {
	t.Parallel()
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := run(t, "completion", shell)
			if code != ExitOK {
				t.Fatalf("exit %d, want 0; stderr: %s", code, stderr)
			}
			if stderr != "" {
				t.Fatalf("stderr must be empty, got: %s", stderr)
			}
			if len(stdout) < 200 || !strings.Contains(stdout, "purlview") {
				t.Fatalf("completion script for %s looks wrong:\n%s", shell, stdout)
			}
		})
	}
}

func TestCompletionRejectsUnknownShell(t *testing.T) {
	t.Parallel()
	code, _, stderr := run(t, "completion", "tcsh")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d; stderr: %s", code, ExitUsage, stderr)
	}
}

// The product flag surface is exactly the reviewed one: nothing more. Every
// flag description ends with a period.
func TestProductFlagSurface(t *testing.T) {
	t.Parallel()
	root := New(testInfo, Streams{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	want := map[string][]string{
		"share":           {"background", "ttl", "to", "no-rewrite"},
		"list":            {},
		"link":            {},
		"stop":            {},
		"unshare":         {},
		"login":           {},
		"logout":          {},
		"whoami":          {},
		"devices":         {},
		"devices signout": {"yes"},
	}
	for name, flags := range want {
		cmd, _, err := root.Find(strings.Fields(name))
		if err != nil || cmd == nil || cmd.CommandPath() != "purlview "+name {
			t.Fatalf("command %q not found: %v", name, err)
		}
		for _, f := range flags {
			if cmd.Flags().Lookup(f) == nil {
				t.Errorf("%s: flag --%s missing", name, f)
			}
		}
		var usages []string
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if !strings.HasSuffix(f.Usage, ".") {
				t.Errorf("%s: --%s description must end with a period: %q", name, f.Name, f.Usage)
			}
			if f.Name != "help" {
				usages = append(usages, f.Name)
			}
		})
		if len(usages) != len(flags) {
			t.Errorf("%s: flag surface differs from the reviewed %v (new product flags need a design proposal): %v", name, flags, usages)
		}
	}
}

// The daemon command group is private plumbing: it works, but it is not
// offered in help or completion.
func TestDaemonCommandIsHiddenFromHelpAndCompletion(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"--help"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"completion", "powershell"}} {
		code, stdout, _ := run(t, args...)
		if code != ExitOK {
			t.Fatalf("%v: exit %d", args, code)
		}
		// Help lists commands as "  name   description"; completion scripts
		// name commands as bare words. Prose may mention the daemon.
		if len(args) > 0 && args[0] == "completion" {
			if strings.Contains(stdout, "daemon") {
				t.Errorf("%v: hidden daemon command leaked into completion", args)
			}
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			if strings.HasPrefix(strings.TrimLeft(line, " "), "daemon ") && strings.HasPrefix(line, "  ") {
				t.Errorf("%v: hidden daemon command listed in help: %q", args, line)
			}
		}
	}
	code, stdout, stderr := run(t, "daemon", "--help")
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, "run") || !strings.Contains(stdout, "stop") {
		t.Fatalf("daemon --help: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, _, stderr = run(t, "daemon", "bogus")
	if code != ExitUsage || !strings.Contains(stderr, "✗ bogus isn't a purlview daemon command") {
		t.Fatalf("daemon bogus: code=%d stderr=%q", code, stderr)
	}
	code, _, stderr = run(t, "daemon", "stop", "extra")
	if code != ExitUsage || !strings.Contains(stderr, "Run 'purlview daemon stop --help'") {
		t.Fatalf("daemon stop extra: code=%d stderr=%q", code, stderr)
	}
}

// The production wiring of a test that forgets its private state directory
// must not reach the developer's own credential or daemon: every command
// that needs per-user state stops with the isolation error instead.
func TestCommandsRefuseTheRealUserStateInsideATestBinary(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"whoami"},
		{"list"},
		{"logout"},
		{"share", "localhost:3000"},
		{"daemon", "status"},
		{"daemon", "start"},
		{"daemon", "stop"},
		{"daemon", "resume"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := RunWithEnv(context.Background(), args, Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, testInfo, func(string) string { return "" })
			if code == ExitOK || !strings.Contains(errOut.String(), daemon.ErrUnisolatedTest.Error()) || out.Len() != 0 {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
			}
		})
	}
}

func TestBuildingTheTreeCreatesNoState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == daemon.EnvStateDir {
			return dir
		}
		return ""
	}
	_ = NewWithEnv(testInfo, Streams{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, getenv)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("building the command tree created state: %v", entries)
	}
}
