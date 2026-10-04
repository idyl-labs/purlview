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

package apiserver_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/testplatform"
	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/apiserver"
	"github.com/idyl-labs/purlview/sdk/purlview"
	"github.com/idyl-labs/purlview/sdk/resource"
)

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
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, e := os.ReadFile("../api/testdata/" + name)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Fatalf("%s\ngot  %s\nwant %s", name, got, want)
	}
}

func TestHTTPProductOperationsAndWire(t *testing.T) {
	backend := testplatform.New()
	backend.Now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	handler := apiserver.New(host, backend)
	var methods, paths, authorities, keys, bodies []string
	var responses [][]byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.Header.Get(api.VersionHeader) != "1" || r.Header.Get("Accept") != "application/json" || r.URL.RawQuery != "" {
			t.Errorf("wrong request metadata")
		}
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		authorities = append(authorities, r.Header.Get("Authorization"))
		keys = append(keys, r.Header.Get(api.IdempotencyHeader))
		bodies = append(bodies, string(b))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get(api.RequestIDHeader) == "" || rec.Header().Get("Set-Cookie") != "" {
			t.Error("unsafe response headers")
		}
		responses = append(responses, bytes.Clone(rec.Body.Bytes()))
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
	c := client(t, h, testplatform.Token)
	ctx := context.Background()
	a, err := c.BeginAuthorization(ctx, api.BeginAuthorizationRequest{DeviceLabel: "test device", Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	status, err := c.ObserveAuthorization(ctx, *a)
	if err != nil || status.State != "approved" || status.Credential.Token != testplatform.Token {
		t.Fatalf("approval: %v %v", status, err)
	}
	id, err := c.Identity(ctx)
	if err != nil || *id != testplatform.Identity() {
		t.Fatalf("identity: %v %v", id, err)
	}
	req := api.CreateShareRequest{Key: "intentional_attempt_1", Target: "http://localhost:3000/demo?version=2", Targets: []string{"http://localhost:3000/demo?version=2", "http://localhost:8000"}, TTL: 15 * time.Minute, Recipients: []string{"Reviewer@example.invalid", "second@example.invalid", "reviewer@example.invalid"}, RewriteURLs: true}
	created, err := c.CreateShare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.ListShares(ctx)
	if err != nil || len(listed.Shares) != 1 || !reflect.DeepEqual(listed.Shares[0], *created) {
		t.Fatalf("list: %v", err)
	}
	installations, err := c.ListInstallations(ctx)
	if err != nil || len(installations.Installations) != 2 || installations.Installations[0].ActiveShares != 1 {
		t.Fatalf("installations: %v %v", installations, err)
	}
	other, err := c.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: testplatform.OtherDevice})
	if err != nil || other.Outcome != "confirmed" || other.AlreadyEnded {
		t.Fatalf("installation by id: %v %v", other, err)
	}
	revoked, err := c.RevokeShare(ctx, api.RevokeShareRequest{Reference: resource.ShareRef{Origin: created.Share.Origin}})
	if err != nil || revoked.Outcome != "confirmed" || revoked.OwnerNotified {
		t.Fatalf("revoke: %v %v", revoked, err)
	}
	installation, err := c.RevokeInstallation(ctx)
	if err != nil || installation.Outcome != "confirmed" {
		t.Fatalf("installation: %v %v", installation, err)
	}
	wantMethods := []string{"POST", "POST", "GET", "POST", "GET", "GET", "POST", "POST", "POST"}
	wantPaths := []string{api.AuthorizationsPath, api.ObservePath, api.IdentityPath, api.SharesPath, api.SharesPath, api.InstallationsPath, api.InstallationsRevokePath, api.ShareRevokePath, api.InstallationRevokePath}
	wantBodies := []string{`{"device_label":"test device","headless":true}`, `{"authorization_id":"auth_test"}`, "", "", "", "", `{"id":"dev_other"}`, `{"reference":{"origin":"https://share1.content.example.invalid"}}`, `{}`}
	for i := range wantPaths {
		if methods[i] != wantMethods[i] || paths[i] != wantPaths[i] {
			t.Errorf("operation %d: %s %s", i, methods[i], paths[i])
		}
		auth := "Bearer " + testplatform.Token
		if i == 0 {
			auth = ""
		}
		if i == 1 {
			auth = "Purlview-Poll " + testplatform.PollToken
		}
		if authorities[i] != auth {
			t.Errorf("wrong credential purpose at %d", i)
		}
		if i != 3 && keys[i] != "" {
			t.Errorf("unexpected key at %d", i)
		}
		if i != 3 && bodies[i] != wantBodies[i] {
			t.Errorf("unexpected body at %d: %s", i, bodies[i])
		}
	}
	if keys[3] != req.Key {
		t.Fatal("lost create key")
	}
	golden(t, "create-request.json", []byte(bodies[3]))
	golden(t, "create-response.json", responses[3])
	golden(t, "installations.json", responses[5])
	golden(t, "installation-revoke-request.json", []byte(bodies[6]))
	golden(t, "installation-revocation.json", responses[6])
	golden(t, "confirmed-revocation.json", responses[7])
	for _, i := range []int{2, 3, 4, 5, 6, 7, 8} {
		if bytes.Contains(responses[i], []byte(testplatform.Token)) || bytes.Contains(responses[i], []byte(testplatform.PollToken)) {
			t.Fatalf("credential escaped authorised issuance at %d", i)
		}
	}
}

// The golden error carries a fixed request id; the live one is substituted.
func TestSixDigitSignInOverHTTP(t *testing.T) {
	backend := testplatform.New()
	handler := apiserver.New(host, backend)
	var last []byte
	var id string
	c := client(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		last, id = bytes.Clone(rec.Body.Bytes()), rec.Header().Get(api.RequestIDHeader)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}), "")
	ctx := context.Background()
	challenge, err := c.StartLogin(ctx, api.LoginStartRequest{Email: "creator@example.invalid", DeviceLabel: "test device"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.VerifyLogin(ctx, api.LoginVerifyRequest{ID: challenge.ID, Code: "000000"})
	if e := checkError(t, err, api.Denied, api.NotApplied); e.AttemptsLeft != 4 {
		t.Fatalf("attempts left %d", e.AttemptsLeft)
	}
	golden(t, "login-denied.json", bytes.Replace(last, []byte(id), []byte("req_test"), 1))
	resent, err := c.ResendLogin(ctx, api.LoginResendRequest{ID: challenge.ID})
	if err != nil || !resent.ExpiresAt.Equal(challenge.ExpiresAt) || !resent.ResendAt.After(challenge.ResendAt) {
		t.Fatalf("resend keeps the expiry and moves the resend time: %v %v", resent, err)
	}
	_, err = c.VerifyLogin(ctx, api.LoginVerifyRequest{ID: challenge.ID, Code: testplatform.Code})
	if e := checkError(t, err, api.Denied, api.NotApplied); e.AttemptsLeft != 3 {
		t.Fatalf("the replaced code must miss: %d", e.AttemptsLeft)
	}
	for left := 2; left >= 1; left-- {
		_, err = c.VerifyLogin(ctx, api.LoginVerifyRequest{ID: challenge.ID, Code: "000000"})
		if e := checkError(t, err, api.Denied, api.NotApplied); e.AttemptsLeft != left {
			t.Fatalf("attempts left %d, want %d", e.AttemptsLeft, left)
		}
	}
	_, err = c.VerifyLogin(ctx, api.LoginVerifyRequest{ID: challenge.ID, Code: "000000"})
	if e := checkError(t, err, api.Expired, api.NotApplied); e.AttemptsLeft != 0 {
		t.Fatal("the fifth miss ends the challenge")
	}
	_, err = c.VerifyLogin(ctx, api.LoginVerifyRequest{ID: challenge.ID, Code: testplatform.Resent})
	_ = checkError(t, err, api.Expired, api.NotApplied)
	fresh := client(t, apiserver.New(host, testplatform.New()), "")
	cred, err := fresh.VerifyLogin(ctx, api.LoginVerifyRequest{ID: challenge.ID, Code: api.NormaliseLoginCode("482 913")})
	if err != nil || cred.Token != testplatform.Token {
		t.Fatalf("verify: %v", err)
	}
}

func TestInviteResultsAreReportedOnceAndReplayed(t *testing.T) {
	backend := testplatform.New()
	backend.InviteStatus = api.InviteSent
	c := client(t, apiserver.New(host, backend), testplatform.Token)
	req := api.CreateShareRequest{Key: "invited", Target: "http://localhost:3000", TTL: time.Hour, Recipients: []string{"ana@example.invalid", "raj@example.invalid"}}
	first, err := c.CreateShare(context.Background(), req)
	want := []api.InviteResult{{Email: "ana@example.invalid", Status: api.InviteSent}, {Email: "raj@example.invalid", Status: api.InviteSent}}
	if err != nil || !reflect.DeepEqual(first.Invites, want) {
		t.Fatalf("create: %v %v", first, err)
	}
	backend.InviteStatus = api.InviteNotSent
	req.Recipients = []string{"Ana@example.invalid", "raj@example.invalid", "ana@example.invalid"}
	again, err := c.CreateShare(context.Background(), req)
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("a replay returns the recorded results: %v %v", again, err)
	}
	req.Recipients = []string{"raj@example.invalid", "ana@example.invalid"}
	_, err = c.CreateShare(context.Background(), req)
	_ = checkError(t, err, api.Conflict, api.NotApplied)
	secret, err := c.CreateShare(context.Background(), api.CreateShareRequest{Key: "open", Target: "http://localhost:3000", TTL: time.Hour})
	if err != nil || secret.Invites != nil {
		t.Fatalf("a secret-link share invites nobody: %v %v", secret, err)
	}
}

func TestInstallationsOverHTTP(t *testing.T) {
	backend := testplatform.New()
	c := client(t, apiserver.New(host, backend), testplatform.Token)
	ctx := context.Background()
	_, err := c.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: "dev_elsewhere"})
	_ = checkError(t, err, api.NotFound, api.NotApplied)
	if _, err = c.CreateShare(ctx, api.CreateShareRequest{Key: "one", Target: "http://localhost:3000", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	again, err := c.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: testplatform.OtherDevice})
	if err != nil || again.AlreadyEnded {
		t.Fatalf("first revoke: %v %v", again, err)
	}
	if again, err = c.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: testplatform.OtherDevice}); err != nil || !again.AlreadyEnded {
		t.Fatalf("repeated revoke: %v %v", again, err)
	}
	list, err := c.ListInstallations(ctx)
	if err != nil || len(list.Installations) != 1 || !list.Installations[0].Current {
		t.Fatalf("list: %v %v", list, err)
	}
	own, err := c.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: testplatform.Identity().Device})
	if err != nil || own.SharesStopped != 1 {
		t.Fatalf("own installation: %v %v", own, err)
	}
	_, err = c.ListInstallations(ctx)
	_ = checkError(t, err, api.Unauthorised, api.NotApplied)
	for _, token := range []string{testplatform.PollToken, "synthetic-recipient-secret"} {
		bad := client(t, apiserver.New(host, testplatform.New()), token)
		_, err = bad.ListInstallations(ctx)
		_ = checkError(t, err, api.Unauthorised, api.NotApplied)
		_, err = bad.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: testplatform.OtherDevice})
		_ = checkError(t, err, api.Unauthorised, api.NotApplied)
	}
}

func TestIdempotencyAccessModesAndPurpose(t *testing.T) {
	backend := testplatform.New()
	c := client(t, apiserver.New(host, backend), testplatform.Token)
	ctx := context.Background()
	req := api.CreateShareRequest{Key: "first", Target: "http://localhost:3000", TTL: time.Hour}
	first, err := c.CreateShare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := c.CreateShare(ctx, req)
	if err != nil || !reflect.DeepEqual(*repeat, *first) {
		t.Fatalf("same key changed identity/link/expiry: %v", err)
	}
	req.TTL = time.Minute
	_, err = c.CreateShare(ctx, req)
	_ = checkError(t, err, api.Conflict, api.NotApplied)
	req.Key = "second"
	req.Recipients = []string{"reviewer@example.invalid"}
	second, err := c.CreateShare(ctx, req)
	if err != nil || second.Share.ID == first.Share.ID || strings.Contains(second.URL, "token=") {
		t.Fatalf("distinct restricted share: %v", err)
	}
	list, err := c.ListShares(ctx)
	if err != nil || len(list.Shares) != 2 {
		t.Fatal("list did not rediscover both")
	}
	found := false
	for _, s := range list.Shares {
		if s.Share.ID == first.Share.ID {
			found = s.URL == first.URL && strings.Contains(s.URL, "?token=")
		}
	}
	if !found {
		t.Fatal("recipient secret lost")
	}
	for _, token := range []string{testplatform.PollToken, "synthetic-recipient-secret", "synthetic-email-secret", "synthetic-session"} {
		bad := c.WithCredentials(func(context.Context) (string, error) { return token, nil })
		_, err = bad.Identity(ctx)
		_ = checkError(t, err, api.Unauthorised, api.NotApplied)
		_, err = bad.RevokeShare(ctx, api.RevokeShareRequest{Reference: resource.ShareRef{ID: first.Share.ID}})
		_ = checkError(t, err, api.Unauthorised, api.NotApplied)
	}
	_, err = c.ObserveAuthorization(ctx, api.Authorization{ID: "auth_test", PollToken: testplatform.Token})
	_ = checkError(t, err, api.Unauthorised, api.NotApplied)
	r := api.RevokeShareRequest{Reference: resource.ShareRef{ID: first.Share.ID}}
	if _, err = c.RevokeShare(ctx, r); err != nil {
		t.Fatal(err)
	}
	again, err := c.RevokeShare(ctx, r)
	if err != nil || !again.AlreadyEnded {
		t.Fatal("repeat revocation not confirmed")
	}
}

func TestNormalBackendEveryOperationIsUnimplemented(t *testing.T) {
	c := client(t, apiserver.New(host, nil), testplatform.Token)
	ctx := context.Background()
	calls := []func() error{
		func() error {
			_, e := c.BeginAuthorization(ctx, api.BeginAuthorizationRequest{DeviceLabel: "test"})
			return e
		},
		func() error {
			_, e := c.ObserveAuthorization(ctx, api.Authorization{ID: "auth_test", PollToken: testplatform.PollToken})
			return e
		},
		func() error { _, e := c.Identity(ctx); return e }, func() error { _, e := c.RevokeInstallation(ctx); return e },
		func() error {
			_, e := c.CreateShare(ctx, api.CreateShareRequest{Key: "test", Target: "http://localhost:3000", TTL: time.Hour})
			return e
		},
		func() error { _, e := c.ListShares(ctx); return e }, func() error {
			_, e := c.RevokeShare(ctx, api.RevokeShareRequest{Reference: resource.ShareRef{ID: "shr_test"}})
			return e
		},
	}
	for i, call := range calls {
		err := call()
		e := checkError(t, err, api.NotImplemented, api.NotApplied)
		if e.Status != 501 || e.RequestID == "" || !errors.Is(err, api.ErrNotImplemented) {
			t.Fatalf("operation %d lost typed error: %#v", i, e)
		}
	}
}

func TestWaitStatesAndCancellation(t *testing.T) {
	for _, state := range []string{"approved", "denied", "expired"} {
		t.Run(state, func(t *testing.T) {
			b := testplatform.New()
			b.ObserveState = state
			c := client(t, apiserver.New(host, b), testplatform.Token)
			a, e := c.BeginAuthorization(context.Background(), api.BeginAuthorizationRequest{DeviceLabel: "test"})
			if e != nil {
				t.Fatal(e)
			}
			cred, e := c.WaitAuthorization(context.Background(), *a)
			if state == "approved" {
				if e != nil || cred.Token != testplatform.Token {
					t.Fatal(e)
				}
			} else {
				_ = checkError(t, e, api.Code(state), api.NotApplied)
			}
		})
	}
	t.Run("pending cancellation", func(t *testing.T) {
		observed := make(chan struct{})
		b := testplatform.New()
		b.ObserveHook = func(context.Context, string, api.ObserveAuthorizationRequest) (api.AuthorizationStatus, error) {
			close(observed)
			return api.AuthorizationStatus{State: "pending"}, nil
		}
		c := client(t, apiserver.New(host, b), testplatform.Token)
		a, e := c.BeginAuthorization(context.Background(), api.BeginAuthorizationRequest{DeviceLabel: "test"})
		if e != nil {
			t.Fatal(e)
		}
		a.PollIntervalMS = 30000
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := c.WaitAuthorization(ctx, *a); done <- err }()
		<-observed
		cancel()
		select {
		case e = <-done:
			if !errors.Is(e, context.Canceled) {
				t.Fatalf("lost cancellation: %v", e)
			}
		case <-time.After(time.Second):
			t.Fatal("poll timer did not release")
		}
	})
	t.Run("in flight deadline", func(t *testing.T) {
		released := make(chan struct{})
		b := testplatform.New()
		b.BeginHook = func(ctx context.Context, _ api.BeginAuthorizationRequest) (api.Authorization, error) {
			<-ctx.Done()
			close(released)
			return api.Authorization{}, ctx.Err()
		}
		c := client(t, apiserver.New(host, b), testplatform.Token)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, e := c.BeginAuthorization(ctx, api.BeginAuthorizationRequest{DeviceLabel: "test"})
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Fatalf("lost deadline: %v", e)
		}
		_ = checkError(t, e, api.Unavailable, api.Unknown)
		select {
		case <-released:
		case <-time.After(time.Second):
			t.Fatal("server operation not released")
		}
	})
}

func TestAnonymousCallsNeverReadCreatorSourceAndTLSRemainsVerified(t *testing.T) {
	backend := testplatform.New()
	s := httptest.NewTLSServer(apiserver.New(host, backend))
	defer s.Close()
	// The loopback certificate is not in system trust: defaults must reject it.
	c, e := purlview.New(purlview.Config{Endpoint: s.URL, Host: host})
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseIdleConnections()
	if _, e = c.BeginAuthorization(context.Background(), api.BeginAuthorizationRequest{DeviceLabel: "test"}); e == nil {
		t.Fatal("normal TLS accepted untrusted certificate")
	}
	var reads atomic.Int32
	c, e = purlview.New(purlview.Config{Endpoint: s.URL, Host: host, Transport: s.Client().Transport, Credentials: func(context.Context) (string, error) { reads.Add(1); return testplatform.Token, nil }})
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseIdleConnections()
	a, e := c.BeginAuthorization(context.Background(), api.BeginAuthorizationRequest{DeviceLabel: "test"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.ObserveAuthorization(context.Background(), *a); e != nil {
		t.Fatal(e)
	}
	if reads.Load() != 0 {
		t.Fatal("anonymous/poll operation consulted creator source")
	}
	if _, e = c.Identity(context.Background()); e != nil || reads.Load() != 1 {
		t.Fatal("explicit management credential source not used")
	}
}

func TestResponseMustMatchWriteIntent(t *testing.T) {
	for _, operation := range []string{"create", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			backend := testplatform.New()
			if operation == "create" {
				backend.CreateHook = func(ctx context.Context, token string, r api.CreateShareRequest) (api.ShareAccess, error) {
					v, e := backend.Create(ctx, token, r)
					v.Share.Recipients = []string{"other@example.invalid"}
					v.URL = v.Share.Origin + "/access"
					return v, e
				}
			} else {
				backend.RevokeHook = func(ctx context.Context, token string, r api.RevokeShareRequest) (api.RevokeShareResult, error) {
					v, e := backend.Revoke(ctx, token, r)
					v.Share.ID = "shr_someone_else"
					return v, e
				}
			}
			c := client(t, apiserver.New(host, backend), testplatform.Token)
			created, e := c.CreateShare(context.Background(), api.CreateShareRequest{Key: "intent", Target: "http://localhost:3000", TTL: time.Hour})
			if operation == "revoke" {
				if e != nil {
					t.Fatal(e)
				}
				_, e = c.RevokeShare(context.Background(), api.RevokeShareRequest{Reference: resource.ShareRef{ID: created.Share.ID}})
			}
			result := checkError(t, e, api.ProtocolError, api.Unknown)
			if result.RequestID == "" {
				t.Fatal("correlation failure lost request id")
			}
		})
	}
}

func TestLostWriteReplyIsNotRetried(t *testing.T) {
	t.Run("lost write reply", func(t *testing.T) {
		var calls atomic.Int32
		b := testplatform.New()
		handler := apiserver.New(host, b)
		c := client(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}), testplatform.Token)
		_, err := c.CreateShare(context.Background(), api.CreateShareRequest{Key: "lost", Target: "http://localhost:3000", TTL: time.Hour})
		_ = checkError(t, err, api.Unavailable, api.Unknown)
		creates, _, records := b.Counts()
		if calls.Load() != 1 || creates != 1 || records != 1 {
			t.Fatal("write blindly retried")
		}
	})
}
