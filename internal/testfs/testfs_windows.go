//go:build windows

package testfs

import (
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
	control, _, err := sd.Control()
	require.NoError(tb, err)
	dacl, _, err := sd.DACL()
	require.NoError(tb, err)
	require.NotNil(tb, dacl, "%s has no DACL", path)
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
