//go:build windows

package testfs

import (
	"slices"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// ACL is what a test needs to know about one object's access list.
type ACL struct {
	// Protected is true when the list does not inherit from the parent.
	Protected bool
	// Allowed holds the SID of every allow entry.
	Allowed []*windows.SID
	// Other counts the entries that are not allow entries.
	Other int
	// Flags holds the inheritance flags of every allow entry, in the order
	// of Allowed.
	Flags []byte
}

// ReadACL reads the access list of path.
func ReadACL(tb testing.TB, path string) ACL {
	tb.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(tb, err)
	return aclOf(tb, sd, path)
}

// ReadHandleACL reads the access list of the open file or directory h, which
// must have been opened with READ_CONTROL access.
func ReadHandleACL(tb testing.TB, h windows.Handle) ACL {
	tb.Helper()
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(tb, err)
	return aclOf(tb, sd, "the handle")
}

// aclOf reads the DACL of sd; what names the object in a failure message.
func aclOf(tb testing.TB, sd *windows.SECURITY_DESCRIPTOR, what string) ACL {
	tb.Helper()
	control, _, err := sd.Control()
	require.NoError(tb, err)
	dacl, _, err := sd.DACL()
	require.NoError(tb, err)
	require.NotNil(tb, dacl, "%s has no DACL", what)
	out := ACL{Protected: control&windows.SE_DACL_PROTECTED != 0}
	for i := range dacl.AceCount {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(tb, windows.GetAce(dacl, uint32(i), &ace))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			out.Other++
			continue
		}
		out.Allowed = append(out.Allowed, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
		out.Flags = append(out.Flags, ace.Header.AceFlags)
	}
	return out
}

// CurrentUser returns the SID of the user the test runs as.
func CurrentUser(tb testing.TB) *windows.SID {
	tb.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(tb, err)
	return user.User.Sid
}

// RequireOnlyCurrentUser fails the test unless the access list has at least
// one allow entry, every allow entry is for the current user and there is no
// other kind of entry. Windows can record an inherited grant as more than one
// entry for the same user. what names the object in the failure message.
func (a ACL) RequireOnlyCurrentUser(tb testing.TB, what string) {
	tb.Helper()
	require.Zero(tb, a.Other, "%s must grant nothing to anyone else", what)
	require.NotEmpty(tb, a.Allowed, "%s must inherit the owner entry", what)
	user := CurrentUser(tb)
	for _, sid := range a.Allowed {
		require.True(tb, sid.Equals(user), "%s grants an entry to %s", what, sid)
	}
}

func requireOwnerOnlyACL(tb testing.TB, path string) {
	tb.Helper()
	ReadACL(tb, path).RequireOnlyCurrentUser(tb, path)
}

// GrantEveryoneRead replaces the access list of the directory dir with a
// protected list of two entries, each of which passes to the files and
// directories created in dir afterwards: full control for the current user
// and read for Everyone. A file created in dir without an access list of its
// own then holds an entry for Everyone, as a file does in a directory that
// grants another local user access. Windows can record one inheritable grant
// as more than one entry on the files that inherit it.
func GrantEveryoneRead(tb testing.TB, dir string) {
	tb.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(tb, err)
	entry := func(sid *windows.SID, rights windows.ACCESS_MASK) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: rights,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		entry(CurrentUser(tb), windows.GENERIC_ALL),
		entry(everyone, windows.GENERIC_READ),
	}, nil)
	require.NoError(tb, err)
	require.NoError(tb, windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil))
}

// HasEntryForAnotherUser reports whether the access list has an entry that
// is not an allow entry for the current user.
func (a ACL) HasEntryForAnotherUser(tb testing.TB) bool {
	tb.Helper()
	if a.Other > 0 {
		return true
	}
	user := CurrentUser(tb)
	return slices.ContainsFunc(a.Allowed, func(sid *windows.SID) bool { return !sid.Equals(user) })
}
