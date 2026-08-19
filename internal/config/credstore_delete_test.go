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

// TestDeleteExecCacheForServerHTTPLegacyHostDeletes verifies that a legacy cache
// record stored with an HTTP host:port spelling (HOST:80) is deleted when the
// logout target is http://host. The delete must interpret the stored host using
// the TARGET's scheme to strip its default port, not always https.
// TestHTTPAndHTTPSServersHaveDistinctCacheIdentity is the P1 regression test
// for the scheme-collapse finding: credentials cached for http://host and
// https://host on the same --name/--cluster must remain fully isolated.
//
// (a) CanonicalServerIdentity produces two distinct values.
// (b) The two entries are stored under different keys.
// (c) Deleting http://host removes only its entry; deleting https://host
//
//	removes only its entry.
//
// (d) A legacy host-only record (no scheme) still deletes when either scheme
//
//	is targeted.
func TestHTTPAndHTTPSServersHaveDistinctCacheIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := NewFileStore(dir)

	httpID := CanonicalServerIdentity("http://samehost")
	httpsID := CanonicalServerIdentity("https://samehost")

	// (a) Distinct identities.
	if httpID == httpsID {
		t.Fatalf("http and https identity must differ: both returned %q", httpID)
	}

	httpKey := CacheKey("prod", httpID, "c-1")
	httpsKey := CacheKey("prod", httpsID, "c-1")

	// (b) Distinct cache keys.
	if httpKey == httpsKey {
		t.Fatalf("http and https cache keys must differ: both are %q", httpKey)
	}

	require.NoError(t, fs.SetCachedCred(httpKey, "tok-http", time.Now().Add(time.Hour), "prod", httpID, "c-1"))
	require.NoError(t, fs.SetCachedCred(httpsKey, "tok-https", time.Now().Add(time.Hour), "prod", httpsID, "c-1"))

	// (d) Pre-seed a legacy host-only record (no scheme) to verify it deletes.
	legacyKey := CacheKey("prod", "SAMEHOST:80", "c-1")
	require.NoError(t, fs.SetCachedCred(legacyKey, "tok-legacy", time.Now().Add(time.Hour), "prod", "SAMEHOST:80", "c-1"))

	// (c) Deleting http://samehost removes only the HTTP entry (and the legacy).
	deleted, err := DeleteExecCacheForServer(fs, "http://samehost")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{httpKey, legacyKey}, deleted,
		"http delete must remove the http entry and the legacy record")

	_, _, err = fs.GetCachedCred(httpKey)
	assert.ErrorIs(t, err, ErrNoToken, "http entry must be gone after http delete")

	tok, _, err := fs.GetCachedCred(httpsKey)
	require.NoError(t, err)
	assert.Equal(t, "tok-https", tok, "https entry must survive the http delete")

	// (c) Deleting https://samehost now removes only the HTTPS entry.
	deleted, err = DeleteExecCacheForServer(fs, "https://samehost")
	require.NoError(t, err)
	assert.Equal(t, []string{httpsKey}, deleted,
		"https delete must remove only the https entry")

	_, _, err = fs.GetCachedCred(httpsKey)
	assert.ErrorIs(t, err, ErrNoToken, "https entry must be gone after https delete")
}

func TestDeleteExecCacheForServerHTTPLegacyHostDeletes(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	legacyKey := CacheKey("kubeuser", "HOST:80", "c-1")
	require.NoError(t, fs.SetCachedCred(legacyKey, "tok", time.Now().Add(time.Hour), "kubeuser", "HOST:80", "c-1"))

	deleted, err := DeleteExecCacheForServer(fs, "http://host")
	require.NoError(t, err)
	assert.Equal(t, []string{legacyKey}, deleted, "HOST:80 record must delete for http://host target")

	_, _, gerr := fs.GetCachedCred(legacyKey)
	assert.ErrorIs(t, gerr, ErrNoToken, "the HOST:80 cache entry must be gone")
}

// TestDeleteExecCacheForServerNormalizesLegacyHostSpelling is the impl-review
// finding-4 repro: a cache record whose Server host was stored with a
// different-but-equivalent spelling (uppercase, explicit default port) must
// still be deleted on logout, since the delete now compares canonical hosts
// rather than raw bytes.
func TestDeleteExecCacheForServerNormalizesLegacyHostSpelling(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	// Pre-seed a legacy-cased/ported record: Server field spelled "HOST:443".
	legacyKey := CacheKey("kubeuser", "HOST:443", "c-1")
	require.NoError(t, fs.SetCachedCred(legacyKey, "tok", time.Now().Add(time.Hour), "kubeuser", "HOST:443", "c-1"))

	deleted, err := DeleteExecCacheForServer(fs, "https://host")
	require.NoError(t, err)
	assert.Equal(t, []string{legacyKey}, deleted, "an equivalent host spelling must still delete the legacy record")

	_, _, gerr := fs.GetCachedCred(legacyKey)
	assert.ErrorIs(t, gerr, ErrNoToken, "the legacy-cased cache entry must be gone")
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

	from, err := DeleteLoginToken(t.Context(), dir, execRef(), "auto")
	require.NoError(t, err)
	assert.Contains(t, from, "helper:fake")

	_, gerr := st.GetToken(execRef())
	assert.ErrorIs(t, gerr, ErrNoToken, "helper credential must be gone")
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
	_, derr := DeleteLoginToken(t.Context(), dir, ref, "exec:fake")
	require.Error(t, derr)
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		require.Len(t, s.forServer("prod"), 1, "failed erase keeps the helper record")
		return nil
	}))

	// Helper heals: retry deletes and clears.
	t.Setenv("FAKEHELPER_MODE", "ok")
	from, derr := DeleteLoginToken(t.Context(), dir, ref, "exec:fake")
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
	// erased too — replayed verbatim from the helper records, not reconstructed.
	from, derr := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://b.example.com"}, "exec:fake")
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
	// FAKEHELPER2_* env vars, falling back to FAKEHELPER_* — so fake2 can
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
	_, derr := DeleteLoginToken(t.Context(), dir, ref, "auto")
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
	// report nothing — only the validation error is returned.
	_, err := DeleteLoginToken(t.Context(), dir, execRef(), "exec:../evil")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "../evil", "error must name the invalid helper")
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

	from, err := DeleteLoginToken(t.Context(), dir, execRef(), "auto") // policy-independent: "auto" names no helper
	require.NoError(t, err, "a leftover crash record deletes as success")
	// Nothing was ever stored, so the no-op erase is not reported as a
	// deletion (friction-4 semantics) — the replay and the cleanup below
	// are the crash-boundary guarantees this test pins.
	assert.NotContains(t, from, "helper:fake")
	assert.Contains(t, fakeHelperLog(t, statePath), "erase https://rancher.example.com prod",
		"the helper saw exactly the recorded identity")
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(s *helperRecords) error {
		assert.Empty(t, s.forServer("prod"), "leftover record cleared")
		return nil
	}))
}

// TestDeleteLoginTokenNoOpEraseNotReported pins live-exercise friction 4:
// when the helper holds no credential for the identity, the delete still runs
// the idempotent erase, but must NOT report the helper in deletedFrom — the
// output could not distinguish a no-op from an actual deletion. The policy
// names the helper, so the identity is a delete target even with no
// helper record and nothing stored.
func TestDeleteLoginTokenNoOpEraseNotReported(t *testing.T) {
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()

	from, err := DeleteLoginToken(t.Context(), dir, execRef(), "exec:fake")
	require.NoError(t, err)
	assert.NotContains(t, from, "helper:fake", "a no-op erase is not a deletion")
	assert.Empty(t, from)
}

// TestDeleteLoginTokenCancellableWhileRecordsLockHeld pins the third-review
// P1: a delete waiting on a contended helper-records lock must be
// interruptible by the caller's context — before this, the wait was built
// from context.Background() and Ctrl-C during logout could not reach it
// (while the command also held the config write lock, blocking every other
// invocation on the machine).
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
		_, derr := DeleteLoginToken(ctx, dir, execRef(), "exec:fake")
		done <- derr
	}()
	<-flockWaiting // let the delete reach the records-lock wait
	cancel()
	select {
	case derr := <-done:
		require.Error(t, derr)
		assert.ErrorIs(t, derr, context.Canceled, "cancellation must interrupt the records-lock wait")
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not unblock the records-lock wait")
	}
}
