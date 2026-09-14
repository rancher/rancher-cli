package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func activeRecord(t *testing.T, entry, token string) identityRecord {
	t.Helper()
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	return identityRecord{
		Version:     identityVersion,
		Entry:       entry,
		State:       identityActive,
		ID:          id,
		Fingerprint: credentialFingerprint(id, token),
	}
}

// TestIdentityFileName asserts that every entry name a user may choose
// becomes a flat hex file name: a case difference, a separator, a traversal
// and a colon all name different files, and none of them name a path.
func TestIdentityFileName(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for _, entry := range []string{"Prod", "prod", "../x", "a/b", "host:8443", ""} {
		name := identityFileName(entry)
		assert.Len(t, name, identityNameLen, "entry %q", entry)
		assert.True(t, isLowerHex(name, identityNameLen), "entry %q gave %q", entry, name)
		assert.Equal(t, name, filepath.Base(name), "entry %q gave a path", entry)
		if other, dup := seen[name]; dup {
			t.Fatalf("entries %q and %q share the file name %q", other, entry, name)
		}
		seen[name] = entry
	}
}

// TestKeychainAccount asserts the account format every raw keyring call in
// the package spells.
func TestKeychainAccount(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "0123456789abcdef0123456789abcdef:prod",
		keychainAccount("0123456789abcdef0123456789abcdef", "prod"))
	assert.Equal(t, "0123456789abcdef0123456789abcdef:host:8443",
		keychainAccount("0123456789abcdef0123456789abcdef", "host:8443"))
}

// TestNewKeychainAccountID asserts the id's width, alphabet and freshness.
func TestNewKeychainAccountID(t *testing.T) {
	t.Parallel()
	first, err := newKeychainAccountID()
	require.NoError(t, err)
	assert.True(t, isLowerHex(first, identityIDLen), "got %q", first)

	second, err := newKeychainAccountID()
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

// TestCredentialFingerprint asserts that the fingerprint is keyed by the
// account id: the same token under two ids gives two values, so a record
// copied to another identity cannot match.
func TestCredentialFingerprint(t *testing.T) {
	t.Parallel()
	const idA = "0123456789abcdef0123456789abcdef"
	const idB = "fedcba9876543210fedcba9876543210"

	got := credentialFingerprint(idA, "token-1")
	assert.True(t, isLowerHex(got, identityFingerprintLen), "got %q", got)
	assert.Equal(t, got, credentialFingerprint(idA, "token-1"))
	assert.NotEqual(t, got, credentialFingerprint(idA, "token-2"))
	assert.NotEqual(t, got, credentialFingerprint(idB, "token-1"))
	assert.NotContains(t, got, "token-1")
}

// TestIdentityRecordRoundTrip asserts that a written record reads back
// whole, including the earlier identity a pending write carries.
func TestIdentityRecordRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	rec := activeRecord(t, "prod", "token-1")
	require.NoError(t, writeIdentityRecord(dir, rec))

	got, err := readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, rec, *got)
	assert.Equal(t, rec.ID+":prod", got.Account())
	assert.Empty(t, got.PriorAccount())

	pending := rec
	pending.State = identityPending
	pending.Prior = &identityPrior{ID: rec.ID, Fingerprint: rec.Fingerprint}
	newID, err := newKeychainAccountID()
	require.NoError(t, err)
	pending.ID = newID
	pending.Fingerprint = credentialFingerprint(newID, "token-2")
	require.NoError(t, writeIdentityRecord(dir, pending))

	got, err = readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, pending, *got)
	assert.Equal(t, rec.ID+":prod", got.PriorAccount())
}

// TestReadIdentityRecordAbsent asserts that a read never creates anything:
// an absent directory and an absent file both mean "nothing was stored for
// this entry under this directory's identity".
func TestReadIdentityRecordAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	got, err := readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.NoDirExists(t, identityDirPath(dir))

	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "staging", "token-1")))
	got, err = readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.NoFileExists(t, filepath.Join(identityDirPath(dir), identityFileName("prod")))
}

// TestIdentityRecordOwnerOnly asserts the directory and its records are
// written owner-only, like every other credential file in the directory.
func TestIdentityRecordOwnerOnly(t *testing.T) {
	skipWithoutSymlinks(t)
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))

	di, err := os.Stat(identityDirPath(dir))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(identityDirPerm), di.Mode().Perm())

	fi, err := os.Stat(filepath.Join(identityDirPath(dir), identityFileName("prod")))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(identityFilePerm), fi.Mode().Perm())
}

// TestReadIdentityRecordErrorStates asserts that every state that is not a
// valid version-1 record for the entry is an error, and that the error
// leaves the file exactly as it found it.
func TestReadIdentityRecordErrorStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
		wants   string
	}{
		{name: "empty", content: "", wants: "empty"},
		{name: "unparseable", content: "\tnot: [yaml", wants: "unreadable"},
		{name: "unknown version", content: "version: 2\nentry: prod\nstate: active\nid: 0123456789abcdef0123456789abcdef\nfingerprint: " + strings.Repeat("a", 64) + "\n", wants: "version 2"},
		{name: "another entry", content: "version: 1\nentry: staging\nstate: active\nid: 0123456789abcdef0123456789abcdef\nfingerprint: " + strings.Repeat("a", 64) + "\n", wants: "a record for another entry"},
		{name: "no state", content: "version: 1\nentry: prod\nid: 0123456789abcdef0123456789abcdef\nfingerprint: " + strings.Repeat("a", 64) + "\n", wants: "in no known state"},
		{name: "malformed id", content: "version: 1\nentry: prod\nstate: active\nid: NOTHEX\nfingerprint: " + strings.Repeat("a", 64) + "\n", wants: "missing its identity"},
		{name: "malformed fingerprint", content: "version: 1\nentry: prod\nstate: active\nid: 0123456789abcdef0123456789abcdef\nfingerprint: abc\n", wants: "missing its fingerprint"},
		{name: "active with an earlier identity", content: "version: 1\nentry: prod\nstate: active\nid: 0123456789abcdef0123456789abcdef\nfingerprint: " + strings.Repeat("a", 64) + "\nprior:\n  id: fedcba9876543210fedcba9876543210\n  fingerprint: " + strings.Repeat("b", 64) + "\n", wants: "still carrying an earlier identity"},
		{name: "malformed earlier identity", content: "version: 1\nentry: prod\nstate: pending\nid: 0123456789abcdef0123456789abcdef\nfingerprint: " + strings.Repeat("a", 64) + "\nprior:\n  id: NOTHEX\n  fingerprint: " + strings.Repeat("b", 64) + "\n", wants: "unreadable earlier identity"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(identityDirPath(dir), identityFileName("prod"))
			require.NoError(t, os.MkdirAll(identityDirPath(dir), identityDirPerm))
			require.NoError(t, os.WriteFile(path, []byte(tc.content), identityFilePerm))

			_, err := readIdentityRecord(dir, "prod")
			require.ErrorContains(t, err, tc.wants)
			require.ErrorContains(t, err, "log in again")

			after, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, tc.content, string(after), "the failed read rewrote the record")
		})
	}
}

// TestReadIdentityRecordRejectsSymlink asserts that a record reached
// through a symlink is refused. os.Root follows a link that stays inside
// the root, so the refusal cannot come from containment alone.
func TestReadIdentityRecordRejectsSymlink(t *testing.T) {
	skipWithoutSymlinks(t)
	t.Parallel()
	dir := t.TempDir()

	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "staging", "token-1")))
	require.NoError(t, os.Symlink(
		identityFileName("staging"),
		filepath.Join(identityDirPath(dir), identityFileName("prod"))))

	_, err := readIdentityRecord(dir, "prod")
	require.ErrorContains(t, err, "a symlink")
}

// TestWriteIdentityRecordRejectsSymlink asserts that a record replaced by
// a symlink is refused on the way in too: a rename through the contained
// directory would follow a link that stays inside it and destroy whatever
// it names.
func TestWriteIdentityRecordRejectsSymlink(t *testing.T) {
	skipWithoutSymlinks(t)
	t.Parallel()
	dir := t.TempDir()

	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "staging", "token-1")))
	link := filepath.Join(identityDirPath(dir), identityFileName("prod"))
	require.NoError(t, os.Symlink(identityFileName("staging"), link))

	require.ErrorContains(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-2")), "a symlink")

	// The record the link named is exactly as it was.
	got, err := readIdentityRecord(dir, "staging")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "staging", got.Entry)
}

// TestListIdentityRecordsRejectsANonRegularRecord asserts the enumerator
// and the reader agree: a name spelled like a record that is not a regular
// file stops both, rather than stopping one command and being skipped by
// another over the same directory.
func TestListIdentityRecordsRejectsANonRegularRecord(t *testing.T) {
	skipWithoutSymlinks(t)
	t.Parallel()
	dir := t.TempDir()

	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "staging", "token-1")))
	require.NoError(t, os.Symlink(
		identityFileName("staging"),
		filepath.Join(identityDirPath(dir), identityFileName("prod"))))

	_, err := listIdentityRecords(dir)
	require.ErrorContains(t, err, "is not a regular file")
}

// TestReadIdentityRecordRemovedWhileBeingRead asserts the answer a reader
// gets when a logout in another process removes the record between the
// stat and the read: the entry is not logged in, which is what the same
// read returns a moment later, and not a broken identity whose remedy
// would name keychain entries that are still this directory's.
func TestReadIdentityRecordRemovedWhileBeingRead(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))

	restore := SetIdentityObserverForTest(func(op string) {
		if op == "recordstat" {
			require.NoError(t, os.Remove(filepath.Join(identityDirPath(dir), identityFileName("prod"))))
		}
	})
	t.Cleanup(restore)

	rec, err := readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

// TestReadIdentityRecordDirectoryRemovedWhileBeingRead asserts the same
// answer one level up, where the whole identity directory goes between the
// stat that trusts it and the open that confines it.
func TestReadIdentityRecordDirectoryRemovedWhileBeingRead(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))

	restore := SetIdentityObserverForTest(func(op string) {
		if op == "dirstat" {
			require.NoError(t, os.RemoveAll(identityDirPath(dir)))
		}
	})
	t.Cleanup(restore)

	rec, err := readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

// TestListIdentityRecordsSkipsARecordRemovedWhileEnumerating asserts that
// `logout --all` racing another logout reaches the entries that are still
// there: a name that went away between the directory read and the file
// read is one this process no longer has to reach, not a failure of the
// whole enumeration.
func TestListIdentityRecordsSkipsARecordRemovedWhileEnumerating(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "staging", "token-2")))

	removed := false
	restore := SetIdentityObserverForTest(func(op string) {
		if op == "list" && !removed {
			removed = true
			require.NoError(t, os.Remove(filepath.Join(identityDirPath(dir), identityFileName("prod"))))
		}
	})
	t.Cleanup(restore)

	got, err := listIdentityRecords(dir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "staging", got[0].Entry)
}

// TestReadIdentityRecordRejectsDirectory asserts a directory in a record's
// place is an error, not an empty record.
func TestReadIdentityRecordRejectsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(identityDirPath(dir), identityFileName("prod")), identityDirPerm))

	_, err := readIdentityRecord(dir, "prod")
	require.ErrorContains(t, err, "a directory")
}

// TestReadIdentityRecordUnreadable asserts that a record the process cannot
// read is an error rather than an absent record.
func TestReadIdentityRecordUnreadable(t *testing.T) {
	skipWithoutSymlinks(t)
	if os.Getuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))
	path := filepath.Join(identityDirPath(dir), identityFileName("prod"))
	require.NoError(t, os.Chmod(path, 0o000))

	_, err := readIdentityRecord(dir, "prod")
	require.ErrorContains(t, err, "unreadable")
}

// TestIdentityDirErrorStates asserts that the directory itself is checked
// before anything inside it is opened.
func TestIdentityDirErrorStates(t *testing.T) {
	skipWithoutSymlinks(t)

	t.Run("symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		elsewhere := t.TempDir()
		require.NoError(t, os.Symlink(elsewhere, identityDirPath(dir)))

		_, err := readIdentityRecord(dir, "prod")
		require.ErrorContains(t, err, "a symlink")
	})

	t.Run("not a directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(identityDirPath(dir), []byte("x"), identityFilePerm))

		_, err := readIdentityRecord(dir, "prod")
		require.ErrorContains(t, err, "not a directory")
	})

	t.Run("reachable by other users", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(identityDirPath(dir), 0o755))

		_, err := readIdentityRecord(dir, "prod")
		require.ErrorContains(t, err, "readable or writable by users other than its owner")
	})
}

// TestListIdentityRecords asserts the enumerator reads every record and
// skips anything that is not one, so a crashed write cannot be parsed as an
// entry's identity.
func TestListIdentityRecords(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	empty, err := listIdentityRecords(dir)
	require.NoError(t, err)
	assert.Empty(t, empty)

	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "staging", "token-2")))

	idDir := identityDirPath(dir)
	require.NoError(t, os.WriteFile(filepath.Join(idDir, "."+identityFileName("prod")+"-0123456789abcdef0123456789abcdef"), []byte("half written"), identityFilePerm))
	require.NoError(t, os.WriteFile(filepath.Join(idDir, "notarecord"), []byte("x"), identityFilePerm))
	require.NoError(t, os.WriteFile(filepath.Join(idDir, "credstore-pin"), []byte("x"), identityFilePerm))

	got, err := listIdentityRecords(dir)
	require.NoError(t, err)
	require.Len(t, got, 2)

	entries := []string{got[0].Entry, got[1].Entry}
	assert.ElementsMatch(t, []string{"prod", "staging"}, entries)
}

// TestListIdentityRecordsRejectsMisnamedRecord asserts a record whose entry
// does not hash to its file name is an error: the file name is the only
// index the enumerator has.
func TestListIdentityRecordsRejectsMisnamedRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))

	path := filepath.Join(identityDirPath(dir), identityFileName("prod"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(identityDirPath(dir), identityFileName("staging")), data, identityFilePerm))

	_, err = listIdentityRecords(dir)
	require.ErrorContains(t, err, "names an entry it is not the record for")
}

// TestRemoveIdentityRecord asserts a removal takes the entry's record and
// nothing else, and that removing what is not there is not an error.
func TestRemoveIdentityRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	require.NoError(t, removeIdentityRecord(dir, "prod"))

	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "staging", "token-2")))
	require.NoError(t, removeIdentityRecord(dir, "prod"))

	got, err := readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = readIdentityRecord(dir, "staging")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "staging", got.Entry)
}

// TestWriteIdentityRecordRefusesUnreadable asserts the writer will not
// leave behind a record it could not read back.
func TestWriteIdentityRecordRefusesUnreadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	rec := activeRecord(t, "prod", "token-1")
	rec.Fingerprint = "not-a-fingerprint"
	require.ErrorContains(t, writeIdentityRecord(dir, rec), "missing its fingerprint")
	assert.NoDirExists(t, identityDirPath(dir))
}

// TestIdentityReadOperationCount asserts what a credential read costs in
// filesystem operations: one stat of the directory, one no-follow open of
// it, one stat of the record and one read. No lock, no second pass.
func TestIdentityReadOperationCount(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))

	var ops []string
	restore := SetIdentityObserverForTest(func(op string) { ops = append(ops, op) })
	t.Cleanup(restore)

	got, err := readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"dirstat", "diropen", "recordstat", "recordread"}, ops)
}

// TestIdentityReadOperationCountWithoutDirectory asserts that a directory
// with no identity at all costs one stat and stops: a file-store directory
// pays a single operation, not an open and a read.
func TestIdentityReadOperationCountWithoutDirectory(t *testing.T) {
	dir := t.TempDir()

	var ops []string
	restore := SetIdentityObserverForTest(func(op string) { ops = append(ops, op) })
	t.Cleanup(restore)

	got, err := readIdentityRecord(dir, "prod")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Equal(t, []string{"dirstat"}, ops)
}

// TestWithIdentityLockSerializes asserts the lock serializes writers in one
// process the way the config lock does. Each holder stays inside long
// enough for a second one to be seen there: a critical section entered and
// left in the same breath reads as serialized whether or not anything
// serialized it.
func TestWithIdentityLockSerializes(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	inside, most := 0, 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			err := withIdentityLock(t.Context(), dir, func() error {
				mu.Lock()
				inside++
				most = max(most, inside)
				mu.Unlock()

				time.Sleep(time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	assert.Equal(t, 1, most)
}

// TestWithIdentityLockWithoutFlock asserts RANCHER_CLI_CONFIG_NO_LOCK skips
// the flock file and keeps the in-process semaphore, as it does everywhere
// else in the directory.
func TestWithIdentityLockWithoutFlock(t *testing.T) {
	t.Setenv(EnvConfigNoLock, "1")
	dir := t.TempDir()

	require.NoError(t, withIdentityLock(t.Context(), dir, func() error { return nil }))
	assert.NoFileExists(t, filepath.Join(dir, identityLockName))
}

// TestWithIdentityLockCancelled asserts a cancelled command never reaches
// the callback.
func TestWithIdentityLockCancelled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	called := false
	err := withIdentityLock(ctx, dir, func() error {
		called = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, called)
}

// TestIdentityRecordsPerEntryWriteConcurrently asserts that two entries
// written at once keep both records: they are separate files, so even the
// unlocked run cannot lose one.
func TestIdentityRecordsPerEntryWriteConcurrently(t *testing.T) {
	t.Setenv(EnvConfigNoLock, "1")
	dir := t.TempDir()

	var wg sync.WaitGroup
	for _, entry := range []string{"prod", "staging", "dev"} {
		wg.Go(func() {
			assert.NoError(t, writeIdentityRecord(dir, activeRecord(t, entry, "token-"+entry)))
		})
	}
	wg.Wait()

	got, err := listIdentityRecords(dir)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}
