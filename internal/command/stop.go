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

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"
)

// newStop builds stop and its hidden alias unshare.
func newStop(p *output.Printer, d Deps, name string, hidden bool) *cobra.Command {
	return &cobra.Command{
		Use:    name + " <id or link>",
		Short:  "Stop a share",
		Hidden: hidden,
		Long: `Stop one of your shares, on this device or on any other device of yours.

Give the share's id from 'purlview list', its link, or the address of a page
you opened through it. Purlview stops the share itself, so this works even
when the device that shares it is offline. The command reports success only
once Purlview has confirmed the stop.`,
		Example: "  purlview " + name + " k7m2p4qx\n  purlview " + name + " https://k7m2p4qx.purlview.invalid/",
		Args: oneArg(
			misuse(name+" needs a share id or link").remedy("see ", bright("purlview list")),
			misuse(name+" takes one share at a time").remedy("see ", bright("purlview list")),
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := share.ParseRef(args[0])
			if err != nil {
				return invalid(err)
			}
			return runStop(cmd.Context(), p, d, ref)
		},
	}
}

func noShare(ref share.Ref, cause error) *problem {
	return fail("No share ", output.Name(ref.String()), " in your account").remedy("see ", bright("purlview list")).because(cause)
}

func runStop(ctx context.Context, p *output.Printer, d Deps, ref share.Ref) error {
	cred, err := loadCredential(d)
	if err != nil {
		return err
	}
	rctx, cancel := clock.WithTimeout(ctx, d.Clock, remoteCallWait)
	res, err := d.Directory.Revoke(rctx, *cred, ref)
	cancel()
	if err != nil {
		switch {
		case errors.Is(err, share.ErrNotImplemented):
			return notBuilt("Stopping shares", err)
		case share.IsKind(err, share.KindNotFound):
			return noShare(ref, err)
		case unauthorised(err):
			return signedOutElsewhere(err)
		case share.IsKind(err, share.KindUncertain):
			return fail("Couldn't confirm ", output.Name(ref.String()), " stopped").remedy("try again, or see ", bright("purlview list")).because(err)
		}
		// Purlview is unreachable. A share of this device can at least stop
		// here; say exactly what that does and does not mean.
		if local, stopped := stopLocally(ctx, d, ref); stopped {
			p.Status(stoppedHere(output.Clock(local.ExpiresAt.In(d.Local))))
			return exit(ExitError)
		}
		return unreachable(err)
	}
	id := share.Label(res.Share.ID)
	if res.AlreadyEnded {
		p.Status(alreadyStopped(id))
		return nil
	}
	p.Status(stoppedShare(id))
	return nil
}

// stopLocally stops a share on this device's daemon when Purlview is
// unreachable. It returns the share and whether it was active here.
func stopLocally(ctx context.Context, d Deps, ref share.Ref) (share.Share, bool) {
	sess, err := d.Runner.Open(ctx, false)
	if err != nil {
		return share.Share{}, false
	}
	defer func() { _ = sess.Close() }()
	var target share.Share
	found := false
	shares, err := sess.Shares(ctx)
	if err != nil {
		return share.Share{}, false
	}
	for _, sh := range shares {
		if sh.Active() && (sh.ID == ref.ID || ref.URL != "" && sh.Origin == ref.URL) {
			target, found = sh, true
			break
		}
	}
	if !found {
		return share.Share{}, false
	}
	sctx, cancel := clock.WithTimeout(ctx, d.Clock, stopWait)
	defer cancel()
	res, err := sess.Stop(sctx, share.StopRequest{ID: target.ID, Reason: share.ReasonStopped})
	if err != nil {
		return target, false
	}
	return res.Share, !res.AlreadyEnded
}
