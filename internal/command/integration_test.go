package command_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/account"
	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/command"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/internal/testplatform"
	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/apiserver"
	"github.com/idyl-labs/purlview/sdk/purlview"
)

func configuredSDK(t *testing.T, backend apiserver.Service) (command.Deps, *purlview.Client) {
	t.Helper()
	server := httptest.NewServer(apiserver.New("account.example.invalid", backend))
	t.Cleanup(server.Close)
	stateDir := t.TempDir()
	env := func(k string) string {
		switch k {
		case "PURLVIEW_PLATFORM_ENDPOINT":
			return server.URL
		case "PURLVIEW_PLATFORM_ALLOW_HTTP":
			return "1"
		case "PURLVIEW_PLATFORM_HOST":
			return "account.example.invalid"
		case "PURLVIEW_STATE_DIR":
			return stateDir
		}
		return ""
	}
	d := command.DefaultDeps(command.Streams{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, buildinfo.Info{}, env)
	d.Store = &account.MemoryStore{}
	d.Runner = noDaemon{}
	d.Hostname = func() string { return "test device" }
	d.Browser = command.BrowserFunc(func(string) error { t.Error("headless login opened browser"); return nil })
	c, e := purlview.New(purlview.Config{Endpoint: server.URL, Host: "account.example.invalid", AllowHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.CloseIdleConnections)
	return d, c
}

type noDaemon struct{}

func (noDaemon) Open(context.Context, bool) (share.Session, error) {
	return nil, share.ErrDaemonNotRunning
}
func executeSDK(d command.Deps, args ...string) (int, string, string) {
	return typed(d, "creator@example.invalid\n"+testplatform.Code+"\n", args...)
}

// typed runs a command whose prompts read these lines.
func typed(d command.Deps, input string, args ...string) (int, string, string) {
	var out, err bytes.Buffer
	lines := bufio.NewScanner(strings.NewReader(input))
	d.ReadLine = func(context.Context) (string, error) {
		if !lines.Scan() {
			return "", io.EOF
		}
		return lines.Text(), nil
	}
	d.ReadSecret = d.ReadLine
	code := command.RunWithDeps(context.Background(), args, command.Streams{In: strings.NewReader(""), Out: &out, Err: &err}, buildinfo.Info{}, d)
	return code, out.String(), err.String()
}

func TestSixDigitSignInAtTheTerminal(t *testing.T) {
	for name, tc := range map[string]struct {
		input string
		code  int
		want  string
	}{
		"as emailed":    {"creator@example.invalid\n482913\n", 0, "✓ Signed in as creator@example.invalid\n"},
		"with a space":  {"creator@example.invalid\n482 913\n", 0, "✓ Signed in as creator@example.invalid\n"},
		"with a dash":   {"creator@example.invalid\n482-913\n", 0, "✓ Signed in as creator@example.invalid\n"},
		"resent":        {"creator@example.invalid\nresend\n" + testplatform.Resent + "\n", 0, "  Code sent. Check your inbox.\n  Code: \n  Code sent. Check your inbox.\n  Code: \n\n✓ Signed in as"},
		"replaced code": {"creator@example.invalid\nresend\n" + testplatform.Code + "\n", 1, "✗ That code didn't match — 4 attempts left\n"},
		"wrong code":    {"creator@example.invalid\n000000\n", 1, "✗ That code didn't match — 4 attempts left\n"},
		"eight digits":  {"creator@example.invalid\n12345678\n", 1, "✗ That code didn't match — codes have 6 digits\n"},
		"five misses":   {"creator@example.invalid\n000000\n000000\n000000\n000000\n000000\n", 1, "✗ That code didn't match — 1 attempt left\n  Code: \n✗ That code has expired — run purlview login to get a new one\n"},
	} {
		t.Run(name, func(t *testing.T) {
			b := testplatform.New()
			// The fixture allows a resend a minute after its start; the CLI
			// keeps to that with the real clock, so start in the past.
			b.Now = b.Now.Add(-2 * time.Minute)
			d, _ := configuredSDK(t, b)
			code, out, stderr := typed(d, tc.input, "login")
			cred, _ := d.Store.Load()
			if code != tc.code || out != "" || !strings.Contains(stderr, tc.want) || (cred != nil) != (tc.code == 0) {
				t.Fatalf("%d %q %q", code, out, stderr)
			}
			if strings.Contains(stderr, testplatform.Code) || strings.Contains(stderr, testplatform.Resent) {
				t.Fatalf("a code must never be shown: %q", stderr)
			}
		})
	}
}

func TestInstallationAdapterOverHTTP(t *testing.T) {
	d, _ := configuredSDK(t, testplatform.New())
	devices, ok := d.Auth.(account.Installations)
	if !ok {
		t.Fatal("the configured CLI cannot manage installations")
	}
	cred := testplatform.Credential()
	ctx := context.Background()
	list, err := devices.ListInstallations(ctx, &cred)
	if err != nil || len(list) != 2 || !list[0].Current || list[1].ID != testplatform.OtherDevice {
		t.Fatalf("list: %+v %v", list, err)
	}
	res, err := devices.RevokeInstallation(ctx, &cred, testplatform.OtherDevice)
	if err != nil || res.AlreadyEnded || res.ID != testplatform.OtherDevice {
		t.Fatalf("revoke: %+v %v", res, err)
	}
	if _, err = devices.RevokeInstallation(ctx, &cred, "dev_elsewhere"); !account.IsKind(err, account.KindNotFound) {
		t.Fatalf("unknown installation: %v", err)
	}
	if list, err = devices.ListInstallations(ctx, &cred); err != nil || len(list) != 1 {
		t.Fatalf("list after revoke: %+v %v", list, err)
	}
	stale := cred
	stale.Token = "synthetic-other-secret"
	if _, err = devices.ListInstallations(ctx, &stale); !account.IsKind(err, account.KindUnauthorised) {
		t.Fatalf("stale credential: %v", err)
	}
}

func TestConfiguredCLIUsesSDKAcrossHTTP(t *testing.T) {
	backend := testplatform.New()
	d, c := configuredSDK(t, backend)
	code, out, stderr := executeSDK(d, "login")
	if code != 0 || out != "" || !strings.HasSuffix(stderr, "  Code sent. Check your inbox.\n  Code: \n\n✓ Signed in as creator@example.invalid\n") {
		t.Fatalf("login %d %s %s", code, out, stderr)
	}
	cred, e := d.Store.Load()
	if e != nil || cred.Token != testplatform.Token {
		t.Fatal("credential not saved by CLI custody")
	}
	code, out, stderr = executeSDK(d, "whoami")
	if code != 0 || !strings.HasSuffix(out, "Status   Signed in\n") || stderr != "" {
		t.Fatalf("whoami %d %s %s", code, out, stderr)
	}
	c = c.WithCredentials(func(context.Context) (string, error) { return cred.Token, nil })
	created, e := c.CreateShare(context.Background(), api.CreateShareRequest{Key: "cli_list", Target: "http://localhost:3000/demo", TTL: time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	label := strings.TrimPrefix(created.Share.ID, "shr_")
	code, out, stderr = executeSDK(d, "list")
	// The list is secret-free: the share by its label, never its link.
	if code != 0 || !strings.Contains(out, "● "+label+"  ") || strings.Contains(out, created.URL) || strings.Contains(out, "token=") || stderr != "" {
		t.Fatalf("list %d %s %s", code, out, stderr)
	}
	code, out, stderr = executeSDK(d, "link", created.Share.ID)
	if code != 0 || out != created.URL+"\n" || stderr != "" {
		t.Fatalf("link %d %q %q", code, out, stderr)
	}
	code, out, stderr = executeSDK(d, "stop", created.URL)
	if code != 0 || out != "" || stderr != "✓ Stopped "+label+" — the link no longer works\n" {
		t.Fatalf("stop %d %s %s", code, out, stderr)
	}
	code, out, stderr = executeSDK(d, "logout")
	if code != 0 || out != "" || !strings.Contains(stderr, "✓ Signed out on this device") {
		t.Fatalf("logout %d %s %s", code, out, stderr)
	}
	cred, e = d.Store.Load()
	if e != nil || cred != nil {
		t.Fatal("logout did not clear local custody")
	}
}

func TestConfiguredCLISurfacesNormalBackendHonestly(t *testing.T) {
	for _, args := range [][]string{{"login"}, {"whoami"}, {"list"}, {"link", "shr_test"}, {"stop", "shr_test"}, {"devices"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			d, _ := configuredSDK(t, nil)
			if args[0] != "login" {
				c := testplatform.Credential()
				if e := d.Store.Save(&c); e != nil {
					t.Fatal(e)
				}
			}
			code, out, stderr := executeSDK(d, args...)
			// whoami shows what this device remembers, not checked; every
			// other command needs the platform and says so.
			if args[0] == "whoami" {
				if code != 0 || !strings.HasSuffix(out, "Status   Signed in (not checked, offline)\n") {
					t.Fatalf("%d %s %s", code, out, stderr)
				}
			} else if code != 1 || out != "" || !strings.Contains(stderr, "is not implemented in this scaffold build") {
				t.Fatalf("%d %s %s", code, out, stderr)
			}
			if strings.Contains(out+stderr, testplatform.Token) || strings.Contains(stderr, "✓") {
				t.Fatal("false success or token leak")
			}
		})
	}
}

func TestPlatformConfigurationIsLazyForOfflineCommands(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, args := range [][]string{{"--help"}, {"--version"}, {"completion", "bash"}, {"share", "--ttl", "2h", "localhost:3000"}, {"unshare", "bad"}} {
		var reads atomic.Int32
		var out, err bytes.Buffer
		env := func(key string) string {
			// The colour decision reads its two conventional variables;
			// nothing else may be read.
			if key != "NO_COLOR" && key != "FORCE_COLOR" {
				reads.Add(1)
			}
			if key == "PURLVIEW_PLATFORM_ENDPOINT" {
				return server.URL
			}
			return ""
		}
		code := command.RunWithEnv(context.Background(), args, command.Streams{In: strings.NewReader("creator@example.invalid\n" + testplatform.Code + "\n"), Out: &out, Err: &err}, buildinfo.Info{}, env)
		if code != 0 && code != 2 {
			t.Fatalf("%v: %d %s", args, code, err.String())
		}
		if reads.Load() != 0 || requests.Load() != 0 {
			t.Fatalf("offline command %v read state/config or used network", args)
		}
	}
}

// readyTunnel is explicitly injected into this test. HTTP creation alone must
// never announce a ready URL or activate the normal daemon's unimplemented IPC.
type readyTunnel struct{ ready bool }
type readyConnection struct{ events chan engine.ConnEvent }

func (c *readyConnection) Events() <-chan engine.ConnEvent { return c.events }
func (c *readyConnection) Close() error                    { return nil }
func (t readyTunnel) ConnectServing(context.Context, api.InstallationCredential, string, engine.Serving) (engine.Connection, error) {
	ch := make(chan engine.ConnEvent, 1)
	if t.ready {
		ch <- engine.ConnEvent{Kind: engine.ConnReady}
	}
	return &readyConnection{events: ch}, nil
}

type readyProbe struct{}

func (readyProbe) Probe(context.Context, string) error { return nil }

func TestCLIShareEngineManagementUsesHTTP(t *testing.T) {
	backend := testplatform.New()
	d, c := configuredSDK(t, backend)
	cred := testplatform.Credential()
	if e := d.Store.Save(&cred); e != nil {
		t.Fatal(e)
	}
	e := engine.New(engine.Config{Platform: engine.ComposedPlatform{Management: engine.SDKManagement{Client: c}, TunnelClient: readyTunnel{ready: true}}, Prober: readyProbe{}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		e.Shutdown(ctx)
	})
	d.Runner = e
	d.NewID = func() string { return "http_engine_attempt" }
	code, out, stderr := executeSDK(d, "share", "localhost:3000/demo", "--background")
	if code != 0 || !strings.Contains(out, "?token=synthetic-recipient-secret") {
		t.Fatalf("share %d %s %s", code, out, stderr)
	}
	session, err := e.Open(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	records, err := session.Shares(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatal("missing engine share")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stopped, err := session.Stop(ctx, share.StopRequest{ID: records[0].ID, Reason: share.ReasonStopped})
	if err != nil || stopped.Remote.Status != share.RemoteConfirmed {
		t.Fatalf("stop: %v %v", stopped, err)
	}
	creates, revokes, count := backend.Counts()
	if creates != 1 || revokes != 1 || count != 1 {
		t.Fatalf("management was not exercised: %d %d %d", creates, revokes, count)
	}
}

func TestCLIShareWithSeveralRecipientsOverHTTP(t *testing.T) {
	backend := testplatform.New()
	backend.InviteStatus = api.InviteSent
	d, c := configuredSDK(t, backend)
	cred := testplatform.Credential()
	if e := d.Store.Save(&cred); e != nil {
		t.Fatal(e)
	}
	e := engine.New(engine.Config{Platform: engine.ComposedPlatform{Management: engine.SDKManagement{Client: c}, TunnelClient: readyTunnel{ready: true}}, Prober: readyProbe{}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		e.Shutdown(ctx)
	})
	d.Runner = e
	code, out, stderr := executeSDK(d, "share", "3000", "--background", "--to", "Ana@example.invalid", "--to", "raj@example.invalid", "--to", "ana@example.invalid")
	if code != 0 || strings.Contains(out, "token=") || !strings.Contains(stderr, "✓ Invites sent to ana@example.invalid and raj@example.invalid\n") || !strings.Contains(stderr, "Access   Only ana@example.invalid and raj@example.invalid\n") {
		t.Fatalf("share %d %s %s", code, out, stderr)
	}
	session, err := e.Open(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	records, err := session.Shares(context.Background())
	want := []share.Invite{{Email: "ana@example.invalid", Status: api.InviteSent}, {Email: "raj@example.invalid", Status: api.InviteSent}}
	if err != nil || len(records) != 1 || !slices.Equal(records[0].Recipients, []string{"ana@example.invalid", "raj@example.invalid"}) || !slices.Equal(records[0].Invites, want) {
		t.Fatalf("the daemon's record: %+v %v", records, err)
	}
	// Redirected stdout is exactly the link: "purlview share 3000 --background | pbcopy".
	if out != records[0].URL+"\n" {
		t.Fatalf("stdout %q, want the link alone", out)
	}
	code, out, stderr = executeSDK(d, "list")
	if code != 0 || strings.Contains(out, records[0].URL) || !strings.Contains(out, "ana@example.invalid +1") {
		t.Fatalf("list %d %s %s", code, out, stderr)
	}
}

func TestCLIUncertainWritesDoNotClaimFailureOrSuccess(t *testing.T) {
	t.Run("unshare unconfirmed", func(t *testing.T) {
		b := testplatform.New()
		b.RevokeHook = func(context.Context, string, api.RevokeShareRequest) (api.RevokeShareResult, error) {
			return api.RevokeShareResult{}, &api.Error{Code: api.Unavailable, Outcome: api.Unknown}
		}
		d, _ := configuredSDK(t, b)
		c := testplatform.Credential()
		if e := d.Store.Save(&c); e != nil {
			t.Fatal(e)
		}
		code, out, err := executeSDK(d, "stop", "https://share.content.example.invalid/access?token=synthetic-secret")
		if code != 1 || out != "" || !strings.HasPrefix(err, "✗ Couldn't confirm ") || !strings.Contains(err, " stopped — try again, or see purlview list\n") || strings.Contains(err, "synthetic-secret") {
			t.Fatalf("%d %s %s", code, out, err)
		}
	})
	t.Run("uncertain startup", func(t *testing.T) {
		b := testplatform.New()
		b.CreateHook = func(context.Context, string, api.CreateShareRequest) (api.ShareAccess, error) {
			return api.ShareAccess{}, &api.Error{Code: api.Unavailable, Outcome: api.Unknown}
		}
		d, c := configuredSDK(t, b)
		cred := testplatform.Credential()
		if err := d.Store.Save(&cred); err != nil {
			t.Fatal(err)
		}
		e := engine.New(engine.Config{Platform: engine.ComposedPlatform{Management: engine.SDKManagement{Client: c}, TunnelClient: readyTunnel{ready: true}}, Prober: readyProbe{}})
		defer e.Shutdown(context.Background())
		d.Runner = e
		code, out, err := executeSDK(d, "share", "localhost:3000", "--background")
		if code != 1 || out != "" || err != "✗ Couldn't reach Purlview — a share may have started; see purlview list\n" {
			t.Fatalf("%d %s %s", code, out, err)
		}
	})
}

type refusedInstallation struct{ apiserver.Unimplemented }

func (refusedInstallation) RevokeInstallation(context.Context, string) (api.Revocation, error) {
	return api.Revocation{}, &api.Error{Code: api.Unauthorised, Outcome: api.NotApplied}
}

func TestSDKLogoutDoesNotCallRemoteRevocation(t *testing.T) {
	d, _ := configuredSDK(t, refusedInstallation{})
	cred := testplatform.Credential()
	if e := d.Store.Save(&cred); e != nil {
		t.Fatal(e)
	}
	code, out, stderr := executeSDK(d, "logout")
	if code != 0 || out != "" || stderr != "✓ Signed out on this device — running shares continue until stopped or expired\n" {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	stored, e := d.Store.Load()
	if e != nil || stored != nil {
		t.Fatal("local credential was not cleared")
	}
}
