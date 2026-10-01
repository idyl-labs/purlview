package command

import (
	"context"
	"errors"
	"unicode"
	"unicode/utf8"

	"github.com/idyl-labs/purlview/internal/account"
	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"
)

// problem is a failed command as data: the one line to print, optional hints
// below it, the underlying cause (shown only with PURLVIEW_DEBUG=1) and the
// exit status. Run prints it; handlers only return it.
type problem struct {
	msg   output.Message
	hints []string
	cause error
	code  int
	usage bool
	// said: the line was already printed as a session event, or there is
	// nothing to say (Ctrl-C at a prompt); only the exit status remains.
	said bool
}

func (p *problem) Error() string { return p.msg.String() }
func (p *problem) Unwrap() error { return p.cause }

// fail is an operational failure (exit 1): "✗ problem".
func fail(parts ...any) *problem {
	return &problem{msg: output.Msg(output.Failure, parts...), code: ExitError}
}

// misuse is a usage error (exit 2); Run adds the pointer to --help.
func misuse(parts ...any) *problem {
	return &problem{msg: output.Msg(output.Failure, parts...), code: ExitUsage, usage: true}
}

// exit carries only an exit status: everything was already said.
func exit(code int) *problem { return &problem{code: code, said: true} }

func (p *problem) remedy(parts ...any) *problem  { p.msg = p.msg.Remedy(parts...); return p }
func (p *problem) because(err error) *problem    { p.cause = err; return p }
func (p *problem) then(hints ...string) *problem { p.hints = hints; return p }

// bright marks the command inside a remedy; it stays bright while the words
// around it are dim.
func bright(command string) output.Span { return output.Text(command) }

// The lines several commands share.

func notSignedIn() *problem {
	return fail("Not signed in").remedy("run ", bright("purlview login"), " first")
}

func unreachable(cause error) *problem {
	return fail("Couldn't reach Purlview").remedy("check your connection and try again").because(cause)
}

func deviceSignedOut() output.Message {
	return output.Msg(output.Failure, "This device was signed out").Remedy("run ", bright("purlview login"))
}

func signedOutElsewhere(cause error) *problem {
	return &problem{msg: deviceSignedOut(), code: ExitError, cause: cause}
}

// notBuilt reports a build without a platform to talk to. A released
// product always has one, so these words are only ever read by developers.
func notBuilt(what string, cause error) *problem {
	return fail(what + " is not implemented in this scaffold build").because(cause)
}

// unexpected renders an error no handler classified. Its text is all there
// is to say, so it is shown as it is.
func unexpected(err error) *problem { return fail(sentence(err.Error())) }

// sentence capitalises the first letter, as every line people read starts.
func sentence(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// offline reports whether err means Purlview could not be asked at all.
func offline(err error) bool {
	return account.IsKind(err, account.KindUnavailable) || share.IsKind(err, share.KindUnavailable) ||
		errors.Is(err, context.DeadlineExceeded)
}

// unauthorised reports whether Purlview rejected this device's sign-in.
func unauthorised(err error) bool {
	return share.IsKind(err, share.KindUnauthorised) || account.IsKind(err, account.KindUnauthorised)
}
