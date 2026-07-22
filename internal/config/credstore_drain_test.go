package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

func TestDrainLoginTokenErasesThroughRecordedHelpers(t *testing.T) {
	// Login stored via exec:fake (records provenance); drain runs with
	// policy "auto" (env-free logout) and must STILL erase via the helper.
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(execRef(), "id:s"))

	from, err := DrainLoginToken(dir, execRef(), "auto")
	require.NoError(t, err)
	assert.Contains(t, from, "helper:fake")

	_, gerr := st.GetToken(execRef())
	assert.ErrorIs(t, gerr, ErrNoToken, "helper credential must be gone")
	require.NoError(t, withProvenanceLock(dir, func(s *provenanceState) error {
		assert.Empty(t, s.forServer("prod"), "provenance entry cleared")
		return nil
	}))
}

func TestDrainLoginTokenFailedEraseKeepsProvenance(t *testing.T) {
	keyring.MockInit()
	useFakeHelper(t, "ok")
	dir := t.TempDir()
	ref := execRef()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, st.SetToken(ref, "id:s"))

	// Helper now fails: drain must error and RETAIN the entry for retry.
	t.Setenv("FAKEHELPER_MODE", "fail")
	_, derr := DrainLoginToken(dir, ref, "exec:fake")
	require.Error(t, derr)
	require.NoError(t, withProvenanceLock(dir, func(s *provenanceState) error {
		require.Len(t, s.forServer("prod"), 1, "failed erase keeps the provenance entry")
		return nil
	}))

	// Helper heals: retry drains and clears.
	t.Setenv("FAKEHELPER_MODE", "ok")
	from, derr := DrainLoginToken(dir, ref, "exec:fake")
	require.NoError(t, derr)
	assert.Contains(t, from, "helper:fake")
	require.NoError(t, withProvenanceLock(dir, func(s *provenanceState) error {
		assert.Empty(t, s.forServer("prod"))
		return nil
	}))
}

func TestDrainSameNameTwoURLsErasesBoth(t *testing.T) {
	keyring.MockInit()
	statePath := useFakeHelper(t, "ok")
	dir := t.TempDir()
	st, err := OpenStore("exec:fake", dir)
	require.NoError(t, err)
	// Login, then re-login same name against a different URL (config URL edit).
	require.NoError(t, st.SetToken(ServerRef{Name: "prod", URL: "https://a.example.com"}, "tok-a"))
	require.NoError(t, st.SetToken(ServerRef{Name: "prod", URL: "https://b.example.com"}, "tok-b"))

	// Drain with only the CURRENT URL: the recorded old identity must be
	// erased too — replayed verbatim from provenance, not reconstructed.
	from, derr := DrainLoginToken(dir, ServerRef{Name: "prod", URL: "https://b.example.com"}, "exec:fake")
	require.NoError(t, derr)
	assert.Contains(t, from, "helper:fake")

	assert.Empty(t, fakeHelperCreds(t, statePath), "both identities (old and current URL) erased")

	require.NoError(t, withProvenanceLock(dir, func(s *provenanceState) error {
		assert.Empty(t, s.forServer("prod"), "all entries cleared after full success")
		return nil
	}))
}

func TestDrainMixedSuccessPersistsPartialRemovals(t *testing.T) {
	// Two helpers recorded for one server. buildFakeHelper ships
	// rancher-credential-fake2 (byte-identical copy) which reads
	// FAKEHELPER2_* env vars, falling back to FAKEHELPER_* — so fake2 can
	// fail while fake succeeds. Drain then errors, fake's entry is removed
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
	_, derr := DrainLoginToken(dir, ref, "auto")
	require.Error(t, derr)

	require.NoError(t, withProvenanceLock(dir, func(s *provenanceState) error {
		entries := s.forServer("prod")
		require.Len(t, entries, 1, "successful removal persisted despite the joined error")
		assert.Equal(t, "fake2", entries[0].Helper)
		return nil
	}))
}

func TestDrainLoginTokenRejectsInvalidPolicyHelperName(t *testing.T) {
	t.Parallel()
	keyring.MockInit()
	dir := t.TempDir()

	// "exec:../evil" passes the "exec:" prefix check but fails ValidHelperName:
	// DrainLoginToken must error (not silently skip) and must not attempt to
	// exec anything. The provenance file is empty, so file/keychain drains also
	// report nothing — only the validation error is returned.
	_, err := DrainLoginToken(dir, execRef(), "exec:../evil")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "../evil", "error must name the invalid helper")
}

func TestDrainCrashResidueErasesRecordedIdentity(t *testing.T) {
	// Crash boundary through the REQUIRED path: an intent was recorded but
	// the process died before store ran. A later policy-independent drain
	// must discover the residue via provenance, replay the recorded
	// identity to the helper (idempotent erase of a never-stored credential
	// succeeds), and clear the entry.
	keyring.MockInit()
	statePath := useFakeHelper(t, "ok")
	dir := t.TempDir()
	require.NoError(t, withProvenanceLock(dir, func(s *provenanceState) error {
		s.add(ProvenanceEntry{Helper: "fake", Name: "prod", CanonicalURL: "https://rancher.example.com"})
		return nil
	}))

	from, err := DrainLoginToken(dir, execRef(), "auto") // policy-independent: "auto" names no helper
	require.NoError(t, err, "crash residue drains as success")
	assert.Contains(t, from, "helper:fake")
	assert.Contains(t, fakeHelperLog(t, statePath), "erase https://rancher.example.com prod",
		"the helper saw exactly the recorded identity")
	require.NoError(t, withProvenanceLock(dir, func(s *provenanceState) error {
		assert.Empty(t, s.forServer("prod"), "residue entry cleared")
		return nil
	}))
}
