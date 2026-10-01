// Package account holds the creator identity the CLI remembers for this
// installation and the two boundaries around it: local credential custody
// (Store) and platform authorisation (Authorizer).
//
// The boundaries are injectable and exchange the SDK's resource and API
// types, so commands run unchanged against the real platform and against
// test fixtures.
package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// Store is local credential custody for this installation.
type Store interface {
	// Load returns the remembered credential, or (nil, nil) when signed out.
	Load() (*api.InstallationCredential, error)
	// Save remembers a credential, replacing any previous one.
	Save(*api.InstallationCredential) error
	// Clear forgets the credential; clearing an absent one succeeds.
	Clear() error
}

// Authorizer is the platform side of login, logout and identity checks.
type Authorizer interface {
	// Begin registers a sign-in request and returns what the user must do.
	Begin(ctx context.Context, req api.BeginAuthorizationRequest) (*api.Authorization, error)
	// Wait blocks until the request is approved (returning the issued
	// credential), denied or expired, or until ctx is done.
	Wait(ctx context.Context, auth *api.Authorization) (*api.InstallationCredential, error)
	// Validate checks that the credential still carries authority and
	// returns the current identity.
	Validate(ctx context.Context, cred *api.InstallationCredential) (*resource.Identity, error)
	// Revoke ends this installation's authorisation on the platform. The
	// platform also ends the device's shares.
	Revoke(ctx context.Context, cred *api.InstallationCredential) error
}

// Kind classifies an authorisation failure so a command can choose its
// message and exit status.
type Kind string

// Failure kinds.
const (
	// KindDenied: the user declined the sign-in.
	KindDenied Kind = "denied"
	// KindExpired: the platform closed the request before approval.
	KindExpired Kind = "expired"
	// KindUnavailable: the platform could not be reached or answered with
	// a transient failure. Retrying later may succeed.
	KindUnavailable Kind = "unavailable"
	// KindUnauthorised: the credential is not (or no longer) valid.
	KindUnauthorised Kind = "unauthorised"
	// KindNotFound: the account has no such installation.
	KindNotFound Kind = "not_found"
)

// Failure is a typed authorisation failure.
type Failure struct {
	Cause  error
	Kind   Kind
	Detail string
}

func (f *Failure) Error() string {
	if f.Detail == "" {
		return string(f.Kind)
	}
	return fmt.Sprintf("%s: %s", f.Kind, f.Detail)
}

// Unwrap retains an underlying SDK failure when adapting command semantics.
func (f *Failure) Unwrap() error { return f.Cause }

// IsKind reports whether err is a Failure of the given kind.
func IsKind(err error, kind Kind) bool {
	var f *Failure
	if errors.As(err, &f) {
		return f.Kind == kind
	}
	var remote *api.Error
	return errors.As(err, &remote) && remote.Outcome == api.NotApplied && string(remote.Code) == string(kind)
}

// ErrNotImplemented is returned when no platform is configured. Commands
// report it verbatim so nobody mistakes such a build for a working sign-in.
var ErrNotImplemented = api.ErrNotImplemented

// Unimplemented is the Authorizer of a build with no platform configured.
type Unimplemented struct{}

// Begin fails honestly.
func (Unimplemented) Begin(context.Context, api.BeginAuthorizationRequest) (*api.Authorization, error) {
	return nil, ErrNotImplemented
}

// Wait fails honestly.
func (Unimplemented) Wait(context.Context, *api.Authorization) (*api.InstallationCredential, error) {
	return nil, ErrNotImplemented
}

// Validate fails honestly.
func (Unimplemented) Validate(context.Context, *api.InstallationCredential) (*resource.Identity, error) {
	return nil, ErrNotImplemented
}

// Revoke fails honestly.
func (Unimplemented) Revoke(context.Context, *api.InstallationCredential) error {
	return ErrNotImplemented
}

// EmailAuthorizer signs a creator in with a code sent by email.
//
// A wrong code fails with KindDenied and AttemptsLeft; the last miss and a
// lapsed challenge fail with KindExpired. A successful resend returns the
// unchanged ExpiresAt and the next ResendAt; one that is too early, or past
// the send limit, is KindDenied, and the challenge's ResendAt tells which.
type EmailAuthorizer interface {
	StartLogin(context.Context, api.LoginStartRequest) (*api.LoginChallenge, error)
	VerifyLogin(context.Context, api.LoginVerifyRequest) (*api.InstallationCredential, error)
	ResendLogin(context.Context, api.LoginResendRequest) (*api.LoginChallenge, error)
}

// AttemptsLeft reports how many more codes the sign-in challenge accepts after
// a wrong one, or 0 when the platform did not say.
func AttemptsLeft(err error) int {
	var remote *api.Error
	if errors.As(err, &remote) {
		return remote.AttemptsLeft
	}
	return 0
}

// Installations lists the account's authorised installations and revokes one
// of them, which also ends its shares. An Authorizer may implement it.
type Installations interface {
	// ListInstallations returns the active installations, oldest first;
	// at most one is Current (none once this one was revoked).
	ListInstallations(ctx context.Context, cred *api.InstallationCredential) ([]api.Installation, error)
	// RevokeInstallation succeeds only when the platform confirmed enforcement.
	// An id the account does not have fails with KindNotFound.
	RevokeInstallation(ctx context.Context, cred *api.InstallationCredential, id string) (*api.RevokeInstallationResult, error)
}
