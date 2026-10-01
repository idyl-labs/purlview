package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// EndpointKind names the transport for documentation and status output.
const EndpointKind = "named pipe"

// Listen creates the daemon's named pipe. The pipe's security descriptor
// names the current user as owner and grants access to that user only; the
// pipe rejects remote (network) clients. The pipe namespace is machine-wide,
// so the pipe name must already be unique per user (see daemon.Paths).
func Listen(pipe string) (net.Listener, error) {
	sid, err := CurrentUserSID()
	if err != nil {
		return nil, err
	}
	// O: owner, G: primary group, D:P protected DACL with one allow ACE
	// granting generic all to the owner SID. No inherited or default
	// entries apply. Administrators can still take ownership, as with any
	// object; that is outside the threat model of same-user privacy.
	sddl := fmt.Sprintf("O:%sG:%sD:P(A;;GA;;;%s)", sid, sid, sid)
	return winio.ListenPipe(pipe, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    MaxMessageSize,
		OutputBufferSize:   MaxMessageSize,
	})
}

// Dial connects to the daemon's pipe and verifies that the pipe server is
// owned by the current user, so another local account that squatted on the
// name cannot impersonate the daemon.
func Dial(ctx context.Context, pipe string) (net.Conn, error) {
	c, err := winio.DialPipeContext(ctx, pipe)
	if err != nil {
		return nil, err
	}
	if err := verifyPipeOwner(c); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func verifyPipeOwner(c net.Conn) error {
	fder, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("pipe connection does not expose its handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fder.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read pipe owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read pipe owner: %w", err)
	}
	me, err := CurrentUserSID()
	if err != nil {
		return err
	}
	if owner.String() != me {
		return fmt.Errorf("pipe is owned by %s, not the current user (%s)", owner.String(), me)
	}
	return nil
}

// CurrentUserSID returns the SID of the user running this process.
func CurrentUserSID() (string, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return "", fmt.Errorf("open process token: %w", err)
	}
	defer func() { _ = tok.Close() }()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read token user: %w", err)
	}
	return u.User.Sid.String(), nil
}

// CheckPrivateDir is a no-op on Windows: the per-user directories under
// %LOCALAPPDATA% are private by their inherited ACL, and the pipe carries its
// own explicit descriptor.
func CheckPrivateDir(string) error { return nil }
