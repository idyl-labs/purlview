package account

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func privateFile(path string, fix bool) error {
	var token windows.Token
	if e := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); e != nil {
		return e
	}
	defer func() { _ = token.Close() }()
	user, e := token.GetTokenUser()
	if e != nil {
		return e
	}
	sid := user.User.Sid
	if fix {
		sd, e := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)", sid.String(), sid.String()))
		if e != nil {
			return e
		}
		acl, _, e := sd.DACL()
		if e != nil {
			return e
		}
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sid, nil, acl, nil)
	}
	sd, e := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if e != nil {
		return e
	}
	owner, _, e := sd.Owner()
	if e != nil || !owner.Equals(sid) {
		return errors.New("credential owner is not current user")
	}
	// Reapply a protected owner-only ACL before reading; inherited broad ACLs
	// cannot remain in use as a supported credential store.
	return privateFile(path, true)
}
