package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/share"
)

// UpdateSource answers the per-share update check.
type UpdateSource interface {
	Check(ctx context.Context, channel string, wait time.Duration) (*share.Release, error)
}

// Timing defaults.
const (
	DefaultProbeTimeout   = 3 * time.Second
	DefaultStartupTimeout = 30 * time.Second
	DefaultRevokeTimeout  = 5 * time.Second
	DefaultReconnectMin   = time.Second
	DefaultReconnectMax   = 30 * time.Second
	sessionEventBuffer    = 256
	recentEnded           = 200
	// expiryDrift is how far this device's clock may be from the
	// platform's when a share the platform no longer lists is read as
	// expired rather than revoked.
	expiryDrift = 5 * time.Second
)

// Config configures an Engine.
type Config struct {
	Platform Platform
	Prober   Prober
	Clock    clock.Clock
	Updates  UpdateSource
	Logger   *log.Logger

	ProbeTimeout   time.Duration
	StartupTimeout time.Duration
	RevokeTimeout  time.Duration
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	// Rand draws the reconnect jitter, uniformly from [0, n). Nil uses
	// math/rand/v2; tests and scenarios fix it, as they fix Clock.
	Rand func(n int64) int64
}

// Engine owns every share of one daemon.
type Engine struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*session
	ops      map[string]*op // by attempt
	byID     map[string]*op
	order    []*op // start order, active and ended
	seq      int
	closed   bool
}

// New returns an engine; Open hands out sessions.
func New(cfg Config) *Engine {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = DefaultProbeTimeout
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = DefaultStartupTimeout
	}
	if cfg.RevokeTimeout <= 0 {
		cfg.RevokeTimeout = DefaultRevokeTimeout
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = DefaultReconnectMin
	}
	if cfg.ReconnectMax < cfg.ReconnectMin {
		cfg.ReconnectMax = DefaultReconnectMax
	}
	if cfg.Rand == nil {
		cfg.Rand = mrand.Int64N
	}
	return &Engine{cfg: cfg, sessions: map[string]*session{}, ops: map[string]*op{}, byID: map[string]*op{}}
}

// Open returns a new session; the start flag is meaningful only to a
// Runner that must start a daemon process, which an in-process engine never
// does.
func (e *Engine) Open(context.Context, bool) (share.Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, share.ErrSessionClosed
	}
	e.seq++
	s := &session{e: e, id: fmt.Sprintf("sess_%d", e.seq), events: make(chan share.Event, sessionEventBuffer)}
	e.sessions[s.id] = s
	return s, nil
}

// Shutdown ends every share with ReasonDaemonShutdown (revocation is
// attempted, bounded) and closes every session. It blocks until the shares
// have ended or ctx is done.
func (e *Engine) Shutdown(ctx context.Context) {
	e.mu.Lock()
	e.closed = true
	sessions := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.Unlock()
	_, _ = e.stopAll(ctx, share.ReasonDaemonShutdown)
	for _, s := range sessions {
		s.close()
	}
}

// Active returns the number of active shares.
func (e *Engine) Active() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, o := range e.ops {
		if o.snapshot().Active() {
			n++
		}
	}
	return n
}

func (e *Engine) broadcast(ev share.Event) {
	e.mu.Lock()
	subs := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		subs = append(subs, s)
	}
	e.mu.Unlock()
	for _, s := range subs {
		s.deliver(ev)
	}
}

func (e *Engine) start(s *session, req share.StartRequest) (share.Share, error) {
	if req.Attempt == "" {
		return share.Share{}, &share.Failure{Kind: share.KindUnauthorised, Detail: "a start needs an attempt id"}
	}
	if req.Credential.Token == "" || req.Credential.Device == "" {
		return share.Share{}, &share.Failure{Kind: share.KindUnauthorised, Detail: "a start needs the installation credential"}
	}
	if len(req.Spec.Targets) == 0 {
		return share.Share{}, &share.Failure{Kind: share.KindUnauthorised, Detail: "a start needs a target"}
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return share.Share{}, share.ErrSessionClosed
	}
	if existing, ok := e.ops[req.Attempt]; ok {
		e.mu.Unlock()
		e.cfg.Logger.Printf("start %s: repeated attempt, returning share %s", req.Attempt, existing.snapshot().ID)
		return existing.snapshot(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Each share has its own Ed25519 key: the platform issues the share's
	// tunnel identity for its public half, and the private key never leaves
	// this process.
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.mu.Unlock()
		cancel()
		return share.Share{}, err
	}
	o := &op{e: e, attempt: req.Attempt, req: req, ctx: ctx, cancel: cancel, done: make(chan struct{}), key: key, lineage: NewLineage()}
	if req.Owner == share.OwnerAttached {
		o.sessionID = s.id
	}
	targets := share.TargetURLs(req.Spec.Targets)
	o.snap = share.Share{
		Attempt: req.Attempt, Target: targets[0], Device: req.Credential.Device, DeviceLabel: req.Credential.DeviceLabel,
		Recipients: req.Spec.Recipients, NoRewrite: req.Spec.NoRewrite, Owner: req.Owner, State: share.StateStarting,
	}
	if len(targets) > 1 {
		o.snap.Targets = targets
	}
	e.ops[req.Attempt] = o
	e.order = append(e.order, o)
	e.mu.Unlock()
	e.cfg.Logger.Printf("start %s (%s)", req.Attempt, req.Owner)
	go o.run()
	return o.snapshot(), nil
}

func (e *Engine) find(req share.StopRequest) *op {
	e.mu.Lock()
	defer e.mu.Unlock()
	if req.ID != "" {
		if o, ok := e.byID[req.ID]; ok {
			return o
		}
	}
	if req.Attempt != "" {
		if o, ok := e.ops[req.Attempt]; ok {
			return o
		}
	}
	return nil
}

func (e *Engine) stop(ctx context.Context, req share.StopRequest) (share.StopResult, error) {
	o := e.find(req)
	if o == nil {
		return share.StopResult{}, &share.Failure{Kind: share.KindNotFound, Detail: fmt.Sprintf("no share %s on this daemon", firstNonEmpty(req.ID, req.Attempt))}
	}
	if req.Reason == "" {
		req.Reason = share.ReasonStopped
	}
	if snap := o.snapshot(); !snap.Active() {
		return share.StopResult{Share: snap, AlreadyEnded: true, Remote: o.remoteOutcome()}, nil
	}
	o.requestStop(req.Reason)
	select {
	case <-o.done:
		return share.StopResult{Share: o.snapshot(), Remote: o.remoteOutcome()}, nil
	case <-ctx.Done():
		return share.StopResult{Share: o.snapshot(), Remote: share.RemoteOutcome{Status: share.RemoteUnconfirmed, Detail: "the stop did not complete in time"}}, ctx.Err()
	}
}

func (e *Engine) stopAll(ctx context.Context, reason share.EndReason) (int, error) {
	e.mu.Lock()
	var active []*op
	for _, o := range e.order {
		if o.snapshot().Active() {
			active = append(active, o)
		}
	}
	e.mu.Unlock()
	for _, o := range active {
		o.requestStop(reason)
	}
	for _, o := range active {
		select {
		case <-o.done:
		case <-ctx.Done():
			return len(active), ctx.Err()
		}
	}
	return len(active), nil
}

func (e *Engine) list() []share.Share {
	e.mu.Lock()
	ops := append([]*op(nil), e.order...)
	e.mu.Unlock()
	var active, ended []share.Share
	for _, o := range ops {
		s := o.snapshot()
		if s.Active() {
			active = append(active, s)
		} else {
			ended = append(ended, s)
		}
	}
	if len(ended) > recentEnded {
		ended = ended[len(ended)-recentEnded:]
	}
	return append(active, ended...)
}

func (e *Engine) closeSession(s *session) {
	e.mu.Lock()
	delete(e.sessions, s.id)
	var owned []*op
	for _, o := range e.order {
		if o.sessionID == s.id && o.snapshot().Active() {
			owned = append(owned, o)
		}
	}
	e.mu.Unlock()
	s.close()
	for _, o := range owned {
		e.cfg.Logger.Printf("session %s closed; ending attached share %s", s.id, o.attempt)
		o.requestStop(share.ReasonCLIDisconnected)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// session is one CLI connection.
type session struct {
	e      *Engine
	id     string
	events chan share.Event

	mu     sync.Mutex
	closed bool
}

func (s *session) deliver(ev share.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.e.cfg.Logger.Printf("session %s: event queue full; dropping %s", s.id, ev.Kind)
	}
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Start implements share.Session.
func (s *session) Start(_ context.Context, req share.StartRequest) (share.Share, error) {
	if s.isClosed() {
		return share.Share{}, share.ErrSessionClosed
	}
	return s.e.start(s, req)
}

// Events implements share.Session.
func (s *session) Events() <-chan share.Event { return s.events }

// Stop implements share.Session.
func (s *session) Stop(ctx context.Context, req share.StopRequest) (share.StopResult, error) {
	return s.e.stop(ctx, req)
}

// StopAll implements share.Session.
func (s *session) StopAll(ctx context.Context, reason share.EndReason) (int, error) {
	return s.e.stopAll(ctx, reason)
}

// Shares implements share.Session.
func (s *session) Shares(context.Context) ([]share.Share, error) {
	if s.isClosed() {
		return nil, share.ErrSessionClosed
	}
	return s.e.list(), nil
}

// CheckUpdate implements share.Session.
func (s *session) CheckUpdate(ctx context.Context, channel string, wait time.Duration) (*share.Release, error) {
	if s.e.cfg.Updates == nil {
		return nil, nil
	}
	return s.e.cfg.Updates.Check(ctx, channel, wait)
}

// Close implements share.Session: attached shares end with
// ReasonCLIDisconnected.
func (s *session) Close() error {
	s.e.closeSession(s)
	return nil
}
