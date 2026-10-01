package engine

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/share"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// op is one share from start to end. Its goroutine (run) is the only
// writer of the lifecycle; other goroutines request a stop by cancelling
// ctx and read snapshots.
type op struct {
	e         *Engine
	attempt   string
	req       share.StartRequest
	sessionID string // owning session for attached shares
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}

	mu        sync.Mutex
	snap      share.Share
	requested share.EndReason
	remote    share.RemoteOutcome
	record    *api.ShareAccess
	wasReady  bool
	// key is the share's own key; every create and replay of the attempt
	// sends its public half. lineage carries the share from one dock to
	// the next.
	key     ed25519.PrivateKey
	lineage *Lineage

	// Owned by run: what the share has already told its subscribers. The
	// first visitor is once per share; the app state is kept per target.
	visited bool
	appDown map[string]bool
	// Owned by run: the reconnect pacing.
	retry backoff
}

var none = share.RemoteOutcome{Status: share.RemoteNone}

func (o *op) snapshot() share.Share {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.snap
}

func (o *op) remoteOutcome() share.RemoteOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.remote
}

func (o *op) update(fn func(*share.Share)) {
	o.mu.Lock()
	fn(&o.snap)
	o.mu.Unlock()
}

// requestStop records the first requested reason and cancels the run.
func (o *op) requestStop(reason share.EndReason) {
	o.mu.Lock()
	if o.requested == "" {
		o.requested = reason
	}
	o.mu.Unlock()
	o.cancel()
}

func (o *op) emit(kind share.EventKind, remote share.RemoteOutcome, detail string) {
	o.e.broadcast(share.Event{Kind: kind, Share: o.snapshot(), Remote: remote, Detail: detail})
}

// finish ends the share exactly once.
func (o *op) finish(reason share.EndReason, remote share.RemoteOutcome, detail string) {
	o.finishApp(reason, remote, detail, "")
}

// finishApp is finish for an end that one target caused: the record names
// that app in EndApp and keeps the cause in Detail, and the ended event also
// names it in its Detail, as the app events do. A start reply or stop result
// that already sees the end carries only the record, so the record must name
// the app too.
func (o *op) finishApp(reason share.EndReason, remote share.RemoteOutcome, detail, app string) {
	o.mu.Lock()
	if o.snap.State == share.StateEnded {
		o.mu.Unlock()
		return
	}
	o.snap.State = share.StateEnded
	o.snap.EndReason = reason
	o.snap.EndApp = app
	o.snap.Detail = detail
	o.remote = remote
	o.lineage = nil // it dies with the share; the record stays for the list
	id := o.snap.ID
	o.mu.Unlock()
	o.e.cfg.Logger.Printf("share %s (%s) ended: %s (remote %s) %s", id, o.attempt, reason, remote.Status, detail)
	if app == "" {
		app = detail
	}
	o.emit(share.EventEnded, remote, app)
}

func (o *op) setRecord(rec *api.ShareAccess) {
	o.mu.Lock()
	o.record = rec
	o.snap.ID = rec.Share.ID
	o.snap.URL = rec.URL
	o.snap.Origin = rec.Share.Origin
	o.snap.CreatedAt = rec.Share.CreatedAt
	o.snap.ExpiresAt = rec.Share.ExpiresAt
	o.snap.Invites = nil
	for _, v := range rec.Invites {
		o.snap.Invites = append(o.snap.Invites, share.Invite{Email: v.Email, Status: v.Status})
	}
	o.mu.Unlock()
	o.e.mu.Lock()
	o.e.byID[rec.Share.ID] = o
	o.e.mu.Unlock()
	o.e.cfg.Logger.Printf("share %s created for attempt %s (expires %s)", rec.Share.ID, o.attempt, rec.Share.ExpiresAt.UTC().Format(time.RFC3339))
}

// stopReason maps the requested stop onto the end reason: a stop before
// the share was ever ready is a cancellation.
func (o *op) stopReason() share.EndReason {
	o.mu.Lock()
	defer o.mu.Unlock()
	reason := o.requested
	if reason == "" {
		reason = share.ReasonStopped
	}
	if !o.wasReady && (reason == share.ReasonStopped || reason == share.ReasonCLIDisconnected) {
		return share.ReasonCancelled
	}
	return reason
}

func (o *op) run() {
	defer close(o.done)
	cfg := o.e.cfg
	ctx := o.ctx
	o.retry = backoff{min: cfg.ReconnectMin, max: cfg.ReconnectMax, rand: cfg.Rand}
	targets := share.TargetURLs(o.req.Spec.Targets)

	// 1. Probe every target before anything exists on the platform, at once
	// and under one bound; the first in the list that refuses names the end.
	pctx, pcancel := clock.WithTimeout(ctx, cfg.Clock, cfg.ProbeTimeout)
	errs := make([]error, len(targets))
	var probes sync.WaitGroup
	for i, target := range targets {
		probes.Go(func() { errs[i] = cfg.Prober.Probe(pctx, target) })
	}
	probes.Wait()
	pcancel()
	for i, err := range errs {
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			o.finish(o.stopReason(), none, "no share was created")
			return
		}
		o.finishApp(share.ReasonTargetUnreachable, none, err.Error(), share.DisplayTarget(targets[i]))
		return
	}

	// 2. Create the share; the attempt id is the idempotency key.
	rec, err := cfg.Platform.CreateShare(ctx, o.req.Credential, o.createRequest())
	if err != nil {
		if ctx.Err() != nil {
			o.recoverCancelledCreate()
			return
		}
		o.finishCreateFailure(err)
		return
	}
	o.setRecord(rec)

	// 3. Connect until the route is ready, bounded by the startup timeout.
	startup := cfg.Clock.NewTimer(cfg.StartupTimeout)
	conn, ok := o.connect(ctx, startup.C(), nil, 0)
	startup.Stop()
	if !ok {
		return
	}

	// 4. Ready. The expiry timer is armed before the announcement so that
	// nothing observes a ready share without a local expiry; readyAt, when
	// the connection became ready, is taken before it too.
	expiry := cfg.Clock.NewTimer(rec.Share.ExpiresAt.Sub(cfg.Clock.Now()))
	defer expiry.Stop()
	readyAt := cfg.Clock.Now()
	o.update(func(s *share.Share) { s.State = share.StateReady })
	o.mu.Lock()
	o.wasReady = true
	o.mu.Unlock()
	o.emit(share.EventReady, none, "")

	// 5. Serve until something ends it.
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			o.endRequested()
			return
		case <-expiry.C():
			_ = conn.Close()
			o.finish(share.ReasonExpired, none, "")
			return
		case ev, ok := <-conn.Events():
			switch {
			case ok && (ev.Kind == ConnReady || o.observe(ev)):
				continue
			case ok && ev.Kind == ConnRevoked:
				_ = conn.Close()
				o.finish(share.ReasonRevoked, none, ev.Detail)
				return
			case ok && ev.Kind == ConnExpired:
				_ = conn.Close()
				o.finish(share.ReasonExpired, none, ev.Detail)
				return
			}
			// Lost, drained, or the connection closed: redock, keeping URL
			// and expiry. A dock that ended unasked may have been torn down
			// by the platform, so the platform is asked first; a drain is
			// the edge moving an active share along.
			_ = conn.Close()
			if !ok || ev.Kind != ConnDrained {
				if reason, detail, ended := o.ended(ctx, rec); ended {
					o.finish(reason, none, detail)
					return
				}
				if ctx.Err() != nil {
					o.endRequested()
					return
				}
			}
			o.update(func(s *share.Share) { s.State = share.StateReconnecting })
			o.emit(share.EventConnectionLost, none, ev.Detail)
			conn, ok = o.connect(ctx, nil, expiry.C(), o.redockDelay(readyAt))
			if !ok {
				return
			}
			readyAt = cfg.Clock.Now()
			o.update(func(s *share.Share) { s.State = share.StateReady })
			o.emit(share.EventRestored, none, "")
		}
	}
}

// observe turns a connection's visitor and app observations into the share's
// events and reports whether ev was one. Each connection starts afresh, so the
// share keeps the state: the first visitor is announced once, and unresponsive
// and responding strictly alternate, starting from responding because the
// target was probed before the share was created.
func (o *op) observe(ev ConnEvent) bool {
	detail := ev.Detail
	if detail == "" {
		detail = share.DisplayTarget(o.req.Spec.Targets[0].URL)
	}
	if o.appDown == nil {
		o.appDown = map[string]bool{}
	}
	switch ev.Kind {
	case ConnFirstVisitor:
		if !o.visited {
			o.visited = true
			o.emit(share.EventFirstVisitor, none, detail)
		}
	case ConnAppUnresponsive:
		if !o.appDown[detail] {
			o.appDown[detail] = true
			o.emit(share.EventAppUnresponsive, none, detail)
		}
	case ConnAppResponding:
		if o.appDown[detail] {
			o.appDown[detail] = false
			o.emit(share.EventAppResponding, none, detail)
		}
	default:
		return false
	}
	return true
}

// createRequest names every target when there are several; rewrite_urls
// stays false on the wire, rewriting being the daemon's own decision.
func (o *op) createRequest() api.CreateShareRequest {
	targets := share.TargetURLs(o.req.Spec.Targets)
	req := api.CreateShareRequest{Key: o.attempt, Target: targets[0], TTL: o.req.Spec.TTL, Recipients: o.req.Spec.Recipients}
	if len(targets) > 1 {
		req.Targets = targets
	}
	req.PublicKey = o.key.Public().(ed25519.PublicKey)
	return req
}

// serving is what the tunnel client needs for this share.
func (o *op) serving() Serving {
	s := Serving{Origin: o.snapshot().Origin, Targets: share.TargetURLs(o.req.Spec.Targets), Rewrite: !o.req.Spec.NoRewrite}
	o.mu.Lock()
	if o.record != nil {
		s.Tunnel = o.record.Tunnel
	}
	s.Key, s.Lineage = o.key, o.lineage
	o.mu.Unlock()
	return s
}

// admitted checks the target admission named against the share's own list.
// When they disagree the connection serves the admitted target alone, so the
// record names only that one: the block then prints no further app.
func (o *op) admitted(ev ConnEvent) {
	if ev.Detail == "" || len(o.req.Spec.Targets) < 2 || resource.TargetKey(ev.Detail) == resource.TargetKey(o.req.Spec.Targets[0].URL) {
		return
	}
	o.e.cfg.Logger.Printf("share %s: admission names %s, not the first listed target %s; serving it alone", o.snapshot().ID, ev.Detail, o.req.Spec.Targets[0].URL)
	o.update(func(s *share.Share) { s.Target, s.Targets = ev.Detail, nil })
}

// connect obtains an admitted, ready connection, first waiting delay.
// deadline (initial start) and expiry (reconnect) bound the attempts; a nil
// channel never fires. It returns false when the op has been finished
// instead.
func (o *op) connect(ctx context.Context, deadline, expiry <-chan time.Time, delay time.Duration) (Connection, bool) {
	cfg := o.e.cfg
	snap := o.snapshot()
	id := snap.ID
	for attempt := 1; ; attempt++ {
		if delay > 0 && !o.pause(ctx, deadline, expiry, delay) {
			return nil, false
		}
		conn, err := cfg.Platform.ConnectServing(ctx, o.req.Credential, id, o.serving())
		if err == nil {
			select {
			case ev, ok := <-conn.Events():
				switch {
				case ok && ev.Kind == ConnReady:
					o.admitted(ev)
					return conn, true
				case ok && ev.Kind == ConnRevoked:
					_ = conn.Close()
					o.finish(share.ReasonRevoked, none, ev.Detail)
					return nil, false
				case ok && ev.Kind == ConnExpired:
					_ = conn.Close()
					o.finish(share.ReasonExpired, none, ev.Detail)
					return nil, false
				default:
					_ = conn.Close() // lost before ready: retry
				}
			case <-ctx.Done():
				_ = conn.Close()
				o.endRequested()
				return nil, false
			case <-deadline:
				_ = conn.Close()
				o.endStartupTimeout()
				return nil, false
			case <-expiry:
				_ = conn.Close()
				o.finish(share.ReasonExpired, none, "")
				return nil, false
			}
		} else {
			if ctx.Err() != nil {
				o.endRequested()
				return nil, false
			}
			if rej := AsRejected(err); rej != nil {
				o.finish(reasonFor(rej), none, rej.Detail)
				return nil, false
			}
		}
		delay = o.retry.next()
		var busy *Unavailable
		if errors.As(err, &busy) {
			delay += busy.RetryAfter
		}
		if err != nil {
			cfg.Logger.Printf("share %s: connect attempt %d failed: %v; retrying in %s", id, attempt, err, delay)
		}
	}
}

// pause waits d before the next connect attempt. It returns false when the
// op has been finished instead.
func (o *op) pause(ctx context.Context, deadline, expiry <-chan time.Time, d time.Duration) bool {
	wait := o.e.cfg.Clock.NewTimer(d)
	defer wait.Stop()
	select {
	case <-wait.C():
		return true
	case <-ctx.Done():
		o.endRequested()
	case <-deadline:
		o.endStartupTimeout()
	case <-expiry:
		o.finish(share.ReasonExpired, none, "")
	}
	return false
}

// redockDelay is the wait before the first attempt of a redock. A share
// whose dock was up for less than ReconnectMax waits its back-off first, so
// a dock that the platform keeps tearing down, or that the edge keeps
// draining, is not redocked in a tight loop. Any other redock starts at
// once, with the back-off from the start.
func (o *op) redockDelay(readyAt time.Time) time.Duration {
	cfg := o.e.cfg
	if cfg.Clock.Now().Sub(readyAt) < cfg.ReconnectMax {
		return o.retry.next()
	}
	o.retry.reset()
	return 0
}

// backoff paces reconnect attempts with full jitter: each wait is drawn
// uniformly between min and a ceiling that starts at min and doubles with
// every draw up to max, so daemons cut off together don't come back in
// step.
type backoff struct {
	min, max, ceiling time.Duration
	rand              func(n int64) int64
}

func (b *backoff) next() time.Duration {
	b.ceiling = max(b.ceiling, b.min)
	d := b.min + time.Duration(b.rand(int64(b.ceiling-b.min)+1))
	b.ceiling = min(2*b.ceiling, b.max)
	return d
}

func (b *backoff) reset() { b.ceiling = b.min }

// ended asks the platform whether a share whose dock ended unasked is
// still active. The dock ends the same way whether the
// network dropped it or the platform tore it down, so only the platform
// can tell. It returns the end reason when the share is no longer active.
// An unanswered check redocks: the platform tears down every dock of an
// inactive share, and the gateway refuses its visitors either way.
func (o *op) ended(ctx context.Context, rec *api.ShareAccess) (share.EndReason, string, bool) {
	cfg := o.e.cfg
	cctx, cancel := clock.WithTimeout(ctx, cfg.Clock, cfg.RevokeTimeout)
	active, err := cfg.Platform.ShareActive(cctx, o.req.Credential, rec.Share.ID)
	cancel()
	switch {
	case err == nil && active:
		return "", "", false
	case err == nil && rec.Share.ExpiresAt.Sub(cfg.Clock.Now()) <= expiryDrift:
		return share.ReasonExpired, "", true
	case err == nil:
		return share.ReasonRevoked, "", true
	}
	if rej := AsRejected(err); rej != nil && rej.Reason == RejectAuthority {
		return share.ReasonAuthorityInvalid, rej.Detail, true
	}
	if ctx.Err() == nil {
		cfg.Logger.Printf("share %s: could not ask whether it is still active: %v; redocking", rec.Share.ID, err)
	}
	return "", "", false
}

func reasonFor(rej *Rejected) share.EndReason {
	switch rej.Reason {
	case RejectExpired:
		return share.ReasonExpired
	case RejectRevoked:
		return share.ReasonRevoked
	case RejectAuthority:
		return share.ReasonAuthorityInvalid
	default:
		return share.ReasonRejected
	}
}

// endRequested finishes after a requested stop: local forwarding is
// already closed by the caller; remote revocation is attempted where the
// reason calls for it.
func (o *op) endRequested() {
	reason := o.stopReason()
	remote := o.revokeFor(reason)
	o.finish(reason, remote, remote.Detail)
}

func (o *op) endStartupTimeout() {
	remote := o.revokeFor(share.ReasonStartupTimeout)
	o.finish(share.ReasonStartupTimeout, remote, fmt.Sprintf("not ready within %s", share.FormatDuration(o.e.cfg.StartupTimeout)))
}

// revokeFor asks the platform to end the share for reasons that originate
// on this device. The result says whether the platform confirmed.
func (o *op) revokeFor(reason share.EndReason) share.RemoteOutcome {
	o.mu.Lock()
	rec := o.record
	o.mu.Unlock()
	if rec == nil {
		return share.RemoteOutcome{Status: share.RemoteNone, Detail: "no share was created"}
	}
	switch reason {
	case share.ReasonSignedOut:
		// logout revokes the whole device on the platform and reports that
		// outcome itself; a per-share call here would only duplicate it.
		return share.RemoteOutcome{Status: share.RemoteNone, Detail: "ended by logout on this device"}
	case share.ReasonExpired, share.ReasonRevoked, share.ReasonAuthorityInvalid, share.ReasonRejected, share.ReasonLimitReached, share.ReasonUpdateRequired:
		return none
	}
	rctx, cancel := clock.WithTimeout(context.Background(), o.e.cfg.Clock, o.e.cfg.RevokeTimeout)
	defer cancel()
	if err := o.e.cfg.Platform.Revoke(rctx, o.req.Credential, rec.Share.ID); err != nil {
		if rctx.Err() != nil && errors.Is(err, rctx.Err()) {
			return share.RemoteOutcome{Status: share.RemoteUnconfirmed, Detail: fmt.Sprintf("Purlview did not confirm within %s", share.FormatDuration(o.e.cfg.RevokeTimeout))}
		}
		return share.RemoteOutcome{Status: share.RemoteUnconfirmed, Detail: err.Error()}
	}
	return share.RemoteOutcome{Status: share.RemoteConfirmed}
}

// recoverCancelledCreate handles a stop that arrived while the create call
// was in flight: the platform may or may not have created the share. The
// same idempotency key recovers the answer, and a created share is
// revoked. A refused or unreachable reconciliation leaves the outcome uncertain.
func (o *op) recoverCancelledCreate() { o.recoverCreate(o.stopReason()) }

func (o *op) recoverCreate(reason share.EndReason) {
	rctx, cancel := clock.WithTimeout(context.Background(), o.e.cfg.Clock, o.e.cfg.RevokeTimeout)
	defer cancel()
	rec, err := o.e.cfg.Platform.CreateShare(rctx, o.req.Credential, o.createRequest())
	switch {
	case err == nil:
		o.setRecord(rec)
		remote := o.revokeFor(reason)
		o.finish(reason, remote, remote.Detail)
	case AsRejected(err) != nil:
		o.finish(reason, share.RemoteOutcome{Status: share.RemoteUnconfirmed, Detail: "Purlview refused reconciliation; the original create outcome is unknown"}, "Purlview refused reconciliation; the original create outcome is unknown")
	default:
		o.finish(reason, share.RemoteOutcome{Status: share.RemoteUnconfirmed, Detail: "Purlview could not be reached to confirm whether a share was created"}, "Purlview could not be reached to confirm whether a share was created")
	}
}

func (o *op) finishCreateFailure(err error) {
	var uncertain *Uncertain
	if errors.As(err, &uncertain) {
		// One bounded reconciliation under the same key, followed by cleanup. A
		// failed start must never leave an allocated record mistaken for rejection.
		o.recoverCreate(share.ReasonPlatformUnavailable)
		return
	}
	if rej := AsRejected(err); rej != nil {
		if rej.Reason == RejectAuthority {
			o.finish(share.ReasonAuthorityInvalid, none, rej.Detail)
			return
		}
		if rej.Reason == RejectUpdate {
			o.finish(share.ReasonUpdateRequired, none, rej.Detail)
			return
		}
		if rej.Limit != "" {
			o.mu.Lock()
			o.snap.EndLimit = rej.Limit
			o.mu.Unlock()
			o.finish(share.ReasonLimitReached, none, rej.Detail)
			return
		}
		o.finish(share.ReasonRejected, none, rej.Detail)
		return
	}
	if IsUnavailable(err) {
		o.finish(share.ReasonPlatformUnavailable, none, err.Error())
		return
	}
	o.finish(share.ReasonRejected, none, err.Error())
}
