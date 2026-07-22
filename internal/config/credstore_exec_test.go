package config

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecStoreRoundTrip(t *testing.T) {
	statePath := useFakeHelper(t, "ok")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, st.SetToken(execRef(), "id:secret"))
	tok, err := st.GetToken(execRef())
	require.NoError(t, err)
	assert.Equal(t, "id:secret", tok)
	// Verify the credential is present in the helper's backing store.
	assert.Equal(t, "id:secret", fakeHelperCreds(t, statePath)["https://rancher.example.com\nprod"])
	require.NoError(t, st.DeleteToken(execRef()))
	_, err = st.GetToken(execRef())
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestExecStoreNotFound(t *testing.T) {
	useFakeHelper(t, "notfound")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	_, err = st.GetToken(execRef())
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestExecStoreUnsupported(t *testing.T) {
	useFakeHelper(t, "unsupported")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	err = st.SetToken(execRef(), "id:secret")
	assert.ErrorIs(t, err, ErrTokenStoreUnsupported)
}

func TestExecStoreHelperFailure(t *testing.T) {
	useFakeHelper(t, "fail")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	_, err = st.GetToken(execRef())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deliberate failure", "helper stderr must surface")
	assert.NotErrorIs(t, err, ErrNoToken)
}

func TestExecStoreGarbageResponse(t *testing.T) {
	useFakeHelper(t, "garbage")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	_, err = st.GetToken(execRef())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rancher-credential-fake")
}

func TestExecStoreMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty PATH dir — helper absent
	st, err := OpenStore("exec:nosuch", t.TempDir())
	require.NoError(t, err, "construction is lazy; failure surfaces on use")
	_, err = st.GetToken(execRef())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `credential helper "rancher-credential-nosuch" not found in PATH`)
	assert.Contains(t, err.Error(), "CREDENTIAL_HELPERS.md")
}

func TestExecStoreTimeout(t *testing.T) {
	useFakeHelper(t, "hang")
	restore := SetExecHelperTimeoutForTest(200 * time.Millisecond)
	defer restore()
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	_, err = st.GetToken(execRef())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Contains(t, err.Error(), "rancher-credential-fake")
}

func TestExecStoreCanonicalWireIdentity(t *testing.T) {
	// Raw-sensitive: fake helper logs the exact serverURL bytes. Store with
	// one spelling, delete with another; the log must show ONE canonical URL.
	statePath := useFakeHelper(t, "ok")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, st.SetToken(ServerRef{Name: "prod", URL: "HTTPS://RANCHER.example.com:443"}, "id:s"))
	require.NoError(t, st.DeleteToken(ServerRef{Name: "prod", URL: "https://rancher.example.com"}))
	log := fakeHelperLog(t, statePath)
	require.Len(t, log, 2)
	assert.Equal(t, "store https://rancher.example.com prod", log[0])
	assert.Equal(t, "erase https://rancher.example.com prod", log[1])
}

func TestExecStoreSilentStoreErase(t *testing.T) {
	// Protocol-compliant helpers may exit 0 with EMPTY stdout on store/erase.
	useFakeHelper(t, "silent")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	assert.NoError(t, st.SetToken(execRef(), "id:secret"))
	assert.NoError(t, st.DeleteToken(execRef()))
}

// Expiry tests inject the clock directly on execStore (same package) —
// deterministic, no time.Now dependence anywhere.

func TestExecStoreExpiryCaching(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	exp := base.Add(10 * time.Minute)
	useFakeHelper(t, "expiresat")
	t.Setenv("FAKEHELPER_EXPIRESAT", exp.Format(time.RFC3339))
	clock := base
	st := newExecStore("fake", t.TempDir())
	st.now = func() time.Time { return clock }

	tok, err := st.GetToken(execRef())
	require.NoError(t, err)
	assert.Equal(t, "short-lived", tok)

	// Within expiresAt-30s: served from memory even if the helper now fails.
	t.Setenv("FAKEHELPER_MODE", "fail")
	clock = exp.Add(-31 * time.Second)
	tok, err = st.GetToken(execRef())
	require.NoError(t, err, "inside the margin the in-process cache serves the token")
	assert.Equal(t, "short-lived", tok)

	// At expiresAt-30s exactly: cache refuses, helper re-invoked (fails → error).
	clock = exp.Add(-30 * time.Second)
	_, err = st.GetToken(execRef())
	require.Error(t, err, "at the margin the helper must be re-invoked")
}

func TestExecStoreMalformedExpiresAt(t *testing.T) {
	useFakeHelper(t, "badexpiry")
	st := newExecStore("fake", t.TempDir())
	_, err := st.GetToken(execRef())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed expiresAt")
	assert.NotErrorIs(t, err, ErrNoToken, "malformed expiry is never a quiet miss")
}

func TestExecStoreRepeatedlyExpired(t *testing.T) {
	useFakeHelper(t, "expired")
	st := newExecStore("fake", t.TempDir())
	_, err := st.GetToken(execRef())
	require.Error(t, err, "one retry then hard error, no loop")
	assert.Contains(t, err.Error(), "expired")
}

func TestExecStoreShortLivedToLongLivedTransitionClearsExpiry(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	exp := base.Add(10 * time.Minute)
	statePath := useFakeHelper(t, "expiresat")
	t.Setenv("FAKEHELPER_EXPIRESAT", exp.Format(time.RFC3339))
	clock := base
	st := newExecStore("fake", t.TempDir())
	st.now = func() time.Time { return clock }

	// First get: short-lived token, in-memory expiry recorded.
	tok, err := st.GetToken(execRef())
	require.NoError(t, err)
	assert.Equal(t, "short-lived", tok)
	_, isShort := st.ShortLivedTokenExpiry(execRef())
	assert.True(t, isShort, "first get must record short-lived expiry")

	// Advance clock past expiry; seed state file with a long-lived credential;
	// switch helper mode to "ok" (state-backed, no expiresAt).
	clock = exp.Add(time.Minute)
	sf := struct {
		Creds map[string]string `json:"creds"`
		Log   []string          `json:"log"`
	}{Creds: map[string]string{"https://rancher.example.com\nprod": "long-lived"}}
	sfBytes, jerr := json.Marshal(sf)
	require.NoError(t, jerr)
	require.NoError(t, os.WriteFile(statePath, sfBytes, 0o600))
	t.Setenv("FAKEHELPER_MODE", "ok")

	// Second get: in-memory cache expired; helper returns a long-lived token
	// (no expiresAt). The stale short-lived entry must be cleared.
	tok, err = st.GetToken(execRef())
	require.NoError(t, err)
	assert.Equal(t, "long-lived", tok)
	_, isShort = st.ShortLivedTokenExpiry(execRef())
	assert.False(t, isShort, "after a long-lived refresh, ShortLivedTokenExpiry must return false")
}
