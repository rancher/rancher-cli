//go:build windows

package testfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// grantProtected replaces the access list of path with one allow entry for
// each SID, blocking inheritance from the parent so that no inherited entry
// is left.
func grantProtected(t *testing.T, path string, sids ...*windows.SID) {
	t.Helper()
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_READ,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil))
}

func ownerOnlyFile(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	grantProtected(t, file, CurrentUser(t))
	return file
}

// TestReadACLReportsAProtectedOwnerOnlyList asserts the reader returns a
// protected list with one allow entry, for the current user, and no other
// kind of entry.
func TestReadACLReportsAProtectedOwnerOnlyList(t *testing.T) {
	got := ReadACL(t, ownerOnlyFile(t))
	assert.True(t, got.Protected)
	assert.Zero(t, got.Other)
	require.Len(t, got.Allowed, 1)
	assert.True(t, got.Allowed[0].Equals(CurrentUser(t)))
	assert.Len(t, got.Flags, 1)
}

// TestRequireOwnerOnlyAcceptsAnOwnerOnlyFile asserts the Windows branch
// passes a file whose list holds only the current user.
func TestRequireOwnerOnlyAcceptsAnOwnerOnlyFile(t *testing.T) {
	file := ownerOnlyFile(t)
	r := run(t, func(tb testing.TB) { tb.Helper(); RequireOwnerOnly(tb, file) })
	assert.False(t, r.failed, r.text())
}

// TestRequireOwnerOnlyRefusesAnEntryForAnotherUser asserts the Windows
// branch fails a file that also grants read to Everyone.
func TestRequireOwnerOnlyRefusesAnEntryForAnotherUser(t *testing.T) {
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	file := ownerOnlyFile(t)
	grantProtected(t, file, CurrentUser(t), everyone)
	r := run(t, func(tb testing.TB) { tb.Helper(); RequireOwnerOnly(tb, file) })
	assert.True(t, r.failed)
	assert.Contains(t, r.text(), "grants an entry to")
}

// TestRequireOnlyCurrentUserAcceptsTwoEntriesForOneUser asserts a list that
// records one grant as two entries for the current user passes.
func TestRequireOnlyCurrentUserAcceptsTwoEntriesForOneUser(t *testing.T) {
	user := CurrentUser(t)
	acl := ACL{Allowed: []*windows.SID{user, user}, Flags: []byte{0, 0}}
	r := run(t, func(tb testing.TB) { tb.Helper(); acl.RequireOnlyCurrentUser(tb, "a file") })
	assert.False(t, r.failed, r.text())
}

// TestRequireOnlyCurrentUserRefusesAnEmptyListAndOtherEntries asserts a list
// with no allow entry fails and so does one with an entry of another type.
func TestRequireOnlyCurrentUserRefusesAnEmptyListAndOtherEntries(t *testing.T) {
	user := CurrentUser(t)
	for name, acl := range map[string]ACL{
		"no entry":     {},
		"another type": {Allowed: []*windows.SID{user}, Other: 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := run(t, func(tb testing.TB) { tb.Helper(); acl.RequireOnlyCurrentUser(tb, "a file") })
			assert.True(t, r.failed)
		})
	}
}
