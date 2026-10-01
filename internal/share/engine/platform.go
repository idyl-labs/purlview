// Package engine is the daemon-side owner of shares: it turns a start
// request into a platform share, keeps the serving connection alive,
// reconnects under the same admission rules, enforces expiry locally, and
// ends the share exactly once with a typed reason and a remote outcome.
//
// It is the executable form of the daemon-to-platform contract. The
// Platform boundary is what a real platform client must provide: the daemon
// composes SDKManagement (the account API over HTTP) with the tunnel client,
// and the scenario runner drives the engine in-process against a controlled
// platform.
package engine

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
)

// ConnEventKind classifies serving-connection events.
type ConnEventKind string

// Connection events.
const (
	// ConnReady: the connection is admitted and the gateway route is
	// serving. Only now may a URL be shown as usable. Detail names the
	// admitted target when the transport knows it, so the engine can tell
	// when admission and the share's own list disagree.
	ConnReady ConnEventKind = "ready"
	// ConnLost: the transport dropped; the share is unavailable until a
	// new connection is admitted.
	ConnLost ConnEventKind = "lost"
	// ConnDrained: the far end asked this connection to move while the
	// share stays active. It reconnects as ConnLost does, without asking
	// the platform first.
	ConnDrained ConnEventKind = "drained"
	// ConnRevoked: the platform ended the share; the gateway already
	// refuses it.
	ConnRevoked ConnEventKind = "revoked"
	// ConnExpired: the platform ended the share at its expiry.
	ConnExpired ConnEventKind = "expired"
	// ConnFirstVisitor: this connection forwarded a page navigation. Detail
	// names the app (localhost:3000), as it does for the two kinds below.
	ConnFirstVisitor ConnEventKind = "first_visitor"
	// ConnAppUnresponsive: a forwarded request got no answer from the app.
	ConnAppUnresponsive ConnEventKind = "app_unresponsive"
	// ConnAppResponding: the app answered a forwarded request, with any
	// status. A connection reports its first observation and every change;
	// the engine reduces them to the share's events across reconnects.
	ConnAppResponding ConnEventKind = "app_responding"
)

// ConnEvent is one serving-connection notification.
type ConnEvent struct {
	Kind   ConnEventKind
	Detail string
}

// Connection is one admitted serving connection for a share.
type Connection interface {
	// Events delivers ConnReady first, then visitor and app observations and
	// lifecycle events, and closes when the connection is gone.
	Events() <-chan ConnEvent
	Close() error
}

// Platform is what the daemon needs from the platform to own a share.
type Platform interface {
	Management
	TunnelClient
}

// Management owns product create/revoke calls, separately from serving connections.
type Management interface {
	// CreateShare registers a share for the credential's device. It is
	// idempotent on req.Key.
	CreateShare(ctx context.Context, cred api.InstallationCredential, req api.CreateShareRequest) (*api.ShareAccess, error)
	// Revoke ends the share on the platform. A share that already ended is
	// a success. Returning nil means the platform confirmed the end.
	Revoke(ctx context.Context, cred api.InstallationCredential, shareID string) error
	// ShareActive reports whether the platform still counts the share as
	// active: not stopped, revoked or expired. A share asks before it
	// redocks, because its dock ends the same way whether the network
	// dropped it or the platform tore it down.
	ShareActive(ctx context.Context, cred api.InstallationCredential, shareID string) (bool, error)
}

// TunnelClient docks shares on the edge and reports their events; HTTP
// management cannot signal readiness.
type TunnelClient interface {
	// ConnectServing docks the share with what s carries. Every call,
	// initial or redock, is validated the same way and returns either a
	// ready Connection or a typed error.
	ConnectServing(ctx context.Context, cred api.InstallationCredential, shareID string, s Serving) (Connection, error)
}

// Serving is what a share's dock needs: the share's public content origin
// as the create response named it, to translate the share's address in
// headers; the share's target list, so the daemon can serve the further
// targets under their mounts; whether bodies are rewritten; the tunnel
// material its create returned; the share's own private key; and what its
// docks hand on to each other.
type Serving struct {
	Origin  string
	Targets []string
	Rewrite bool
	Tunnel  *api.Tunnel
	Key     ed25519.PrivateKey
	Lineage *Lineage
}

// ComposedPlatform joins independently supplied management and tunnel clients.
type ComposedPlatform struct {
	Management
	TunnelClient
}

// Prober checks that the target answers before a share is created.
type Prober interface {
	Probe(ctx context.Context, target string) error
}

// RejectReason classifies a defined platform refusal.
type RejectReason string

// Reject reasons.
const (
	RejectExpired   RejectReason = "expired"
	RejectRevoked   RejectReason = "revoked"
	RejectUnknown   RejectReason = "unknown_share"
	RejectAuthority RejectReason = "authority_invalid"
	RejectRequest   RejectReason = "bad_request"
	RejectUpdate    RejectReason = "update_required"
)

// Rejected is a defined refusal; retrying with the same inputs will not
// succeed.
type Rejected struct {
	Cause  error
	Reason RejectReason
	Detail string
	// Limit names the account limit that refused a create (an api.Limit
	// value); empty for any other refusal.
	Limit string
}

func (r *Rejected) Error() string {
	if r.Detail == "" {
		return string(r.Reason)
	}
	return fmt.Sprintf("%s: %s", r.Reason, r.Detail)
}

// Unavailable is a transient failure: the platform could not be reached or
// answered with a temporary error. Retrying may succeed.
type Unavailable struct {
	Cause  error
	Detail string
	// RetryAfter is the far end's hint for the earliest useful retry; the
	// engine waits it plus its own back-off.
	RetryAfter time.Duration
}

func (u *Unavailable) Error() string {
	if u.Detail == "" {
		return "Purlview is unreachable"
	}
	return u.Detail
}

// IsUnavailable reports whether err is transient.
func IsUnavailable(err error) bool {
	var u *Unavailable
	return errors.As(err, &u)
}

// AsRejected returns the rejection carried by err, if any.
func AsRejected(err error) *Rejected {
	var r *Rejected
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// Unwrap retains API outcome and cancellation information.
func (r *Rejected) Unwrap() error { return r.Cause }

// Unwrap retains the underlying API failure.
func (u *Unavailable) Unwrap() error { return u.Cause }
