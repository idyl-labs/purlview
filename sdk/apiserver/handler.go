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

package apiserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
)

// DefaultTimeout bounds one request in a handler from New.
const DefaultTimeout = 30 * time.Second

// New returns the account API handler for accountHost, backed by service. A
// nil service selects Unimplemented.
//
// The handler answers only requests for accountHost (any other host is
// misdirected), assigns every request an identifier, bounds request bodies to
// api.MaxRequestBytes and each request to DefaultTimeout, and answers every
// failure with the api.Error envelope. Paths outside the versioned API are
// not_found.
func New(accountHost string, service Service) http.Handler {
	if service == nil {
		service = Unimplemented{}
	}
	h := &handler{host: normaliseHost(accountHost), service: service}
	bounded := WithTimeout(h, DefaultTimeout)
	return RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w.Header())
		bounded.ServeHTTP(w, r)
	}))
}

func setHeaders(h http.Header) {
	h.Set(api.VersionHeader, api.Version)
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

type handler struct {
	host    string
	service Service
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			WriteError(w, r, &api.Error{Code: api.InternalError, Outcome: api.Unknown})
		}
	}()
	fail := func(code api.Code) { WriteError(w, r, &api.Error{Code: code, Outcome: api.NotApplied}) }
	if normaliseHost(r.Host) != h.host {
		fail(api.Misdirected)
		return
	}
	if v := r.Header.Get(api.VersionHeader); v != "" && v != api.Version {
		fail(api.UnsupportedVersion)
		return
	}
	if api.IsAPIPath(r.URL.Path) && !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		fail(api.UnsupportedVersion)
		return
	}
	var allowed string
	switch r.URL.Path {
	case api.AuthorizationsPath, api.ObservePath, api.InstallationRevokePath, api.InstallationsRevokePath, api.ShareRevokePath, api.LoginStartPath, api.LoginVerifyPath, api.LoginResendPath:
		allowed = "POST"
	case api.IdentityPath, api.InstallationsPath:
		allowed = "GET"
	case api.SharesPath:
		allowed = "GET, POST"
	default:
		fail(api.NotFound)
		return
	}
	if (r.Method != "GET" && r.Method != "POST") || !strings.Contains(allowed, r.Method) {
		w.Header().Set("Allow", allowed)
		fail(api.MethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(api.InvalidRequest)
		return
	}
	scheme := "Bearer"
	if r.URL.Path == api.ObservePath {
		scheme = "Purlview-Poll"
	}
	authority := ""
	headers := r.Header.Values("Authorization")
	if r.URL.Path == api.AuthorizationsPath || r.URL.Path == api.LoginStartPath || r.URL.Path == api.LoginVerifyPath || r.URL.Path == api.LoginResendPath {
		// Anonymous routes refuse any credential, so that a client that
		// attaches one by mistake learns of it rather than leaking it.
		if len(headers) != 0 {
			fail(api.InvalidRequest)
			return
		}
	} else {
		if len(headers) != 1 {
			fail(api.Unauthorised)
			return
		}
		prefix := scheme + " "
		if !strings.HasPrefix(headers[0], prefix) || !api.Token(strings.TrimPrefix(headers[0], prefix)) {
			fail(api.Unauthorised)
			return
		}
		authority = strings.TrimPrefix(headers[0], prefix)
	}
	var data []byte
	if r.Method == "POST" {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			fail(api.UnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBytes)
		data, err = io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				fail(api.RequestTooLarge)
			} else {
				fail(api.InvalidRequest)
			}
			return
		}
	} else if r.Body != nil {
		// GET bodies and chunked bodies are refused even when ContentLength is unknown.
		b, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(b) != 0 {
			fail(api.InvalidRequest)
			return
		}
	}
	decode := func(out any) bool {
		// Requests reject unknown fields: a misspelt field must not silently
		// change what a request asks for.
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			fail(api.InvalidRequest)
			return false
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if d.Decode(out) != nil {
			fail(api.InvalidRequest)
			return false
		}
		var extra any
		if !errors.Is(d.Decode(&extra), io.EOF) {
			fail(api.InvalidRequest)
			return false
		}
		return true
	}
	var result interface{ Validate() error }
	var err error
	switch r.URL.Path {
	case api.LoginStartPath, api.LoginVerifyPath, api.LoginResendPath:
		live, ok := h.service.(EmailService)
		if !ok {
			fail(api.NotImplemented)
			return
		}
		switch r.URL.Path {
		case api.LoginStartPath:
			var req api.LoginStartRequest
			if !decode(&req) {
				return
			}
			if req.Validate() != nil {
				fail(api.InvalidRequest)
				return
			}
			result, err = live.StartLogin(r.Context(), req)
		case api.LoginVerifyPath:
			var req api.LoginVerifyRequest
			if !decode(&req) {
				return
			}
			if req.Validate() != nil {
				fail(api.InvalidRequest)
				return
			}
			result, err = live.VerifyLogin(r.Context(), req)
		case api.LoginResendPath:
			var req api.LoginResendRequest
			if !decode(&req) {
				return
			}
			if req.Validate() != nil {
				fail(api.InvalidRequest)
				return
			}
			result, err = live.ResendLogin(r.Context(), req)
		}
	case api.AuthorizationsPath:
		var req api.BeginAuthorizationRequest
		if !decode(&req) {
			return
		}
		if req.Validate() != nil {
			fail(api.InvalidRequest)
			return
		}
		result, err = h.service.BeginAuthorization(r.Context(), req)
	case api.ObservePath:
		var req api.ObserveAuthorizationRequest
		if !decode(&req) {
			return
		}
		if req.Validate() != nil {
			fail(api.InvalidRequest)
			return
		}
		result, err = h.service.ObserveAuthorization(r.Context(), authority, req)
	case api.IdentityPath:
		result, err = h.service.Identity(r.Context(), authority)
	case api.InstallationRevokePath:
		var req struct{}
		if !decode(&req) {
			return
		}
		result, err = h.service.RevokeInstallation(r.Context(), authority)
	case api.InstallationsPath, api.InstallationsRevokePath:
		installations, ok := h.service.(InstallationService)
		if !ok {
			fail(api.NotImplemented)
			return
		}
		if r.URL.Path == api.InstallationsPath {
			result, err = installations.ListInstallations(r.Context(), authority)
		} else {
			var req api.RevokeInstallationRequest
			if !decode(&req) {
				return
			}
			if req.Validate() != nil {
				fail(api.InvalidRequest)
				return
			}
			result, err = installations.RevokeInstallationByID(r.Context(), authority, req)
		}
	case api.SharesPath:
		if r.Method == "GET" {
			result, err = h.service.ListShares(r.Context(), authority)
		} else {
			var req api.CreateShareRequest
			if !decode(&req) {
				return
			}
			keys := r.Header.Values(api.IdempotencyHeader)
			if len(keys) != 1 {
				fail(api.InvalidRequest)
				return
			}
			req.Key = keys[0]
			if req.Validate() != nil {
				fail(api.InvalidRequest)
				return
			}
			// Services see recipients in recorded form, whatever the client
			// sent, and the CLI version through ClientVersion.
			result, err = h.service.CreateShare(WithClient(r.Context(), r.Header.Get(api.ClientHeader)), authority, req.Normalised())
		}
	case api.ShareRevokePath:
		var req api.RevokeShareRequest
		if !decode(&req) {
			return
		}
		if req.Validate() != nil {
			fail(api.InvalidRequest)
			return
		}
		result, err = h.service.RevokeShare(r.Context(), authority, req)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if result == nil || result.Validate() != nil {
		WriteError(w, r, &api.Error{Code: api.InternalError, Outcome: api.Unknown})
		return
	}
	// Encode before committing headers, so that an oversized or unencodable
	// result still becomes a clean error response.
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded)+1 > api.MaxResponseBytes {
		WriteError(w, r, &api.Error{Code: api.InternalError, Outcome: api.Unknown})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(encoded, '\n'))
}

// WriteError answers r with the api.Error envelope for err.
//
// Only the code, outcome and retryability of an *api.Error in err's chain
// reach the client, plus the attempts left and the limit of a denied request;
// any other error is internal_error with an unknown outcome. The message is
// always the fixed text for the code, so nothing a service or its
// dependencies wrote is reflected. An outcome other than not_applied is
// reported as unknown. The response carries the request identifier assigned
// by RequestID, when there is one.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	result := api.Error{Code: api.InternalError, Outcome: api.Unknown}
	var e *api.Error
	if errors.As(err, &e) {
		result.Code = e.Code
		result.Outcome = e.Outcome
		result.Retryable = e.Retryable
		if e.Code == api.Denied && e.AttemptsLeft > 0 {
			result.AttemptsLeft = e.AttemptsLeft
		}
		if e.Code == api.Denied {
			result.Limit = e.Limit
		}
	}
	if result.Outcome != api.NotApplied && result.Outcome != api.Unknown {
		result.Outcome = api.Unknown
	}
	result.Message = api.Message(result.Code)
	result.RequestID = RequestIDFrom(r.Context())
	setHeaders(w.Header())
	w.WriteHeader(api.HTTPStatus(result.Code))
	_ = json.NewEncoder(w).Encode(result)
}

// WithTimeout bounds each request to next while keeping the api.Error
// envelope and JSON headers; net/http's own timeout answer is a text/html 503.
// A timed-out request is unavailable, retryable, with an unknown outcome.
// Wrap the result in RequestID so that a timed-out request keeps its
// identifier.
func WithTimeout(next http.Handler, timeout time.Duration) http.Handler {
	body, _ := json.Marshal(api.Error{Code: api.Unavailable, Message: api.Message(api.Unavailable), Outcome: api.Unknown, Retryable: true})
	bounded := http.TimeoutHandler(next, timeout, string(body))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bounded.ServeHTTP(jsonWriter{w}, r) })
}

type jsonWriter struct{ http.ResponseWriter }

func (w jsonWriter) WriteHeader(status int) {
	setHeaders(w.Header())
	w.ResponseWriter.WriteHeader(status)
}
func (w jsonWriter) Write(data []byte) (int, error) { return w.ResponseWriter.Write(data) }

// normaliseHost strips an optional port and trailing dot and lowercases the
// host. An IPv6 literal loses its brackets, as net.SplitHostPort removes them.
func normaliseHost(host string) string {
	h := strings.TrimSpace(host)
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	} else {
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	}
	h = strings.TrimSuffix(h, ".")
	return strings.ToLower(h)
}
