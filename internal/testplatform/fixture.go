// Package testplatform is a fake Purlview platform for integration tests: a
// controlled apiserver.Service with synthetic records. Serve it with
// apiserver.New to test the CLI, the share engine or the SDK over real HTTP.
// It is never imported by the purlview executable. The fixture proves
// boundary semantics, not real authentication, persistence or enforcement.
package testplatform

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/apiserver"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// Token is synthetic installation authority with no live validity.
const Token = "synthetic-installation-secret"

// PollToken is separately scoped synthetic authorisation observation authority.
const PollToken = "synthetic-poll-secret"

// Identity returns the fixture's public installation.
func Identity() resource.Identity {
	return resource.Identity{Account: "creator@example.invalid", AccountID: "acc_test", Device: "dev_test", DeviceLabel: "test device"}
}

// Credential returns synthetic credential material for isolated test custody.
func Credential() api.InstallationCredential {
	return api.InstallationCredential{Identity: Identity(), Token: Token, IssuedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)}
}

// Service is a controlled backend. Hooks must be configured before serving.
type Service struct {
	apiserver.Unimplemented
	Now          time.Time
	ObserveState string
	BeginHook    func(context.Context, api.BeginAuthorizationRequest) (api.Authorization, error)
	ObserveHook  func(context.Context, string, api.ObserveAuthorizationRequest) (api.AuthorizationStatus, error)
	CreateHook   func(context.Context, string, api.CreateShareRequest) (api.ShareAccess, error)
	RevokeHook   func(context.Context, string, api.RevokeShareRequest) (api.RevokeShareResult, error)
	// InviteStatus makes create report this result for every recipient, as a
	// platform that sends invites does. Empty: no invites are attempted.
	InviteStatus string
	mu           sync.Mutex
	requests     map[string]api.CreateShareRequest
	shares       map[string]api.ShareAccess
	revoked      bool
	otherRevoked bool
	creates      int
	revokes      int
	misses       int
	sends        int
}

// Code is the sign-in code of the fixture's first email; Resent replaces it.
const (
	Code   = "482913"
	Resent = "307561"
)

// OtherDevice is a second installation of the fixture account.
const OtherDevice = "dev_other"

// New builds an isolated fixture; callers opt in by injecting it into real handlers.
func New() *Service {
	return &Service{Now: time.Now().UTC().Truncate(time.Second), ObserveState: "approved", requests: map[string]api.CreateShareRequest{}, shares: map[string]api.ShareAccess{}}
}
func failure(code api.Code) error               { return &api.Error{Code: code, Outcome: api.NotApplied} }
func (s *Service) authorised(token string) bool { return token == Token && !s.revoked }

// BeginAuthorization returns synthetic hand-off and polling credentials.
func (s *Service) BeginAuthorization(ctx context.Context, r api.BeginAuthorizationRequest) (api.Authorization, error) {
	if s.BeginHook != nil {
		return s.BeginHook(ctx, r)
	}
	return api.Authorization{ID: "auth_test", PollToken: PollToken, BrowserURL: "https://account.example.invalid/login", VerificationURL: "https://account.example.invalid/device", UserCode: "TEST-CODE", ExpiresAt: s.Now.Add(5 * time.Minute), PollIntervalMS: 100}, nil
}

// ObserveAuthorization enforces polling purpose even though both tokens are strings.
func (s *Service) ObserveAuthorization(ctx context.Context, token string, r api.ObserveAuthorizationRequest) (api.AuthorizationStatus, error) {
	if s.ObserveHook != nil {
		return s.ObserveHook(ctx, token, r)
	}
	if token != PollToken || r.ID != "auth_test" {
		return api.AuthorizationStatus{}, failure(api.Unauthorised)
	}
	result := api.AuthorizationStatus{State: s.ObserveState}
	if result.State == "approved" {
		c := Credential()
		result.Credential = &c
	}
	return result, nil
}

// Identity validates creator authority on every call.
func (s *Service) Identity(_ context.Context, token string) (resource.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorised(token) {
		return resource.Identity{}, failure(api.Unauthorised)
	}
	return Identity(), nil
}

// RevokeInstallation revokes this installation, with explicit confirmation.
func (s *Service) RevokeInstallation(_ context.Context, token string) (api.Revocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token != Token {
		return api.Revocation{}, failure(api.Unauthorised)
	}
	already := s.revoked
	s.revoked = true
	for k, v := range s.shares {
		v.Share.State = "ended"
		s.shares[k] = v
	}
	return api.Revocation{Outcome: "confirmed", AlreadyEnded: already}, nil
}

// CreateShare enforces same-key/same-input identity and original expiry.
func (s *Service) CreateShare(ctx context.Context, token string, r api.CreateShareRequest) (api.ShareAccess, error) {
	if s.CreateHook != nil {
		return s.CreateHook(ctx, token, r)
	}
	return s.Create(ctx, token, r)
}

// Create is the fixture's default create implementation, usable by controlled hooks.
func (s *Service) Create(_ context.Context, token string, r api.CreateShareRequest) (api.ShareAccess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if !s.authorised(token) {
		return api.ShareAccess{}, failure(api.Unauthorised)
	}
	if prev, ok := s.requests[r.Key]; ok {
		if !prev.Same(r) {
			return api.ShareAccess{}, failure(api.Conflict)
		}
		// A replay reports the recorded invite results and sends nothing again.
		return s.shares[r.Key], nil
	}
	origin := fmt.Sprintf("https://share%d.content.example.invalid", len(s.shares)+1)
	sh := resource.Share{ID: fmt.Sprintf("shr_test%d", len(s.shares)+1), Origin: origin, Target: r.Target, Targets: r.Targets, Device: Identity().Device, DeviceLabel: Identity().DeviceLabel, Recipients: r.Recipients, RewriteURLs: r.RewriteURLs, State: "starting", CreatedAt: s.Now, ExpiresAt: s.Now.Add(r.TTL)}
	link := origin + "/access"
	if len(r.Recipients) == 0 {
		link += "?token=synthetic-recipient-secret"
	}
	result := api.ShareAccess{Share: sh, URL: link}
	// A share key is answered with synthetic tunnel material, which only the
	// injected tunnel clients of these tests receive.
	if r.PublicKey != nil {
		result.Tunnel = &api.Tunnel{Endpoint: "edge.example.invalid:443", Edge: "spiffe://example.invalid/edge", Gateway: "spiffe://example.invalid/gateway",
			SVID: [][]byte{[]byte("synthetic-svid")}, Lease: []byte("synthetic-lease"), Bundle: [][]byte{[]byte("synthetic-root")}}
	}
	for _, email := range r.Recipients {
		if s.InviteStatus != "" {
			result.Invites = append(result.Invites, api.InviteResult{Email: email, Status: s.InviteStatus})
		}
	}
	s.requests[r.Key] = r
	s.shares[r.Key] = result
	return result, nil
}

// ListShares intentionally redelivers access links only to creator authority.
func (s *Service) ListShares(_ context.Context, token string) (api.ListSharesResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorised(token) {
		return api.ListSharesResult{}, failure(api.Unauthorised)
	}
	result := api.ListSharesResult{Shares: []api.ShareAccess{}}
	for _, v := range s.shares {
		v.Tunnel = nil // only a create carries a share's tunnel material
		result.Shares = append(result.Shares, v)
	}
	return result, nil
}

// RevokeShare models confirmed enforcement, not only owner notification.
func (s *Service) RevokeShare(ctx context.Context, token string, r api.RevokeShareRequest) (api.RevokeShareResult, error) {
	if s.RevokeHook != nil {
		return s.RevokeHook(ctx, token, r)
	}
	return s.Revoke(ctx, token, r)
}

// Revoke is the fixture's default revoke implementation, usable by hooks.
func (s *Service) Revoke(_ context.Context, token string, r api.RevokeShareRequest) (api.RevokeShareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokes++
	if !s.authorised(token) {
		return api.RevokeShareResult{}, failure(api.Unauthorised)
	}
	for k, v := range s.shares {
		if v.Share.ID == r.Reference.ID || v.Share.Origin == r.Reference.Origin {
			already := v.Share.State == "ended"
			v.Share.State = "ended"
			s.shares[k] = v
			return api.RevokeShareResult{Revocation: api.Revocation{Outcome: "confirmed", AlreadyEnded: already}, Share: v.Share, OwnerNotified: false}, nil
		}
	}
	return api.RevokeShareResult{}, failure(api.NotFound)
}

// Counts reports attempted creates/revokes and distinct records under lock.
func (s *Service) Counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates, s.revokes, len(s.shares)
}

// StartLogin starts an anonymous email challenge without selecting credentials.
func (s *Service) StartLogin(_ context.Context, r api.LoginStartRequest) (api.LoginChallenge, error) {
	return api.LoginChallenge{ID: "auth_test", ExpiresAt: s.Now.Add(5 * time.Minute), ResendAt: s.Now.Add(time.Minute)}, nil
}

// VerifyLogin consumes a single-use code and returns installation authority. A
// miss reports the attempts left; the fifth ends the challenge.
func (s *Service) VerifyLogin(_ context.Context, r api.LoginVerifyRequest) (api.InstallationCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := Code
	if s.sends > 0 {
		want = Resent
	}
	if r.ID != "auth_test" || s.misses >= 5 {
		return api.InstallationCredential{}, failure(api.Expired)
	}
	if r.Code != want {
		s.misses++
		if s.misses >= 5 {
			return api.InstallationCredential{}, failure(api.Expired)
		}
		return api.InstallationCredential{}, &api.Error{Code: api.Denied, Outcome: api.NotApplied, AttemptsLeft: 5 - s.misses}
	}
	return Credential(), nil
}

// ResendLogin replaces the code once, keeping the challenge's expiry. The
// fixture has no clock: the first resend is always on time.
func (s *Service) ResendLogin(_ context.Context, r api.LoginResendRequest) (api.LoginChallenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID != "auth_test" || s.sends > 0 {
		return api.LoginChallenge{}, failure(api.Denied)
	}
	s.sends++
	return api.LoginChallenge{ID: r.ID, ExpiresAt: s.Now.Add(5 * time.Minute), ResendAt: s.Now.Add(2 * time.Minute)}, nil
}

// ListInstallations returns this installation and, until it is revoked, another.
func (s *Service) ListInstallations(_ context.Context, token string) (api.ListInstallationsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorised(token) {
		return api.ListInstallationsResult{}, failure(api.Unauthorised)
	}
	own := api.Installation{ID: Identity().Device, Label: Identity().DeviceLabel, CreatedAt: s.Now, LastUsedAt: s.Now, Current: true}
	for _, v := range s.shares {
		if v.Share.State != "ended" {
			own.ActiveShares++
		}
	}
	out := api.ListInstallationsResult{Installations: []api.Installation{own}}
	if !s.otherRevoked {
		out.Installations = append(out.Installations, api.Installation{ID: OtherDevice, Label: "other device", CreatedAt: s.Now.Add(-(14*24*time.Hour + 150*time.Minute))})
	}
	return out, nil
}

// RevokeInstallationByID revokes either fixture installation; any other id is
// not_found, as an id of another account is.
func (s *Service) RevokeInstallationByID(_ context.Context, token string, r api.RevokeInstallationRequest) (api.RevokeInstallationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorised(token) {
		return api.RevokeInstallationResult{}, failure(api.Unauthorised)
	}
	out := api.RevokeInstallationResult{Revocation: api.Revocation{Outcome: "confirmed"}, ID: r.ID}
	switch r.ID {
	case OtherDevice:
		out.AlreadyEnded = s.otherRevoked
		s.otherRevoked = true
	case Identity().Device:
		s.revoked = true
		for k, v := range s.shares {
			if v.Share.State != "ended" {
				out.SharesStopped++
			}
			v.Share.State = "ended"
			s.shares[k] = v
		}
	default:
		return api.RevokeInstallationResult{}, failure(api.NotFound)
	}
	return out, nil
}
