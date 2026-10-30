package token

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// rowOutput is what one call of the plugin shows besides its token: the
// result's kind and warnings, the notices it printed, and the shape of the
// ExecCredential with the token and expiry values set aside.
type rowOutput struct {
	Kind       string
	Warnings   []string
	Notices    []string
	APIVersion string
	CredKind   string
	TokenSet   bool
	ExpirySet  bool
}

// outputOfCall reads a finished call into a rowOutput. The test's own
// directory and server URL are replaced by names.
func outputOfCall(t *testing.T, res runtime.Result, notices []string, dir, url string) rowOutput {
	t.Helper()
	cred, ok := res.Data.(*execCredential)
	require.True(t, ok)
	clean := func(in []string) []string {
		var out []string
		for _, s := range in {
			s = strings.ReplaceAll(s, dir, "<dir>")
			out = append(out, strings.ReplaceAll(s, url, "<url>"))
		}
		return out
	}
	return rowOutput{
		Kind:       res.Kind,
		Warnings:   clean(res.Warnings),
		Notices:    clean(notices),
		APIVersion: cred.APIVersion,
		CredKind:   cred.Kind,
		TokenSet:   cred.Status.Token != "",
		ExpirySet:  cred.Status.ExpirationTimestamp != nil,
	}
}

// legacyRancher is a Rancher that serves no ext.cattle.io and no other
// path: a stored login is served as is.
func legacyRancher(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// outputOfRow runs one call of each row of a miss and returns its output:
// row 1 creates from a stored login, row 2 serves the stored login on a
// Rancher without ext tokens, row 3 creates the plugin login, row 4 signs in
// with token reuse off, row 5 is the legacy command without a cluster.
func outputOfRow(t *testing.T, row int) rowOutput {
	t.Helper()
	switch row {
	case 1:
		x := newPluginFixture(t, false, testrancher.Options{})
		seedStoredLogin(t, x, "stored")
		p := newCollectedNotices(ui.Discard())
		res, err := x.command(x.store).Execute(t.Context(), x.input("local", "kc"), p)
		require.NoError(t, err)
		return outputOfCall(t, res, p.notices(), x.dir, x.url)
	case 2:
		srv := legacyRancher(t)
		dir := t.TempDir()
		store := config.NewFileStore(dir)
		require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		p := newCollectedNotices(ui.Discard())
		cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
		res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local", "cacert": serverCAPEM(srv)}}, p)
		require.NoError(t, err)
		return outputOfCall(t, res, p.notices(), dir, srv.URL)
	case 3, 4:
		x := newPluginFixture(t, row == 3, testrancher.Options{})
		p := newCollectedNotices(localPrompter())
		res, err := x.getToken(t, "c-test", "n", p)
		require.NoError(t, err)
		return outputOfCall(t, res, p.notices(), x.dir, x.url)
	case 5:
		x := newPluginFixture(t, true, testrancher.Options{})
		p := newCollectedNotices(localPrompter())
		cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
		res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "user": "alice", "auth-provider": "localProvider"}}, p)
		require.NoError(t, err)
		return outputOfCall(t, res, p.notices(), x.dir, x.url)
	}
	require.Fail(t, "no such row")
	return rowOutput{}
}

// TestARegisteredInstallIDRequestAddsNothingToWhatACallShows asserts, per
// row of a miss, the kind and warnings of the result, the notices the call
// prints and the shape of the ExecCredential: they are the fixed lists
// below, read from the code before the install id request was added, and a
// run with a sign-in request function registered shows the same as one
// with none.
func TestARegisteredInstallIDRequestAddsNothingToWhatACallShows(t *testing.T) {
	clearRecords(t)
	shape := rowOutput{
		Kind:       "ExecCredential",
		APIVersion: "client.authentication.k8s.io/v1beta1",
		CredKind:   "ExecCredential",
		TokenSet:   true,
		ExpirySet:  true,
	}
	wantRow2 := shape
	wantRow2.Notices = []string{"this Rancher does not serve ext.cattle.io; caching the stored login token for 15m0s"}
	wantRow3 := shape
	wantRow3.Notices = []string{"token reuse is on and the credential store is the plaintext file <dir>" + string(filepath.Separator) + "credentials.yaml: the login credential this sign-in creates (unscoped, reusable across clusters) will be written there"}
	want := map[int]rowOutput{1: shape, 2: wantRow2, 3: wantRow3, 4: shape, 5: shape}
	for row := 1; row <= 5; row++ {
		assert.Equal(t, want[row], outputOfRow(t, row), "row %d", row)
	}

	probe := registerSignInProbe(t, func() []string { return nil })
	assert.Equal(t, want[4], outputOfRow(t, 4), "row 4 with a sign-in request function registered")
	assert.Len(t, probe.got(), 1, "the registered function was called by the sign-in")
}
