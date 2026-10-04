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

package purlview

import (
	"context"
	"net/http"

	"github.com/idyl-labs/purlview/sdk/api"
)

// StartLogin starts an anonymous email challenge without selecting credentials.
func (c *Client) StartLogin(ctx context.Context, r api.LoginStartRequest) (*api.LoginChallenge, error) {
	if r.Validate() != nil {
		return nil, invalid()
	}
	var v api.LoginChallenge
	err := c.transport.Call(ctx, http.MethodPost, api.LoginStartPath, "", "", r, &v, true)
	return &v, err
}

// VerifyLogin consumes a single-use code and returns installation authority.
func (c *Client) VerifyLogin(ctx context.Context, r api.LoginVerifyRequest) (*api.InstallationCredential, error) {
	if r.Validate() != nil {
		return nil, invalid()
	}
	var v api.InstallationCredential
	err := c.transport.Call(ctx, http.MethodPost, api.LoginVerifyPath, "", "", r, &v, true)
	return &v, err
}

// ResendLogin requests a replacement code within the original bounded challenge.
func (c *Client) ResendLogin(ctx context.Context, r api.LoginResendRequest) (*api.LoginChallenge, error) {
	if r.Validate() != nil {
		return nil, invalid()
	}
	var v api.LoginChallenge
	err := c.transport.Call(ctx, http.MethodPost, api.LoginResendPath, "", "", r, &v, true)
	return &v, err
}
