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
	"fmt"

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"

	"github.com/idyl-labs/purlview/sdk/api"
)

func newList(p *output.Printer, d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your active shares",
		Long: `List the active shares of your account, on all of your devices.

Each row shows the share's id, your app (with +1 for each further app the
share names), who can open it, the device that shares it, its status
(Sharing, Reconnecting or Starting) and the time left.
A dot marks this device's rows. Links are never shown here: print one with
'purlview link <id>'. If Purlview can't be reached, only this device's shares
are listed and the command says so.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return runList(cmd.Context(), p, d) },
	}
}

func runList(ctx context.Context, p *output.Printer, d Deps) error {
	cred, err := loadCredential(d)
	if err != nil {
		return err
	}
	shares, err := accountShares(ctx, d, cred)
	if err != nil {
		var pr *problem
		if errors.As(err, &pr) {
			return pr
		}
		// The account view failed. Show what this device knows, labelled as
		// such, so a failure never reads as an empty account.
		local := localShares(ctx, d)
		if len(local) == 0 {
			return unreachable(err)
		}
		p.Status(output.Msg(output.Attend, "Showing this device only").Remedy("couldn't reach Purlview"))
		p.Table(shareTable(d, cred, local))
		return exit(ExitError)
	}
	if len(shares) == 0 {
		p.Empty("No active shares", "purlview share 3000")
		return nil
	}
	p.Table(shareTable(d, cred, shares))
	return nil
}

// accountShares is the account's active shares. A failure that has its own
// line comes back as a problem; any other error means Purlview could not be
// reached.
func accountShares(ctx context.Context, d Deps, cred *api.InstallationCredential) ([]share.Share, error) {
	lctx, cancel := clock.WithTimeout(ctx, d.Clock, remoteCallWait)
	defer cancel()
	shares, err := d.Directory.List(lctx, *cred)
	switch {
	case err == nil:
	case errors.Is(err, share.ErrNotImplemented):
		return nil, notBuilt("Listing shares", err)
	case unauthorised(err):
		return nil, signedOutElsewhere(err)
	default:
		return nil, err
	}
	active := shares[:0]
	for _, sh := range shares {
		if sh.Active() {
			active = append(active, sh)
		}
	}
	return active, nil
}

// localShares asks a running daemon for its active shares; a missing or
// unhelpful daemon yields nothing.
func localShares(ctx context.Context, d Deps) []share.Share {
	sess, err := d.Runner.Open(ctx, false)
	if err != nil {
		return nil
	}
	defer func() { _ = sess.Close() }()
	all, err := sess.Shares(ctx)
	if err != nil {
		return nil
	}
	var active []share.Share
	for _, sh := range all {
		if sh.Active() {
			active = append(active, sh)
		}
	}
	return active
}

// shareTable is the secret-free list: no link, so it is safe on a shared
// screen.
func shareTable(d Deps, cred *api.InstallationCredential, shares []share.Share) *output.Table {
	now := d.Clock.Now()
	t := output.NewTable(output.Col{Header: "ID"}, output.Col{Header: "APP"}, output.Col{Header: "ACCESS"}, output.Col{Header: "DEVICE"}, output.Col{Header: "STATUS"}, output.Col{Header: "EXPIRES"})
	for _, sh := range shares {
		mine := sh.Device == cred.Device
		device := sh.DeviceLabel
		switch {
		case mine:
			device = "this device"
		case device == "":
			device = sh.Device
		}
		access := "Anyone with the link"
		if n := len(sh.Recipients); n > 0 {
			access = sh.Recipients[0]
			if n > 1 {
				access += fmt.Sprintf(" +%d", n-1)
			}
		}
		expires := share.FormatRemaining(now, sh.ExpiresAt)
		if sh.ExpiresAt.After(now) {
			expires = "in " + expires
		}
		apps := sh.Apps()
		app := share.DisplayTarget(apps[0])
		if n := len(apps); n > 1 {
			app += fmt.Sprintf(" +%d", n-1)
		}
		t.Row(mine, output.Name(share.Label(sh.ID)), output.Text(app), output.Text(access), output.Text(device), statusWord(sh.State), output.Text(expires))
	}
	return t
}

// statusWord is the status and its colour, the same words the console uses.
func statusWord(st share.State) output.Span {
	switch st {
	case share.StateReady:
		return output.Working("Sharing")
	case share.StateReconnecting:
		return output.Attention("Reconnecting")
	case share.StateStarting:
		return output.Attention("Starting")
	default:
		return output.Text(sentence(string(st)))
	}
}
