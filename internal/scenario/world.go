// Package scenario runs the real purlview command handlers against a
// controlled, stateful world: a fake platform, fake sign-in, a fake target
// prober, fake release metadata, an in-process daemon built on the real
// share engine, a fake clock and an isolated credential file.
//
// SCENARIO MODE: nothing here contacts a real service, a real browser or
// the developer's own daemon. Identities and domains are synthetic
// (example.invalid, purlview.invalid). What a scenario demonstrates is the
// intended command behaviour and the contract it needs; process and
// transport guarantees are proven separately by the daemon lifecycle tests.
package scenario

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/account"
	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/command"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/internal/updatecheck"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// Synthetic identities. Reserved example domains only.
const (
	AccountEmail  = "creator@example.invalid"
	AccountID     = "acc_creator"
	ThisDevice    = "dev_studio"
	ThisLabel     = "studio"
	OtherDevice   = "dev_laptop"
	OtherLabel    = "laptop"
	EntryDomain   = "purlview.invalid"
	ShareDomain   = "purlview-content.invalid"
	AccountDomain = "app.purlview.invalid"
	Version       = "0.9.0"
)

// Epoch is the fake clock's starting time.
var Epoch = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

var shareLabels = []string{"k7m2p4qx", "p4q9x2bd", "f8q3w6hn", "b5x2d4nt", "w6h2j7tc", "d4n8r3cm", "j7t4g9vs", "r3c6z5kp"}
var userCodes = []string{"WDJB-MJHT", "KHGA-BRTC", "PQRS-TUVW"}

// loginCodes are the emailed sign-in codes, one per send of a challenge.
var loginCodes = []string{"482913", "307561", "915204"}

// loginAttempts is how many codes a challenge accepts, as on the platform.
const loginAttempts = 5

// World is one isolated environment for a scenario.
type World struct {
	Clock    *clock.Fake
	Platform *Platform
	Prober   *Prober
	Updates  *Updates
	Browser  *Browser
	Daemon   *Daemon
	Store    *Store
	Info     buildinfo.Info
	// Interactive controls whether commands see a terminal on every stream.
	Interactive bool
	// Color renders with colour, for the goldens; transcripts are plain.
	Color bool
	// Advice is how this copy says it is updated.
	Advice updatecheck.Advice

	rec     *recorder
	attempt int
	noticed map[string]time.Time
	typed   chan string
	mu      sync.Mutex
}

// NewWorld builds a world with a signed-out installation, a reachable
// target for every share, no update available and a running-capable daemon.
func NewWorld() *World {
	w := &World{Clock: clock.NewFake(Epoch), Store: &Store{}, Interactive: true, rec: &recorder{}, noticed: map[string]time.Time{}}
	w.Advice = updatecheck.Advice{Method: "homebrew", Command: "brew upgrade --cask purlview"}
	w.Info = buildinfo.Info{Version: Version, Commit: "0123456789abcdef", BuiltBy: "test", GoVersion: "go1.27.1", OS: "scenario", Arch: "fixture"}
	w.Platform = newPlatform(w.Clock)
	w.Prober = &Prober{unreachable: map[string]string{}}
	w.Updates = &Updates{clock: w.Clock}
	w.Browser = &Browser{}
	w.Daemon = newDaemon(w)
	return w
}

// Deps wires the world into the command package.
func (w *World) Deps() command.Deps {
	return command.Deps{
		ReadLine:  w.reader(),
		Clock:     w.Clock,
		Local:     time.UTC,
		Terminal:  command.Terminal{Interactive: w.Interactive, Attended: w.Interactive},
		Color:     w.Color,
		Browser:   w.Browser,
		Store:     w.Store,
		Auth:      authorizer{w.Platform},
		Directory: directory{w.Platform},
		Runner:    w.Daemon,
		NewID: func() string {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.attempt++
			return fmt.Sprintf("att_%02d", w.attempt)
		},
		Hostname:     func() string { return ThisLabel },
		UpdateAdvice: func(buildinfo.Info) updatecheck.Advice { return w.Advice },
		NoticeDue: func(version string, now time.Time) bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			if last, ok := w.noticed[version]; ok && now.Sub(last) < 24*time.Hour {
				return false
			}
			w.noticed[version] = now
			return true
		},
		Getenv: func(string) string { return "" },
	}
}

// reader answers one command's prompts. By default the user types the account
// email and then the code from the newest sign-in email. After Typed, prompts
// wait for what the scenario types.
func (w *World) reader() func(context.Context) (string, error) {
	prompts := 0
	return func(ctx context.Context) (string, error) {
		w.mu.Lock()
		typed := w.typed
		w.mu.Unlock()
		if typed != nil {
			select {
			case line := <-typed:
				return line, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		prompts++
		if prompts == 1 {
			return AccountEmail, nil
		}
		return w.Platform.LoginCode(), nil
	}
}

// Typed makes commands started afterwards read what the scenario types.
func (w *World) Typed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.typed = make(chan string, 16)
}

// Type answers the next prompt of a command started after Typed.
func (w *World) Type(line string) {
	w.Note("the user types: %s", line)
	w.mu.Lock()
	typed := w.typed
	w.mu.Unlock()
	typed <- line
}

// TypeHidden answers a hidden prompt of a command started after Typed. The
// transcript notes the answer only when it is a word, never a code.
func (w *World) TypeHidden(line string) {
	shown := line
	if strings.Trim(api.NormaliseLoginCode(line), "0123456789") == "" {
		shown = strings.Repeat("·", len(line))
	}
	w.Note("the user types, unseen: %s", shown)
	w.mu.Lock()
	typed := w.typed
	w.mu.Unlock()
	typed <- line
}

// SignIn makes the installation signed in as the creator without running
// the login flow: the device exists on the platform and the credential is
// stored locally.
func (w *World) SignIn() *api.InstallationCredential {
	cred := w.Platform.issueDevice(ThisDevice, ThisLabel)
	_ = w.Store.Save(cred)
	return cred
}

// Store is the in-memory credential store of a scenario; Save can be made
// to fail as a full disk or a refused keychain would.
type Store struct {
	inner   account.MemoryStore
	mu      sync.Mutex
	saveErr error
}

// SaveFails makes every Save fail with err.
func (s *Store) SaveFails(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveErr = err
}

// Load implements account.Store.
func (s *Store) Load() (*api.InstallationCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Load()
}

// Save implements account.Store.
func (s *Store) Save(c *api.InstallationCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.inner.Save(c)
}

// Clear implements account.Store.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Clear()
}

// Note records a fixture action in the transcript.
func (w *World) Note(format string, args ...any) {
	w.rec.add(entry{kind: kindNote, text: fmt.Sprintf(format, args...)})
}

// Advance moves the fake clock and records it.
func (w *World) Advance(d time.Duration) {
	w.rec.add(entry{kind: kindClock, d: d})
	w.Clock.Advance(d)
	w.Platform.expireShares()
}

// ---------------------------------------------------------------------------
// Platform: control plane (accounts, devices, shares, revocation) and the
// data plane's serving connections, in one stateful fake.

// Platform is the fake control and data plane.
type Platform struct {
	clock *clock.Fake
	mu    sync.Mutex

	unreachable         bool
	servingDisconnected map[string]bool // connection loss only; management remains reachable
	devices             map[string]*device
	shares              map[string]*platformShare
	byKey               map[string]string
	order               []string
	nextShare           int
	nextReq             int

	// Login controls.
	loginMode   string // "approve" (default), "deny", "wait"
	pending     map[string]*pendingLogin
	lastLogin   string
	deviceSeq   int
	holdCreate  chan struct{}
	heldCreates int
	// unreachableAfterApproval trips the unreachable flag once a sign-in
	// is approved.
	unreachableAfterApproval bool
	holdReady                bool
	invites                  bool
	inviteFails              map[string]bool
	revokeMode               string // "" (confirm), "uncertain"
	limit                    api.Limit
	updateRequired           bool
	createCalls              int
	connectCalls             int
	access                   recipientState
}

type device struct {
	id, label, token string
	revoked          bool
	createdAt        time.Time
	lastUsedAt       time.Time
}

type platformShare struct {
	id, origin, entryOrigin, url, key string
	secret                            string
	grants                            []recipientGrant
	target                            string
	targets                           []string // the whole list when several
	served                            engine.Serving
	recipients                        []string
	invites                           []api.InviteResult
	rewrite                           bool
	device                            string
	createdAt                         time.Time
	expiresAt                         time.Time
	ended                             bool
	endReason                         string
	conns                             map[*platformConn]struct{}
	everServed                        bool
}

type pendingLogin struct {
	auth     api.Authorization
	label    string
	decision chan string
	// Email-code state, with the platform's limits: five attempts, a resend
	// after one minute, three sends.
	code     string
	attempts int
	sends    int
	resendAt time.Time
	used     bool
}

type platformConn struct {
	p       *Platform
	share   *platformShare
	ch      chan engine.ConnEvent
	closeMu sync.Once
}

func (c *platformConn) Events() <-chan engine.ConnEvent { return c.ch }

func (c *platformConn) Close() error {
	c.closeMu.Do(func() {
		c.p.mu.Lock()
		delete(c.share.conns, c)
		c.p.mu.Unlock()
	})
	return nil
}

func (c *platformConn) send(ev engine.ConnEvent) {
	select {
	case c.ch <- ev:
	default:
	}
}

func newPlatform(clk *clock.Fake) *Platform {
	p := &Platform{clock: clk, devices: map[string]*device{}, shares: map[string]*platformShare{}, byKey: map[string]string{}, pending: map[string]*pendingLogin{}, loginMode: "approve"}
	p.servingDisconnected = map[string]bool{}
	p.access.init()
	p.devices[OtherDevice] = &device{id: OtherDevice, label: OtherLabel, token: "tok_laptop", createdAt: Epoch.Add(-14 * 24 * time.Hour), lastUsedAt: Epoch.Add(-time.Hour)}
	return p
}

// Unreachable makes every platform call fail as transient until Reachable.
func (p *Platform) Unreachable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreachable = true
}

// UnreachableAfterApproval makes the platform unreachable as soon as the
// pending sign-in is approved, so the credential is issued but any call
// after it fails.
func (p *Platform) UnreachableAfterApproval() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreachableAfterApproval = true
}

// Reachable restores the platform.
func (p *Platform) Reachable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreachable = false
}

// DropConnection ends the serving connection(s) of a share (network loss)
// without ending the share.
func (p *Platform) DropConnection(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.shares[id]; ok {
		for c := range s.conns {
			c.send(engine.ConnEvent{Kind: engine.ConnLost, Detail: "transport closed"})
			delete(s.conns, c)
		}
	}
}

// Observe makes a share's serving connection report what the daemon's proxy
// would see: the first visitor, an app that stops answering, one that resumes.
// The observations name the share's first app.
func (p *Platform) Observe(id string, kinds ...engine.ConnEventKind) {
	p.mu.Lock()
	target := ""
	if s, ok := p.shares[id]; ok {
		target = s.target
	}
	p.mu.Unlock()
	p.ObserveApp(id, share.DisplayTarget(target), kinds...)
}

// ObserveApp is Observe for a named app of a share with several.
func (p *Platform) ObserveApp(id, app string, kinds ...engine.ConnEventKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.shares[id]; ok {
		for c := range s.conns {
			for _, kind := range kinds {
				c.send(engine.ConnEvent{Kind: kind, Detail: app})
			}
		}
	}
}

// DisconnectServing holds one share's daemon connection offline, while
// creator management and recipient authority remain reachable. Unlike a
// transient DropConnection, the engine cannot immediately reconnect.
func (p *Platform) DisconnectServing(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.servingDisconnected[id] = true
	if s := p.shares[id]; s != nil {
		for c := range s.conns {
			c.send(engine.ConnEvent{Kind: engine.ConnLost, Detail: "owner network disconnected"})
			delete(s.conns, c)
		}
	}
}

// Restart drops every serving connection and makes the platform
// unreachable until Reachable is called, as a data-plane restart would.
func (p *Platform) Restart() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreachable = true
	for _, s := range p.shares {
		for c := range s.conns {
			c.send(engine.ConnEvent{Kind: engine.ConnLost, Detail: "server restarting"})
			delete(s.conns, c)
		}
	}
}

// RevokeElsewhere revokes a share as the account page or another device
// would, notifying any serving connection.
func (p *Platform) RevokeElsewhere(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.shares[id]; ok {
		p.endShare(s, "revoked")
	}
}

// AddDevice signs another device in to the account at a given time.
func (p *Platform) AddDevice(id, label string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.devices[id] = &device{id: id, label: label, token: "tok_" + id, createdAt: at, lastUsedAt: at}
}

// RevokeDevice revokes a device's authorisation (account page) and ends its
// shares.
func (p *Platform) RevokeDevice(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.devices[id]; ok {
		d.revoked = true
	}
	for _, s := range p.shares {
		if s.device == id {
			p.endShare(s, "revoked")
		}
	}
}

func (p *Platform) endShare(s *platformShare, reason string) {
	if s.ended {
		return
	}
	s.ended = true
	s.endReason = reason
	p.closeStreams(s.id, "", reason)
	kind := engine.ConnRevoked
	if reason == "expired" {
		kind = engine.ConnExpired
	}
	for c := range s.conns {
		c.send(engine.ConnEvent{Kind: kind})
		delete(s.conns, c)
	}
}

// SeedRemoteShare records a share served by the other device.
func (p *Platform) SeedRemoteShare(target string, ttl time.Duration, connected bool, recipients ...string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.newShare("seed_"+target, target, ttl, recipients, false, OtherDevice)
	s.everServed = true
	if connected {
		c := &platformConn{p: p, share: s, ch: make(chan engine.ConnEvent, 8)}
		s.conns[c] = struct{}{}
	}
	return s.id
}

// ExpireNow marks a share expired on the platform side.
func (p *Platform) ExpireNow(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.shares[id]; ok {
		p.endShare(s, "expired")
	}
}

// HoldCreate makes CreateShare block until ReleaseCreate is called.
func (p *Platform) HoldCreate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holdCreate = make(chan struct{})
}

// WaitHeldCreates blocks (bounded, real time) until n create calls have
// entered the hold, so a scenario can order its actions after the daemon's
// recovery retry.
func (p *Platform) WaitHeldCreates(n int) bool {
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := p.heldCreates
		p.mu.Unlock()
		if got >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// WaitConnects blocks (bounded, real time) until the daemon has asked n times
// to serve a share, which is how a scenario knows a start is under way: the
// CLI prints nothing before the link works.
func (p *Platform) WaitConnects(n int) bool {
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		if _, got := p.Calls(); got >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// ReleaseCreate lets a held CreateShare continue.
func (p *Platform) ReleaseCreate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holdCreate != nil {
		close(p.holdCreate)
		p.holdCreate = nil
	}
}

// HoldReady makes new serving connections never become ready.
func (p *Platform) HoldReady(hold bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holdReady = hold
}

// RevokeUncertain makes the next account-side revocation perform the
// revocation but lose its confirmation.
func (p *Platform) RevokeUncertain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revokeMode = "uncertain"
}

// LoginMode selects how sign-in requests are answered: "approve"
// (immediately), "deny" (immediately) or "wait" (until Approve/Deny).
func (p *Platform) LoginMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loginMode = mode
}

// Approve answers the pending sign-in request.
func (p *Platform) Approve() { p.decide("approve") }

// Deny answers the pending sign-in request with a refusal.
func (p *Platform) Deny() { p.decide("deny") }

func (p *Platform) decide(d string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pl := range p.pending {
		select {
		case pl.decision <- d:
		default:
		}
	}
}

// Calls reports how many create and connect calls the daemon made.
func (p *Platform) Calls() (creates, connects int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.createCalls, p.connectCalls
}

// ShareState reports a share's platform state for assertions.
func (p *Platform) ShareState(id string) (exists, ended bool, reason string, conns int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.shares[id]
	if !ok {
		return false, false, "", 0
	}
	return true, s.ended, s.endReason, len(s.conns)
}

// DeviceRevoked reports whether a device's authorisation was revoked.
func (p *Platform) DeviceRevoked(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.devices[id]
	return ok && d.revoked
}

func (p *Platform) issueDevice(id, label string) *api.InstallationCredential {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.issueDeviceLocked(id, label)
}

func (p *Platform) issueDeviceLocked(id, label string) *api.InstallationCredential {
	if id == "" {
		p.deviceSeq++
		id = fmt.Sprintf("dev_%s%d", strings.ToLower(label), p.deviceSeq)
		if _, taken := p.devices[ThisDevice]; !taken && label == ThisLabel {
			id = ThisDevice
		}
	}
	d := &device{id: id, label: label, token: "tok_" + id, createdAt: p.clock.Now(), lastUsedAt: p.clock.Now()}
	p.devices[id] = d
	return &api.InstallationCredential{Identity: resource.Identity{Account: AccountEmail, AccountID: AccountID, Device: id, DeviceLabel: label}, Token: d.token, IssuedAt: p.clock.Now()}
}

func (p *Platform) authorize(cred api.InstallationCredential) error {
	d, ok := p.devices[cred.Device]
	if !ok || d.token != cred.Token {
		return &engine.Rejected{Reason: engine.RejectAuthority, Detail: "unknown device credential"}
	}
	if d.revoked {
		return &engine.Rejected{Reason: engine.RejectAuthority, Detail: "this device's authorisation was revoked"}
	}
	return nil
}

func (p *Platform) newShare(key, target string, ttl time.Duration, recipients []string, rewrite bool, dev string) *platformShare {
	i := p.nextShare
	p.nextShare++
	secret := fmt.Sprintf("synthetic-secret-%02d", i+1)
	// Later labels stay in the label alphabet: "z" and the index in base 28.
	label := []byte("z2222222")
	for k, n := len(label)-1, i; n > 0; k, n = k-1, n/28 {
		label[k] = "23456789bcdfghjkmnpqrstvwxyz"[n%28]
	}
	if i < len(shareLabels) {
		label = []byte(shareLabels[i])
	}
	id := "shr_" + string(label)
	now := p.clock.Now()
	origin := "https://" + string(label) + "." + ShareDomain
	entryOrigin := "https://" + string(label) + "." + EntryDomain
	link := entryOrigin + accessPath
	if len(recipients) == 0 {
		link += "?token=" + secret
	} else {
		secret = ""
	}
	s := &platformShare{id: id, origin: origin, entryOrigin: entryOrigin, url: link, secret: secret, key: key, target: target, recipients: recipients, rewrite: rewrite, device: dev, createdAt: now, expiresAt: now.Add(ttl), conns: map[*platformConn]struct{}{}}
	for n, email := range recipients {
		s.grants = append(s.grants, recipientGrant{id: fmt.Sprintf("grant_%s_%d", id, n+1), email: email, active: true})
	}
	p.shares[id] = s
	p.byKey[key] = id
	p.order = append(p.order, id)
	return s
}

// The three boundaries share one state but differ in signature, so each is
// a thin adapter.

type platformClient struct{ p *Platform }
type directory struct{ p *Platform }
type authorizer struct{ p *Platform }

// Engine returns the daemon-facing boundary.
func (p *Platform) Engine() engine.Platform { return platformClient{p} }

func (c platformClient) CreateShare(ctx context.Context, cred api.InstallationCredential, req api.CreateShareRequest) (*api.ShareAccess, error) {
	return c.p.createShare(ctx, cred, req)
}

// ConnectServing is the engine's dock: the daemon names the origin it will
// translate, which must be the one the create response gave, the targets it
// serves and whether it rewrites.
func (c platformClient) ConnectServing(ctx context.Context, cred api.InstallationCredential, id string, s engine.Serving) (engine.Connection, error) {
	return c.p.connect(ctx, cred, id, s)
}

func (c platformClient) Revoke(ctx context.Context, cred api.InstallationCredential, id string) error {
	return c.p.revokeShare(ctx, cred, id)
}

func (c platformClient) ShareActive(ctx context.Context, cred api.InstallationCredential, id string) (bool, error) {
	return c.p.shareActive(ctx, cred, id)
}

func (d directory) List(ctx context.Context, cred api.InstallationCredential) ([]share.Share, error) {
	return d.p.listShares(ctx, cred)
}

func (d directory) Revoke(ctx context.Context, cred api.InstallationCredential, ref share.Ref) (share.RevokeResult, error) {
	return d.p.revokeRef(ctx, cred, ref)
}

func (a authorizer) Begin(ctx context.Context, req api.BeginAuthorizationRequest) (*api.Authorization, error) {
	return a.p.begin(ctx, req)
}

func (a authorizer) Wait(ctx context.Context, auth *api.Authorization) (*api.InstallationCredential, error) {
	return a.p.wait(ctx, auth)
}

func (a authorizer) Validate(ctx context.Context, cred *api.InstallationCredential) (*resource.Identity, error) {
	return a.p.validate(ctx, cred)
}

func (a authorizer) Revoke(_ context.Context, cred *api.InstallationCredential) error {
	return a.p.revokeDevice(cred)
}

// createShare is the daemon's create call: idempotent on the key.
func (p *Platform) createShare(ctx context.Context, cred api.InstallationCredential, req api.CreateShareRequest) (*api.ShareAccess, error) {
	p.mu.Lock()
	hold := p.holdCreate
	if hold != nil {
		p.heldCreates++
	}
	p.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return nil, &engine.Unavailable{Detail: "connection refused"}
	}
	p.createCalls++
	if err := p.authorize(cred); err != nil {
		return nil, err
	}
	if id, ok := p.byKey[req.Key]; ok {
		// A replay reports the recorded invite results and sends nothing again.
		s := p.shares[id]
		return p.accessResult(s), nil
	}
	if p.limit != "" {
		return nil, &engine.Rejected{Reason: engine.RejectRequest, Detail: "sign-in was denied", Limit: string(p.limit)}
	}
	if p.updateRequired {
		return nil, &engine.Rejected{Reason: engine.RejectUpdate, Detail: api.Message(api.UpdateRequired)}
	}
	if req.TTL <= 0 || req.TTL > share.MaxTTL {
		return nil, &engine.Rejected{Reason: engine.RejectRequest, Detail: "lifetime out of range"}
	}
	s := p.newShare(req.Key, req.Target, req.TTL, resource.NormaliseRecipients(req.Recipients), req.RewriteURLs, cred.Device)
	s.targets = req.Targets
	if p.invites {
		for i := range s.grants {
			status := api.InviteSent
			if p.inviteFails[s.grants[i].email] {
				status = api.InviteNotSent
			} else {
				p.sendEmailLink(s, &s.grants[i], req.TTL)
			}
			s.invites = append(s.invites, api.InviteResult{Email: s.grants[i].email, Status: status})
		}
	}
	return p.accessResult(s), nil
}

// RefuseShares makes every new share create fail as the live platform does at
// an account limit: denied, naming the limit.
func (p *Platform) RefuseShares(limit api.Limit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limit = limit
}

// RequireUpdate makes share creation refuse this CLI's version, as the
// platform does below its minimum CLI version.
func (p *Platform) RequireUpdate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updateRequired = true
}

// SendInvites makes share creation email every recipient an invite that lasts
// as long as the share, and report the results; the addresses named here are
// reported as not sent. Without it the platform attempts no invites, as the
// live platform does until it implements them.
func (p *Platform) SendInvites(notSent ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invites = true
	p.inviteFails = map[string]bool{}
	for _, email := range notSent {
		p.inviteFails[email] = true
	}
}

func (p *Platform) accessResult(s *platformShare) *api.ShareAccess {
	view := p.view(s)
	state := string(view.State)
	if s.ended {
		state = "ended"
	}
	return &api.ShareAccess{Share: resource.Share{ID: s.id, Origin: s.origin, EntryOrigin: s.entryOrigin, Target: s.target, Targets: s.targets, Device: s.device, DeviceLabel: view.DeviceLabel, Recipients: s.recipients, RewriteURLs: s.rewrite, State: state, CreatedAt: s.createdAt, ExpiresAt: s.expiresAt}, URL: s.url, Invites: s.invites}
}

// Served reports what the daemon said it serves on its latest connection for
// a share: the target list and whether it rewrites bodies.
func (p *Platform) Served(id string) (targets []string, rewrite bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.shares[id]; ok {
		return s.served.Targets, s.served.Rewrite
	}
	return nil, false
}

// connect is the daemon's serving connection: every attempt is admitted by
// the same rules.
func (p *Platform) connect(_ context.Context, cred api.InstallationCredential, id string, serving engine.Serving) (engine.Connection, error) {
	origin := serving.Origin
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return nil, &engine.Unavailable{Detail: "connection refused"}
	}
	p.connectCalls++
	if p.servingDisconnected[id] {
		return nil, &engine.Unavailable{Detail: "owner network disconnected"}
	}
	if err := p.authorize(cred); err != nil {
		return nil, err
	}
	s, ok := p.shares[id]
	if !ok {
		return nil, &engine.Rejected{Reason: engine.RejectUnknown, Detail: "no such share"}
	}
	if s.device != cred.Device {
		return nil, &engine.Rejected{Reason: engine.RejectAuthority, Detail: "share belongs to another device"}
	}
	if origin != "" && origin != s.origin {
		return nil, &engine.Rejected{Reason: engine.RejectRequest, Detail: "the daemon named another origin than the share's"}
	}
	if s.ended {
		if s.endReason == "expired" {
			return nil, &engine.Rejected{Reason: engine.RejectExpired, Detail: "share expired"}
		}
		return nil, &engine.Rejected{Reason: engine.RejectRevoked, Detail: "share was revoked"}
	}
	if !p.clock.Now().Before(s.expiresAt) {
		p.endShare(s, "expired")
		return nil, &engine.Rejected{Reason: engine.RejectExpired, Detail: "share expired"}
	}
	c := &platformConn{p: p, share: s, ch: make(chan engine.ConnEvent, 8)}
	s.conns[c] = struct{}{}
	s.everServed = true
	s.served = serving
	if !p.holdReady {
		c.send(engine.ConnEvent{Kind: engine.ConnReady})
	}
	return c, nil
}

// revokeShare is the daemon's revocation of a share it served.
func (p *Platform) revokeShare(_ context.Context, cred api.InstallationCredential, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return &engine.Unavailable{Detail: "connection refused"}
	}
	if err := p.authorize(cred); err != nil {
		return err
	}
	s, ok := p.shares[id]
	if !ok || s.device != cred.Device {
		return &engine.Rejected{Reason: engine.RejectUnknown, Detail: "no such share"}
	}
	p.endShare(s, "revoked")
	return nil
}

// shareActive is whether the account's list still holds the share.
func (p *Platform) shareActive(_ context.Context, cred api.InstallationCredential, id string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return false, &engine.Unavailable{Detail: "connection refused"}
	}
	if err := p.authorize(cred); err != nil {
		return false, err
	}
	s, ok := p.shares[id]
	return ok && !s.ended && p.clock.Now().Before(s.expiresAt), nil
}

// listShares is the account-scoped query.
func (p *Platform) listShares(_ context.Context, cred api.InstallationCredential) ([]share.Share, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return nil, &share.Failure{Kind: share.KindUnavailable, Detail: "connection refused"}
	}
	if err := p.authorize(cred); err != nil {
		return nil, &share.Failure{Kind: share.KindUnauthorised, Detail: err.Error()}
	}
	var out []share.Share
	for _, id := range p.order {
		s := p.shares[id]
		if s.ended || !p.clock.Now().Before(s.expiresAt) {
			continue
		}
		out = append(out, p.view(s))
	}
	return out, nil
}

func (p *Platform) view(s *platformShare) share.Share {
	st := share.StateStarting
	switch {
	case len(s.conns) > 0:
		st = share.StateReady
	case s.everServed:
		st = share.StateReconnecting
	}
	label := ""
	if d, ok := p.devices[s.device]; ok {
		label = d.label
	}
	return share.Share{ID: s.id, Origin: s.origin, URL: s.url, Attempt: s.key, Target: s.target, Targets: s.targets, Device: s.device, DeviceLabel: label, Recipients: s.recipients, RewriteURLs: s.rewrite, State: st, CreatedAt: s.createdAt, ExpiresAt: s.expiresAt}
}

// revokeRef is the account-scoped revocation by id or URL (owner authority).
func (p *Platform) revokeRef(_ context.Context, cred api.InstallationCredential, ref share.Ref) (share.RevokeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return share.RevokeResult{}, &share.Failure{Kind: share.KindUnavailable, Detail: "connection refused"}
	}
	if err := p.authorize(cred); err != nil {
		return share.RevokeResult{}, &share.Failure{Kind: share.KindUnauthorised, Detail: err.Error()}
	}
	var s *platformShare
	for _, cand := range p.shares {
		if cand.id == ref.ID || ref.URL != "" && (cand.origin == ref.URL || cand.entryOrigin == ref.URL) {
			s = cand
			break
		}
	}
	if s == nil {
		return share.RevokeResult{}, &share.Failure{Kind: share.KindNotFound, Detail: "no such share in this account"}
	}
	if s.ended {
		return share.RevokeResult{Share: p.view(s), AlreadyEnded: true}, nil
	}
	notified := len(s.conns) > 0
	p.endShare(s, "revoked")
	if p.revokeMode == "uncertain" {
		p.revokeMode = ""
		return share.RevokeResult{}, &share.Failure{Kind: share.KindUncertain, Detail: "no acknowledgement within 10s"}
	}
	v := p.view(s)
	v.State = share.StateEnded
	v.EndReason = share.ReasonRevoked
	return share.RevokeResult{Share: v, OwnerNotified: notified}, nil
}

// begin registers a sign-in request.
func (p *Platform) begin(_ context.Context, req api.BeginAuthorizationRequest) (*api.Authorization, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return nil, &account.Failure{Kind: account.KindUnavailable, Detail: "connection refused"}
	}
	p.nextReq++
	id := fmt.Sprintf("req_%04d", p.nextReq)
	code := userCodes[(p.nextReq-1)%len(userCodes)]
	auth := api.Authorization{
		ID: id, PollToken: "synthetic-poll-" + id, PollIntervalMS: 1000,
		BrowserURL:      "https://" + AccountDomain + "/cli/authorize?request=" + id,
		VerificationURL: "https://" + AccountDomain + "/device",
		UserCode:        code,
		ExpiresAt:       p.clock.Now().Add(5 * time.Minute),
	}
	_ = req.Headless
	p.pending[id] = &pendingLogin{auth: auth, label: req.DeviceLabel, decision: make(chan string, 1), code: loginCodes[0], sends: 1, resendAt: p.clock.Now().Add(time.Minute)}
	p.lastLogin = id
	return &auth, nil
}

// LoginCode is the code in the newest sign-in email, as the user would read it.
func (p *Platform) LoginCode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pl := p.pending[p.lastLogin]; pl != nil {
		return pl.code
	}
	return ""
}

// LoginSends reports how many sign-in emails the newest challenge has sent.
func (p *Platform) LoginSends() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pl := p.pending[p.lastLogin]; pl != nil {
		return pl.sends
	}
	return 0
}

// wait blocks for the decision on a sign-in request.
func (p *Platform) wait(ctx context.Context, auth *api.Authorization) (*api.InstallationCredential, error) {
	p.mu.Lock()
	var pl *pendingLogin
	for _, cand := range p.pending {
		if cand.auth.BrowserURL == auth.BrowserURL {
			pl = cand
		}
	}
	mode := p.loginMode
	p.mu.Unlock()
	if pl == nil {
		return nil, &account.Failure{Kind: account.KindExpired, Detail: "unknown sign-in request"}
	}
	var decision string
	switch mode {
	case "approve", "deny":
		decision = mode
	default:
		select {
		case decision = <-pl.decision:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return nil, &account.Failure{Kind: account.KindUnavailable, Detail: "connection refused"}
	}
	if decision == "deny" {
		return nil, &account.Failure{Kind: account.KindDenied, Detail: "the sign-in was declined"}
	}
	cred := p.issueDeviceLocked("", pl.label)
	if p.unreachableAfterApproval {
		p.unreachable = true
		p.unreachableAfterApproval = false
	}
	return cred, nil
}

// validate checks a device credential.
func (p *Platform) validate(_ context.Context, cred *api.InstallationCredential) (*resource.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return nil, &account.Failure{Kind: account.KindUnavailable, Detail: "connection refused"}
	}
	if err := p.authorize(*cred); err != nil {
		return nil, &account.Failure{Kind: account.KindUnauthorised, Detail: err.Error()}
	}
	id := cred.Identity
	return &id, nil
}

// revokeDevice ends a device's authorisation and its shares.
func (p *Platform) revokeDevice(cred *api.InstallationCredential) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreachable {
		return &account.Failure{Kind: account.KindUnavailable, Detail: "connection refused"}
	}
	d, ok := p.devices[cred.Device]
	if !ok || d.token != cred.Token {
		return &account.Failure{Kind: account.KindUnauthorised, Detail: "unknown device credential"}
	}
	if d.revoked {
		return &account.Failure{Kind: account.KindUnauthorised, Detail: "already revoked"}
	}
	d.revoked = true
	for _, s := range p.shares {
		if s.device == d.id {
			p.endShare(s, "revoked")
		}
	}
	return nil
}

func (a authorizer) StartLogin(ctx context.Context, r api.LoginStartRequest) (*api.LoginChallenge, error) {
	v, e := a.Begin(ctx, api.BeginAuthorizationRequest{DeviceLabel: r.DeviceLabel})
	if e != nil {
		return nil, e
	}
	return &api.LoginChallenge{ID: v.ID, ExpiresAt: v.ExpiresAt, ResendAt: a.p.clock.Now().Add(time.Minute)}, nil
}

// VerifyLogin checks the code as the platform does: a miss says how many
// attempts are left, and the fifth miss or a lapsed challenge is expired.
func (a authorizer) VerifyLogin(ctx context.Context, r api.LoginVerifyRequest) (*api.InstallationCredential, error) {
	a.p.mu.Lock()
	p := a.p.pending[r.ID]
	var failure error
	switch {
	case p == nil || p.used || p.attempts >= loginAttempts || !a.p.clock.Now().Before(p.auth.ExpiresAt):
		failure = &account.Failure{Kind: account.KindExpired}
	case r.Code != p.code:
		p.attempts++
		failure = &account.Failure{Kind: account.KindExpired}
		if left := loginAttempts - p.attempts; left > 0 {
			failure = &account.Failure{Kind: account.KindDenied, Detail: "the code did not match", Cause: &api.Error{Code: api.Denied, Outcome: api.NotApplied, AttemptsLeft: left}}
		}
	}
	a.p.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	cred, e := a.Wait(ctx, &p.auth)
	if e == nil {
		a.p.mu.Lock()
		p.used = true
		a.p.mu.Unlock()
	}
	return cred, e
}

// ResendLogin replaces the code within the same challenge: not before ResendAt,
// at most three sends, never extending the expiry.
func (a authorizer) ResendLogin(_ context.Context, r api.LoginResendRequest) (*api.LoginChallenge, error) {
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	if a.p.unreachable {
		return nil, &account.Failure{Kind: account.KindUnavailable, Detail: "connection refused"}
	}
	p, now := a.p.pending[r.ID], a.p.clock.Now()
	if p == nil || p.used || p.attempts >= loginAttempts || !now.Before(p.auth.ExpiresAt) || now.Before(p.resendAt) || p.sends >= len(loginCodes) {
		return nil, &account.Failure{Kind: account.KindDenied, Detail: "a new code cannot be sent yet"}
	}
	p.code = loginCodes[p.sends]
	p.sends++
	p.resendAt = now.Add(time.Minute)
	return &api.LoginChallenge{ID: r.ID, ExpiresAt: p.auth.ExpiresAt, ResendAt: p.resendAt}, nil
}

// ListInstallations is the account's active devices, oldest first.
func (a authorizer) ListInstallations(_ context.Context, cred *api.InstallationCredential) ([]api.Installation, error) {
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	if a.p.unreachable {
		return nil, &account.Failure{Kind: account.KindUnavailable, Detail: "connection refused"}
	}
	if err := a.p.authorize(*cred); err != nil {
		return nil, &account.Failure{Kind: account.KindUnauthorised, Detail: err.Error()}
	}
	var out []api.Installation
	for _, d := range a.p.devices {
		if d.revoked {
			continue
		}
		v := api.Installation{ID: d.id, Label: d.label, CreatedAt: d.createdAt, LastUsedAt: d.lastUsedAt, Current: d.id == cred.Device}
		for _, s := range a.p.shares {
			if s.device == d.id && !s.ended && a.p.clock.Now().Before(s.expiresAt) {
				v.ActiveShares++
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// RevokeInstallation signs out one of the account's devices and ends its
// shares. The fake has one account, so an unknown id is the only not_found.
func (a authorizer) RevokeInstallation(_ context.Context, cred *api.InstallationCredential, id string) (*api.RevokeInstallationResult, error) {
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	if a.p.unreachable {
		return nil, &account.Failure{Kind: account.KindUnavailable, Detail: "connection refused"}
	}
	if err := a.p.authorize(*cred); err != nil {
		return nil, &account.Failure{Kind: account.KindUnauthorised, Detail: err.Error()}
	}
	d, ok := a.p.devices[id]
	if !ok {
		return nil, &account.Failure{Kind: account.KindNotFound, Detail: "no such device in this account"}
	}
	out := &api.RevokeInstallationResult{Revocation: api.Revocation{Outcome: "confirmed", AlreadyEnded: d.revoked}, ID: id}
	d.revoked = true
	for _, s := range a.p.shares {
		if s.device == id && !s.ended {
			if a.p.clock.Now().Before(s.expiresAt) {
				out.SharesStopped++
			}
			a.p.endShare(s, "revoked")
		}
	}
	return out, nil
}
