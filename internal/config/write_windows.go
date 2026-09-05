//go:build windows

package config

import (
	"golang.org/x/sys/windows"
)

// hardenACL replaces path's DACL with a single owner-only (current user,
// full control) entry, with inheritance BLOCKED
// (PROTECTED_DACL_SECURITY_INFORMATION): without the PROTECTED flag the
// parent directory's inherited ACEs would remain and the restriction
// would be cosmetic. Called by atomicWriteFile on the temp file BEFORE
// any data is written and before the rename - an intra-volume
// MoveFileEx retains the security descriptor, so the final file carries
// this DACL with no exposure window. This is the Windows counterpart of
// the Unix 0600 chmod, which NTFS ignores.
func hardenACL(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	ea := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	dacl, err := windows.ACLFromEntries(ea, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}
