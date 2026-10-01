package share

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	t.Parallel()
	good := map[string]string{
		"localhost:3000":                           "http://localhost:3000",
		"LOCALHOST:3000":                           "http://localhost:3000",
		"127.0.0.1:8080":                           "http://127.0.0.1:8080",
		"[::1]:8080":                               "http://[::1]:8080",
		"http://localhost:3000":                    "http://localhost:3000",
		"https://internal.example":                 "https://internal.example",
		"HTTPS://Internal.Example/Demo":            "https://internal.example/Demo",
		"http://localhost:3000/app/index.html":     "http://localhost:3000/app/index.html",
		"http://localhost:3000/search?q=a%20b&x=1": "http://localhost:3000/search?q=a%20b&x=1",
		"localhost:3000/dashboard?tab=2":           "http://localhost:3000/dashboard?tab=2",
		"my-app.internal:9000":                     "http://my-app.internal:9000",
		"http://localhost:3000/":                   "http://localhost:3000/",
		"3000":                                     "http://localhost:3000",
		":3000":                                    "http://localhost:3000",
		"3000/dashboard?tab=2":                     "http://localhost:3000/dashboard?tab=2",
		":8080/":                                   "http://localhost:8080/",
		"192.168.1.20:8080":                        "http://192.168.1.20:8080",
		// The length bound is on the origin: a long start page is not counted.
		"3000/app?token=" + strings.Repeat("t", 300):                "http://localhost:3000/app?token=" + strings.Repeat("t", 300),
		"http://" + strings.Repeat("a", 230) + ".example:3000/page": "http://" + strings.Repeat("a", 230) + ".example:3000/page",
	}
	for in, want := range good {
		got, err := ParseTarget(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if got.URL != want {
			t.Errorf("%q: got %q, want %q", in, got.URL, want)
		}
	}
	bad := map[string]string{
		"":                              "share needs a port or an address — try purlview share 3000",
		"70000":                         "70000 isn't a valid port — ports run from 1 to 65535",
		":0":                            "0 isn't a valid port — ports run from 1 to 65535",
		"localhost":                     "localhost needs a port — try purlview share localhost:3000",
		"my-app.internal":               "my-app.internal needs a port — try purlview share my-app.internal:3000",
		"localhost:99999":               "99999 isn't a valid port",
		"localhost:abc":                 "abc isn't a valid port",
		"ftp://host:21":                 "ftp addresses can't be shared — use http or https",
		"http://user:pw@localhost:3000": "An address with a username or password can't be shared — remove them; your app's own login still applies",
		"http://localhost:3000/#top":    "The part after # never reaches an app — remove it",
		"http://localhost:3000#":        "The part after # never reaches an app",
		"http://":                       "http:// isn't a port or an address — try purlview share 3000",
		"local host:3000":               "isn't a port or an address",
		"http://bad host:3000":          "isn't a port or an address",
		"http://[::1:3000":              "isn't a port or an address",
		"http://exa mple.com":           "isn't a port or an address",
		"http://..:3000":                "isn't a port or an address",
		"http://" + strings.Repeat("a", 250) + ".example:3000": "That address is too long — addresses are up to 256 characters",
	}
	for in, want := range bad {
		_, err := ParseTarget(in)
		if err == nil {
			t.Errorf("%q: expected an error", in)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q does not mention %q", in, err, want)
		}
	}
}

func TestParseTTL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{"": time.Hour, "1h": time.Hour, "15m": 15 * time.Minute, "90s": 90 * time.Second, "1h0m0s": time.Hour, "1s": time.Second} {
		got, err := ParseTTL(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v %v, want %v", in, got, err, want)
		}
	}
	for in, want := range map[string]string{"abc": "--ttl abc isn't a duration — use something like 15m", "0": "--ttl 0 is too short — use 1s or more", "-5m": "--ttl -5m is too short", "500ms": "--ttl 500ms is too short", "61m": "--ttl 61m is over the 1h limit — use 1h or less", "2h": "--ttl 2h is over the 1h limit — use 1h or less", "1h1s": "over the 1h limit"} {
		_, err := ParseTTL(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v does not mention %q", in, err, want)
		}
	}
}

func TestParseRecipients(t *testing.T) {
	t.Parallel()
	got, err := ParseRecipients([]string{"Ana@Example.Invalid", " raj@example.invalid ", "ana@example.invalid"})
	if err != nil || strings.Join(got, ",") != "ana@example.invalid,raj@example.invalid" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err = ParseRecipients(nil); err != nil || got != nil {
		t.Fatalf("no --to means anyone with the link: %q %v", got, err)
	}
	var many []string
	for i := range 11 {
		many = append(many, string(rune('a'+i))+"@example.invalid")
	}
	if got, err = ParseRecipients(append(slices.Clone(many[:10]), many[0])); err != nil || len(got) != 10 {
		t.Fatalf("ten distinct addresses: %q %v", got, err)
	}
	// An empty value must never widen access to anyone with the link.
	for _, list := range [][]string{many, {"ana@example.invalid", "nobody"}, {"a@b.c,d@e.f"}, {""}, {"  "}, {"ana@example.invalid", ""}} {
		if _, err = ParseRecipients(list); err == nil {
			t.Errorf("accepted %q", list)
		}
	}
}

func TestDisplayTarget(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"http://localhost:3000": "localhost:3000", "http://localhost:3000/demo?x=1": "localhost:3000", "https://staging.internal/demo": "staging.internal", "http://[::1]:8080": "[::1]:8080", "not a url": "not a url"} {
		if got := DisplayTarget(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestParseRecipient(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"Reviewer@Example.Invalid": "reviewer@example.invalid", "a.b+c@sub.example.invalid": "a.b+c@sub.example.invalid"} {
		got, err := ParseRecipient(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v", in, got, err)
		}
	}
	for in, want := range map[string]string{"": "--to needs an email address", "reviewer": "reviewer isn't an email address", "@example.invalid": "@example.invalid isn't an email address", "a@b": "a@b isn't an email address", "a@@b.c": "isn't an email address", "a@b.c, d@e.f": "a@b.c, d@e.f isn't one email address — repeat --to for each", "a@b.c d@e.f": "isn't one email address — repeat --to for each", "\"a\"@b.c": "isn't an email address"} {
		_, err := ParseRecipient(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v does not mention %q", in, err, want)
		}
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()
	// The label people type and the API's canonical id name the same share;
	// both display as the bare label.
	for _, in := range []string{"k7m2p4qx", "shr_k7m2p4qx", " k7m2p4qx\n"} {
		if r, err := ParseRef(in); err != nil || r.ID != "shr_k7m2p4qx" || r.URL != "" || r.String() != "k7m2p4qx" {
			t.Fatalf("%q: %+v %v", in, r, err)
		}
	}
	for in, want := range map[string]string{
		"https://k7m2p4qx.purlview.link/?token=Zk3vQ9x7Lm2Np5RtYw8AbC": "https://k7m2p4qx.purlview.link",
		"https://k7m2p4qx.purlview.link/":                              "https://k7m2p4qx.purlview.link",
		"https://k7m2p4qx.purlview.invalid/demo?version=2":             "https://k7m2p4qx.purlview.invalid",
		"https://K7M2P4QX.staging.purlview.invalid/some/path?x=1":      "https://k7m2p4qx.staging.purlview.invalid",
	} {
		// A link names its share by the label in its hostname.
		if r, err := ParseRef(in); err != nil || r.ID != "" || r.URL != want || r.String() != "k7m2p4qx" {
			t.Fatalf("%q: %+v %v", in, r, err)
		}
	}
	if r, _ := ParseRef("https://demo.example/app"); r.String() != "https://demo.example" {
		t.Fatalf("a URL without a label is shown as its origin: %q", r.String())
	}
	// What is short enough to be a mistyped id is repeated; anything longer
	// may be a secret and is not.
	if _, err := ParseRef("7K2M"); err == nil || err.Error() != "7K2M isn't a share id or link — see purlview list" {
		t.Fatalf("mistyped id: %v", err)
	}
	if _, err := ParseRef("Zk3vQ9x7Lm2Np5RtYw8AbC"); err == nil || err.Error() != "That isn't a share id or link — see purlview list" {
		t.Fatalf("a token-length word: %v", err)
	}
	// A bare word must have the label shape: eight characters, a leading
	// letter, and none of 0 1 i l o u or the vowels.
	for _, in := range []string{"", "shr_", "shr_7K/2M", "7K2M", "k7m2p4q", "k7m2p4qxz", "27m2p4qx", "K7M2P4QX", "k7m2p4q0", "k7m2p4ql", "k7m2p4qa", "k7m2-4qx", "k7m2p4qx.example.invalid", "ftp://x.y", "https://"} {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

// Display drops the prefix; the management boundary must still carry the
// canonical id, whatever shape the platform gives it.
func TestRefCleanKeepsCanonicalID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"shr_k7m2p4qx", "shr_test1"} {
		ref, err := ParseRef(id)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ref.clean(); err != nil || got.ID != id || got.URL != "" {
			t.Errorf("%s: %+v %v", id, got, err)
		}
	}
	if got, err := (Ref{URL: "https://k7m2p4qx.purlview.link/?token=synthetic-secret"}).clean(); err != nil || got.URL != "https://k7m2p4qx.purlview.link" {
		t.Errorf("url: %+v %v", got, err)
	}
}

func TestLabel(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"shr_k7m2p4qx": "k7m2p4qx", "k7m2p4qx": "k7m2p4qx", "": ""} {
		if got := Label(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestFormatDurations(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{time.Hour: "1h", 90 * time.Minute: "1h30m", 15 * time.Minute: "15m", 45 * time.Second: "45s", 61 * time.Second: "1m1s", 0: "0s", time.Hour + 30*time.Second: "1h"} {
		if got := FormatDuration(d); got != want {
			t.Errorf("%v: got %q, want %q", d, got, want)
		}
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for left, want := range map[time.Duration]string{time.Hour: "1h", time.Hour - 800*time.Millisecond: "1h", 43*time.Minute + 20*time.Second: "43m", 29 * time.Second: "29s", 0: "expired", -time.Minute: "expired"} {
		if got := FormatRemaining(now, now.Add(left)); got != want {
			t.Errorf("%v left: got %q, want %q", left, got, want)
		}
	}
}

func TestEntryAndOrigin(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][2]string{
		"https://internal.example/demo?version=2":       {"/demo?version=2", "https://internal.example"},
		"http://localhost:3000/dashboard?tab=2":         {"/dashboard?tab=2", "http://localhost:3000"},
		"http://localhost:3000/app/":                    {"/app/", "http://localhost:3000"},
		"http://localhost:3000?x=1":                     {"/?x=1", "http://localhost:3000"},
		"http://localhost:3000/search?q=a%20b&sort=asc": {"/search?q=a%20b&sort=asc", "http://localhost:3000"},
	} {
		entry, origin, ok := EntryAndOrigin(in)
		if !ok || entry != want[0] || origin != want[1] {
			t.Errorf("%q: got %q %q %v, want %q %q", in, entry, origin, ok, want[0], want[1])
		}
	}
	for _, in := range []string{"http://localhost:3000", "http://localhost:3000/", "https://internal.example", "not a url"} {
		if _, _, ok := EntryAndOrigin(in); ok {
			t.Errorf("%q: root or invalid targets have no entry page", in)
		}
	}
}

func TestManagementReferenceSeparatesIdentityAndSecrets(t *testing.T) {
	t.Parallel()
	const origin = "https://k7m2p4qx.purlview.invalid"
	for _, raw := range []string{origin, origin + "/?token=synthetic-secret", origin + "/open?token=synthetic-secret", origin + "/app?token=application-token#fragment"} {
		ref, err := ParseRef(raw)
		if err != nil || ref.URL != origin || ref.String() != "k7m2p4qx" {
			t.Errorf("identity resolution: %+v, %v", ref, err)
		}
	}
	for _, raw := range []string{"https://bad%host/?token=synthetic-secret", "https://user:synthetic-secret@host", "ftp://host/?token=synthetic-secret", "shr_bad?token=synthetic-secret", "synthetic-secret"} {
		_, err := ParseRef(raw)
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Errorf("malformed reference must fail without echoing input: %v", err)
		}
		if strings.Contains((Ref{URL: raw}).String(), "synthetic-secret") {
			t.Error("diagnostic reference leaked credentials")
		}
	}
}
