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

package command

import (
	"context"
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/internal/updatecheck"

	"github.com/idyl-labs/purlview/sdk/api"
)

// expiryWarning is how long before the end a foreground share says so.
const expiryWarning = 5 * time.Minute

// newShare builds the share command. Output contract: the link is the only
// thing written to stdout; the rest of the share block, session events, the
// update notice and errors go to stderr. Redirecting stdout therefore
// captures exactly the link.
func newShare(info buildinfo.Info, p *output.Printer, d Deps) *cobra.Command {
	var background, noRewrite bool
	var ttl string
	var to []string
	cmd := &cobra.Command{
		Use:   "share <target> [<target>...] [--to <email>]... [--ttl 15m] [--background] [--no-rewrite]",
		Short: "Share your running app and print its link",
		// The use line lists the flags itself.
		DisableFlagsInUseLine: true,
		Long: `Share your running app and print its link.

Name your app by its port (3000), by host and port (localhost:3000,
192.168.1.20:8080) or by a full address
(https://staging.example.invalid/demo). A path or query, as in
3000/dashboard?tab=2, is the page the link opens; the whole app stays
reachable.

Name several apps, separated by spaces (5173 8000), when one calls another:
the link opens the first app's page, and the others are reachable from it
as they are on your machine. Their addresses in what your apps send are
translated for the visitor; --no-rewrite leaves the bodies alone.

The link is the only thing printed on standard output; everything else goes
to standard error. Anyone with the link can open your app, or only the people
you name with --to. Ctrl-C stops the share. With --background the command
returns once the share is running and says how to stop it. Shares last one
hour at most; --ttl asks for less.

The first share signs you in with an email code when this is a terminal;
otherwise run 'purlview login' first.`,
		Example: `  purlview share 3000
  purlview share 5173 8000
  purlview share 3000 --to ana@example.com
  purlview share 3000/dashboard --background
  purlview share https://staging.example.invalid/demo --ttl 15m`,
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := share.ParseTargets(args)
			if err != nil {
				return invalid(err)
			}
			lifetime, err := share.ParseTTL(ttl)
			if err != nil {
				return invalid(err)
			}
			recipients, err := share.ParseRecipients(to)
			if err != nil {
				return invalid(err)
			}
			spec := share.Spec{Targets: targets, TTL: lifetime, Recipients: recipients, NoRewrite: noRewrite}
			return runShare(cmd.Context(), info, p, d, spec, background)
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "Keep sharing after this command returns.")
	cmd.Flags().StringVar(&ttl, "ttl", "", "How long the share lasts, a `duration` up to 1h (default 1h); for example 15m.")
	cmd.Flags().StringArrayVar(&to, "to", nil, "Let in only this `email` address; repeat for several. Each address is sent an invite.")
	cmd.Flags().BoolVar(&noRewrite, "no-rewrite", false, "Leave the app's own addresses in response bodies unchanged.")
	return cmd
}

// invalid turns a rejected argument into a usage error.
func invalid(err error) error {
	var in *share.InputError
	if !errors.As(err, &in) {
		return &usageError{err: err}
	}
	pr := misuse(in.Problem)
	switch {
	case in.Try != "":
		pr = pr.remedy(in.Remedy, bright(in.Try))
	case in.Remedy != "":
		pr = pr.remedy(in.Remedy)
	}
	return pr
}

func runShare(ctx context.Context, info buildinfo.Info, p *output.Printer, d Deps, spec share.Spec, background bool) error {
	// The local daemon comes first: it is needed in every case, its start
	// is quick, and the update check it performs then overlaps with
	// sign-in and share creation instead of delaying readiness.
	sess, err := d.Runner.Open(ctx, true)
	if err != nil {
		if errors.Is(err, daemon.ErrMaintenance) {
			return fail("Purlview is being updated").remedy("try again in a moment").because(err)
		}
		return backgroundFailed(err)
	}
	defer func() { _ = sess.Close() }()
	notice := newUpdateNotice(ctx, d, sess, info)
	defer notice.stop()

	cred, err := d.Store.Load()
	if err != nil {
		return err
	}
	if cred == nil {
		if !d.Terminal.Interactive {
			return notSignedIn()
		}
		if cred, err = signIn(ctx, p, d); err != nil {
			return err
		}
		p.Blank()
	}

	owner := share.OwnerAttached
	if background {
		owner = share.OwnerDetached
	}
	req := share.StartRequest{Attempt: d.NewID(), Spec: spec, Owner: owner, Credential: *cred}
	if !background {
		// Only a foreground share can still print a fresh answer.
		notice.startFresh(ctx)
	}
	snap, started, err := startShare(ctx, d, sess, req)
	if started != sess {
		sess = started // closed by the deferred Close above
	}
	if err != nil {
		return err
	}
	w := &shareWatch{p: p, d: d, info: info, sess: sess, req: req, background: background, notice: notice}
	return w.run(ctx, snap)
}

// backgroundFailed is the daemon that would not start. Its reason is the one
// raw cause a person gets to see, because nothing else explains it.
func backgroundFailed(err error) *problem {
	return fail("Couldn't start Purlview's background process").remedy(err.Error()).then("purlview daemon status").because(err)
}

// lostBackground is a daemon that went away under a command.
func lostBackground(err error) *problem {
	return fail("Purlview's background process stopped").remedy("see ", bright("purlview list")).because(err)
}

// startShare submits the request. A transport failure means the reply was
// lost, so the CLI reconnects and repeats the same attempt: the daemon
// answers with the share it may already own instead of creating another.
func startShare(ctx context.Context, d Deps, sess share.Session, req share.StartRequest) (share.Share, share.Session, error) {
	snap, err := sess.Start(ctx, req)
	if err == nil {
		return snap, sess, nil
	}
	if errors.Is(err, share.ErrNotImplemented) {
		return snap, sess, notBuilt("Sharing", err)
	}
	var f *share.Failure
	if errors.As(err, &f) {
		return snap, sess, fail("The share didn't start").remedy("nothing was left running; try again").because(err)
	}
	if ctx.Err() != nil {
		return snap, sess, cancelled(share.RemoteOutcome{})
	}
	_ = sess.Close()
	again, oerr := d.Runner.Open(ctx, true)
	if oerr != nil {
		return snap, sess, lostBackground(errors.Join(err, oerr))
	}
	snap, err2 := again.Start(ctx, req)
	if err2 != nil {
		if errors.Is(err2, share.ErrNotImplemented) {
			return snap, again, notBuilt("Sharing", err2)
		}
		return snap, again, lostBackground(errors.Join(err, err2))
	}
	return snap, again, nil
}

// cancelled is Ctrl-C before the link was shown (exit 130). Nothing is left
// running here in any case; what Purlview could not confirm is said.
func cancelled(remote share.RemoteOutcome) *problem {
	pr := &problem{msg: output.Msg(output.None, "Cancelled").Remedy("nothing was left running"), code: ExitInterrupted}
	if remote.Status == share.RemoteUnconfirmed {
		pr.msg = output.Msg(output.None, "Cancelled").Remedy("Purlview couldn't confirm; see ", bright("purlview list"))
		pr.cause = errors.New(remote.Detail)
	}
	return pr
}

// shareWatch follows one share from submission to its end.
type shareWatch struct {
	p          *output.Printer
	d          Deps
	info       buildinfo.Info
	sess       share.Session
	req        share.StartRequest
	background bool
	notice     *updateNotice

	share share.Share
	ready bool
	// spaced: the blank line between the share block and what follows it has
	// been printed. It is printed only when something follows.
	spaced bool
	warn   clock.Timer
}

func (w *shareWatch) run(ctx context.Context, snap share.Share) error {
	w.share = snap
	switch snap.State {
	case share.StateEnded:
		// A retried start found the share already over; ask for its outcome.
		res, err := w.sess.Stop(ctx, share.StopRequest{Attempt: w.req.Attempt})
		if err != nil {
			return w.ended(share.Event{Kind: share.EventEnded, Share: snap})
		}
		return w.ended(share.Event{Kind: share.EventEnded, Share: res.Share, Remote: res.Remote})
	case share.StateReady, share.StateReconnecting:
		w.announceReady(snap)
		if w.background {
			return nil
		}
	}
	defer func() {
		if w.warn != nil {
			w.warn.Stop()
		}
	}()
	for {
		var warn <-chan time.Time
		if w.warn != nil {
			warn = w.warn.C()
		}
		select {
		case <-ctx.Done():
			return w.interrupt()
		case u := <-w.notice.C:
			// An answer that beats the link waits for the share block.
			w.notice.known = u
			w.announceUpdate(u)
		case <-warn:
			w.warn = nil
			w.event(expiresSoon())
		case ev, ok := <-w.sess.Events():
			if !ok {
				return w.daemonGone()
			}
			if ev.Share.Attempt != w.req.Attempt {
				continue
			}
			w.share = ev.Share
			switch ev.Kind {
			case share.EventReady:
				w.announceReady(ev.Share)
				if w.background {
					return nil
				}
			case share.EventFirstVisitor:
				w.event(firstVisitor())
			case share.EventAppUnresponsive:
				w.event(appUnresponsive(w.app(ev)))
			case share.EventAppResponding:
				w.event(appResponding(w.app(ev)))
			case share.EventConnectionLost:
				w.event(connectionLost())
			case share.EventRestored:
				w.event(reconnected(w.expiresAt()))
			case share.EventEnded:
				return w.ended(ev)
			default:
				// Event kinds are additive; a later daemon may know more.
			}
		}
	}
}

// app names the app an event is about: the event says, or the share does.
func (w *shareWatch) app(ev share.Event) string {
	if ev.Detail != "" {
		return ev.Detail
	}
	return share.DisplayTarget(w.share.Target)
}

func (w *shareWatch) expiresAt() string {
	return output.Clock(w.share.ExpiresAt.In(w.d.Local))
}

// event appends one session event below the share block.
func (w *shareWatch) event(m output.Message) {
	if !w.ready {
		return
	}
	w.space()
	w.p.Event(w.d.Clock.Now().In(w.d.Local), m)
}

func (w *shareWatch) space() {
	if !w.spaced {
		w.spaced = true
		w.p.Blank()
	}
}

// announceReady prints the share block: the link on stdout, the rest on
// stderr. A cached update notice follows it, never precedes it.
func (w *shareWatch) announceReady(sh share.Share) {
	if w.ready {
		return
	}
	w.ready = true
	apps := sh.Apps()
	block := output.ShareBlock{
		App: share.DisplayTarget(apps[0]), Link: sh.URL, Recipients: sh.Recipients,
		ExpiresIn: share.FormatRemaining(w.d.Clock.Now(), sh.ExpiresAt), ExpiresAt: output.Clock(sh.ExpiresAt.In(w.d.Local)),
	}
	for _, app := range apps[1:] {
		block.More = append(block.More, share.DisplayTarget(app))
	}
	for _, inv := range sh.Invites {
		if inv.Status == api.InviteSent {
			block.InvitesSent = append(block.InvitesSent, inv.Email)
		} else {
			block.InvitesNotSent = append(block.InvitesNotSent, inv.Email)
		}
	}
	if entry, _, ok := share.EntryAndOrigin(sh.Target); ok {
		block.Opens = entry
	}
	if w.background {
		block.StopID = share.Label(sh.ID)
	}
	w.p.Share(block)
	w.announceUpdate(w.notice.cached())
	if left := sh.ExpiresAt.Sub(w.d.Clock.Now()); !w.background && left > expiryWarning {
		// The CLI times the warning itself from the expiry it was given: a
		// foreground share has a CLI for exactly as long as it runs.
		w.warn = w.d.Clock.NewTimer(left - expiryWarning)
	}
}

// announceUpdate prints the update notice when a person is there to read
// it, at most once per version per day.
func (w *shareWatch) announceUpdate(u *output.Update) {
	if u == nil || !w.ready || !w.d.Terminal.Attended || w.notice.shown {
		return
	}
	w.notice.shown = true
	if !w.d.NoticeDue(u.Latest, w.d.Clock.Now()) {
		return
	}
	w.space()
	w.p.UpdateNotice(*u)
}

// interrupt handles Ctrl-C: stop the share, report what that achieved.
func (w *shareWatch) interrupt() error {
	sctx, cancel := clock.WithTimeout(context.Background(), w.d.Clock, stopWait)
	res, err := w.sess.Stop(sctx, share.StopRequest{ID: w.share.ID, Attempt: w.req.Attempt, Reason: share.ReasonStopped})
	cancel()
	if !w.ready {
		if err != nil {
			return cancelled(share.RemoteOutcome{Status: share.RemoteUnconfirmed, Detail: err.Error()})
		}
		return cancelled(res.Remote)
	}
	if err != nil {
		return w.stoppedUnconfirmed()
	}
	if res.AlreadyEnded {
		return w.ended(share.Event{Kind: share.EventEnded, Share: res.Share, Remote: res.Remote})
	}
	w.share = res.Share
	if res.Remote.Status == share.RemoteConfirmed {
		w.event(stopped())
		return nil
	}
	return w.stoppedUnconfirmed()
}

func (w *shareWatch) stoppedUnconfirmed() error {
	w.event(stoppedHere(w.expiresAt()))
	return exit(ExitError)
}

func (w *shareWatch) daemonGone() error {
	if !w.ready {
		return lostBackground(share.ErrSessionClosed)
	}
	w.event(backgroundStopped(false))
	return exit(ExitError)
}

// ended turns the terminal event into the command's result.
func (w *shareWatch) ended(ev share.Event) error {
	sh := ev.Share
	cause := errors.New(string(sh.EndReason) + ": " + sh.Detail)
	if !w.ready {
		switch sh.EndReason {
		case share.ReasonTargetUnreachable:
			// The record names the app that did not answer, whichever way
			// the end arrived. An older daemon names it only in the ended
			// event, and one from before several targets names none: then
			// it is the one.
			app := sh.EndApp
			if app == "" && ev.Detail != sh.Detail {
				app = ev.Detail
			}
			if app == "" {
				app = share.DisplayTarget(sh.Target)
			}
			return fail("Nothing is answering at ", output.Name(app)).remedy("start your app, then share again").because(cause)
		case share.ReasonPlatformUnavailable:
			if ev.Remote.Status == share.RemoteUnconfirmed {
				return fail("Couldn't reach Purlview").remedy("a share may have started; see ", bright("purlview list")).because(errors.New(ev.Remote.Detail))
			}
			return unreachable(cause)
		case share.ReasonAuthorityInvalid, share.ReasonSignedOut:
			return signedOutElsewhere(cause)
		case share.ReasonStartupTimeout:
			return fail("The share didn't start within " + share.FormatDuration(engine.DefaultStartupTimeout)).remedy("nothing was left running; try again").because(cause)
		case share.ReasonDaemonShutdown:
			return (&problem{msg: backgroundStopped(true), code: ExitError}).because(cause)
		case share.ReasonCancelled:
			return cancelled(ev.Remote)
		case share.ReasonLimitReached:
			return limitReached(sh.EndLimit).because(cause)
		case share.ReasonUpdateRequired:
			return updateRequired(w.d.UpdateAdvice(w.info)).because(cause)
		default:
			return fail("The share didn't start").remedy("nothing was left running; try again").because(cause)
		}
	}
	switch sh.EndReason {
	case share.ReasonExpired:
		w.event(shareExpired())
		return nil
	case share.ReasonRevoked:
		w.event(stoppedElsewhere())
		return nil
	case share.ReasonSignedOut, share.ReasonAuthorityInvalid:
		w.event(deviceSignedOut())
	case share.ReasonStopped, share.ReasonCancelled:
		if ev.Remote.Status == share.RemoteConfirmed {
			w.event(stopped())
			return nil
		}
		w.event(stoppedHere(w.expiresAt()))
	case share.ReasonDaemonShutdown:
		w.event(backgroundStopped(false))
	default:
		w.event(purlviewStoppedShare())
	}
	if w.d.Getenv(EnvDebug) == "1" {
		w.p.Cause(cause.Error())
	}
	return exit(ExitError)
}

// updateNotice is the per-share update check beside the share. Neither
// readiness nor completion ever waits for release metadata:
//
//  1. newUpdateNotice makes one local call that asks the daemon for what
//     it already knows (zero wait). A newer cached version is announced
//     right after the share block; the call also starts the daemon's
//     refresh for the next share.
//  2. startFresh, used only for a foreground share about to start, asks
//     for the fresh answer in the background; it is announced only if it
//     arrives while the share is running. A command that finishes first
//     simply does not print it, and a background share never asks.
//
// The notice is printed from the command's own goroutine, never
// interleaved with other output, and only after the share block: a share
// that fails says what failed and nothing else.
type updateNotice struct {
	C      chan *output.Update
	done   chan struct{}
	cancel context.CancelFunc

	d       Deps
	sess    share.Session
	channel string
	update  func(*share.Release) *output.Update
	known   *output.Update
	shown   bool
}

func newUpdateNotice(ctx context.Context, d Deps, sess share.Session, info buildinfo.Info) *updateNotice {
	n := &updateNotice{C: make(chan *output.Update, 1), done: make(chan struct{}), cancel: func() {}, d: d, sess: sess}
	close(n.done)
	if !updatecheck.Comparable(info) || (d.Getenv != nil && updatecheck.Disabled(d.Getenv)) {
		n.shown = true // nothing to compare against, or not wanted; never ask
		return n
	}
	n.channel = updatecheck.Channel(info)
	n.update = func(rel *share.Release) *output.Update {
		if rel == nil {
			return nil
		}
		latest := &ipc.ReleaseInfo{Version: rel.Version, Tag: rel.Tag, Prerelease: rel.Prerelease, URL: rel.URL}
		if !updatecheck.Newer(info, latest) {
			return nil
		}
		advice := d.UpdateAdvice(info)
		return &output.Update{Latest: rel.Version, Current: info.Version, Command: advice.Command, Page: advice.Link}
	}
	cctx, ccancel := clock.WithTimeout(ctx, d.Clock, localCallWait)
	rel, err := sess.CheckUpdate(cctx, n.channel, 0)
	ccancel()
	if err == nil {
		n.known = n.update(rel)
	}
	return n
}

// cached is the answer the daemon already had when the command started.
func (n *updateNotice) cached() *output.Update { return n.known }

// startFresh asks for the fresh answer in the background. It does nothing
// when the daemon already had one.
func (n *updateNotice) startFresh(ctx context.Context) {
	if n.shown || n.known != nil {
		return
	}
	n.done = make(chan struct{})
	uctx, cancel := clock.WithTimeout(ctx, n.d.Clock, updateNoticeWait+time.Second)
	n.cancel = cancel
	go func() {
		defer close(n.done)
		rel, err := n.sess.CheckUpdate(uctx, n.channel, updateNoticeWait)
		if err != nil {
			return
		}
		if u := n.update(rel); u != nil {
			n.C <- u
		}
	}()
}

// stop cancels a check still in flight so nothing is printed after the
// command returned.
func (n *updateNotice) stop() {
	n.cancel()
	<-n.done
}

// limitReached names the account limit that refused a share, never its size:
// limits differ by deployment and can be raised for one account.
func limitReached(limit string) *problem {
	switch api.Limit(limit) {
	case api.LimitRunningShares:
		return fail("You've reached your limit of running shares").remedy("stop one with ", bright("purlview stop"), ", then share again")
	case api.LimitSharesPerHour:
		return fail("You've reached your limit of new shares this hour").remedy("try again later")
	default:
		return fail("You've reached a Purlview limit").remedy("try again later")
	}
}

// updateRequired is a share the platform refused because it no longer
// serves this version: how to update comes from how this copy was
// installed, as in the update notice.
func updateRequired(advice updatecheck.Advice) *problem {
	pr := fail("This version of Purlview is no longer supported")
	if advice.Command != "" {
		return pr.remedy("update with ", bright(advice.Command), ", then share again")
	}
	return pr.remedy("download the latest from ", output.URL(advice.Link))
}
