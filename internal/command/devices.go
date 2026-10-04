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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/account"
	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/output"

	"github.com/idyl-labs/purlview/sdk/api"
)

// newDevices builds devices and devices signout. The API calls a device an
// installation; nothing a person reads does.
func newDevices(p *output.Printer, d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List the devices signed in to your account",
		Long: `List the devices signed in to your account: when each signed in, when it
was last used and how many shares it runs. A dot marks this device.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return runDevices(cmd.Context(), p, d) },
	}
	var yes bool
	signout := &cobra.Command{
		Use:   "signout <name>",
		Short: "Sign out a device and stop its shares",
		Long: `Sign out one of your other devices. Its shares stop, and it has to sign in
again before it can share. To sign out this device, use 'purlview logout'.

When two devices have the same name, give the id this command then shows.`,
		Example: "  purlview devices signout laptop\n  purlview devices signout laptop --yes",
		Args: oneArg(
			misuse("signout needs a device name").remedy("see ", bright("purlview devices")),
			misuse("signout takes one device at a time").remedy("names with spaces need quotes"),
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSignout(cmd.Context(), p, d, args[0], yes)
		},
	}
	signout.Flags().BoolVar(&yes, "yes", false, "Sign out without asking.")
	cmd.AddCommand(signout)
	return cmd
}

// installations lists the account's devices, with the shared failures
// already turned into their lines.
func installations(ctx context.Context, d Deps) (account.Installations, *api.InstallationCredential, []api.Installation, error) {
	cred, err := loadCredential(d)
	if err != nil {
		return nil, nil, nil, err
	}
	devices, ok := d.Auth.(account.Installations)
	if !ok {
		return nil, nil, nil, notBuilt("Listing devices", account.ErrNotImplemented)
	}
	lctx, cancel := clock.WithTimeout(ctx, d.Clock, remoteCallWait)
	defer cancel()
	list, err := devices.ListInstallations(lctx, cred)
	switch {
	case err == nil:
		return devices, cred, list, nil
	case errors.Is(err, account.ErrNotImplemented):
		return nil, nil, nil, notBuilt("Listing devices", err)
	case unauthorised(err):
		return nil, nil, nil, signedOutElsewhere(err)
	default:
		return nil, nil, nil, unreachable(err)
	}
}

func runDevices(ctx context.Context, p *output.Printer, d Deps) error {
	_, _, list, err := installations(ctx, d)
	if err != nil {
		return err
	}
	p.Table(deviceTable(d, list, false))
	return nil
}

// deviceTable lists devices by name, or by id when names cannot tell them
// apart.
func deviceTable(d Deps, list []api.Installation, byID bool) *output.Table {
	first := output.Col{Header: "NAME"}
	if byID {
		first.Header = "ID"
	}
	t := output.NewTable(first, output.Col{Header: "SIGNED IN"}, output.Col{Header: "LAST USED"}, output.Col{Header: "SHARES", Right: true})
	now := d.Clock.Now().In(d.Local)
	for _, v := range list {
		name := v.Label
		if byID {
			name = v.ID
		}
		t.Row(v.Current, output.Name(name), output.Text(day(now, v.CreatedAt.In(d.Local), false)), output.Text(day(now, v.LastUsedAt.In(d.Local), true)), output.Text(strconv.Itoa(v.ActiveShares)))
	}
	return t
}

// day renders a date as "Sep 19", with the year when it is not this one. A
// last use is "today" when it was, and "never" when there was none.
func day(now, t time.Time, lastUse bool) string {
	switch {
	case t.IsZero() || t.Year() < 2000:
		return "never"
	case lastUse && t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return "today"
	case t.Year() != now.Year():
		return t.Format("Jan 2 2006")
	}
	return t.Format("Jan 2")
}

func runSignout(ctx context.Context, p *output.Printer, d Deps, name string, yes bool) error {
	devices, cred, list, err := installations(ctx, d)
	if err != nil {
		return err
	}
	target, err := pickDevice(p, d, list, name)
	if err != nil {
		return err
	}
	if target.Current || target.ID == cred.Device {
		return fail(output.Name(target.Label), " is this device").then("purlview logout")
	}
	if !yes {
		if !d.Terminal.Interactive {
			return fail("Signing out ", output.Name(target.Label), " needs a yes").remedy("add ", bright("--yes"))
		}
		question := "Sign out " + target.Label + "?"
		if target.ActiveShares > 0 {
			question += fmt.Sprintf(" Its %s will stop.", output.Count(target.ActiveShares, "share"))
		}
		p.Confirm(question)
		answer, rerr := d.ReadLine(ctx)
		if rerr != nil {
			p.PromptEnd()
			if ctx.Err() != nil {
				return exit(ExitInterrupted)
			}
			return fail("Signing out ", output.Name(target.Label), " needs a yes").remedy("add ", bright("--yes")).because(rerr)
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return nil
		}
	}
	rctx, cancel := clock.WithTimeout(ctx, d.Clock, remoteCallWait)
	defer cancel()
	_, err = devices.RevokeInstallation(rctx, cred, target.ID)
	switch {
	case err == nil:
		p.Status(output.Msg(output.Done, "Signed out ", output.Name(target.Label)))
		return nil
	case account.IsKind(err, account.KindNotFound):
		return noDevice(target.Label, err)
	case unauthorised(err):
		return signedOutElsewhere(err)
	case offline(err):
		return unreachable(err)
	default:
		return fail("Couldn't confirm ", output.Name(target.Label), " was signed out").remedy("try again, or see ", bright("purlview devices")).because(err)
	}
}

func noDevice(name string, cause error) *problem {
	return fail("No device ", output.Name(name), " in your account").remedy("see ", bright("purlview devices")).because(cause)
}

// pickDevice finds the device a person named. A name that several devices
// share is refused, with the ids that tell them apart; an id is accepted in
// its place.
func pickDevice(p *output.Printer, d Deps, list []api.Installation, name string) (api.Installation, error) {
	var named []api.Installation
	for _, v := range list {
		if v.ID == name {
			return v, nil
		}
		if v.Label == name {
			named = append(named, v)
		}
	}
	if len(named) == 0 {
		for _, v := range list {
			if strings.EqualFold(v.Label, name) {
				named = append(named, v)
			}
		}
	}
	switch len(named) {
	case 0:
		return api.Installation{}, noDevice(name, nil)
	case 1:
		return named[0], nil
	}
	p.Status(output.Msg(output.Failure, fmt.Sprintf("%d devices are named ", len(named)), output.Name(name)).Remedy("sign one out by its id"))
	p.StatusTable(deviceTable(d, named, true))
	return api.Installation{}, exit(ExitError)
}
