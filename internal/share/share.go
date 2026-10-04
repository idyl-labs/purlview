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

// Package share defines what the CLI needs from the local daemon (Runner
// and Session) and from the platform's account-scoped share directory
// (Directory), together with the share, event and failure types those
// boundaries exchange.
//
// These are internal Go behaviour types, deliberately separate from the
// IPC wire messages in package ipc and the canonical SDK product models. The
// runnable scenarios in package scenario exercise the contract they express.
package share

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
)

// Spec is a validated share request.
type Spec struct {
	// Targets is the share's target list from ParseTargets: the first is what
	// the link opens, the others are served beside it.
	Targets []Target
	// TTL is the requested lifetime, at most MaxTTL.
	TTL time.Duration
	// Recipients restricts access to these verified addresses (normalised by
	// ParseRecipients); empty means anyone with the link.
	Recipients []string
	// NoRewrite leaves the apps' addresses in response bodies unchanged
	// (--no-rewrite); rewriting is otherwise on.
	NoRewrite bool
}

// Owner says who keeps a share alive.
type Owner string

// Owners.
const (
	// OwnerAttached: the CLI connection that started the share owns it;
	// Ctrl-C, an explicit stop or loss of that connection ends it.
	OwnerAttached Owner = "attached"
	// OwnerDetached: the daemon owns it once it has acknowledged the start;
	// the starting CLI may exit.
	OwnerDetached Owner = "detached"
)

// State of a share as the daemon reports it.
type State string

// States.
const (
	StateStarting     State = "starting"
	StateReady        State = "ready"
	StateReconnecting State = "reconnecting"
	StateEnded        State = "ended"
)

// EndReason says why a share ended. Reasons are part of the contract: a
// CLI chooses its message and exit status from them.
type EndReason string

// End reasons.
const (
	// ReasonStopped: the owner asked this daemon to stop it (Ctrl-C, unshare).
	ReasonStopped EndReason = "stopped"
	// ReasonCancelled: stopped before it became ready.
	ReasonCancelled EndReason = "cancelled"
	// ReasonExpired: the lifetime elapsed.
	ReasonExpired EndReason = "expired"
	// ReasonRevoked: the platform revoked it (unshare elsewhere, account page).
	ReasonRevoked EndReason = "revoked"
	// ReasonSignedOut: logout on this device ended it.
	ReasonSignedOut EndReason = "signed_out"
	// ReasonCLIDisconnected: the attached CLI went away.
	ReasonCLIDisconnected EndReason = "cli_disconnected"
	// ReasonDaemonShutdown: the daemon stopped (upgrade, explicit stop).
	ReasonDaemonShutdown EndReason = "daemon_shutdown"
	// ReasonTargetUnreachable: the target did not answer before creation.
	ReasonTargetUnreachable EndReason = "target_unreachable"
	// ReasonPlatformUnavailable: the platform could not be reached to
	// create the share.
	ReasonPlatformUnavailable EndReason = "platform_unavailable"
	// ReasonAuthorityInvalid: the platform rejected this device's
	// credential; the user must sign in again.
	ReasonAuthorityInvalid EndReason = "authority_invalid"
	// ReasonRejected: the platform refused the share for another defined
	// reason (unknown share, malformed request).
	ReasonRejected EndReason = "rejected"
	// ReasonStartupTimeout: the share did not become ready in time.
	ReasonStartupTimeout EndReason = "startup_timeout"
	// ReasonLimitReached: an account limit refused the share; EndLimit names
	// which one.
	ReasonLimitReached EndReason = "limit_reached"
	// ReasonUpdateRequired: the platform no longer serves this version of
	// Purlview; the user must update it.
	ReasonUpdateRequired EndReason = "update_required"
)

// RemoteStatus says whether the platform confirmed that a share ended.
type RemoteStatus string

// Remote statuses.
const (
	// RemoteConfirmed: the platform acknowledged the revocation; the link
	// is refused at the gateway and active streams were closed.
	RemoteConfirmed RemoteStatus = "confirmed"
	// RemoteUnconfirmed: local forwarding ended but the platform did not
	// acknowledge; the record stands until expiry or a later revocation.
	RemoteUnconfirmed RemoteStatus = "unconfirmed"
	// RemoteNone: no confirmation was needed (the platform initiated the
	// end, or device revocation follows).
	RemoteNone RemoteStatus = "none"
)

// RemoteOutcome accompanies an ended share.
type RemoteOutcome struct {
	Status RemoteStatus
	Detail string
}

// Invite is the platform's report on one recipient's invite email: Status is
// api.InviteSent or api.InviteNotSent.
type Invite struct {
	Email  string
	Status string
}

// Share is the daemon's or the platform's view of one share.
type Share struct {
	ID string
	// Origin is the canonical share origin; it identifies the route, not access.
	Origin string
	// URL is returned to the authenticated creator for distribution. Default links contain a
	// recipient secret and must not be included in operational diagnostics.
	URL string
	// Attempt is the idempotency key the starting CLI chose; the daemon
	// answers a repeated start for the same attempt with the same share.
	Attempt string
	// Target is the first target; Targets is the whole list when the share
	// names several, the first included, and empty otherwise.
	Target      string
	Targets     []string
	Device      string
	DeviceLabel string
	Recipients  []string
	// Invites is what share creation reported, one entry per recipient in
	// order; empty when the platform attempted no invites.
	Invites []Invite
	// RewriteURLs is the platform's record of the request's flag, which this
	// CLI never sets; NoRewrite is the daemon's own decision not to rewrite.
	RewriteURLs bool
	NoRewrite   bool
	Owner       Owner
	State       State
	CreatedAt   time.Time
	ExpiresAt   time.Time
	EndReason   EndReason
	// EndApp names the app whose failure ended the share, as the CLI shows
	// it (a target that did not answer); empty when no one app caused the
	// end, and from a daemon that predates it.
	EndApp string
	// EndLimit names the account limit that refused the share (an api.Limit
	// value) when EndReason is limit_reached; empty otherwise.
	EndLimit string
	// Detail carries a short human explanation of a failure or end.
	Detail string
}

// Active reports whether the share is still being served or reconnecting.
func (s Share) Active() bool { return s.State != StateEnded }

// Apps is every target the share names, the first first: Targets when the
// record carries the list, else the one Target.
func (s Share) Apps() []string {
	if len(s.Targets) != 0 {
		return s.Targets
	}
	return []string{s.Target}
}

// EventKind classifies daemon events.
type EventKind string

// Event kinds. A share produces ready, connection_lost/restored pairs and
// ended in that order; the visitor and app kinds arrive between ready and
// ended. Their Detail names the app as the developer sees it (localhost:3000).
const (
	// EventReady: the platform route is ready; the URL may be shown.
	EventReady EventKind = "ready"
	// EventConnectionLost: the daemon lost its platform connection and is
	// reconnecting; the URL and expiry are unchanged.
	EventConnectionLost EventKind = "connection_lost"
	// EventRestored: the same share is being served again.
	EventRestored EventKind = "restored"
	// EventEnded: terminal; Share.EndReason and Remote say why and whether
	// the platform confirmed.
	EventEnded EventKind = "ended"
	// EventFirstVisitor: the first page navigation was forwarded to the app.
	// At most once per share, also across reconnects.
	EventFirstVisitor EventKind = "first_visitor"
	// EventAppUnresponsive: a forwarded request got no answer from the app
	// (refused, reset before headers, or the response-header timeout) while it
	// was considered responding. A share starts out responding.
	EventAppUnresponsive EventKind = "app_unresponsive"
	// EventAppResponding: the app answered again, with any status, after
	// EventAppUnresponsive. The two strictly alternate.
	EventAppResponding EventKind = "app_responding"
)

// Event is one lifecycle notification.
type Event struct {
	Kind   EventKind
	Share  Share
	Remote RemoteOutcome
	Detail string
}

// StartRequest asks the daemon to create and serve a share.
type StartRequest struct {
	// Attempt is the CLI's idempotency key for this start.
	Attempt    string
	Spec       Spec
	Owner      Owner
	Credential api.InstallationCredential
}

// StopRequest ends a share by id or attempt.
type StopRequest struct {
	ID      string
	Attempt string
	Reason  EndReason
}

// StopResult reports what a stop did.
type StopResult struct {
	Share        Share
	AlreadyEnded bool
	Remote       RemoteOutcome
}

// Release is the newest published version the daemon knows about.
type Release struct {
	Version    string
	Tag        string
	Prerelease bool
	URL        string
}

// Runner opens sessions with the local daemon.
type Runner interface {
	// Open connects to the daemon. With start, a missing daemon is started
	// (and an outdated one replaced); without it, ErrDaemonNotRunning is
	// returned instead.
	Open(ctx context.Context, start bool) (Session, error)
}

// Session is one connection to the daemon. Attached shares started on it
// end when it closes.
type Session interface {
	// Start registers the request and returns the share's current state.
	// The same attempt returns the same share; later changes arrive as
	// events.
	Start(ctx context.Context, req StartRequest) (Share, error)
	// Events delivers lifecycle events for every share on this daemon; the
	// channel closes when the session ends.
	Events() <-chan Event
	// Stop ends one share and reports whether the platform confirmed.
	Stop(ctx context.Context, req StopRequest) (StopResult, error)
	// StopAll ends every share this daemon owns (logout) and returns how
	// many were active.
	StopAll(ctx context.Context, reason EndReason) (int, error)
	// Shares lists this daemon's shares, active first.
	Shares(ctx context.Context) ([]Share, error)
	// CheckUpdate asks the daemon for release metadata, waiting at most
	// wait for a fresh answer. A nil release means nothing to report.
	CheckUpdate(ctx context.Context, channel string, wait time.Duration) (*Release, error)
	Close() error
}

// Ref names a share by its canonical id or by its URL.
type Ref struct {
	ID  string
	URL string
}

// clean re-derives the identity-only reference, dropping any recipient secret.
func (r Ref) clean() (Ref, error) {
	raw := r.URL
	if r.ID != "" {
		raw = r.ID
	}
	return ParseRef(raw)
}

// String renders the reference for people, without recipient secrets: the
// bare label for an id, and for a link too, since the label is its
// hostname; any other URL is shown as its origin.
func (r Ref) String() string {
	parsed, err := r.clean()
	if err != nil {
		return "invalid share reference"
	}
	if parsed.ID != "" {
		return Label(parsed.ID)
	}
	if u, err := url.Parse(parsed.URL); err == nil {
		if label, _, _ := strings.Cut(u.Hostname(), "."); isLabel(label) {
			return label
		}
	}
	return parsed.URL
}

// RevokeResult reports a platform-side revocation.
type RevokeResult struct {
	Share        Share
	AlreadyEnded bool
	// OwnerNotified reports whether the owning daemon was connected and
	// told; a false value means it learns on its next connection attempt.
	OwnerNotified bool
}

// Directory is the platform's account-scoped view of shares. It does not
// depend on the owning daemon being reachable.
type Directory interface {
	List(ctx context.Context, cred api.InstallationCredential) ([]Share, error)
	Revoke(ctx context.Context, cred api.InstallationCredential, ref Ref) (RevokeResult, error)
}

// FailureKind classifies a directory or runner failure.
type FailureKind string

// Failure kinds.
const (
	KindNotFound     FailureKind = "not_found"
	KindUnavailable  FailureKind = "unavailable"
	KindUncertain    FailureKind = "uncertain"
	KindUnauthorised FailureKind = "unauthorised"
)

// Failure is a typed failure from a Directory or Session call.
type Failure struct {
	Kind   FailureKind
	Detail string
}

func (f *Failure) Error() string {
	if f.Detail == "" {
		return string(f.Kind)
	}
	return fmt.Sprintf("%s: %s", f.Kind, f.Detail)
}

// IsKind reports whether err is a Failure of the given kind.
func IsKind(err error, kind FailureKind) bool {
	var f *Failure
	if errors.As(err, &f) {
		return f.Kind == kind
	}
	var remote *api.Error
	if errors.As(err, &remote) {
		if remote.Outcome == api.Unknown {
			return kind == KindUncertain
		}
		return string(remote.Code) == string(kind)
	}
	return false
}

// Sentinel errors.
var (
	// ErrDaemonNotRunning: Open without start found no daemon.
	ErrDaemonNotRunning = errors.New("the local daemon is not running")
	// ErrNotImplemented: no platform is configured.
	ErrNotImplemented = api.ErrNotImplemented
	// ErrSessionClosed: the daemon connection ended.
	ErrSessionClosed = errors.New("the connection to the local daemon was lost")
)

// UnimplementedDirectory is the Directory of a build with no platform
// configured.
type UnimplementedDirectory struct{}

// List fails honestly.
func (UnimplementedDirectory) List(context.Context, api.InstallationCredential) ([]Share, error) {
	return nil, ErrNotImplemented
}

// Revoke fails honestly.
func (UnimplementedDirectory) Revoke(context.Context, api.InstallationCredential, Ref) (RevokeResult, error) {
	return RevokeResult{}, ErrNotImplemented
}
