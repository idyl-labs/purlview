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

package engine

import (
	"context"
	"errors"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/purlview"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// SDKManagement implements the engine's management boundary over the
// account API. A tunnel client is composed with it (ComposedPlatform).
type SDKManagement struct{ Client *purlview.Client }

func (s SDKManagement) authorised(cred api.InstallationCredential) *purlview.Client {
	return s.Client.WithCredentials(func(context.Context) (string, error) { return cred.Token, nil })
}

// CreateShare preserves the engine's attempt key and original result unchanged.
func (s SDKManagement) CreateShare(ctx context.Context, cred api.InstallationCredential, r api.CreateShareRequest) (*api.ShareAccess, error) {
	result, err := s.authorised(cred).CreateShare(ctx, r)
	return result, managementError(err)
}

// Revoke returns nil only for explicit enforcement confirmation.
func (s SDKManagement) Revoke(ctx context.Context, cred api.InstallationCredential, id string) error {
	r, err := s.authorised(cred).RevokeShare(ctx, api.RevokeShareRequest{Reference: resource.ShareRef{ID: id}})
	if err != nil {
		return managementError(err)
	}
	if r.Outcome != "confirmed" {
		return &Uncertain{Cause: &api.Error{Code: api.Unavailable, Outcome: api.Unknown}}
	}
	return nil
}

// ShareActive looks for the share among the account's active shares, which
// are the only ones the platform lists.
func (s SDKManagement) ShareActive(ctx context.Context, cred api.InstallationCredential, id string) (bool, error) {
	r, err := s.authorised(cred).ListShares(ctx)
	if err != nil {
		return false, managementError(err)
	}
	for _, v := range r.Shares {
		if v.Share.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// Uncertain means a write may have applied. It must not be described as rejected
// or replayed under a new key. The SDK itself performs no automatic write retries.
type Uncertain struct{ Cause error }

func (u *Uncertain) Error() string {
	return "Purlview did not confirm whether the operation completed"
}

// Unwrap preserves the typed API error and cancellation identity.
func (u *Uncertain) Unwrap() error { return u.Cause }

func managementError(err error) error {
	if err == nil {
		return nil
	}
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	if e.Outcome == api.Unknown {
		return &Uncertain{Cause: err}
	}
	var reason RejectReason
	switch e.Code {
	case api.Unauthorised:
		reason = RejectAuthority
	case api.NotFound:
		reason = RejectUnknown
	case api.Expired:
		reason = RejectExpired
	case api.InvalidRequest, api.Conflict, api.Denied:
		reason = RejectRequest
	case api.UpdateRequired:
		reason = RejectUpdate
	}
	if reason != "" {
		rej := &Rejected{Reason: reason, Detail: err.Error(), Cause: err}
		if e.Code == api.Denied {
			rej.Limit = string(e.Limit)
		}
		return rej
	}
	return &Unavailable{Detail: err.Error(), Cause: err}
}
