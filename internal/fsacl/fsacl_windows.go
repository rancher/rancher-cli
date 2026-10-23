//go:build windows

package fsacl

import "golang.org/x/sys/windows"

// Harden replaces path's DACL with a single owner-only (current user, full
// control) entry, with inheritance BLOCKED
// (PROTECTED_DACL_SECURITY_INFORMATION): without the PROTECTED flag the
// parent directory's inherited ACEs would remain and the restriction
// would be cosmetic. A caller sets it on a temp file BEFORE any data is
// written and before the rename - an intra-volume MoveFileEx retains the
// security descriptor; the final file carries this DACL with no
// exposure window. This is the Windows counterpart of the Unix 0600 chmod,
// which NTFS ignores.
func Harden(path string) error {
	return set(path, windows.NO_INHERITANCE)
}

// HardenInheritable is Harden for a directory whose entries should carry
// the same restriction: the single owner-only entry is marked to pass to
// files and subdirectories created in the directory afterwards; a
// caller sets it once on the directory and creates files in it with no
// further call.
func HardenInheritable(dir string) error {
	return set(dir, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
}

func set(path string, inheritance uint32) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	ea := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
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
