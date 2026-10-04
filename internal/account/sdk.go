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

package account

import (
	"context"
	"errors"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/purlview"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// SDK adapts the platform client to CLI authorisation. Client is resolved lazily
// by application wiring; browser, credential custody and output stay in the CLI.
type SDK struct {
	Client func() (*purlview.Client, error)
}

// Begin starts anonymous authorisation, without reading the local credential store.
func (s SDK) Begin(ctx context.Context, r api.BeginAuthorizationRequest) (*api.Authorization, error) {
	c, e := s.Client()
	if e != nil {
		return nil, e
	}
	return c.BeginAuthorization(ctx, r)
}

// Wait observes approval using only its purpose-scoped polling credential.
func (s SDK) Wait(ctx context.Context, a *api.Authorization) (*api.InstallationCredential, error) {
	c, e := s.Client()
	if e != nil {
		return nil, e
	}
	if a == nil {
		return nil, &api.Error{Code: api.InvalidRequest, Outcome: api.NotApplied}
	}
	return c.WaitAuthorization(ctx, *a)
}

// Validate asks for the installation's current identity.
func (s SDK) Validate(ctx context.Context, cred *api.InstallationCredential) (*resource.Identity, error) {
	c, e := s.authorised(cred)
	if e != nil {
		return nil, e
	}
	return c.Identity(ctx)
}

// Revoke succeeds only after enforcement was explicitly confirmed.
func (s SDK) Revoke(ctx context.Context, cred *api.InstallationCredential) error {
	c, e := s.authorised(cred)
	if e != nil {
		return e
	}
	r, e := c.RevokeInstallation(ctx)
	if e != nil {
		var failure *api.Error
		if errors.As(e, &failure) && failure.Code == api.Unauthorised {
			// A rejected bearer confirms nothing about device or share
			// enforcement, so an HTTP authentication error is never read as
			// "already revoked".
			return &Failure{Kind: KindDenied, Detail: "Purlview refused the revocation; this device's authorisation was not confirmed revoked", Cause: e}
		}
		return e
	}
	if r.Outcome != "confirmed" {
		return &api.Error{Code: api.Unavailable, Outcome: api.Unknown}
	}
	return nil
}
func (s SDK) authorised(cred *api.InstallationCredential) (*purlview.Client, error) {
	c, e := s.Client()
	if e != nil {
		return nil, e
	}
	if cred == nil {
		return nil, &api.Error{Code: api.Unauthorised, Outcome: api.NotApplied}
	}
	token := cred.Token
	return c.WithCredentials(func(context.Context) (string, error) { return token, nil }), nil
}

// ListInstallations returns the account's active installations.
func (s SDK) ListInstallations(ctx context.Context, cred *api.InstallationCredential) ([]api.Installation, error) {
	c, e := s.authorised(cred)
	if e != nil {
		return nil, e
	}
	r, e := c.ListInstallations(ctx)
	if e != nil {
		return nil, e
	}
	return r.Installations, nil
}

// RevokeInstallation succeeds only after enforcement was explicitly confirmed.
func (s SDK) RevokeInstallation(ctx context.Context, cred *api.InstallationCredential, id string) (*api.RevokeInstallationResult, error) {
	c, e := s.authorised(cred)
	if e != nil {
		return nil, e
	}
	r, e := c.RevokeInstallationByID(ctx, api.RevokeInstallationRequest{ID: id})
	if e != nil {
		return nil, e
	}
	if r.Outcome != "confirmed" {
		return r, &api.Error{Code: api.Unavailable, Outcome: api.Unknown}
	}
	return r, nil
}

// StartLogin starts an anonymous email challenge without selecting credentials.
func (s SDK) StartLogin(ctx context.Context, r api.LoginStartRequest) (*api.LoginChallenge, error) {
	c, e := s.Client()
	if e != nil {
		return nil, e
	}
	return c.StartLogin(ctx, r)
}

// VerifyLogin consumes a single-use code and returns installation authority.
func (s SDK) VerifyLogin(ctx context.Context, r api.LoginVerifyRequest) (*api.InstallationCredential, error) {
	c, e := s.Client()
	if e != nil {
		return nil, e
	}
	return c.VerifyLogin(ctx, r)
}

// ResendLogin requests a replacement code within the original bounded challenge.
func (s SDK) ResendLogin(ctx context.Context, r api.LoginResendRequest) (*api.LoginChallenge, error) {
	c, e := s.Client()
	if e != nil {
		return nil, e
	}
	return c.ResendLogin(ctx, r)
}
