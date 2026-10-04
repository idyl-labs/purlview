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

package share

import (
	"context"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/purlview"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// SDKDirectory adapts account-owned product results to the CLI presentation view.
type SDKDirectory struct {
	Client func() (*purlview.Client, error)
}

func (s SDKDirectory) authorised(cred api.InstallationCredential) (*purlview.Client, error) {
	c, e := s.Client()
	if e != nil {
		return nil, e
	}
	return c.WithCredentials(func(context.Context) (string, error) { return cred.Token, nil }), nil
}

// List intentionally redisplays creator-authorised distributable links.
func (s SDKDirectory) List(ctx context.Context, cred api.InstallationCredential) ([]Share, error) {
	c, e := s.authorised(cred)
	if e != nil {
		return nil, e
	}
	r, e := c.ListShares(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]Share, 0, len(r.Shares))
	for _, v := range r.Shares {
		sh := view(v.Share)
		sh.URL = v.URL
		out = append(out, sh)
	}
	return out, nil
}

// Revoke strips recipient credentials before crossing the management boundary.
func (s SDKDirectory) Revoke(ctx context.Context, cred api.InstallationCredential, ref Ref) (RevokeResult, error) {
	parsed, e := ref.clean()
	if e != nil {
		return RevokeResult{}, &api.Error{Code: api.InvalidRequest, Outcome: api.NotApplied}
	}
	c, e := s.authorised(cred)
	if e != nil {
		return RevokeResult{}, e
	}
	r, e := c.RevokeShare(ctx, api.RevokeShareRequest{Reference: resource.ShareRef{ID: parsed.ID, Origin: parsed.URL}})
	if e != nil {
		return RevokeResult{}, e
	}
	if r.Outcome != "confirmed" {
		return RevokeResult{}, &api.Error{Code: api.Unavailable, Outcome: api.Unknown}
	}
	return RevokeResult{Share: view(r.Share), AlreadyEnded: r.AlreadyEnded, OwnerNotified: r.OwnerNotified}, nil
}

func view(s resource.Share) Share {
	return Share{ID: s.ID, Origin: s.Origin, Target: s.Target, Targets: s.Targets, Device: s.Device, DeviceLabel: s.DeviceLabel, Recipients: s.Recipients, RewriteURLs: s.RewriteURLs, State: State(s.State), CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt}
}
