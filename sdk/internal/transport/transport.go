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

// Package transport implements bounded, single-attempt HTTP exchanges.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
)

// Config is explicit application-supplied transport configuration.
type Config struct {
	Endpoint  string
	Transport http.RoundTripper
	Timeout   time.Duration
	// AllowHTTP permits plaintext only to a loopback IP or localhost.
	AllowHTTP bool
	// Host is a local-development virtual host, permitted only on loopback.
	Host string
	// Client is sent as the Purlview-Client header when set.
	Client string
}

// Client holds immutable endpoint settings; it never follows redirects.
type Client struct {
	endpoint string
	host     string
	client   string
	http     *http.Client
	timeout  time.Duration
}

// New validates settings without network or filesystem access.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.Endpoint, "#") || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("platform endpoint must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && (u.Scheme != "http" || !cfg.AllowHTTP || !loopback) {
		return nil, errors.New("platform endpoint requires HTTPS; explicit local HTTP is limited to loopback")
	}
	if cfg.Host != "" && (!loopback || strings.ContainsAny(cfg.Host, "/\\?#@ \r\n\t") || len(cfg.Host) > 253) {
		return nil, errors.New("platform host override requires a loopback endpoint and a valid host")
	}
	if len(cfg.Client) > 64 || strings.ContainsFunc(cfg.Client, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return nil, errors.New("platform client name must be at most 64 printable characters without spaces")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("platform timeout must be positive")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	rt := cfg.Transport
	if rt == nil {
		// Application wiring owns proxy/environment configuration. Do not use
		// http.DefaultTransport's ProxyFromEnvironment or mutable global settings.
		rt = &http.Transport{
			DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
		}
	}
	return &Client{endpoint: strings.TrimSuffix(cfg.Endpoint, "/"), host: cfg.Host, client: cfg.Client, timeout: cfg.Timeout, http: &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// CloseIdleConnections releases pooled connections owned by this transport.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

// Call performs exactly one request. Unknown fields are accepted, but trailing
// data, oversize responses, absent fields and invalid success shapes fail closed.
func (c *Client) Call(ctx context.Context, method, path, credential, key string, input, output any, write bool) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	outcome := api.NotApplied
	fail := func(code api.Code, cause error) *api.Error {
		return &api.Error{Code: code, Message: api.Message(code), Outcome: outcome, Retryable: code == api.Unavailable, Cause: cause}
	}
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil || len(body) > api.MaxRequestBytes {
			return fail(api.InvalidRequest, nil)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return fail(api.InvalidRequest, nil)
	}
	// Disable net/http replay of body-bearing requests on reused connections.
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	req.Header.Set(api.VersionHeader, api.Version)
	if c.client != "" {
		req.Header.Set(api.ClientHeader, c.client)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		req.Header.Set("Authorization", credential)
	}
	if key != "" {
		req.Header.Set(api.IdempotencyHeader, key)
	}
	if c.host != "" {
		req.Host = c.host
	}
	if ctx.Err() != nil {
		return fail(api.Unavailable, ctx.Err())
	}
	// Once dispatched, a write may have happened even if no reply arrives. The
	// SDK does not infer safety from a transport error or retry it automatically.
	if write {
		outcome = api.Unknown
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A failed dial precedes request transmission; other transport failures
		// conservatively leave write completion unknown.
		var dial *net.OpError
		if errors.As(err, &dial) && dial.Op == "dial" {
			outcome = api.NotApplied
		}
		// Never wrap url.Error (URLs/custom transports may contain secrets).
		var cause error
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		return fail(api.Unavailable, cause)
	}
	defer func() { _ = resp.Body.Close() }()
	id := api.SafeRequestID(resp.Header.Get(api.RequestIDHeader))
	protocol := func() error {
		e := fail(api.ProtocolError, nil)
		e.RequestID = id
		e.Status = resp.StatusCode
		return e
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBytes+1))
	if err != nil {
		e := fail(api.Unavailable, ctx.Err())
		e.RequestID = id
		return e
	}
	if len(data) > api.MaxResponseBytes {
		return protocol()
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || resp.Header.Get(api.VersionHeader) != api.Version {
		return protocol()
	}
	if resp.StatusCode != http.StatusOK {
		var e api.Error
		if Decode(data, &e) != nil || e.Code == "" || (e.Outcome != api.NotApplied && e.Outcome != api.Unknown) || resp.StatusCode < 400 || resp.StatusCode > 599 {
			return protocol()
		}
		// Attempts left and the limit belong to denied alone; attempts left
		// stay within the challenge's bound.
		if e.Code != api.Denied {
			e.AttemptsLeft = 0
			e.Limit = ""
		} else if e.AttemptsLeft < 0 || e.AttemptsLeft >= api.LoginAttempts {
			return protocol()
		}
		// Known codes have one HTTP meaning. Unknown codes are retained as errors.
		if api.KnownCode(e.Code) && api.HTTPStatus(e.Code) != resp.StatusCode {
			return protocol()
		}
		e.Message = api.Message(e.Code)
		e.RequestID = id
		e.Status = resp.StatusCode
		return &e
	}
	if Decode(data, output) != nil {
		return protocol()
	}
	if v, ok := output.(interface{ Validate() error }); !ok || v.Validate() != nil {
		return protocol()
	}
	return nil
}

// Decode accepts one non-null JSON object. Forward-compatible fields are ignored.
func Decode(data []byte, out any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("expected JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}
