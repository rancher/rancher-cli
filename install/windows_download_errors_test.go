package install_test

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// windowsDlAssets are the three release assets install.ps1 downloads.
func windowsDlAssets() []string {
	return []string{"rancher-" + windowsPlat + ".exe", "rancher-credential-vault-" + windowsPlat + ".exe", "SHA256SUMS"}
}

// windowsDlRun is one install.ps1 run of the release against the stub.
type windowsDlRun struct {
	s      *stub
	home   winHome
	out    string
	errOut string
	code   int
}

// runWindowsDownload runs install.ps1 with the failures registered by setup
// and the route's stub configuration.
func runWindowsDownload(t *testing.T, sh powerShell, r dlRoute, env []string, ghDir string, setup func(s *stub)) windowsDlRun {
	t.Helper()
	s := newStub(t)
	writeWindowsRelease(t, s, dlTag)
	s.setLatest(dlTag)
	r.setup(s)
	setup(s)
	if ghDir != "" {
		env = append(env, "PATH="+hermeticPath(ghDir))
	}
	home := newWinHome(t)
	out, errOut, code := runInstallPs(t, s, sh, home, env)
	return windowsDlRun{s: s, home: home, out: out, errOut: errOut, code: code}
}

func (d windowsDlRun) assertNoSecrets(t *testing.T) {
	t.Helper()
	for _, secret := range []string{dlToken, "sig=", "SECRETSIG"} {
		assert.NotContains(t, d.out, secret)
		assert.NotContains(t, d.errOut, secret)
	}
	assert.Zero(t, d.s.cdnAuthorizedCount(), "no request to the redirect URL carries an Authorization header")
}

// TestWindowsDownloadFailsOnceThenSucceeds asserts every asset on both
// paths is retried once: the first request answers 403, the install ends 0
// with the usual result, and a warning on stderr names the asset and the
// status.
func TestWindowsDownloadFailsOnceThenSucceeds(t *testing.T) {
	t.Parallel()
	type row struct {
		route dlRoute
		asset string
	}
	assets := windowsDlAssets()
	var rows []row
	for _, asset := range assets {
		rows = append(rows, row{dlRoutes[0], asset}, row{dlRoutes[3], asset})
	}
	rows = append(rows, row{dlRoutes[1], assets[0]}, row{dlRoutes[2], assets[0]}, row{dlRoutes[4], assets[0]})
	for _, sh := range powerShells(t) {
		for _, rw := range rows {
			t.Run(sh.name+"/"+rw.route.name+"/"+rw.asset, func(t *testing.T) {
				t.Parallel()
				var path string
				d := runWindowsDownload(t, sh, rw.route, dlTokenEnv(rw.route), "", func(s *stub) {
					path = rw.route.failingPath(s, dlAssetID(t, s, rw.asset), rw.asset)
					s.failPath(path, 403)
				})
				require.Equalf(t, 0, d.code, "stderr: %s", d.errOut)
				assert.Contains(t, d.out, "installed: rancher version "+dlTag)
				assertJunctionTo(t, d.home.binDir(), filepath.Join(d.home.versionsDir(), dlTag))
				assert.Equal(t, dlTag, readReceipt(t, d.home)["tag"])
				want := fmt.Sprintf("warning: download of %s failed (HTTP 403); retrying once", rw.asset)
				assert.Equal(t, 1, strings.Count(d.errOut, want), d.errOut)
				assert.Equal(t, 2, d.s.pathRequestCount(path), "one failed request and one retry")
				d.assertNoSecrets(t)
			})
		}
	}
}

// TestWindowsDownloadFailsTwiceNamesStatusURLAndTokenSource asserts the
// error of a download that fails twice on each path: the asset, the URL
// requested, the redirect URL without its query string, the status of the
// second try or the transport error, the tag hint only for two 404 answers
// from the release download URL, and the source of the token exactly when
// the download used one.
func TestWindowsDownloadFailsTwiceNamesStatusURLAndTokenSource(t *testing.T) {
	t.Parallel()
	shells := powerShells(t)
	assets := windowsDlAssets()
	cli, helper, sums := assets[0], assets[1], assets[2]
	ghDir, _, ghEnv := ghStubDir(t, "token")
	cases := []struct {
		name   string
		route  dlRoute
		asset  string
		status int
		env    []string // replaces the route's token variables when set
		ghDir  string
		source string // the token source the error names; "" = anonymous
	}{
		{name: "BrowserAnonymous404", route: dlRoutes[0], asset: cli, status: 404},
		{name: "BrowserWithToken404", route: dlRoutes[1], asset: cli, status: 404, source: "GH_TOKEN"},
		{name: "APIEndpoint404", route: dlRoutes[2], asset: helper, status: 404, source: "GH_TOKEN"},
		{name: "APIRedirectURL404", route: dlRoutes[3], asset: sums, status: 404, source: "GH_TOKEN"},
		{name: "BrowserRedirectURL404", route: dlRoutes[4], asset: helper, status: 404},
		{name: "BrowserRedirectURLClosed", route: dlRoutes[4], asset: helper, status: dropConnection},
		{name: "APIRedirectURLClosed", route: dlRoutes[3], asset: cli, status: dropConnection, source: "GH_TOKEN"},
		{name: "APIEndpointNoLocation", route: dlRoutes[2], asset: helper, status: 302, source: "GH_TOKEN"},
		{name: "APIEndpointGithubToken", route: dlRoutes[2], asset: cli, status: 403,
			env: []string{"GITHUB_TOKEN=" + dlToken}, source: "GITHUB_TOKEN"},
		{name: "APIEndpointGhAuthToken", route: dlRoutes[2], asset: sums, status: 503,
			env: append([]string{"RANCHER_TEST_GH_TOKEN=" + dlToken}, ghEnv...), ghDir: ghDir, source: "gh auth token"},
	}
	for _, sh := range shells {
		for _, tc := range cases {
			t.Run(sh.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				env := tc.env
				if env == nil {
					env = dlTokenEnv(tc.route)
				}
				var path, id string
				d := runWindowsDownload(t, sh, tc.route, env, tc.ghDir, func(s *stub) {
					id = dlAssetID(t, s, tc.asset)
					path = tc.route.failingPath(s, id, tc.asset)
					// A closed connection fails every request: .NET sends a request
					// again by itself when the connection closes before a response,
					// so a count of closures would run out inside one try.
					s.failPathTimes(path, 100, tc.status)
				})
				require.NotEqual(t, 0, d.code)
				base := d.s.srv.URL
				from := base + dlReleaseAt + tc.asset
				if !tc.route.browser {
					from = base + dlAssetAt + id
				}
				want := "download failed: " + tc.asset + " from " + from
				if tc.route.redirect {
					want += ", redirected to " + tc.route.finalURL(base, id, tc.asset)
				}
				want += " ("
				if tc.status == dropConnection {
					assert.Regexp(t, regexp.QuoteMeta(want)+`no complete response after one retry: \S`, d.errOut)
				} else {
					assert.Contains(t, d.errOut, want+fmt.Sprintf("HTTP %d after one retry", tc.status))
				}
				assert.Equal(t, 1, strings.Count(d.errOut, "download failed"), d.errOut)

				if tc.status == 302 {
					assert.Contains(t, d.errOut, "; redirect with no Location")
				}
				hint := "tag '" + dlTag + "' has no asset with this name"
				if tc.route.browser && !tc.route.redirect && tc.status == 404 {
					assert.Contains(t, d.errOut, hint)
				} else {
					assert.NotContains(t, d.errOut, hint)
				}
				assert.NotContains(t, d.errOut, "no such asset")
				if tc.source != "" {
					assert.Contains(t, d.errOut, "the download used the token from "+tc.source)
				} else {
					assert.NotContains(t, d.errOut, "token")
				}
				if tc.status == dropConnection {
					// The installer retries once, as the one warning shows; .NET may
					// send the closed request more than once inside each try.
					assert.GreaterOrEqual(t, d.s.pathRequestCount(path), 2)
					assert.Equal(t, 1, strings.Count(d.errOut, "retrying once"), d.errOut)
					// The first request of each try is answered normally, so its
					// count is exact and shows two tries ran.
					assert.Equal(t, 2, d.s.pathRequestCount(tc.route.firstPath(id, tc.asset)), "two tries")
				} else {
					assert.Equal(t, 2, d.s.pathRequestCount(path), "one try and one retry")
				}
				d.assertNoSecrets(t)
			})
		}
	}
}
