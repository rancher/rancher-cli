package config

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestHTTPAndHTTPSServersHaveDistinctCacheIdentity asserts that credentials
// cached for http://host and https://host on the same --name/--cluster stay
// fully isolated.
//
// (a) CanonicalServerIdentity produces two distinct values.
// (b) The two entries get different cache keys.
// (c) A record carrying a full URL matches only its own scheme's target.
// (d) A legacy host-only record (no scheme) matches either scheme's target.
func TestHTTPAndHTTPSServersHaveDistinctCacheIdentity(t *testing.T) {
	t.Parallel()
	httpID := CanonicalServerIdentity("http://samehost")
	httpsID := CanonicalServerIdentity("https://samehost")

	// (a) Distinct identities.
	if httpID == httpsID {
		t.Fatalf("http and https identity must differ: both returned %q", httpID)
	}

	// (b) Distinct cache keys.
	httpKey := CacheKey("prod", httpID, "c-1")
	httpsKey := CacheKey("prod", httpsID, "c-1")
	if httpKey == httpsKey {
		t.Fatalf("http and https cache keys must differ: both are %q", httpKey)
	}

	// (c) A full-URL record matches only its own scheme.
	assert.True(t, CacheRecordForServer(httpID, "http://samehost"))
	assert.False(t, CacheRecordForServer(httpID, "https://samehost"), "an http record must not match an https target")
	assert.True(t, CacheRecordForServer(httpsID, "https://samehost"))
	assert.False(t, CacheRecordForServer(httpsID, "http://samehost"), "an https record must not match an http target")

	// (d) A legacy host-only record matches either scheme's target.
	assert.True(t, CacheRecordForServer("SAMEHOST:80", "http://samehost"))
	assert.True(t, CacheRecordForServer("SAMEHOST:443", "https://samehost"))
	assert.True(t, CacheRecordForServer("samehost", "http://samehost"))
	assert.True(t, CacheRecordForServer("samehost", "https://samehost"))
}

// TestCacheRecordForServerLegacyHostSpelling asserts that a record whose
// server is a bare host, in any spelling that canonicalizes to the target's
// host (case, the target scheme's default port), matches the target, and that
// the default port of the other scheme does not.
func TestCacheRecordForServerLegacyHostSpelling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		recordServer string
		serverURL    string
		want         bool
	}{
		{name: "port 80 under an http target", recordServer: "HOST:80", serverURL: "http://host", want: true},
		{name: "port 443 under an https target", recordServer: "HOST:443", serverURL: "https://host", want: true},
		{name: "uppercase host", recordServer: "HOST", serverURL: "https://host", want: true},
		{name: "port 80 under an https target", recordServer: "HOST:80", serverURL: "https://host", want: false},
		{name: "port 443 under an http target", recordServer: "HOST:443", serverURL: "http://host", want: false},
		{name: "another host", recordServer: "other:443", serverURL: "https://host", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, CacheRecordForServer(tc.recordServer, tc.serverURL))
		})
	}
}

func TestDeleteLoginTokenErasesThroughRecordedHelpers(t *testing.T) {
	// Login stored via exec:fake (records the helper record); delete runs with
	// policy "auto" (env-free logout) and must STILL erase via the helper.
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(execRef(), "id:s"))

	from, _, err := DeleteLoginToken(t.Context(), dir, execRef(), "auto")
	require.NoError(t, err)
	assert.Contains(t, from, "helper:fake")

	_, gerr := st.GetToken(execRef())
	require.ErrorIs(t, gerr, ErrNoToken, "helper credential must be gone")
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"), "helper record cleared")
		return nil
	}))
}

func TestDeleteLoginTokenFailedEraseKeepsHelperRecord(t *testing.T) {
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()
	ref := execRef()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(ref, "id:s"))

	// Helper now fails: delete must error and RETAIN the entry for retry.
	t.Setenv("FAKEHELPER_MODE", "fail")
	_, _, derr := DeleteLoginToken(t.Context(), dir, ref, "exec:fake")
	require.Error(t, derr)
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		require.Len(t, s.forServer("prod"), 1, "failed erase keeps the helper record")
		return nil
	}))

	// Helper heals: retry deletes and clears.
	t.Setenv("FAKEHELPER_MODE", "ok")
	from, _, derr := DeleteLoginToken(t.Context(), dir, ref, "exec:fake")
	require.NoError(t, derr)
	assert.Contains(t, from, "helper:fake")
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"))
		return nil
	}))
}

func TestDeleteSameNameTwoURLsErasesBoth(t *testing.T) {
	keyring.MockInit()
	statePath := useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	// Login, then re-login same name against a different URL (config URL edit).
	require.NoError(t, st.SetToken(ServerRef{Name: "prod", URL: "https://a.example.com"}, "tok-a"))
	require.NoError(t, st.SetToken(ServerRef{Name: "prod", URL: "https://b.example.com"}, "tok-b"))

	// Delete with only the CURRENT URL: the recorded old identity must be
	// erased too - replayed verbatim from the helper records, not reconstructed.
	from, _, derr := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://b.example.com"}, "exec:fake")
	require.NoError(t, derr)
	assert.Contains(t, from, "helper:fake")

	assert.Empty(t, fakeHelperCreds(t, statePath), "both identities (old and current URL) erased")

	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"), "all entries cleared after full success")
		return nil
	}))
}

func TestDeleteMixedSuccessPersistsPartialRemovals(t *testing.T) {
	// Two helpers recorded for one server. buildFakeHelper ships
	// rancher-credential-fake2 (byte-identical copy) which reads
	// FAKEHELPER2_* env vars, falling back to FAKEHELPER_* - so fake2 can
	// fail while fake succeeds. Delete then errors, fake's entry is removed
	// AND PERSISTED, fake2's entry survives on disk for retry.
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()
	ref := execRef()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(ref, "id:s"))
	st2, err := OpenStore("exec:fake2", dir)
	require.NoError(t, err)
	require.NoError(t, st2.SetToken(ref, "id:s"))

	t.Setenv("FAKEHELPER2_MODE", "fail")
	_, _, derr := DeleteLoginToken(t.Context(), dir, ref, "auto")
	require.Error(t, derr)

	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		entries := s.forServer("prod")
		require.Len(t, entries, 1, "successful removal persisted despite the joined error")
		assert.Equal(t, "fake2", entries[0].Helper)
		return nil
	}))
}

func TestDeleteLoginTokenRejectsInvalidPolicyHelperName(t *testing.T) {
	t.Parallel()
	keyring.MockInit()
	dir := t.TempDir()

	// "exec:../evil" passes the "exec:" prefix check but fails ValidHelperName:
	// DeleteLoginToken must error (not silently skip) and must not attempt to
	// exec anything. The helper-records file is empty, so file/keychain deletes also
	// report nothing - only the validation error is returned.
	_, _, err := DeleteLoginToken(t.Context(), dir, execRef(), "exec:../evil")
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid credential helper name", "error must name the problem")
	assert.NotContains(t, err.Error(), "../evil")
}

func TestDeleteOrphanedRecordErasesRecordedIdentity(t *testing.T) {
	// Crash boundary through the REQUIRED path: an intent was recorded but
	// the process died before store ran. A later policy-independent delete
	// must discover the leftover record, replay the recorded
	// identity to the helper (idempotent erase of a never-stored credential
	// succeeds), and clear the entry.
	keyring.MockInit()
	statePath := useFakeHelper(t, "ok")
	dir := t.TempDir()
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		s.add(HelperRecord{Helper: "fake", Name: "prod", CanonicalURL: "https://rancher.example.com"})
		return nil
	}))

	from, _, err := DeleteLoginToken(t.Context(), dir, execRef(), "auto") // policy-independent: "auto" names no helper
	require.NoError(t, err, "a leftover crash record deletes as success")
	// Nothing was ever stored, so the no-op erase is not reported as a
	// deletion - the replay and the cleanup below
	// are the crash-boundary guarantees this test asserts.
	assert.NotContains(t, from, "helper:fake")
	assert.Contains(t, fakeHelperLog(t, statePath), "erase https://rancher.example.com prod",
		"the helper saw exactly the recorded identity")
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"), "leftover record cleared")
		return nil
	}))
}

// TestDeleteLoginTokenNoOpEraseNotReported asserts that
// when the helper holds no credential for the identity, the delete still runs
// the idempotent erase, but must NOT report the helper in deletedFrom - the
// output could not distinguish a no-op from an actual deletion. The policy
// names the helper, so the identity is a delete target even with no
// helper record and nothing stored.
func TestDeleteLoginTokenNoOpEraseNotReported(t *testing.T) {
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()

	from, _, err := DeleteLoginToken(t.Context(), dir, execRef(), "exec:fake")
	require.NoError(t, err)
	assert.NotContains(t, from, "helper:fake", "a no-op erase is not a deletion")
	assert.Empty(t, from)
}

// TestDeleteLoginTokenCancellableWhileRecordsLockHeld asserts that
// a delete waiting on a contended helper-records lock is interruptible by
// the caller's context: a wait built from context.Background() would let
// Ctrl-C during logout not reach it while the command also held the config
// write lock, blocking every other invocation on the machine.
func TestDeleteLoginTokenCancellableWhileRecordsLockHeld(t *testing.T) {
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()

	fl := flock.New(filepath.Join(dir, helperRecordsLockName))
	locked, err := fl.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { _ = fl.Unlock() }()

	ctx, cancel := context.WithCancel(t.Context())
	flockWaiting := make(chan struct{})
	restore := SetWaitForFlockHookForTest(func() { close(flockWaiting) })
	defer restore()
	done := make(chan error, 1)
	go func() {
		_, _, derr := DeleteLoginToken(ctx, dir, execRef(), "exec:fake")
		done <- derr
	}()
	<-flockWaiting // let the delete reach the records-lock wait
	cancel()
	select {
	case derr := <-done:
		require.Error(t, derr)
		require.ErrorIs(t, derr, context.Canceled, "cancellation must interrupt the records-lock wait")
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not unblock the records-lock wait")
	}
}
