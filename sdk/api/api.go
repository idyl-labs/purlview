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

// Package api owns the versioned product API shapes. JSON tags and the wire
// fixtures here are authoritative; there is no separate handwritten schema.
package api

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/sdk/resource"
)

// Version and routes are shared by the client and Go boundary.
const (
	Version       = "1"
	VersionHeader = "Purlview-API-Version"
	// ClientHeader names the calling software and its version as
	// "purlview/<version>". A client that sends none is treated as older
	// than any minimum the server requires.
	ClientHeader           = "Purlview-Client"
	RequestIDHeader        = "X-Request-Id"
	IdempotencyHeader      = "Idempotency-Key"
	AuthorizationsPath     = "/api/v1/authorizations"
	ObservePath            = "/api/v1/authorizations/observe"
	IdentityPath           = "/api/v1/identity"
	InstallationRevokePath = "/api/v1/installation/revoke"
	SharesPath             = "/api/v1/shares"
	ShareRevokePath        = "/api/v1/shares/revoke"
	MaxRequestBytes        = 64 << 10
	MaxResponseBytes       = 1 << 20
)

// Account-wide installation management. InstallationRevokePath (singular) stays
// the caller's own revocation; these list and revoke any of the account's.
const (
	InstallationsPath       = "/api/v1/installations"
	InstallationsRevokePath = "/api/v1/installations/revoke"
)

// InstallationCredential is issued only by approved authorisation observation.
// It is also the credential custody value; no other product result issues it.
type InstallationCredential struct {
	resource.Identity
	Token    string    `json:"token"`
	IssuedAt time.Time `json:"issued_at"`
}

// Validate checks credential structure, never its authority or purpose.
func (c InstallationCredential) Validate() error {
	if c.Identity.Validate() != nil || !Token(c.Token) || c.IssuedAt.IsZero() {
		return errors.New("invalid installation credential")
	}
	return nil
}

// Token checks safe header encoding; the service must authenticate its purpose.
func Token(s string) bool {
	if len(s) == 0 || len(s) > 4096 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

// BeginAuthorizationRequest registers a browser/headless sign-in.
type BeginAuthorizationRequest struct {
	DeviceLabel string `json:"device_label"`
	Headless    bool   `json:"headless"`
}

// Validate checks the installation label.
func (r BeginAuthorizationRequest) Validate() error {
	if !resource.Text(r.DeviceLabel, 256) {
		return errors.New("invalid device label")
	}
	return nil
}

// Authorization is returned only by begin. PollToken grants observation of this
// request, never account management. BrowserURL/UserCode are intentional UI output.
type Authorization struct {
	ID              string    `json:"id"`
	PollToken       string    `json:"poll_token"`
	BrowserURL      string    `json:"browser_url"`
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	ExpiresAt       time.Time `json:"expires_at"`
	PollIntervalMS  int       `json:"poll_interval_ms"`
}

// Validate checks a complete pending authorisation response.
func (a Authorization) Validate() error {
	if !resource.Identifier(a.ID) || !Token(a.PollToken) || !resource.URL(a.BrowserURL) || !resource.URL(a.VerificationURL) || !resource.Text(a.UserCode, 128) || a.ExpiresAt.IsZero() || a.PollIntervalMS < 100 || a.PollIntervalMS > 30000 {
		return errors.New("invalid authorisation")
	}
	return nil
}

// ObserveAuthorizationRequest identifies an authorisation; its secret is a header.
type ObserveAuthorizationRequest struct {
	ID string `json:"authorization_id"`
}

// Validate checks the authorisation identity.
func (r ObserveAuthorizationRequest) Validate() error {
	if !resource.Identifier(r.ID) {
		return errors.New("invalid authorisation id")
	}
	return nil
}

// AuthorizationStatus describes approval progress, without fabricated completion.
type AuthorizationStatus struct {
	State      string                  `json:"state"`
	Credential *InstallationCredential `json:"credential,omitempty"`
}

// Validate enforces that only approval may carry an installation credential.
func (s AuthorizationStatus) Validate() error {
	switch s.State {
	case "pending", "denied", "expired":
		if s.Credential == nil {
			return nil
		}
	case "approved":
		if s.Credential != nil {
			return s.Credential.Validate()
		}
	}
	return errors.New("invalid authorisation status")
}

// CreateShareRequest describes one intentional attempt. Key is carried only in
// Idempotency-Key. TTL is an integer number of nanoseconds on the JSON wire.
//
// Recipients restricts the share to those verified addresses, at most
// resource.MaxRecipients; empty means a secret-link share. Letter case and
// repeats are not significant: the share records the normalised list.
//
// Targets names every target when the share has several, the first included
// and equal to Target's origin; it is absent for one target. Further targets
// are bare origins. The platform records the list for display; the daemon
// serves it.
//
// PublicKey is the share's own Ed25519 public key, 32 bytes, generated by
// the daemon for this share. Every share is served through the edge: the
// platform refuses a create without a key, and the result carries the Tunnel
// to dock with. The private key never leaves the daemon.
type CreateShareRequest struct {
	Key         string        `json:"-"`
	Target      string        `json:"target"`
	Targets     []string      `json:"targets,omitempty"`
	TTL         time.Duration `json:"ttl_nanoseconds"`
	Recipients  []string      `json:"recipients,omitempty"`
	RewriteURLs bool          `json:"rewrite_urls"`
	PublicKey   []byte        `json:"public_key,omitempty"`
}

// Validate is structural; permission and current lifecycle remain server-owned.
func (r CreateShareRequest) Validate() error {
	if !resource.Identifier(r.Key) || !resource.URL(r.Target) || r.TTL < time.Second || r.TTL > time.Hour || len(r.Recipients) > resource.MaxRecipients || !resource.Recipients(resource.NormaliseRecipients(r.Recipients)) {
		return errors.New("invalid create request")
	}
	if len(r.Targets) != 0 && !resource.TargetsFor(r.Target, r.Targets) {
		return errors.New("invalid create request targets")
	}
	if r.PublicKey != nil && len(r.PublicKey) != ed25519.PublicKeySize {
		return errors.New("invalid create request public key")
	}
	return nil
}

// Normalised returns the request with its recipients in recorded form.
func (r CreateShareRequest) Normalised() CreateShareRequest {
	r.Recipients = resource.NormaliseRecipients(r.Recipients)
	return r
}

// Same reports whether two requests are one attempt with the same inputs: the
// comparison an idempotent replay needs.
func (r CreateShareRequest) Same(o CreateShareRequest) bool {
	return r.Key == o.Key && r.Target == o.Target && slices.Equal(r.Targets, o.Targets) && r.TTL == o.TTL && r.RewriteURLs == o.RewriteURLs &&
		bytes.Equal(r.PublicKey, o.PublicKey) &&
		slices.Equal(resource.NormaliseRecipients(r.Recipients), resource.NormaliseRecipients(o.Recipients))
}

// Invite statuses.
const (
	InviteSent    = "sent"
	InviteNotSent = "not_sent"
)

// InviteResult says whether the platform emailed one recipient their invite.
type InviteResult struct {
	Email  string `json:"email"`
	Status string `json:"status"`
}

// ShareAccess is returned only by creator-authorised create/list. URL is sensitive
// for secret-link shares, while Share.Origin is always identity only.
//
// Invites is set by create when the platform attempted invites: one entry per
// recipient, in Share.Recipients order. It is omitted when none were attempted.
// A replay of the same create returns the recorded results and sends nothing.
//
// Tunnel is set by a create that carried a public key, and only then; a
// replay that finds the share ended carries none.
type ShareAccess struct {
	Share   resource.Share `json:"share"`
	URL     string         `json:"url"`
	Invites []InviteResult `json:"invites,omitempty"`
	Tunnel  *Tunnel        `json:"tunnel,omitempty"`
}

// Tunnel is what a share docks on the edge with. The
// daemon pairs SVID with its own private key; Bundle authenticates the
// edge and the gateway, whose exact SPIFFE IDs are Edge and Gateway. SVID
// and Lease end with the share.
type Tunnel struct {
	// Endpoint is host:port, QUIC first with TCP fallback on the same port.
	Endpoint string `json:"endpoint"`
	Edge     string `json:"edge"`
	Gateway  string `json:"gateway"`
	// SVID is the share's certificate chain, DER, leaf first.
	SVID [][]byte `json:"svid"`
	// Lease is the admission lease envelope, sent to the edge byte for byte.
	Lease []byte `json:"lease"`
	// Bundle holds the trust roots, DER.
	Bundle [][]byte `json:"bundle"`
}

// Validate is structural: the daemon's TLS and the edge judge the material.
func (t Tunnel) Validate() error {
	host, port, err := net.SplitHostPort(t.Endpoint)
	if err != nil || host == "" || port == "" || !strings.HasPrefix(t.Edge, "spiffe://") || !strings.HasPrefix(t.Gateway, "spiffe://") ||
		len(t.SVID) == 0 || len(t.SVID) > 4 || len(t.Lease) == 0 || len(t.Bundle) == 0 || len(t.Bundle) > 16 {
		return errors.New("invalid share tunnel")
	}
	for _, c := range append(slices.Clone(t.SVID), t.Bundle...) {
		if len(c) == 0 {
			return errors.New("invalid share tunnel certificate")
		}
	}
	return nil
}

// Validate checks access-mode separation and that the link belongs to the origin.
func (s ShareAccess) Validate() error {
	if s.Share.Validate() != nil || !resource.URL(s.URL) {
		return errors.New("invalid share access")
	}
	u, _ := url.Parse(s.URL)
	entry := s.Share.EntryOrigin
	if entry == "" {
		entry = s.Share.Origin
	}
	if !resource.Origin(entry) || u.Scheme+"://"+u.Host != entry {
		return errors.New("invalid share link origin")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("invalid share link")
	}
	if len(s.Share.Recipients) != 0 {
		if len(q) != 0 {
			return errors.New("restricted link carries a query")
		}
	} else if len(q["token"]) != 1 || !Token(q.Get("token")) {
		return errors.New("secret link lacks a token")
	}
	if s.Tunnel != nil && s.Tunnel.Validate() != nil {
		return errors.New("invalid share tunnel")
	}
	if len(s.Invites) != 0 && len(s.Invites) != len(s.Share.Recipients) {
		return errors.New("invite results do not match the recipients")
	}
	for i, v := range s.Invites {
		if v.Email != s.Share.Recipients[i] || (v.Status != InviteSent && v.Status != InviteNotSent) {
			return errors.New("invalid invite result")
		}
	}
	return nil
}

// ListSharesResult is a complete account-owned list; nil is not an empty list.
type ListSharesResult struct {
	Shares []ShareAccess `json:"shares"`
}

// Validate refuses absent lists and malformed entries.
func (r ListSharesResult) Validate() error {
	if r.Shares == nil {
		return errors.New("missing shares")
	}
	for _, s := range r.Shares {
		if err := s.Validate(); err != nil {
			return err
		}
		// Tunnel material is returned once, to the create that asked.
		if s.Tunnel != nil {
			return errors.New("listed share carries a tunnel")
		}
	}
	return nil
}

// Installation is one active authorised installation of the caller's account.
// LastUsedAt is coarse (about hourly) and absent until first recorded.
type Installation struct {
	ID           string    `json:"id"`
	Label        string    `json:"label"`
	CreatedAt    time.Time `json:"created_at"`
	LastUsedAt   time.Time `json:"last_used_at,omitzero"`
	ActiveShares int       `json:"active_shares"`
	Current      bool      `json:"current"`
}

// Validate checks structure, not current authority.
func (v Installation) Validate() error {
	if !resource.Identifier(v.ID) || !resource.Text(v.Label, 256) || v.CreatedAt.IsZero() || v.ActiveShares < 0 {
		return errors.New("invalid installation")
	}
	return nil
}

// ListInstallationsResult is the account's active installations; nil is not an
// empty list. At most one entry is current: the installation that authenticated
// the request. A caller that is not an installation (the console's browser
// session) sees none.
type ListInstallationsResult struct {
	Installations []Installation `json:"installations"`
}

// Validate refuses an absent list, malformed entries and several current ones.
func (r ListInstallationsResult) Validate() error {
	if r.Installations == nil {
		return errors.New("missing installations")
	}
	current := 0
	for _, v := range r.Installations {
		if err := v.Validate(); err != nil {
			return err
		}
		if v.Current {
			current++
		}
	}
	if current > 1 {
		return errors.New("invalid installation list")
	}
	return nil
}

// RevokeInstallationRequest names one of the account's installations.
type RevokeInstallationRequest struct {
	ID string `json:"id"`
}

// Validate checks the installation identity.
func (r RevokeInstallationRequest) Validate() error {
	if !resource.Identifier(r.ID) {
		return errors.New("invalid installation id")
	}
	return nil
}

// RevokeInstallationResult reports enforcement for a named installation.
// SharesStopped counts the shares this call ended; AlreadyEnded means the
// installation had been revoked before.
type RevokeInstallationResult struct {
	Revocation
	ID            string `json:"id"`
	SharesStopped int    `json:"shares_stopped"`
}

// Validate checks the enforcement outcome and count.
func (r RevokeInstallationResult) Validate() error {
	if err := r.Revocation.Validate(); err != nil {
		return err
	}
	if !resource.Identifier(r.ID) || r.SharesStopped < 0 {
		return errors.New("invalid installation revocation")
	}
	return nil
}

// RevokeShareRequest carries only route identity, never a recipient secret.
type RevokeShareRequest struct {
	Reference resource.ShareRef `json:"reference"`
}

// Validate checks the management reference.
func (r RevokeShareRequest) Validate() error { return r.Reference.Validate() }

// Revocation is an explicit enforcement outcome, not just a changed database row.
type Revocation struct {
	Outcome      string `json:"outcome"` // confirmed or unconfirmed
	AlreadyEnded bool   `json:"already_ended"`
}

// Validate requires an explicit outcome.
func (r Revocation) Validate() error {
	if r.Outcome != "confirmed" && r.Outcome != "unconfirmed" {
		return errors.New("invalid revocation outcome")
	}
	if r.AlreadyEnded && r.Outcome != "confirmed" {
		return errors.New("invalid prior revocation")
	}
	return nil
}

// RevokeShareResult contains public state only; it never redelivers an access link.
type RevokeShareResult struct {
	Revocation
	Share         resource.Share `json:"share"`
	OwnerNotified bool           `json:"owner_notified"`
}

// Validate checks the enforcement outcome and state consistency.
func (r RevokeShareResult) Validate() error {
	if err := r.Revocation.Validate(); err != nil {
		return err
	}
	if err := r.Share.Validate(); err != nil {
		return err
	}
	if r.Outcome == "confirmed" && r.Share.State != "ended" {
		return errors.New("confirmed revocation has active state")
	}
	return nil
}

// SafeRequestID accepts only bounded non-sensitive correlation identifiers.
func SafeRequestID(s string) string {
	if resource.Identifier(s) {
		return s
	}
	return ""
}

// IsAPIPath identifies versioned API requests, including unsupported versions.
func IsAPIPath(path string) bool { return strings.HasPrefix(path, "/api/") }
