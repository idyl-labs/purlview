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

// Package purlview provides a native, context-aware product API client. It
// does not read configuration, store credentials, print or control processes.
package purlview

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/internal/transport"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// CredentialSource supplies installation authority for management operations.
// It is never called for anonymous begin or purpose-scoped authorisation polling.
type CredentialSource func(context.Context) (string, error)

// Config is supplied by application wiring. HTTPS verification is never disabled.
type Config struct {
	Endpoint       string
	Transport      http.RoundTripper
	RequestTimeout time.Duration
	Credentials    CredentialSource
	AllowHTTP      bool
	Host           string
	// Client names the calling software, "purlview/<version>"; it is sent
	// on every request so the platform can refuse versions it no longer
	// serves.
	Client string
}

// Client is safe for concurrent use when its supplied transport/source are safe.
type Client struct {
	transport   *transport.Client
	credentials CredentialSource
}

// New validates explicit configuration without performing I/O.
func New(cfg Config) (*Client, error) {
	t, err := transport.New(transport.Config{Endpoint: cfg.Endpoint, Transport: cfg.Transport, Timeout: cfg.RequestTimeout, AllowHTTP: cfg.AllowHTTP, Host: cfg.Host, Client: cfg.Client})
	if err != nil {
		return nil, err
	}
	return &Client{transport: t, credentials: cfg.Credentials}, nil
}

// WithCredentials returns a client sharing transport but using a per-consumer
// installation source. This avoids shared mutable tokens across daemon shares.
func (c *Client) WithCredentials(source CredentialSource) *Client {
	return &Client{transport: c.transport, credentials: source}
}

// CloseIdleConnections releases pooled connections when the application is done.
func (c *Client) CloseIdleConnections() { c.transport.CloseIdleConnections() }

func invalid() error { return &api.Error{Code: api.InvalidRequest, Outcome: api.NotApplied} }
func (c *Client) bearer(ctx context.Context) (string, error) {
	if c.credentials == nil {
		return "", &api.Error{Code: api.Unauthorised, Outcome: api.NotApplied}
	}
	token, err := c.credentials(ctx)
	if err != nil {
		return "", &api.Error{Code: api.Unauthorised, Outcome: api.NotApplied, Cause: ctx.Err()}
	}
	if !api.Token(token) {
		return "", &api.Error{Code: api.Unauthorised, Outcome: api.NotApplied}
	}
	return "Bearer " + token, nil
}

// BeginAuthorization starts a sign-in anonymously. It has no automatic retries.
func (c *Client) BeginAuthorization(ctx context.Context, r api.BeginAuthorizationRequest) (*api.Authorization, error) {
	if r.Validate() != nil {
		return nil, invalid()
	}
	var result api.Authorization
	err := c.transport.Call(ctx, http.MethodPost, api.AuthorizationsPath, "", "", r, &result, true)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ObserveAuthorization checks one request with polling authority only.
func (c *Client) ObserveAuthorization(ctx context.Context, a api.Authorization) (*api.AuthorizationStatus, error) {
	if !resource.Identifier(a.ID) || !api.Token(a.PollToken) {
		return nil, invalid()
	}
	var result api.AuthorizationStatus
	err := c.transport.Call(ctx, http.MethodPost, api.ObservePath, "Purlview-Poll "+a.PollToken, "", api.ObserveAuthorizationRequest{ID: a.ID}, &result, false)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// WaitAuthorization polls until approval, denial, expiry or cancellation. Polling
// is read-only; transient errors return to the caller, never hide an outage.
func (c *Client) WaitAuthorization(ctx context.Context, a api.Authorization) (*api.InstallationCredential, error) {
	if a.Validate() != nil {
		return nil, invalid()
	}
	deadline := a.ExpiresAt
	if capAt := time.Now().Add(10 * time.Minute); deadline.After(capAt) {
		deadline = capAt
	}
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		if wctx.Err() != nil {
			return nil, &api.Error{Code: api.Expired, Outcome: api.NotApplied, Cause: wctx.Err()}
		}
		result, err := c.ObserveAuthorization(wctx, a)
		if err != nil {
			return nil, err
		}
		switch result.State {
		case "approved":
			return result.Credential, nil
		case "denied":
			return nil, &api.Error{Code: api.Denied, Outcome: api.NotApplied}
		case "expired":
			return nil, &api.Error{Code: api.Expired, Outcome: api.NotApplied}
		}
		timer := time.NewTimer(time.Duration(a.PollIntervalMS) * time.Millisecond)
		select {
		case <-wctx.Done():
			timer.Stop()
			return nil, &api.Error{Code: api.Expired, Outcome: api.NotApplied, Cause: wctx.Err()}
		case <-timer.C:
		}
	}
}

// Identity validates installation authority and returns current public identity.
func (c *Client) Identity(ctx context.Context) (*resource.Identity, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	var result resource.Identity
	err = c.transport.Call(ctx, http.MethodGet, api.IdentityPath, token, "", nil, &result, false)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RevokeInstallation revokes this installation and reports whether the
// platform confirmed that its access and its shares have ended.
func (c *Client) RevokeInstallation(ctx context.Context) (*api.Revocation, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	var result api.Revocation
	err = c.transport.Call(ctx, http.MethodPost, api.InstallationRevokePath, token, "", struct{}{}, &result, true)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ListInstallations returns the account's active installations; the caller's is current.
func (c *Client) ListInstallations(ctx context.Context) (*api.ListInstallationsResult, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	var result api.ListInstallationsResult
	err = c.transport.Call(ctx, http.MethodGet, api.InstallationsPath, token, "", nil, &result, false)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RevokeInstallationByID revokes one of the account's installations and ends its
// shares. An id of another account is not_found, like an unknown id. As with
// every revocation, only Outcome "confirmed" proves enforcement.
func (c *Client) RevokeInstallationByID(ctx context.Context, r api.RevokeInstallationRequest) (*api.RevokeInstallationResult, error) {
	if r.Validate() != nil {
		return nil, invalid()
	}
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	result := installationRevokeResult{id: r.ID}
	err = c.transport.Call(ctx, http.MethodPost, api.InstallationsRevokePath, token, "", r, &result, true)
	if err != nil {
		return nil, err
	}
	return &result.RevokeInstallationResult, nil
}

// CreateShare submits one attempt; callers retain Key when reconciling lost replies.
// Recipients are sent in recorded form (lower case, no repeats).
func (c *Client) CreateShare(ctx context.Context, r api.CreateShareRequest) (*api.ShareAccess, error) {
	if r.Validate() != nil {
		return nil, invalid()
	}
	r = r.Normalised()
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	result := createResult{request: r}
	err = c.transport.Call(ctx, http.MethodPost, api.SharesPath, token, r.Key, r, &result, true)
	if err != nil {
		return nil, err
	}
	return &result.ShareAccess, nil
}

// ListShares retrieves account-owned shares, including intentional link rediscovery.
func (c *Client) ListShares(ctx context.Context) (*api.ListSharesResult, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	var result api.ListSharesResult
	err = c.transport.Call(ctx, http.MethodGet, api.SharesPath, token, "", nil, &result, false)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RevokeShare reports confirmed or unconfirmed enforcement using canonical identity.
func (c *Client) RevokeShare(ctx context.Context, r api.RevokeShareRequest) (*api.RevokeShareResult, error) {
	if r.Validate() != nil {
		return nil, invalid()
	}
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	result := revokeResult{reference: r.Reference}
	err = c.transport.Call(ctx, http.MethodPost, api.ShareRevokePath, token, "", r, &result, true)
	if err != nil {
		return nil, err
	}
	return &result.RevokeShareResult, nil
}

// Response correlation adds operation-specific checks without another wire shape.
type createResult struct {
	api.ShareAccess
	request api.CreateShareRequest
}

func (r createResult) Validate() error {
	if err := r.ShareAccess.Validate(); err != nil {
		return err
	}
	if r.Share.Target != r.request.Target || !slices.Equal(r.Share.Recipients, r.request.Recipients) || r.Share.RewriteURLs != r.request.RewriteURLs || r.Share.ExpiresAt.Sub(r.Share.CreatedAt) != r.request.TTL {
		return errors.New("create response does not match the request")
	}
	// A platform from before several targets records none; one that records
	// them must record what was asked.
	if len(r.Share.Targets) != 0 && !slices.Equal(r.Share.Targets, r.request.Targets) {
		return errors.New("create response does not match the request's targets")
	}
	// A tunnel comes back exactly when a share key was sent, unless a replay
	// finds the share already ended.
	if r.Tunnel != nil && r.request.PublicKey == nil || r.Tunnel == nil && r.request.PublicKey != nil && r.Share.State != "ended" {
		return errors.New("create response tunnel does not match the request")
	}
	return nil
}

type installationRevokeResult struct {
	api.RevokeInstallationResult
	id string
}

func (r installationRevokeResult) Validate() error {
	if err := r.RevokeInstallationResult.Validate(); err != nil {
		return err
	}
	if r.ID != r.id {
		return errors.New("revocation response does not match the installation")
	}
	return nil
}

type revokeResult struct {
	api.RevokeShareResult
	reference resource.ShareRef
}

func (r revokeResult) Validate() error {
	if err := r.RevokeShareResult.Validate(); err != nil {
		return err
	}
	if r.reference.ID != "" && r.reference.ID != r.Share.ID || r.reference.Origin != "" && r.reference.Origin != r.Share.Origin && r.reference.Origin != r.Share.EntryOrigin {
		return errors.New("revocation response does not match the reference")
	}
	return nil
}
