//go:build windows

package atrest

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// hardenKeyFile rewrites the file's DACL so only the current user's
// SID has access. The default on a newly-created file under
// %USERPROFILE% is "inherit from parent", which on most desktop
// installs is already restrictive but on shared / domain-joined
// machines may include "Authenticated Users". This function pins
// the file to "only me, no inheritance" regardless of where the
// home directory's ACL came from.
//
// Reach goal: bring the Windows perms claim ("0600 on disk") into
// actual line with what Linux/macOS deliver. The user explicitly
// flagged the lack of this as the at-rest-encryption story's
// weakest link on Windows.
//
// Failure modes (running under a service account without
// SeRestorePrivilege, weird mounted filesystems that ignore ACLs)
// are reported but don't kill the key-creation -- the user still
// gets an encrypted at-rest file, just with whatever inherited ACL
// the parent dir had. Caller logs the message; the file is still
// usable.
func hardenKeyFile(path string) error {
	// Resolve the current user's SID. We use Token().User().Sid via
	// CurrentProcessToken so it picks up the impersonation SID
	// rather than the primary SID when relevant.
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("get token user: %v", err)
	}
	sid := user.User.Sid

	// Build an EXPLICIT_ACCESS that grants FILE_ALL_ACCESS to this
	// SID and inherits to nothing (the file has no children).
	ea := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{ea}, nil)
	if err != nil {
		return fmt.Errorf("acl from entries: %v", err)
	}

	// Apply the DACL with PROTECTED_DACL set so inheritance from the
	// parent is severed. Without PROTECTED_DACL we'd add our entry
	// on top of whatever the parent grants, which doesn't actually
	// tighten access.
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("set named security info: %v", err)
	}
	return nil
}
