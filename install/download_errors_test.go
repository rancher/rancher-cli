package install_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	dlTag       = "v1.2.3"
	dlToken     = "tok-DISTINCT-7f3a91"
	dlReleaseAt = "/rancher/rancher-cli/releases/download/" + dlTag + "/"
	dlAssetAt   = "/repos/rancher/rancher-cli/releases/assets/"
	dlCDNAt     = "/cdn/assets/"
	dlStorageAt = "/cdn/download/"
)

// dlAssets are the three release assets the installer downloads, by name.
func dlAssets() []string {
	plat := hostPlatform()
	return []string{"rancher-" + plat, "rancher-credential-vault-" + plat, "SHA256SUMS"}
}

// dlAssetID is the numeric id the stub gave the named asset of the release.
func dlAssetID(t *testing.T, s *stub, name string) string {
	t.Helper()
	var doc struct {
		Assets []struct {
			Name string `json:"name"`
			ID   int64  `json:"id"`
		} `json:"assets"`
	}
	require.NoError(t, json.Unmarshal(s.releaseJSONFor(dlTag), &doc))
	for _, a := range doc.Assets {
		if a.Name == name {
			return fmt.Sprint(a.ID)
		}
	}
	require.FailNow(t, "asset not in the release", name)
	return ""
}

// dlRoute is one way the installer reaches an asset, and the request that
// the stub makes fail.
type dlRoute struct {
	name     string
	token    bool // the download carries a GitHub token
	browser  bool // the release download URL; false = the API asset endpoint
	redirect bool // the failing request is the redirect URL of the download URL or API endpoint
	setup    func(s *stub)
}

var dlRoutes = []dlRoute{
	{name: "BrowserAnonymous", browser: true, setup: func(*stub) {}},
	{name: "BrowserWithToken", token: true, browser: true, setup: func(s *stub) {
		// A token that cannot read the release metadata falls back to the
		// release download URLs.
		s.setReleaseTagStatus(403)
	}},
	{name: "APIEndpoint", token: true, setup: func(s *stub) { s.setPrivate(true) }},
	{name: "APIRedirectURL", token: true, redirect: true, setup: func(s *stub) {
		s.setPrivate(true)
		s.setAssetRedirect(true)
	}},
	{name: "BrowserRedirectURL", browser: true, redirect: true, setup: func(s *stub) {
		s.setBrowserRedirect(true)
	}},
}

// failingPath is the path of the request that fails for the asset.
func (r dlRoute) failingPath(s *stub, id, asset string) string {
	switch {
	case r.browser && r.redirect:
		return dlStorageAt + dlTag + "/" + asset
	case r.browser:
		return dlReleaseAt + asset
	case r.redirect:
		return dlCDNAt + id
	default:
		return dlAssetAt + id
	}
}

// firstPath is the path of the first request of a try.
func (r dlRoute) firstPath(id, asset string) string {
	if r.browser {
		return dlReleaseAt + asset
	}
	return dlAssetAt + id
}

// finalURL is the redirect URL the error names, without its query string.
func (r dlRoute) finalURL(base, id, asset string) string {
	if r.browser {
		return base + dlStorageAt + dlTag + "/" + asset
	}
	return base + dlCDNAt + id
}

// dlRun is one finished install of the release against the stub.
type dlRun struct {
	s       *stub
	home    string
	out     string
	errOut  string
	code    int
	argvLog string
	elapsed time.Duration
}

// dlOpts is the environment of one install: the token variables (none =
// anonymous), more variables, and a directory put first on PATH.
type dlOpts struct {
	tokenEnv []string
	extraEnv []string
	pathDir  string
}

// runDownloadInstall installs the release after registering the failures
// with setup.
func runDownloadInstall(t *testing.T, r dlRoute, o dlOpts, setup func(s *stub), args ...string) dlRun {
	t.Helper()
	s := newStub(t)
	writeRelease(s, dlTag, false)
	s.setLatest(dlTag)
	r.setup(s)
	if setup != nil {
		setup(s)
	}
	wrapDir, argvLog, wrapEnv := recordingCurlWrapper(t)
	home := t.TempDir()
	path := wrapDir + ":" + toolPath(t, baseTools...)
	if o.pathDir != "" {
		path = o.pathDir + ":" + path
	}
	env := append(envHome(home), "PATH="+path)
	env = append(env, wrapEnv...)
	env = append(env, o.tokenEnv...)
	env = append(env, o.extraEnv...)
	start := time.Now()
	out, errOut, code := runInstall(t, s, env, args...)
	return dlRun{s: s, home: home, out: out, errOut: errOut, code: code, argvLog: argvLog, elapsed: time.Since(start)}
}

// assertNoSecrets asserts neither the token nor the signed URL query appears
// in the output or in any curl argument, and that no request to the redirect
// URL of the API endpoint, failed ones and retries included, carried an
// Authorization header.
func (d dlRun) assertNoSecrets(t *testing.T, token string) {
	t.Helper()
	argv, err := os.ReadFile(d.argvLog)
	require.NoError(t, err)
	for _, secret := range []string{token, "sig=", "SECRETSIG"} {
		assert.NotContains(t, d.out, secret)
		assert.NotContains(t, d.errOut, secret)
	}
	assert.NotContains(t, string(argv), token, "the token never travels in curl arguments")
	assert.Zero(t, d.s.cdnAuthorizedCount(), "no request to the redirect URL carries an Authorization header")
}

// assertInstalled asserts the pair and the receipt are in place as after a
// clean install.
func (d dlRun) assertInstalled(t *testing.T) {
	t.Helper()
	bin := filepath.Join(d.home, ".local", "bin")
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		fi, err := os.Lstat(filepath.Join(bin, name))
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&os.ModeSymlink, "%s must be a symlink", name)
	}
	assert.Contains(t, d.out, "rancher version "+dlTag)
	raw, err := os.ReadFile(filepath.Join(d.home, ".config", "rancher-cli", "install-receipt.json"))
	require.NoError(t, err)
	var receipt map[string]string
	require.NoError(t, json.Unmarshal(raw, &receipt))
	assert.Equal(t, dlTag, receipt["tag"])
}

// assertRequestCount asserts the stub saw want requests for path. When the
// stub closes the connection of a request that follows a redirect on the
// same connection, curl sends that request again on a fresh connection by
// itself, so each try counts one or two.
func assertRequestCount(t *testing.T, s *stub, r dlRoute, status int, path string, want int, msg string) {
	t.Helper()
	got := s.pathRequestCount(path)
	if status == dropConnection && r.browser && r.redirect {
		assert.GreaterOrEqual(t, got, want, msg)
		assert.LessOrEqual(t, got, 2*want, msg)
		return
	}
	assert.Equal(t, want, got, msg)
}

// dlStatuses are the first-failure outcomes of the table: three statuses and
// a connection closed with no response.
var dlStatuses = []int{403, 404, 503, dropConnection}

func dlStatusName(status int) string {
	if status == dropConnection {
		return "ConnectionClosed"
	}
	return fmt.Sprintf("HTTP%d", status)
}

func dlTokenEnv(r dlRoute) []string {
	if !r.token {
		return nil
	}
	return []string{"GH_TOKEN=" + dlToken}
}

// TestDownloadFailsOnceThenSucceeds asserts that every asset on every path
// is retried once: the first request fails with each status in turn, the
// install ends 0 with the same result as a clean install, and a warning on
// stderr names the asset and the status.
func TestDownloadFailsOnceThenSucceeds(t *testing.T) {
	t.Parallel()
	for _, r := range dlRoutes {
		for _, asset := range dlAssets() {
			for _, status := range dlStatuses {
				if status == dropConnection && r.browser && r.redirect {
					// curl sends the request again by itself when a reused
					// connection closes, so one closed request never fails a try.
					continue
				}
				t.Run(r.name+"/"+asset+"/"+dlStatusName(status), func(t *testing.T) {
					t.Parallel()
					var path string
					d := runDownloadInstall(t, r, dlOpts{tokenEnv: dlTokenEnv(r)}, func(s *stub) {
						path = r.failingPath(s, dlAssetID(t, s, asset), asset)
						s.failPath(path, status)
					})
					require.Equalf(t, 0, d.code, "stderr: %s", d.errOut)
					d.assertInstalled(t)
					want := fmt.Sprintf("warning: download of %s failed (HTTP %d); retrying once", asset, status)
					if status == dropConnection {
						want = fmt.Sprintf("warning: download of %s failed (no complete response); retrying once", asset)
					}
					assert.Equal(t, 1, strings.Count(d.errOut, want), d.errOut)
					for line := range strings.SplitSeq(d.errOut, "\n") {
						if strings.HasPrefix(line, "warning: download of") {
							assert.NotContains(t, line, "://", "the retry warning names no URL")
						}
					}
					assertRequestCount(t, d.s, r, status, path, 2, "one failed request and one retry")
					d.assertNoSecrets(t, dlToken)
				})
			}
		}
	}
}

// TestDownloadFailsTwiceNamesStatusURLAndTokenSource asserts the error of a
// download that fails twice: the asset, the URL requested (and the redirect
// URL without its query string), the status of the second try, the tag hint
// only for two 404 answers from the release download URL, and the source of
// the token exactly when the download used one.
func TestDownloadFailsTwiceNamesStatusURLAndTokenSource(t *testing.T) {
	t.Parallel()
	for _, r := range dlRoutes {
		for _, asset := range dlAssets() {
			for _, status := range dlStatuses {
				t.Run(r.name+"/"+asset+"/"+dlStatusName(status), func(t *testing.T) {
					t.Parallel()
					var path, id string
					d := runDownloadInstall(t, r, dlOpts{tokenEnv: dlTokenEnv(r)}, func(s *stub) {
						id = dlAssetID(t, s, asset)
						path = r.failingPath(s, id, asset)
						s.failPathTimes(path, 5, status)
					})
					require.NotEqual(t, 0, d.code)
					base := d.s.srv.URL
					from := base + dlReleaseAt + asset
					if !r.browser {
						from = base + dlAssetAt + id
					}
					want := "error: download failed: " + asset + " from " + from
					if r.redirect {
						want += ", redirected to " + r.finalURL(base, id, asset)
					}
					want += " ("
					if status == dropConnection {
						want += "no complete response after one retry: curl exit "
					} else {
						want += fmt.Sprintf("HTTP %d after one retry", status)
					}
					assert.Contains(t, d.errOut, want)
					assert.Equal(t, 1, strings.Count(d.errOut, "download failed"), d.errOut)

					// The tag has no such asset only when the release download URL
					// answered 404 on both tries.
					hint := "tag '" + dlTag + "' has no asset with this name"
					if r.browser && !r.redirect && status == 404 {
						assert.Contains(t, d.errOut, hint)
					} else {
						assert.NotContains(t, d.errOut, hint)
					}
					assert.NotContains(t, d.errOut, "no such asset")

					if r.token {
						assert.Contains(t, d.errOut, "the download used the token from GH_TOKEN")
					} else {
						assert.NotContains(t, d.errOut, "token")
					}
					assertRequestCount(t, d.s, r, status, path, 2, "one try and one retry")
					if r.redirect {
						first := dlAssetAt + id
						if r.browser {
							first = dlReleaseAt + asset
						}
						assert.Equal(t, 2, d.s.pathRequestCount(first),
							"the retry starts again at the first request")
					}
					d.assertNoSecrets(t, dlToken)
				})
			}
		}
	}
}

// TestDownloadErrorNamesEveryTokenSource asserts the source the error names
// for each place a token can come from, on the API endpoint and on the
// release download URL, and that the token appears nowhere in the output or
// in curl arguments.
func TestDownloadErrorNamesEveryTokenSource(t *testing.T) {
	t.Parallel()
	ghDir := ghStub(t, "#!/bin/sh\n"+
		"case \"$1 $2\" in\n"+
		"  'auth token') echo '"+dlToken+"' ;;\n"+
		"  *) exit 1 ;;\n"+
		"esac\n")
	sources := []struct {
		name   string
		env    []string
		prefix string
		want   string
	}{
		{name: "GHTOKEN", env: []string{"GH_TOKEN=" + dlToken}, want: "from GH_TOKEN"},
		{name: "GITHUBTOKEN", env: []string{"GITHUB_TOKEN=" + dlToken}, want: "from GITHUB_TOKEN"},
		{name: "GhAuthToken", prefix: ghDir, want: "from gh auth token"},
	}
	for _, r := range []dlRoute{dlRoutes[1], dlRoutes[2], dlRoutes[3]} {
		for _, src := range sources {
			t.Run(r.name+"/"+src.name, func(t *testing.T) {
				t.Parallel()
				asset := dlAssets()[0]
				d := runDownloadInstall(t, r, dlOpts{tokenEnv: src.env, pathDir: src.prefix}, func(s *stub) {
					id := dlAssetID(t, s, asset)
					s.failPathTimes(r.failingPath(s, id, asset), 5, 503)
				})
				require.NotEqual(t, 0, d.code)
				assert.Contains(t, d.errOut, "the download used the token "+src.want)
				if src.prefix != "" {
					assert.Contains(t, d.out, "using gh auth token")
				}
				assert.NotContains(t, d.out, dlToken)
				assert.NotContains(t, d.errOut, dlToken)
				assert.Zero(t, d.s.cdnAuthorizedCount(), "no request to the redirect URL carries an Authorization header")
				argv, err := os.ReadFile(d.argvLog)
				require.NoError(t, err)
				assert.NotContains(t, string(argv), dlToken)
			})
		}
	}
}

// TestDownloadErrorReportsTheSecondTry asserts mixed statuses: the error
// carries the status of the second try, and the tag hint appears only when
// both tries answered 404.
func TestDownloadErrorReportsTheSecondTry(t *testing.T) {
	t.Parallel()
	hint := "has no asset with this name"
	cases := []struct {
		name     string
		statuses []int
		want     string
		hint     bool
	}{
		{"NotFoundThenUnavailable", []int{404, 503}, "HTTP 503 after one retry", false},
		{"UnavailableThenNotFound", []int{503, 404}, "HTTP 404 after one retry", false},
		{"NotFoundTwice", []int{404, 404}, "HTTP 404 after one retry", true},
		{"NotFoundThenClosed", []int{404, dropConnection}, "no complete response after one retry", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			asset := dlAssets()[0]
			d := runDownloadInstall(t, dlRoutes[0], dlOpts{}, func(s *stub) {
				s.failPath(dlReleaseAt+asset, tc.statuses...)
			})
			require.NotEqual(t, 0, d.code)
			assert.Contains(t, d.errOut, tc.want)
			assert.Equal(t, tc.hint, strings.Contains(d.errOut, hint), d.errOut)
		})
	}
}

// TestDownloadFailureKeepsMetadataNote asserts the note about unreadable
// release metadata stays on a failed download that used a token, after the
// token source.
func TestDownloadFailureKeepsMetadataNote(t *testing.T) {
	t.Parallel()
	asset := dlAssets()[0]
	r := dlRoutes[1]
	d := runDownloadInstall(t, r, dlOpts{tokenEnv: dlTokenEnv(r)}, func(s *stub) {
		s.failPathTimes(dlReleaseAt+asset, 5, 404)
	})
	require.NotEqual(t, 0, d.code)
	assert.Contains(t, d.errOut, "the download used the token from GH_TOKEN) - or the GitHub token is expired or lacks access; release metadata was also unreadable (HTTP 403)")
}

// TestDownloadRetryPausesTwoSecondsByDefault asserts the pause before the
// retry is 2 seconds unless the test variable shortens it.
func TestDownloadRetryPausesTwoSecondsByDefault(t *testing.T) {
	t.Parallel()
	asset := dlAssets()[0]
	d := runDownloadInstall(t, dlRoutes[0], dlOpts{extraEnv: []string{"RANCHER_CLI_INSTALL_TEST_RETRY_DELAY="}}, func(s *stub) {
		s.failPath(dlReleaseAt+asset, 503)
	})
	require.Equalf(t, 0, d.code, "stderr: %s", d.errOut)
	assert.GreaterOrEqual(t, d.elapsed, 2*time.Second)
}

// TestDownloadRetryAppliesToPreAndLeavesDryRunAlone asserts a prerelease
// install retries like any other, and a dry run makes no asset request, so
// it prints no retry warning.
func TestDownloadRetryAppliesToPreAndLeavesDryRunAlone(t *testing.T) {
	t.Parallel()
	asset := dlAssets()[0]
	t.Run("Pre", func(t *testing.T) {
		t.Parallel()
		if _, err := exec.LookPath("jq"); err != nil {
			if err := exec.Command("python3", "-c", "pass").Run(); err != nil {
				t.Skip("--pre needs jq or python3 on the host")
			}
		}
		d := runDownloadInstall(t, dlRoutes[0], dlOpts{extraEnv: withParser(t)}, func(s *stub) {
			s.failPath(dlReleaseAt+"SHA256SUMS", 503)
		}, "--pre")
		require.Equalf(t, 0, d.code, "stderr: %s", d.errOut)
		d.assertInstalled(t)
		assert.Contains(t, d.errOut, "warning: download of SHA256SUMS failed (HTTP 503); retrying once")
	})
	t.Run("DryRun", func(t *testing.T) {
		t.Parallel()
		r := dlRoutes[2]
		d := runDownloadInstall(t, r, dlOpts{tokenEnv: dlTokenEnv(r)}, func(s *stub) {
			id := dlAssetID(t, s, asset)
			s.failPathTimes(dlAssetAt+id, 5, 503)
			s.failPathTimes(dlReleaseAt+asset, 5, 503)
		}, "--dry-run")
		require.Equalf(t, 0, d.code, "stderr: %s", d.errOut)
		assert.Contains(t, d.out, "asset: "+d.s.srv.URL+dlAssetAt)
		assert.NotContains(t, d.errOut, "retrying")
		assert.Zero(t, d.s.pathRequestCount(dlAssetAt+dlAssetID(t, d.s, asset)))
		assert.Zero(t, d.s.pathRequestCount(dlReleaseAt+asset))
	})
}

// unrelatedCAPath writes a PEM bundle with one fresh self-signed certificate
// that no stub uses.
func unrelatedCAPath(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "unrelated-ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

// TestDownloadTransportErrorKeepsOnlyCurlsFirstLine asserts the error of a
// download that fails with a certificate error carries curl's first message
// line and none of the explanation lines that follow it, so the error stays
// one line.
func TestDownloadTransportErrorKeepsOnlyCurlsFirstLine(t *testing.T) {
	t.Parallel()
	d := runDownloadInstall(t, dlRoutes[0], dlOpts{extraEnv: []string{
		// A CA bundle that does not hold the stub's certificate.
		"CURL_CA_BUNDLE=" + unrelatedCAPath(t),
	}}, func(*stub) {}, "--version", dlTag)
	require.NotEqual(t, 0, d.code)
	assert.Contains(t, d.errOut, "no complete response after one retry: curl exit 60")
	assert.NotContains(t, d.errOut, "More details")
	assert.NotContains(t, d.errOut, "legitimacy")
	var errorLines int
	for line := range strings.SplitSeq(d.errOut, "\n") {
		if strings.HasPrefix(line, "error:") {
			errorLines++
		}
	}
	assert.Equal(t, 1, errorLines, d.errOut)
}

// TestDownloadRedirectWithNoLocationSaysSo asserts a redirect status with no
// Location header fails with the status and a note that names the missing
// header.
func TestDownloadRedirectWithNoLocationSaysSo(t *testing.T) {
	t.Parallel()
	asset := dlAssets()[0]
	r := dlRoutes[2]
	d := runDownloadInstall(t, r, dlOpts{tokenEnv: dlTokenEnv(r)}, func(s *stub) {
		s.failPathTimes(dlAssetAt+dlAssetID(t, s, asset), 5, 302)
	})
	require.NotEqual(t, 0, d.code)
	assert.Contains(t, d.errOut, "(HTTP 302 after one retry; redirect with no Location; the download used the token from GH_TOKEN)")
}
