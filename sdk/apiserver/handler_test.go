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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/apiserver"
)

func TestDirectHTTPValidationPrecedesServiceEffects(t *testing.T) {
	const host = "account.example.invalid"
	var effects atomic.Int32
	b := &serviceStub{}
	b.CreateHook = func(context.Context, string, api.CreateShareRequest) (api.ShareAccess, error) {
		effects.Add(1)
		return api.ShareAccess{}, nil
	}
	s := httptest.NewServer(apiserver.New(host, b))
	defer s.Close()
	valid := `{"target":"http://localhost:3000","ttl_nanoseconds":60000000000,"rewrite_urls":false}`
	tests := []struct {
		name, method, path, body, authority, key, version, content, host string
		status                                                           int
		code                                                             api.Code
	}{
		{name: "ttl", body: `{"target":"http://localhost","ttl_nanoseconds":3600000000001}`, status: 400, code: api.InvalidRequest},
		{name: "target credentials", body: `{"target":"https://u:secret@example.invalid","ttl_nanoseconds":60000000000}`, status: 400, code: api.InvalidRequest},
		{name: "recipient", body: `{"target":"http://localhost","ttl_nanoseconds":60000000000,"recipients":["a@b.invalid;c@d.invalid"]}`, status: 400, code: api.InvalidRequest},
		{name: "empty recipient", body: `{"target":"http://localhost","ttl_nanoseconds":60000000000,"recipients":[""]}`, status: 400, code: api.InvalidRequest},
		{name: "too many recipients", body: `{"target":"http://localhost","ttl_nanoseconds":60000000000,"recipients":["a1@b.invalid","a2@b.invalid","a3@b.invalid","a4@b.invalid","a5@b.invalid","a6@b.invalid","a7@b.invalid","a8@b.invalid","a9@b.invalid","a10@b.invalid","a11@b.invalid"]}`, status: 400, code: api.InvalidRequest},
		{name: "former scalar recipient", body: `{"target":"http://localhost","ttl_nanoseconds":60000000000,"recipient":"a@b.invalid"}`, status: 400, code: api.InvalidRequest},
		{name: "installations need authority", method: "GET", path: api.InstallationsPath, authority: "-", status: 401, code: api.Unauthorised},
		{name: "installation revoke needs authority", path: api.InstallationsRevokePath, body: `{"id":"dev_other"}`, authority: "-", status: 401, code: api.Unauthorised},
		{name: "installations are not posted", path: api.InstallationsPath, body: `{}`, status: 405, code: api.MethodNotAllowed},
		{name: "missing key", key: "-", status: 400, code: api.InvalidRequest},
		{name: "query", path: api.SharesPath + "?token=secret", status: 400, code: api.InvalidRequest},
		{name: "unknown field", body: `{"target":"http://localhost","ttl_nanoseconds":60000000000,"recipent":"a@b.invalid"}`, status: 400, code: api.InvalidRequest},
		{name: "json null", body: `null`, status: 400, code: api.InvalidRequest},
		{name: "array", body: `[]`, status: 400, code: api.InvalidRequest},
		{name: "trailing", body: valid + ` {}`, status: 400, code: api.InvalidRequest},
		{name: "malformed", body: `{`, status: 400, code: api.InvalidRequest},
		{name: "oversize", body: `{"padding":"` + strings.Repeat("x", api.MaxRequestBytes) + `"}`, status: 413, code: api.RequestTooLarge},
		{name: "content type", content: "text/plain", status: 415, code: api.UnsupportedMediaType},
		{name: "unauthenticated", authority: "-", status: 401, code: api.Unauthorised},
		{name: "poll is not bearer", authority: "Purlview-Poll " + "synthetic-poll-secret", status: 401, code: api.Unauthorised},
		{name: "begin must be anonymous", path: api.AuthorizationsPath, body: `{"device_label":"test","headless":true}`, status: 400, code: api.InvalidRequest},
		{name: "poll requires purpose", path: api.ObservePath, body: `{"authorization_id":"auth_test"}`, status: 401, code: api.Unauthorised},
		{name: "unknown route", path: "/api/v1/other", status: 404, code: api.NotFound},
		{name: "outside the API", method: "GET", path: "/", status: 404, code: api.NotFound},
		{name: "wrong method", method: "DELETE", status: 405, code: api.MethodNotAllowed},
		{name: "unsupported route version", path: "/api/v2/shares", status: 400, code: api.UnsupportedVersion},
		{name: "unsupported header version", version: "2", status: 400, code: api.UnsupportedVersion},
		{name: "wrong host", host: "share.content.example.invalid", status: 421, code: api.Misdirected},
		{name: "GET body", method: "GET", status: 400, code: api.InvalidRequest},
		{name: "secret reference", path: api.ShareRevokePath, body: `{"reference":{"origin":"https://share.content.example.invalid/access?token=secret"}}`, status: 400, code: api.InvalidRequest},
		{name: "ambiguous reference", path: api.ShareRevokePath, body: `{"reference":{"id":"shr_test","origin":"https://share.content.example.invalid"}}`, status: 400, code: api.InvalidRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method, path, body, authority, key, version, content, requestHost := tc.method, tc.path, tc.body, tc.authority, tc.key, tc.version, tc.content, tc.host
			if method == "" {
				method = "POST"
			}
			if path == "" {
				path = api.SharesPath
			}
			if body == "" {
				body = valid
			}
			if authority == "" {
				authority = "Bearer " + "synthetic-installation-secret"
			}
			if key == "" {
				key = "direct_attempt"
			}
			if version == "" {
				version = "1"
			}
			if content == "" {
				content = "application/json"
			}
			if requestHost == "" {
				requestHost = host
			}
			req, e := http.NewRequest(method, s.URL+path, strings.NewReader(body))
			if e != nil {
				t.Fatal(e)
			}
			req.Host = requestHost
			req.Header.Set("Content-Type", content)
			req.Header.Set(api.VersionHeader, version)
			if authority != "-" {
				req.Header.Set("Authorization", authority)
			}
			if key != "-" {
				req.Header.Set(api.IdempotencyHeader, key)
			}
			res, e := s.Client().Do(req)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = res.Body.Close() }()
			data, e := io.ReadAll(res.Body)
			if e != nil {
				t.Fatal(e)
			}
			var failure api.Error
			if e = json.Unmarshal(data, &failure); e != nil {
				t.Fatal(e)
			}
			if res.StatusCode != tc.status || failure.Code != tc.code || failure.Outcome != api.NotApplied || failure.RequestID == "" {
				t.Fatalf("response %d %s", res.StatusCode, data)
			}
			if allow := res.Header.Get("Allow"); tc.status == 405 && allow != "GET, POST" && (path != api.InstallationsPath || allow != "GET") {
				t.Fatal("missing Allow")
			}
			if strings.Contains(string(data), "secret") || res.Header.Get("Set-Cookie") != "" {
				t.Fatal("reflected sensitive input")
			}
		})
	}
	if effects.Load() != 0 {
		t.Fatal("malformed input reached product service")
	}
}

func TestBackendResultsAreValidatedAndErrorsSanitised(t *testing.T) {
	for _, serviceError := range []bool{false, true} {
		t.Run(map[bool]string{false: "malformed result", true: "service failure"}[serviceError], func(t *testing.T) {
			b := &serviceStub{}
			b.BeginHook = func(context.Context, api.BeginAuthorizationRequest) (api.Authorization, error) {
				if serviceError {
					return api.Authorization{}, &api.Error{Code: api.Unavailable, Message: "synthetic-secret", RequestID: "synthetic-secret", Outcome: api.Unknown, Retryable: true}
				}
				return api.Authorization{}, nil
			}
			s := httptest.NewServer(apiserver.New("account.example.invalid", b))
			defer s.Close()
			req, _ := http.NewRequest("POST", s.URL+api.AuthorizationsPath, strings.NewReader(`{"device_label":"test","headless":false}`))
			req.Host = "account.example.invalid"
			req.Header.Set("Content-Type", "application/json")
			res, e := s.Client().Do(req)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = res.Body.Close() }()
			data, _ := io.ReadAll(res.Body)
			want := 500
			if serviceError {
				want = 503
			}
			if res.StatusCode != want || strings.Contains(string(data), "synthetic-secret") {
				t.Fatalf("unsafe success/failure: %d %s", res.StatusCode, data)
			}
		})
	}
}

func TestTimeoutAndPanicStayTypedAndDoNotReflectServiceSecrets(t *testing.T) {
	for _, mode := range []string{"timeout", "panic"} {
		t.Run(mode, func(t *testing.T) {
			released := make(chan struct{})
			b := &serviceStub{}
			b.BeginHook = func(ctx context.Context, _ api.BeginAuthorizationRequest) (api.Authorization, error) {
				if mode == "panic" {
					close(released)
					panic("synthetic-service-secret")
				}
				<-ctx.Done()
				close(released)
				return api.Authorization{}, ctx.Err()
			}
			handler := apiserver.RequestID(apiserver.WithTimeout(apiserver.New("account.example.invalid", b), 20*time.Millisecond))
			s := httptest.NewServer(handler)
			defer s.Close()
			req, _ := http.NewRequest("POST", s.URL+api.AuthorizationsPath, strings.NewReader(`{"device_label":"test","headless":false}`))
			req.Host = "account.example.invalid"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(api.RequestIDHeader, "synthetic-client-secret")
			res, e := s.Client().Do(req)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = res.Body.Close() }()
			data, _ := io.ReadAll(res.Body)
			var failure api.Error
			if e = json.Unmarshal(data, &failure); e != nil {
				t.Fatal(e)
			}
			status, code := 503, api.Unavailable
			if mode == "panic" {
				status, code = 500, api.InternalError
			}
			if res.StatusCode != status || failure.Code != code || failure.Outcome != api.Unknown || res.Header.Get(api.VersionHeader) != api.Version || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") || strings.Contains(string(data), "synthetic") {
				t.Fatalf("unsafe timeout/panic: %d %s", res.StatusCode, data)
			}
			if res.Header.Get(api.RequestIDHeader) == "" || strings.Contains(res.Header.Get(api.RequestIDHeader), "synthetic") {
				t.Fatal("invalid correlation")
			}
			select {
			case <-released:
			case <-time.After(time.Second):
				t.Fatal("timed out service did not release")
			}
		})
	}
}

func exchange(t *testing.T, s *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, e := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Host = "account.example.invalid"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if path != api.LoginVerifyPath {
		req.Header.Set("Authorization", "Bearer synthetic-installation-secret")
		req.Header.Set(api.IdempotencyHeader, "attempt")
	}
	res, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data
}

func TestCreateReachesTheServiceWithRecordedRecipients(t *testing.T) {
	var got []string
	b := &serviceStub{}
	b.CreateHook = func(_ context.Context, _ string, r api.CreateShareRequest) (api.ShareAccess, error) {
		got = r.Recipients
		return api.ShareAccess{}, &api.Error{Code: api.Unavailable, Outcome: api.NotApplied}
	}
	s := httptest.NewServer(apiserver.New("account.example.invalid", b))
	defer s.Close()
	exchange(t, s, "POST", api.SharesPath, `{"target":"http://localhost:3000","ttl_nanoseconds":60000000000,"recipients":["Ana@Example.invalid","raj@example.invalid","ana@example.invalid"],"rewrite_urls":false}`)
	if strings.Join(got, ",") != "ana@example.invalid,raj@example.invalid" {
		t.Fatalf("service saw %q", got)
	}
}

func TestWrongCodeCarriesAttemptsLeftAndNothingElseDoes(t *testing.T) {
	b := &emailStub{}
	s := httptest.NewServer(apiserver.New("account.example.invalid", b))
	defer s.Close()
	for _, tc := range []struct {
		err  *api.Error
		want string
	}{
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied, AttemptsLeft: 4}, `"attempts_left":4`},
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied}, ""},
		{&api.Error{Code: api.Expired, Outcome: api.NotApplied, AttemptsLeft: 3}, ""},
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied, AttemptsLeft: -1}, ""},
	} {
		b.err = tc.err
		status, data := exchange(t, s, "POST", api.LoginVerifyPath, `{"id":"auth_test","code":"482913"}`)
		if status != api.HTTPStatus(tc.err.Code) || strings.Contains(string(data), "attempts_left") != (tc.want != "") || !strings.Contains(string(data), tc.want) {
			t.Fatalf("%v: %d %s", tc.err, status, data)
		}
	}
	for _, code := range []string{"12345678", "482 913", "48291"} {
		if status, data := exchange(t, s, "POST", api.LoginVerifyPath, `{"id":"auth_test","code":"`+code+`"}`); status != 400 {
			t.Fatalf("code %q: %d %s", code, status, data)
		}
	}
}

func TestDeniedCarriesTheLimitAndNothingElseDoes(t *testing.T) {
	b := &emailStub{}
	s := httptest.NewServer(apiserver.New("account.example.invalid", b))
	defer s.Close()
	for _, tc := range []struct {
		err  *api.Error
		want string
	}{
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied, Limit: api.LimitRunningShares}, `"limit":"running_shares"`},
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied, Limit: api.LimitSharesPerHour}, `"limit":"shares_per_hour"`},
		{&api.Error{Code: api.Denied, Outcome: api.NotApplied}, ""},
		{&api.Error{Code: api.Unavailable, Outcome: api.Unknown, Limit: api.LimitRunningShares}, ""},
	} {
		b.err = tc.err
		status, data := exchange(t, s, "POST", api.LoginVerifyPath, `{"id":"auth_test","code":"482913"}`)
		if status != api.HTTPStatus(tc.err.Code) || strings.Contains(string(data), `"limit"`) != (tc.want != "") || !strings.Contains(string(data), tc.want) {
			t.Fatalf("%v: %d %s", tc.err, status, data)
		}
	}
}

func TestInstallationRoutes(t *testing.T) {
	plain := httptest.NewServer(apiserver.New("account.example.invalid", &serviceStub{}))
	defer plain.Close()
	for _, call := range [][3]string{{"GET", api.InstallationsPath, ""}, {"POST", api.InstallationsRevokePath, `{"id":"dev_other"}`}} {
		if status, data := exchange(t, plain, call[0], call[1], call[2]); status != 501 {
			t.Fatalf("service without installation management: %d %s", status, data)
		}
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	b := &installationStub{list: api.ListInstallationsResult{Installations: []api.Installation{{ID: "dev_test", Label: "test device", CreatedAt: now, Current: true}}}}
	s := httptest.NewServer(apiserver.New("account.example.invalid", b))
	defer s.Close()
	status, data := exchange(t, s, "GET", api.InstallationsPath, "")
	if want := `{"installations":[{"id":"dev_test","label":"test device","created_at":"2026-09-16T12:00:00Z","active_shares":0,"current":true}]}`; status != 200 || strings.TrimSpace(string(data)) != want || b.token != "synthetic-installation-secret" {
		t.Fatalf("list: %d %s", status, data)
	}
	status, data = exchange(t, s, "POST", api.InstallationsRevokePath, `{"id":"dev_other"}`)
	if want := `{"outcome":"confirmed","already_ended":false,"id":"dev_other","shares_stopped":2}`; status != 200 || strings.TrimSpace(string(data)) != want || b.revoked != "dev_other" {
		t.Fatalf("revoke: %d %s", status, data)
	}
	for _, body := range []string{`{"id":"not an id"}`, `{"id":"dev_other","all":true}`, `{}`} {
		if status, data = exchange(t, s, "POST", api.InstallationsRevokePath, body); status != 400 {
			t.Fatalf("revoke %s: %d %s", body, status, data)
		}
	}
	// A caller that is not an installation sees nothing marked current; two
	// current entries or an absent list are a service fault, never a result.
	b.list.Installations[0].Current = false
	if status, data = exchange(t, s, "GET", api.InstallationsPath, ""); status != 200 || !strings.Contains(string(data), `"current":false`) {
		t.Fatalf("list without a current installation: %d %s", status, data)
	}
	b.list.Installations = []api.Installation{{ID: "dev_a", Label: "a", CreatedAt: now, Current: true}, {ID: "dev_b", Label: "b", CreatedAt: now, Current: true}}
	if status, _ = exchange(t, s, "GET", api.InstallationsPath, ""); status != 500 {
		t.Fatalf("two current installations: %d", status)
	}
	b.list.Installations = nil
	if status, _ = exchange(t, s, "GET", api.InstallationsPath, ""); status != 500 {
		t.Fatalf("absent list: %d", status)
	}
	b.missing = true
	if status, _ = exchange(t, s, "POST", api.InstallationsRevokePath, `{"id":"dev_elsewhere"}`); status != 404 {
		t.Fatalf("unknown installation: %d", status)
	}
}

type emailStub struct {
	serviceStub
	err *api.Error
}

func (s *emailStub) StartLogin(context.Context, api.LoginStartRequest) (api.LoginChallenge, error) {
	return api.LoginChallenge{}, s.err
}
func (s *emailStub) VerifyLogin(context.Context, api.LoginVerifyRequest) (api.InstallationCredential, error) {
	return api.InstallationCredential{}, s.err
}
func (s *emailStub) ResendLogin(context.Context, api.LoginResendRequest) (api.LoginChallenge, error) {
	return api.LoginChallenge{}, s.err
}

type installationStub struct {
	serviceStub
	list           api.ListInstallationsResult
	token, revoked string
	missing        bool
}

func (s *installationStub) ListInstallations(_ context.Context, token string) (api.ListInstallationsResult, error) {
	s.token = token
	return s.list, nil
}
func (s *installationStub) RevokeInstallationByID(_ context.Context, _ string, r api.RevokeInstallationRequest) (api.RevokeInstallationResult, error) {
	if s.missing {
		return api.RevokeInstallationResult{}, &api.Error{Code: api.NotFound, Outcome: api.NotApplied}
	}
	s.revoked = r.ID
	return api.RevokeInstallationResult{Revocation: api.Revocation{Outcome: "confirmed"}, ID: r.ID, SharesStopped: 2}, nil
}

// serviceStub exercises handler ordering with hooks for the two operations under test.
type serviceStub struct {
	apiserver.Unimplemented
	CreateHook func(context.Context, string, api.CreateShareRequest) (api.ShareAccess, error)
	BeginHook  func(context.Context, api.BeginAuthorizationRequest) (api.Authorization, error)
}

func (s *serviceStub) CreateShare(ctx context.Context, token string, r api.CreateShareRequest) (api.ShareAccess, error) {
	return s.CreateHook(ctx, token, r)
}
func (s *serviceStub) BeginAuthorization(ctx context.Context, r api.BeginAuthorizationRequest) (api.Authorization, error) {
	return s.BeginHook(ctx, r)
}
