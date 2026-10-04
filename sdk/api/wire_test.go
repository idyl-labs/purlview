package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// Every serialised example decodes strictly into its Go type, validates and
// encodes back to the same bytes: the tags and the fixtures cannot drift apart.
func TestWireFixturesRoundTrip(t *testing.T) {
	cases := map[string]any{
		"create-request.json":              &api.CreateShareRequest{},
		"create-response.json":             &api.ShareAccess{},
		"create-request-tunnel.json":       &api.CreateShareRequest{},
		"create-response-tunnel.json":      &api.ShareAccess{},
		"create-response-invites.json":     &api.ShareAccess{},
		"confirmed-revocation.json":        &api.RevokeShareResult{},
		"installations.json":               &api.ListInstallationsResult{},
		"installation-revoke-request.json": &api.RevokeInstallationRequest{},
		"installation-revocation.json":     &api.RevokeInstallationResult{},
		"login-denied.json":                &api.Error{},
		"share-limit-denied.json":          &api.Error{},
		"share-update-required.json":       &api.Error{},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			d := json.NewDecoder(bytes.NewReader(want))
			d.DisallowUnknownFields()
			if err = d.Decode(v); err != nil {
				t.Fatal(err)
			}
			if r, ok := v.(*api.CreateShareRequest); ok {
				r.Key = "attempt"
			}
			if c, ok := v.(interface{ Validate() error }); ok {
				if err = c.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, bytes.TrimSpace(want)) {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
		})
	}
	files, err := os.ReadDir("testdata")
	if err != nil || len(files) != len(cases) {
		t.Fatalf("testdata has %d files, %d are checked (%v)", len(files), len(cases), err)
	}
}

func TestLoginCode(t *testing.T) {
	for in, want := range map[string]string{"482913": "482913", "482 913": "482913", "482-913": "482913", " 482\u00a0913\n": "482913", "482\u2013913": "482913", "48291": "48291", "abc def": "abcdef"} {
		if got := api.NormaliseLoginCode(in); got != want {
			t.Errorf("NormaliseLoginCode(%q) = %q, want %q", in, got, want)
		}
	}
	if api.LoginCodeDigits != 6 || api.LoginAttempts != 5 {
		t.Fatalf("a sign-in code has six digits and five attempts: %d, %d", api.LoginCodeDigits, api.LoginAttempts)
	}
	for code, ok := range map[string]bool{"482913": true, "000000": true, "48291": false, "4829130": false, "12345678": false, "482 913": false, "48291a": false, "": false, "４８２９１３": false} {
		if got := (api.LoginVerifyRequest{ID: "auth_test", Code: code}).Validate() == nil; got != ok {
			t.Errorf("code %q valid = %v, want %v", code, got, ok)
		}
	}
}

func TestRecipients(t *testing.T) {
	many := make([]string, resource.MaxRecipients+1)
	for i := range many {
		many[i] = "r" + strings.Repeat("x", i) + "@example.invalid"
	}
	request := func(list ...string) error {
		return api.CreateShareRequest{Key: "k", Target: "http://localhost:3000", TTL: time.Hour, Recipients: list}.Validate()
	}
	for _, list := range [][]string{nil, {}, {"a@example.invalid"}, {"A@Example.invalid", "a@example.invalid", "b@example.invalid"}, many[:resource.MaxRecipients]} {
		if err := request(list...); err != nil {
			t.Errorf("%q: %v", list, err)
		}
	}
	for _, list := range [][]string{{""}, {"a@example.invalid", ""}, {"not-an-address"}, {"a@example.invalid;b@example.invalid"}, many} {
		if request(list...) == nil {
			t.Errorf("accepted %q", list)
		}
	}
	got := resource.NormaliseRecipients([]string{"B@example.invalid", "a@example.invalid", "b@EXAMPLE.invalid"})
	if strings.Join(got, ",") != "b@example.invalid,a@example.invalid" {
		t.Fatalf("normalised %q", got)
	}
	// Recorded state is always normalised; a response that is not fails closed.
	for _, list := range [][]string{{"A@example.invalid"}, {"a@example.invalid", "a@example.invalid"}, {""}, many} {
		if resource.Recipients(list) {
			t.Errorf("accepted recorded %q", list)
		}
	}
	a := api.CreateShareRequest{Key: "k", Target: "http://localhost:3000", TTL: time.Hour, Recipients: []string{"A@example.invalid", "b@example.invalid"}}
	b := a
	b.Recipients = []string{"a@example.invalid", "b@example.invalid", "A@example.invalid"}
	if !a.Same(b) || !a.Same(a.Normalised()) {
		t.Fatal("equivalent recipients must be the same attempt")
	}
	b.Recipients = []string{"b@example.invalid", "a@example.invalid"}
	if a.Same(b) {
		t.Fatal("order is part of the request")
	}
	b = a
	b.Recipients = nil
	if a.Same(b) {
		t.Fatal("a restricted and a secret-link request differ")
	}
}

// Several targets: the list names the first target's origin first,
// bare origins after it, at most MaxTargets, no target twice under any of its
// loopback names; the list is part of the request's identity.
func TestTargets(t *testing.T) {
	for key, want := range map[string]string{
		"http://localhost:5173":                 "http://localhost:5173",
		"http://127.0.0.1:5173/x?y=1":           "http://localhost:5173",
		"http://[::1]:5173":                     "http://localhost:5173",
		"HTTP://LocalHost:5173":                 "http://localhost:5173",
		"http://localhost":                      "http://localhost:80",
		"https://staging.example.invalid":       "https://staging.example.invalid:443",
		"https://staging.example.invalid:8443":  "https://staging.example.invalid:8443",
		"http://192.168.1.20:8080":              "http://192.168.1.20:8080",
		"localhost:5173":                        "",
		"ftp://localhost:21":                    "",
		"http://user@localhost:5173":            "",
		"":                                      "",
		"https://staging.example.invalid/x#top": "",
	} {
		if got := resource.TargetKey(key); got != want {
			t.Errorf("TargetKey(%q) = %q, want %q", key, got, want)
		}
	}
	many := make([]string, resource.MaxTargets+1)
	for i := range many {
		many[i] = "http://localhost:" + strconv.Itoa(5000+i)
	}
	for _, list := range [][]string{{"http://localhost:5173"}, {"http://localhost:5173/app?x=1", "http://localhost:8000"}, {"http://localhost:5173", "https://staging.example.invalid", "http://192.168.1.20:8080"}, many[:resource.MaxTargets]} {
		if !resource.Targets(list) || !resource.TargetsFor(list[0], list) {
			t.Errorf("refused %q", list)
		}
	}
	for _, list := range [][]string{nil, {}, {""}, {"localhost:5173"}, {"http://localhost:5173", "http://127.0.0.1:5173"}, {"http://localhost:5173", "http://[::1]:5173"}, {"http://localhost:5173", "http://localhost:8000/api"}, {"http://localhost:5173", "http://localhost:8000?x"}, {"http://localhost:5173", ""}, many} {
		if resource.Targets(list) {
			t.Errorf("accepted %q", list)
		}
	}
	if resource.TargetsFor("http://localhost:3000", []string{"http://localhost:5173", "http://localhost:8000"}) || !resource.TargetsFor("http://127.0.0.1:5173/page", []string{"http://localhost:5173", "http://localhost:8000"}) {
		t.Error("the first listed target must be the share's target")
	}
	one := api.CreateShareRequest{Key: "k", Target: "http://localhost:5173/app", TTL: time.Hour}
	several := one
	several.Targets = []string{"http://localhost:5173/app", "http://localhost:8000"}
	if one.Validate() != nil || several.Validate() != nil {
		t.Fatal("valid requests refused")
	}
	if one.Same(several) || !several.Same(several.Normalised()) {
		t.Fatal("the targets are part of the request")
	}
	for _, targets := range [][]string{{"http://localhost:8000"}, {"http://localhost:5173/app", "http://localhost:8000/api"}, many} {
		bad := one
		bad.Targets = targets
		if bad.Validate() == nil {
			t.Errorf("accepted targets %q", targets)
		}
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	sh := resource.Share{ID: "shr_test1", Origin: "https://share1.content.example.invalid", Target: "http://localhost:5173/app", Device: "dev_test", DeviceLabel: "test", State: "ready", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	sh.Targets = []string{"http://localhost:5173/app", "http://localhost:8000"}
	if sh.Validate() != nil {
		t.Fatal("valid share refused")
	}
	sh.Targets = []string{"http://localhost:8000", "http://localhost:5173/app"}
	if sh.Validate() == nil {
		t.Fatal("a share whose list does not start with its target")
	}
}

func TestShareAccessLinkShapeAndInvites(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	base := resource.Share{ID: "shr_test1", Origin: "https://share1.content.example.invalid", EntryOrigin: "https://share1.entry.example.invalid", Target: "http://localhost:3000", Device: "dev_test", DeviceLabel: "test", State: "ready", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	restricted := base
	restricted.Recipients = []string{"a@example.invalid", "b@example.invalid"}
	sent := []api.InviteResult{{Email: "a@example.invalid", Status: api.InviteSent}, {Email: "b@example.invalid", Status: api.InviteNotSent}}
	valid := []api.ShareAccess{
		{Share: base, URL: base.EntryOrigin + "/?token=secret"},
		{Share: restricted, URL: base.EntryOrigin + "/"},
		{Share: restricted, URL: base.EntryOrigin + "/", Invites: sent},
	}
	for i, v := range valid {
		if err := v.Validate(); err != nil {
			t.Errorf("valid %d: %v", i, err)
		}
	}
	invalid := []api.ShareAccess{
		{Share: base, URL: base.EntryOrigin + "/"},
		{Share: restricted, URL: base.EntryOrigin + "/?token=secret"},
		{Share: base, URL: base.EntryOrigin + "/?token=secret", Invites: sent[:1]},
		{Share: restricted, URL: base.EntryOrigin + "/", Invites: sent[:1]},
		{Share: restricted, URL: base.EntryOrigin + "/", Invites: []api.InviteResult{sent[1], sent[0]}},
		{Share: restricted, URL: base.EntryOrigin + "/", Invites: []api.InviteResult{sent[0], {Email: "b@example.invalid", Status: "queued"}}},
	}
	for i, v := range invalid {
		if v.Validate() == nil {
			t.Errorf("accepted invalid %d", i)
		}
	}
}

func TestInstallations(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	own := api.Installation{ID: "dev_a", Label: "a", CreatedAt: now, Current: true}
	other := api.Installation{ID: "dev_b", Label: "b", CreatedAt: now, LastUsedAt: now, ActiveShares: 2}
	// A caller that is not an installation sees none current, or none at all.
	for i, list := range [][]api.Installation{{other, own}, {other}, {}} {
		if err := (api.ListInstallationsResult{Installations: list}).Validate(); err != nil {
			t.Errorf("list %d: %v", i, err)
		}
	}
	second := own
	second.ID = "dev_c"
	bad := other
	bad.ActiveShares = -1
	for i, list := range [][]api.Installation{nil, {own, second}, {own, bad}, {own, {ID: "dev_d", CreatedAt: now}}} {
		if (api.ListInstallationsResult{Installations: list}).Validate() == nil {
			t.Errorf("accepted list %d", i)
		}
	}
	if (api.RevokeInstallationRequest{ID: "dev b"}).Validate() == nil || (api.RevokeInstallationRequest{}).Validate() == nil {
		t.Fatal("accepted a malformed installation id")
	}
	ok := api.RevokeInstallationResult{Revocation: api.Revocation{Outcome: "confirmed"}, ID: "dev_b", SharesStopped: 2}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, v := range []api.RevokeInstallationResult{{ID: "dev_b"}, {Revocation: ok.Revocation, SharesStopped: 1}, {Revocation: ok.Revocation, ID: "dev_b", SharesStopped: -1}, {Revocation: api.Revocation{Outcome: "unconfirmed", AlreadyEnded: true}, ID: "dev_b"}} {
		if v.Validate() == nil {
			t.Errorf("accepted result %d", i)
		}
	}
}
