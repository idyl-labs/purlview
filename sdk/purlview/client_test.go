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

package purlview_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/purlview"
	"github.com/idyl-labs/purlview/sdk/resource"
)

const token = "synthetic-installation-secret"
const pollToken = "synthetic-poll-secret"

func identityFixture() resource.Identity {
	return resource.Identity{Account: "creator@example.invalid", AccountID: "acc_test", Device: "dev_test", DeviceLabel: "test device"}
}

const host = "account.example.invalid"

func client(t *testing.T, h http.Handler, token string) *purlview.Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, err := purlview.New(purlview.Config{Endpoint: s.URL, Host: host, AllowHTTP: true, Credentials: func(context.Context) (string, error) { return token, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}
func checkError(t *testing.T, err error, code api.Code, outcome api.Outcome) *api.Error {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Code != code || e.Outcome != outcome {
		t.Fatalf("error = %#v (%v), want %s/%s", err, err, code, outcome)
	}
	return e
}
func TestResponseCompatibilitySafetyAndNoRetries(t *testing.T) {
	identity, _ := json.Marshal(identityFixture())
	cases := []struct {
		name, body string
		status     int
		version    string
		code       api.Code
		outcome    api.Outcome
	}{
		{"empty", "{}", 200, "1", api.ProtocolError, api.NotApplied},
		{"null", "null", 200, "1", api.ProtocolError, api.NotApplied},
		{"trailing", string(identity) + ` {}`, 200, "1", api.ProtocolError, api.NotApplied},
		{"unknown field", strings.TrimSuffix(string(identity), "}") + `,"future":true}`, 200, "1", "", api.NotApplied},
		{"unknown code", `{"error":"future_failure","message":"synthetic-recipient-secret","outcome":"not_applied","retryable":true}`, 429, "1", "future_failure", api.NotApplied},
		{"wrong version", string(identity), 200, "2", api.ProtocolError, api.NotApplied},
		{"untyped error", "oops synthetic-recipient-secret", 503, "1", api.ProtocolError, api.NotApplied},
		{"oversize", `{"padding":"` + strings.Repeat("x", api.MaxResponseBytes) + `"}`, 200, "1", api.ProtocolError, api.NotApplied},
		{"wrong internal status", `{"error":"internal_error","outcome":"unknown"}`, 409, "1", api.ProtocolError, api.NotApplied},
		{"wrong status", `{"error":"unauthorised","outcome":"not_applied"}`, 503, "1", api.ProtocolError, api.NotApplied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := client(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(api.VersionHeader, tc.version)
				w.Header().Set(api.RequestIDHeader, "req_test")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), token)
			_, err := c.Identity(context.Background())
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				e := checkError(t, err, tc.code, tc.outcome)
				if e.RequestID != "req_test" || strings.Contains(err.Error(), "secret") {
					t.Fatal("unsafe or missing diagnostic metadata")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("automatic retry")
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		var hits atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		defer other.Close()
		c := client(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/?token=synthetic-secret", http.StatusTemporaryRedirect)
		}), token)
		_, err := c.Identity(context.Background())
		_ = checkError(t, err, api.ProtocolError, api.NotApplied)
		if hits.Load() != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatal("credential redirect escaped")
		}
	})
}

func TestClientValidationBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	c := client(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }), token)
	ctx := context.Background()
	for _, r := range []api.CreateShareRequest{{Key: "x", Target: "http://localhost:3000", TTL: 2 * time.Hour}, {Key: "bad\r\nkey", Target: "http://localhost", TTL: time.Hour}, {Key: "x", Target: "https://user:secret@example.invalid", TTL: time.Hour}, {Key: "x", Target: "http://localhost", TTL: time.Hour, Recipients: []string{"one@example.invalid;two@example.invalid"}}, {Key: "x", Target: "http://localhost", TTL: time.Hour, Recipients: make([]string, 11)}} {
		_, e := c.CreateShare(ctx, r)
		_ = checkError(t, e, api.InvalidRequest, api.NotApplied)
	}
	_, e := c.RevokeShare(ctx, api.RevokeShareRequest{Reference: resource.ShareRef{Origin: "https://example.invalid/access?token=secret"}})
	_ = checkError(t, e, api.InvalidRequest, api.NotApplied)
	_, e = c.BeginAuthorization(ctx, api.BeginAuthorizationRequest{DeviceLabel: "bad\nlabel"})
	_ = checkError(t, e, api.InvalidRequest, api.NotApplied)
	_, e = c.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: "not an id"})
	_ = checkError(t, e, api.InvalidRequest, api.NotApplied)
	for _, code := range []string{"12345678", "482 913", "48291"} {
		_, e = c.VerifyLogin(ctx, api.LoginVerifyRequest{ID: "auth_test", Code: code})
		_ = checkError(t, e, api.InvalidRequest, api.NotApplied)
	}
	if hits.Load() != 0 {
		t.Fatal("invalid input reached network")
	}
	for _, endpoint := range []string{"http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid/?token=secret", "https://example.invalid/path"} {
		if _, e = purlview.New(purlview.Config{Endpoint: endpoint, AllowHTTP: true}); e == nil {
			t.Fatalf("accepted endpoint %s", endpoint)
		}
	}
}

func respond(t *testing.T, status int, body string, seen *string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			b, _ := io.ReadAll(r.Body)
			*seen = r.Method + " " + r.URL.Path + " " + string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(api.VersionHeader, "1")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func TestWrongCodeReportsAttemptsLeft(t *testing.T) {
	c := client(t, respond(t, 403, `{"error":"denied","message":"x","outcome":"not_applied","retryable":false,"attempts_left":4}`, nil), token)
	_, err := c.VerifyLogin(context.Background(), api.LoginVerifyRequest{ID: "auth_test", Code: "482913"})
	if e := checkError(t, err, api.Denied, api.NotApplied); e.AttemptsLeft != 4 {
		t.Fatalf("attempts left = %d", e.AttemptsLeft)
	}
	c = client(t, respond(t, 410, `{"error":"expired","message":"x","outcome":"not_applied","retryable":false}`, nil), token)
	_, err = c.VerifyLogin(context.Background(), api.LoginVerifyRequest{ID: "auth_test", Code: "482913"})
	if e := checkError(t, err, api.Expired, api.NotApplied); e.AttemptsLeft != 0 {
		t.Fatalf("attempts left = %d", e.AttemptsLeft)
	}
	// Only denied carries the value; on anything else it is dropped.
	c = client(t, respond(t, 410, `{"error":"expired","message":"x","outcome":"not_applied","retryable":false,"attempts_left":3}`, nil), token)
	_, err = c.VerifyLogin(context.Background(), api.LoginVerifyRequest{ID: "auth_test", Code: "482913"})
	if e := checkError(t, err, api.Expired, api.NotApplied); e.AttemptsLeft != 0 {
		t.Fatalf("attempts left on expired = %d", e.AttemptsLeft)
	}
	c = client(t, respond(t, 403, `{"error":"denied","message":"x","outcome":"not_applied","retryable":false}`, nil), token)
	_, err = c.VerifyLogin(context.Background(), api.LoginVerifyRequest{ID: "auth_test", Code: "482913"})
	if e := checkError(t, err, api.Denied, api.NotApplied); e.AttemptsLeft != 0 {
		t.Fatalf("attempts left = %d", e.AttemptsLeft)
	}
	for left, ok := range map[int]bool{-1: false, 1: true, api.LoginAttempts - 1: true, api.LoginAttempts: false, 1000: false} {
		c = client(t, respond(t, 403, fmt.Sprintf(`{"error":"denied","message":"x","outcome":"not_applied","retryable":false,"attempts_left":%d}`, left), nil), token)
		_, err = c.VerifyLogin(context.Background(), api.LoginVerifyRequest{ID: "auth_test", Code: "482913"})
		if ok {
			if e := checkError(t, err, api.Denied, api.NotApplied); e.AttemptsLeft != left {
				t.Fatalf("attempts left = %d, want %d", e.AttemptsLeft, left)
			}
		} else {
			_ = checkError(t, err, api.ProtocolError, api.Unknown)
		}
	}
}

func TestDeniedCreateNamesTheLimit(t *testing.T) {
	req := api.CreateShareRequest{Key: "attempt", Target: "http://localhost:3000", TTL: time.Hour}
	for _, limit := range []api.Limit{api.LimitRunningShares, api.LimitSharesPerHour, "a_future_limit"} {
		c := client(t, respond(t, 403, `{"error":"denied","message":"x","outcome":"not_applied","retryable":false,"limit":"`+string(limit)+`"}`, nil), token)
		_, err := c.CreateShare(context.Background(), req)
		if e := checkError(t, err, api.Denied, api.NotApplied); e.Limit != limit {
			t.Fatalf("limit = %q, want %q", e.Limit, limit)
		}
	}
	// Only denied carries a limit; on anything else it is dropped.
	c := client(t, respond(t, 400, `{"error":"invalid_request","message":"x","outcome":"not_applied","retryable":false,"limit":"running_shares"}`, nil), token)
	_, err := c.CreateShare(context.Background(), req)
	if e := checkError(t, err, api.InvalidRequest, api.NotApplied); e.Limit != "" {
		t.Fatalf("limit on invalid_request = %q", e.Limit)
	}
	c = client(t, respond(t, 403, `{"error":"denied","message":"x","outcome":"not_applied","retryable":false}`, nil), token)
	_, err = c.CreateShare(context.Background(), req)
	if e := checkError(t, err, api.Denied, api.NotApplied); e.Limit != "" {
		t.Fatalf("limit = %q", e.Limit)
	}
}

func TestClientHeaderAndUpdateRequired(t *testing.T) {
	var got string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(api.ClientHeader)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(api.VersionHeader, "1")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = io.WriteString(w, `{"error":"update_required","message":"x","outcome":"not_applied","retryable":false}`)
	})
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, err := purlview.New(purlview.Config{Endpoint: s.URL, Host: host, AllowHTTP: true, Client: "purlview/0.2.0",
		Credentials: func(context.Context) (string, error) { return token, nil }})
	if err != nil {
		t.Fatal(err)
	}
	req := api.CreateShareRequest{Key: "attempt", Target: "http://localhost:3000", TTL: time.Hour}
	_, err = c.CreateShare(context.Background(), req)
	if e := checkError(t, err, api.UpdateRequired, api.NotApplied); e.Status != http.StatusUpgradeRequired {
		t.Fatalf("status = %d", e.Status)
	}
	if got != "purlview/0.2.0" {
		t.Fatalf("%s = %q", api.ClientHeader, got)
	}
	// Without a client name no header is sent.
	_, _ = client(t, h, token).CreateShare(context.Background(), req)
	if got != "" {
		t.Fatalf("%s sent without a client name: %q", api.ClientHeader, got)
	}
	for _, bad := range []string{"purlview 0.2", "purlview/\n", strings.Repeat("x", 65)} {
		if _, err := purlview.New(purlview.Config{Endpoint: s.URL, AllowHTTP: true, Client: bad}); err == nil {
			t.Errorf("client name %q must refuse", bad)
		}
	}
}

func TestCreateWithAShareKeyGetsItsTunnel(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("../api/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	var fixture api.CreateShareRequest
	if err := json.Unmarshal([]byte(read("create-request-tunnel.json")), &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Key = "attempt"
	withTunnel, withoutTunnel := read("create-response-tunnel.json"), read("create-response.json")

	var seen string
	got, err := client(t, respond(t, 200, withTunnel, &seen), token).CreateShare(context.Background(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tunnel == nil || got.Tunnel.Endpoint != "edge.example.invalid:443" || !strings.Contains(seen, `"public_key":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`) {
		t.Fatalf("tunnel = %+v, sent %s", got.Tunnel, seen)
	}
	// A key without a tunnel, or a tunnel without a key, is a broken reply.
	_, err = client(t, respond(t, 200, withoutTunnel, nil), token).CreateShare(context.Background(), fixture)
	_ = checkError(t, err, api.ProtocolError, api.Unknown)
	// A replay that finds the share ended carries no tunnel.
	ended := strings.Replace(withoutTunnel, `"state":"starting"`, `"state":"ended"`, 1)
	if got, err := client(t, respond(t, 200, ended, nil), token).CreateShare(context.Background(), fixture); err != nil || got.Tunnel != nil {
		t.Fatalf("ended replay: %v, %+v", err, got)
	}
	plain := fixture
	plain.PublicKey = nil
	_, err = client(t, respond(t, 200, withTunnel, nil), token).CreateShare(context.Background(), plain)
	_ = checkError(t, err, api.ProtocolError, api.Unknown)
	// A key that isn't 32 bytes never leaves the client.
	short := fixture
	short.PublicKey = fixture.PublicKey[:31]
	_, err = client(t, respond(t, 200, withTunnel, nil), token).CreateShare(context.Background(), short)
	_ = checkError(t, err, api.InvalidRequest, api.NotApplied)
}

func TestCreateSendsRecordedRecipientsAndChecksTheReply(t *testing.T) {
	const share = `{"share":{"id":"shr_test1","origin":"https://share1.content.example.invalid","target":"http://localhost:3000","device":"dev_test","device_label":"test device","recipients":%s,"rewrite_urls":false,"state":"starting","created_at":"2026-09-16T12:00:00Z","expires_at":"2026-09-16T13:00:00Z"},"url":"https://share1.content.example.invalid/"%s}`
	req := api.CreateShareRequest{Key: "attempt", Target: "http://localhost:3000", TTL: time.Hour, Recipients: []string{"Ana@example.invalid", "raj@example.invalid", "ana@example.invalid"}}
	var seen string
	invites := `,"invites":[{"email":"ana@example.invalid","status":"sent"},{"email":"raj@example.invalid","status":"not_sent"}]`
	c := client(t, respond(t, 200, fmt.Sprintf(share, `["ana@example.invalid","raj@example.invalid"]`, invites), &seen), token)
	got, err := c.CreateShare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if want := `POST /api/v1/shares {"target":"http://localhost:3000","ttl_nanoseconds":3600000000000,"recipients":["ana@example.invalid","raj@example.invalid"],"rewrite_urls":false}`; seen != want {
		t.Fatalf("sent %s", seen)
	}
	if len(got.Invites) != 2 || got.Invites[1] != (api.InviteResult{Email: "raj@example.invalid", Status: api.InviteNotSent}) {
		t.Fatalf("invites %v", got.Invites)
	}
	for _, recipients := range []string{`["ana@example.invalid"]`, `["raj@example.invalid","ana@example.invalid"]`, `["Ana@example.invalid","raj@example.invalid"]`} {
		c = client(t, respond(t, 200, fmt.Sprintf(share, recipients, ""), nil), token)
		_, err = c.CreateShare(context.Background(), req)
		_ = checkError(t, err, api.ProtocolError, api.Unknown)
	}
}

func TestInstallationManagement(t *testing.T) {
	var seen string
	list := `{"installations":[{"id":"dev_test","label":"test device","created_at":"2026-09-16T12:00:00Z","active_shares":1,"current":true},{"id":"dev_other","label":"other","created_at":"2026-09-02T09:30:00Z","last_used_at":"2026-09-16T11:00:00Z","active_shares":0,"current":false}]}`
	c := client(t, respond(t, 200, list, &seen), token)
	got, err := c.ListInstallations(context.Background())
	if err != nil || seen != "GET /api/v1/installations " || len(got.Installations) != 2 || !got.Installations[0].LastUsedAt.IsZero() || got.Installations[1].LastUsedAt.IsZero() {
		t.Fatalf("list: %v %v (%s)", got, err, seen)
	}
	c = client(t, respond(t, 200, `{"installations":[]}`, nil), token)
	if got, err = c.ListInstallations(context.Background()); err != nil || len(got.Installations) != 0 {
		t.Fatalf("a caller that is not an installation may see an empty list: %v %v", got, err)
	}
	for _, body := range []string{`{}`, `{"installations":null}`, strings.Replace(list, `"current":false`, `"current":true`, 1)} {
		c = client(t, respond(t, 200, body, nil), token)
		_, err = c.ListInstallations(context.Background())
		_ = checkError(t, err, api.ProtocolError, api.NotApplied)
	}
	c = client(t, respond(t, 200, `{"outcome":"confirmed","already_ended":false,"id":"dev_other","shares_stopped":2}`, &seen), token)
	revoked, err := c.RevokeInstallationByID(context.Background(), api.RevokeInstallationRequest{ID: "dev_other"})
	if err != nil || seen != `POST /api/v1/installations/revoke {"id":"dev_other"}` || revoked.SharesStopped != 2 || revoked.AlreadyEnded {
		t.Fatalf("revoke: %v %v (%s)", revoked, err, seen)
	}
	_, err = c.RevokeInstallationByID(context.Background(), api.RevokeInstallationRequest{ID: "dev_else"})
	_ = checkError(t, err, api.ProtocolError, api.Unknown)
	c = client(t, respond(t, 404, `{"error":"not_found","message":"x","outcome":"not_applied","retryable":false}`, nil), token)
	_, err = c.RevokeInstallationByID(context.Background(), api.RevokeInstallationRequest{ID: "dev_else"})
	_ = checkError(t, err, api.NotFound, api.NotApplied)
}

func TestUnavailableDialAndExpiredWaitDoNotClaimWriteCompletion(t *testing.T) {
	stopped := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := stopped.URL
	stopped.Close()
	c, e := purlview.New(purlview.Config{Endpoint: endpoint, AllowHTTP: true, Credentials: func(context.Context) (string, error) { return token, nil }})
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseIdleConnections()
	_, e = c.CreateShare(context.Background(), api.CreateShareRequest{Key: "dial", Target: "http://localhost:3000", TTL: time.Hour})
	_ = checkError(t, e, api.Unavailable, api.NotApplied)
	a := api.Authorization{ID: "auth_test", PollToken: pollToken, BrowserURL: "https://account.example.invalid/login", VerificationURL: "https://account.example.invalid/device", UserCode: "TEST", ExpiresAt: time.Now().Add(-time.Second), PollIntervalMS: 100}
	_, e = c.WaitAuthorization(context.Background(), a)
	_ = checkError(t, e, api.Expired, api.NotApplied)
}
