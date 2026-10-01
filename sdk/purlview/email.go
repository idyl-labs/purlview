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
