package token

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// normanOptions is a Rancher without ext.cattle.io that serves the Norman
// token create, whose create answer has an empty expiresAt.
func normanOptions() testrancher.Options {
	return testrancher.Options{Legacy: true, V3TokenCreate: true, LegacyLoginBodyUserID: true}
}

// assertNinetyDaysFromNow asserts an expiry is the 90 days a Norman create
// with no ttl gets.
func assertNinetyDaysFromNow(t *testing.T, got time.Time) {
	t.Helper()
	assert.WithinDuration(t, time.Now().Add(90*24*time.Hour), got, time.Minute)
}

// newRewritingPluginFixture is a plugin fixture whose Rancher answers the
// Norman create through rewrite, which edits the decoded answer.
func newRewritingPluginFixture(t *testing.T, reuse bool, opts testrancher.Options, rewrite func(map[string]any)) *pluginFixture {
	t.Helper()
	f := newFacadeWithClusters(&opts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != normanTokensPath {
			f.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, r)
		var answer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		rewrite(answer)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	x := &pluginFixture{f: f, srv: srv, url: srv.URL}
	x.init(t, reuse)
	return x
}

// TestExecPluginSignInThroughNormanCachesTheExpiryTheAnswerGives asserts that
// a cluster token created through the Norman route, whose answer carries an
// empty expiresAt, is cached with the expiry the answer's createdTS and ttl
// give, and that the ExecCredential expires then: through the sign-in the
// plugin makes with token reuse off, through the first use with token reuse
// on, and through a later cache miss that creates a cluster token from the
// stored login.
func TestExecPluginSignInThroughNormanCachesTheExpiryTheAnswerGives(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reuse bool
		// later is the second call, a cache miss for another cache name.
		later bool
	}{
		{"sign-in with token reuse off", false, false},
		{"first use with token reuse on", true, false},
		{"cache miss with a stored login", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newPluginFixture(t, tc.reuse, normanOptions())
			name := "n"
			var notices bytes.Buffer
			p := ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true)
			if tc.later {
				_, err := x.getToken(t, "c-test", "n", p)
				require.NoError(t, err)
				name = "m"
			}
			res, err := x.getToken(t, "c-test", name, p)
			require.NoError(t, err)
			cred := res.Data.(*execCredential)
			require.NotNil(t, cred.Status.ExpirationTimestamp, "the ExecCredential expires")
			assertNinetyDaysFromNow(t, *cred.Status.ExpirationTimestamp)
			rec := cachedInfo(t, x.dir, name)
			assert.True(t, rec.Expiry.Equal(*cred.Status.ExpirationTimestamp), "the cache record expires with the credential")
			require.NotNil(t, rec.TokenRef)
			assert.Equal(t, TokenKindV3, rec.TokenRef.Kind)
			assert.NotContains(t, notices.String(), "not caching")
		})
	}
}

// TestSignInForClusterTokenThroughNormanCarriesTheComputedExpiry asserts the
// expiry of the cluster token a sign-in created through the Norman route is
// the answer's createdTS plus its ttl, in RFC 3339 UTC.
func TestSignInForClusterTokenThroughNormanCarriesTheComputedExpiry(t *testing.T) {
	t.Parallel()
	_, _, si, err := signInAgainst(t, testrancher.Options{Legacy: true, V3TokenCreate: true}, "c-test")
	require.NoError(t, err)
	expiry, err := time.Parse(time.RFC3339, si.Cluster.ExpiresAt)
	require.NoError(t, err)
	assertNinetyDaysFromNow(t, expiry)
	assert.Equal(t, expiry.UTC().Format(time.RFC3339), si.Cluster.ExpiresAt)
}

// TestNormanAnswerWithoutAUsableCreatedTSAndTTLIsNotCached asserts that an
// answer that gives no expiry, no createdTS or no ttl, or a sum out of range,
// leaves the token with no expiry: the ExecCredential has none, nothing is
// cached and the notice says why.
func TestNormanAnswerWithoutAUsableCreatedTSAndTTLIsNotCached(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rewrite func(map[string]any)
	}{
		{"no createdTS", func(a map[string]any) { delete(a, "createdTS") }},
		{"a zero createdTS", func(a map[string]any) { a["createdTS"] = 0 }},
		{"a ttl of zero", func(a map[string]any) { a["ttl"] = 0 }},
		{"a sum past year 9999", func(a map[string]any) { a["createdTS"], a["ttl"] = int64(253402300799000), 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRewritingPluginFixture(t, false, normanOptions(), tc.rewrite)
			var notices bytes.Buffer
			res, err := x.getToken(t, "c-test", "n", ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true))
			require.NoError(t, err)
			cred := res.Data.(*execCredential)
			assert.NotEmpty(t, cred.Status.Token)
			assert.Nil(t, cred.Status.ExpirationTimestamp)
			assert.Contains(t, notices.String(), "not caching: the new token has no expiry")
			creds, err := x.store.ListCachedCreds()
			require.NoError(t, err)
			assert.Empty(t, creds)
		})
	}
}
