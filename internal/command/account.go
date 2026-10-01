package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/account"
	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"

	"github.com/idyl-labs/purlview/sdk/api"
)

// maxResends is how many new codes one sign-in may ask for.
const maxResends = 2

// loadCredential returns the remembered credential or a signed-out error.
func loadCredential(d Deps) (*api.InstallationCredential, error) {
	cred, err := d.Store.Load()
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, notSignedIn()
	}
	return cred, nil
}

func newLogin(p *output.Printer, d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Sign in with an email code",
		Long: `Sign in with a 6-digit code sent to your email address.

The code is typed as hidden text; spaces and dashes in it are ignored. Type
resend at the code prompt for a new code. The first sign-in creates your
account. Ctrl-C cancels. Without a terminal, the email address and the code
are read as two lines of standard input.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return runLogin(cmd.Context(), p, d) },
	}
}

func runLogin(ctx context.Context, p *output.Printer, d Deps) error {
	cred, err := d.Store.Load()
	if err != nil {
		return err
	}
	if cred != nil {
		// Repeated login: confirm the current identity rather than signing
		// this device in twice. Only a rejected sign-in starts over.
		vctx, cancel := clock.WithTimeout(ctx, d.Clock, validateWait)
		_, verr := d.Auth.Validate(vctx, cred)
		cancel()
		switch {
		case verr == nil:
			p.Status(signedInAs(cred.Account))
			return nil
		case unauthorised(verr):
			p.Status(output.Msg(output.Attend, "This device was signed out").Remedy("sign in again"))
		case errors.Is(verr, account.ErrNotImplemented):
			return notBuilt("Signing in", verr)
		default:
			return unreachable(verr)
		}
	}
	_, err = signIn(ctx, p, d)
	return err
}

func signedInAs(email string) output.Message {
	return output.Msg(output.Done, "Signed in as ", output.Name(email))
}

// ask prints a prompt and reads its answer. A terminal echoes what is typed
// and the Enter that ends it, except for hidden input; whenever it did not,
// the prompt line is ended here. Ctrl-C ends the line and nothing more.
func ask(ctx context.Context, p *output.Printer, d Deps, label string, hidden bool) (string, error) {
	p.Prompt(label)
	read := d.ReadLine
	if hidden {
		read = d.ReadSecret
	}
	value, err := read(ctx)
	if hidden || !d.Terminal.Interactive || err != nil {
		p.PromptEnd()
	}
	return value, err
}

// signIn runs the sign-in block and remembers the credential. It is shared
// by login and by a first share at a terminal.
func signIn(ctx context.Context, p *output.Printer, d Deps) (*api.InstallationCredential, error) {
	emailAuth, ok := d.Auth.(account.EmailAuthorizer)
	if !ok {
		return nil, notBuilt("Signing in", account.ErrNotImplemented)
	}
	unfinished := func(err error) error {
		if ctx.Err() != nil {
			return exit(ExitInterrupted)
		}
		return fail("Sign-in was not finished").remedy("run ", bright("purlview login"), " again").because(err)
	}
	p.SignIn()
	typed, err := ask(ctx, p, d, "Email:", false)
	if err != nil {
		return nil, unfinished(err)
	}
	email, err := share.ParseRecipient(typed)
	if err != nil {
		return nil, invalidEmail(typed)
	}
	challenge, err := emailAuth.StartLogin(ctx, api.LoginStartRequest{Email: email, DeviceLabel: d.Hostname()})
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, exit(ExitInterrupted)
	case errors.Is(err, account.ErrNotImplemented):
		return nil, notBuilt("Signing in", err)
	case account.IsKind(err, account.KindDenied):
		return nil, fail("Too many codes were sent to ", output.Name(email)).remedy("try again later").because(err)
	default:
		return nil, unreachable(err)
	}
	p.Note("Code sent. Check your inbox.")

	wait := min(challenge.ExpiresAt.Sub(d.Clock.Now()), loginWait)
	wctx, cancel := clock.WithTimeout(ctx, d.Clock, wait)
	defer cancel()
	expired := &problem{msg: codeExpired(), code: ExitError}
	resendAt, resends := challenge.ResendAt, 0
	var cred *api.InstallationCredential
	for cred == nil {
		value, err := ask(wctx, p, d, "Code:", true)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return nil, exit(ExitInterrupted)
		case wctx.Err() != nil:
			return nil, expired
		default:
			return nil, unfinished(err)
		}
		if strings.EqualFold(value, "resend") {
			now := d.Clock.Now()
			switch {
			case resends >= maxResends:
				p.Status(output.Msg(output.Attend, "No more codes for this sign-in").Remedy("run ", bright("purlview login"), " to start over"))
			case now.Before(resendAt):
				p.Status(output.Msg(output.Attend, fmt.Sprintf("Wait %ds before asking for another code", seconds(resendAt.Sub(now)))))
			default:
				next, rerr := emailAuth.ResendLogin(wctx, api.LoginResendRequest{ID: challenge.ID})
				switch {
				case rerr == nil:
					resendAt, resends = next.ResendAt, resends+1
					p.Note("Code sent. Check your inbox.")
				case ctx.Err() != nil:
					return nil, exit(ExitInterrupted)
				case wctx.Err() != nil || account.IsKind(rerr, account.KindExpired):
					return nil, expired.because(rerr)
				case account.IsKind(rerr, account.KindDenied):
					p.Status(output.Msg(output.Attend, "No more codes for this sign-in").Remedy("run ", bright("purlview login"), " to start over"))
				default:
					return nil, unreachable(rerr)
				}
			}
			continue
		}
		code := api.NormaliseLoginCode(value)
		if len(code) != api.LoginCodeDigits || strings.Trim(code, "0123456789") != "" {
			p.Status(output.Msg(output.Failure, "That code didn't match").Remedy(fmt.Sprintf("codes have %d digits", api.LoginCodeDigits)))
			continue
		}
		// Only a verified code is a credential: a refusal may come with a
		// non-nil, empty one.
		got, err := emailAuth.VerifyLogin(wctx, api.LoginVerifyRequest{ID: challenge.ID, Code: code})
		switch {
		case err == nil:
			cred = got
		case ctx.Err() != nil:
			return nil, exit(ExitInterrupted)
		case wctx.Err() != nil || account.IsKind(err, account.KindExpired):
			return nil, expired.because(err)
		case account.IsKind(err, account.KindDenied):
			miss := output.Msg(output.Failure, "That code didn't match")
			if left := account.AttemptsLeft(err); left > 0 {
				miss = miss.Remedy(output.Count(left, "attempt") + " left")
			}
			p.Status(miss)
		case errors.Is(err, account.ErrNotImplemented):
			return nil, notBuilt("Signing in", err)
		default:
			return nil, unreachable(err)
		}
	}

	if err := d.Store.Save(cred); err != nil {
		// Purlview signed this device in, but the device cannot keep it. Sign
		// it out again and say what is true now: a device left signed in on
		// Purlview must be named, never assumed gone.
		rctx, rcancel := clock.WithTimeout(context.Background(), d.Clock, remoteCallWait)
		rerr := d.Auth.Revoke(rctx, cred)
		rcancel()
		if rerr == nil {
			return nil, fail("Couldn't save your sign-in on this device").remedy("nothing stays signed in").because(err)
		}
		return nil, fail("Couldn't save your sign-in on this device").remedy("Purlview still lists it; sign it out with ", bright("purlview devices")).because(errors.Join(err, rerr))
	}
	p.Blank()
	p.Status(signedInAs(cred.Account))
	return cred, nil
}

// seconds rounds a wait up, so "Wait 0s" is never said.
func seconds(d time.Duration) int { return int((d + time.Second - 1) / time.Second) }

func invalidEmail(typed string) *problem {
	if typed == "" {
		return fail("An email address is needed to sign in")
	}
	return fail(typed + " isn't an email address")
}

func newLogout(p *output.Printer, d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out on this device",
		Long: `Sign out on this device. It works offline.

Running shares continue until they are stopped or expire; a new share asks
you to sign in again. To sign out another device, and stop its shares, use
'purlview devices signout'.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			if err := d.Store.Clear(); err != nil {
				return fail("Couldn't remove the sign-in from this device").because(err)
			}
			p.Status(signedOutHere())
			return nil
		},
	}
}

func newWhoami(p *output.Printer, d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the signed-in account and this device",
		Long: `Show the account signed in on this device, the device's name and whether
Purlview confirmed the sign-in just now. Nothing secret is shown.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return runWhoami(cmd.Context(), p, d) },
	}
}

func runWhoami(ctx context.Context, p *output.Printer, d Deps) error {
	cred, err := loadCredential(d)
	if err != nil {
		return err
	}
	vctx, cancel := clock.WithTimeout(ctx, d.Clock, validateWait)
	_, verr := d.Auth.Validate(vctx, cred)
	cancel()
	id := output.Identity{Account: cred.Account, Device: cred.DeviceLabel, Status: output.Working("Signed in")}
	switch {
	case verr == nil:
	case unauthorised(verr):
		id.Status = output.Failed("Signed out from another device")
		p.Whoami(id)
		p.Hint("purlview login")
		return exit(ExitError)
	default:
		id.Status = output.Attention("Signed in (not checked, offline)")
	}
	p.Whoami(id)
	return nil
}
