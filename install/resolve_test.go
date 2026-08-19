package install_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rawInstallURL = "https://raw.githubusercontent.com/rancher/rancher-cli/main/install.sh"

func TestDryRunResolvesStableLatest(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	out, _, code := runInstall(t, s, nil, "--dry-run")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "tag: v1.2.3")
	assert.Contains(t, out, "asset: "+s.srv.URL+"/rancher/rancher-cli/releases/download/v1.2.3/SHA256SUMS")
}

// TestStable404WithTokenHintsAtScope covers F2: when a token is present and
// the stable-latest endpoint returns 404, the error must suggest the token may
// lack access (scope/SSO) — not just steer the user toward --pre.
func TestStable404WithTokenHintsAtScope(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	// No release registered: stable-latest returns 404.
	_, errOut, code := runInstall(t, s, []string{
		"GH_TOKEN=test-token",
		"PATH=" + toolPath(t, baseTools...),
	}, "--dry-run")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "use --pre or --version")
	assert.Contains(t, errOut, "token lacks access",
		"404 with a token set must hint at scope/SSO as a possible cause")
}

func TestDryRunNoStableRelease(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	_, errOut, code := runInstall(t, s, nil, "--dry-run")
	require.NotEqual(t, 0, code)
	// Anonymous 404 is ambiguous (empty public repo vs unauthenticated
	// internal repo); the message must name both readings and the way out
	// of each.
	assert.Contains(t, errOut, "no stable release found")
	assert.Contains(t, errOut, "gh auth login or GH_TOKEN")
	assert.Contains(t, errOut, "use --pre or --version")
}

func TestPreSkipsDraftsAcrossPages(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	for range 100 { // a full first page of drafts
		writeRelease(s, "v9.9.9-alpha.1", true)
	}
	writeRelease(s, "v0.1.0-alpha.1", false) // page 2
	out, _, code := runInstall(t, s, withParser(t), "--dry-run", "--pre")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "tag: v0.1.0-alpha.1")
}

func TestPreExhaustedListingErrors(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v9.9.9-alpha.1", true)
	_, errOut, code := runInstall(t, s, withParser(t), "--dry-run", "--pre")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "no published release found")
}

func TestTagGrammar(t *testing.T) {
	t.Parallel()
	accept := []string{"v0.1.0", "v1.0.0", "v10.2.30", "v1.0.0-alpha.1", "v1.0.0-rc.2"}
	reject := []string{"v01.0.0", "v1.0.0-.", "v1.0.0-alpha..1", "v1.0.0-alpha-1",
		"v1.0.0+meta", "v1/evil", "1.0.0", "v1.0", "v1.0.0-alpha"}
	for _, tag := range accept {
		s := newStub(t)
		writeRelease(s, tag, false)
		out, _, code := runInstall(t, s, nil, "--dry-run", "--version", tag)
		assert.Equalf(t, 0, code, "tag %s must be accepted", tag)
		assert.Contains(t, out, "tag: "+tag)
	}
	for _, tag := range reject {
		s := newStub(t)
		_, errOut, code := runInstall(t, s, nil, "--dry-run", "--version", tag)
		assert.NotEqualf(t, 0, code, "tag %s must be rejected", tag)
		assert.Contains(t, errOut, "tag grammar")
	}
}

func TestVersionAndPreAreExclusive(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	_, errOut, code := runInstall(t, s, nil, "--dry-run", "--version", "v1.0.0", "--pre")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "mutually exclusive")
}

func TestDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	tmp := t.TempDir()
	before := digestTree(t, home) + digestTree(t, tmp)
	_, _, code := runInstall(t, s, append(envHome(home), envTmpDir(tmp)...), "--dry-run")
	require.Equal(t, 0, code)
	assert.Equal(t, before, digestTree(t, home)+digestTree(t, tmp),
		"dry-run must not touch the filesystem — HOME or TMPDIR")
}

// digestTree captures type, mode, symlink target, and regular-file content
// hash for every entry — mode+path alone would miss a rewritten receipt or
// binary.
func digestTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(p + "|" + info.Mode().String())
		if info.Mode()&os.ModeSymlink != 0 {
			target, lerr := os.Readlink(p)
			require.NoError(t, lerr)
			b.WriteString("|->" + target)
		} else if info.Mode().IsRegular() {
			raw, rerr := os.ReadFile(p)
			require.NoError(t, rerr)
			sum := sha256.Sum256(raw)
			b.WriteString("|" + hex.EncodeToString(sum[:]))
		}
		b.WriteString("\n")
		return nil
	}))
	return b.String()
}

func TestDryRunWritesNothingOnExistingInstall(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)
	writeRelease(s, "v1.3.0", false)
	s.setLatest("v1.3.0")
	// A TMPDIR of its own, digested with HOME: the real install above cleans up
	// its own work dir, and pinning a fresh one keeps the dry-run comparison
	// about the dry run alone.
	tmp := t.TempDir()
	before := digestTree(t, home) + digestTree(t, tmp)
	_, _, code = runInstall(t, s, append(envHome(home), envTmpDir(tmp)...), "--dry-run")
	require.Equal(t, 0, code)
	assert.Equal(t, before, digestTree(t, home)+digestTree(t, tmp),
		"dry-run over an existing installation must not touch receipt, links, version dirs, or TMPDIR")
}

func TestLatestTransportFailureIsNotNoStableRelease(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	// Point at a closed port: curl fails at the transport layer.
	_, errOut, code := runInstall(t, s,
		[]string{"RANCHER_CLI_INSTALL_BASE_URL=https://127.0.0.1:1"}, "--dry-run")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "release resolution request failed")
	assert.NotContains(t, errOut, "no stable release",
		"a network error must never masquerade as release absence")
}

func TestLatestHTTPErrorIsNotNoStableRelease(t *testing.T) {
	t.Parallel()
	for _, status := range []int{401, 500} {
		s := newStub(t)
		s.setLatestStatus(status)
		_, errOut, code := runInstall(t, s, nil, "--dry-run")
		require.NotEqualf(t, 0, code, "HTTP %d must fail", status)
		assert.Contains(t, errOut, "release resolution failed (HTTP")
		assert.NotContains(t, errOut, "no stable release",
			"only GitHub's 404 means release absence")
	}
}

func TestNewlineBinDirSkipsReceiptWithWarning(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	binDir := filepath.Join(home, "line1\nline2")
	_, errOut, code := runInstall(t, s, envHome(home), "--bin-dir", binDir)
	require.Equal(t, 0, code, "newline bin-dir installs fine, only the receipt is skipped")
	assert.Contains(t, errOut, "receipt skipped: value contains unsupported control bytes")
	assert.NotContains(t, errOut, "not found",
		"the guard branch must be reached by detection, not by a missing tool failing open")
	_, err := os.Stat(filepath.Join(home, ".config", "rancher-cli", "install-receipt.json"))
	assert.True(t, os.IsNotExist(err), "no receipt may be written for an unrepresentable path")
}

// Parser-path matrix: each optional parser is forced via toolPath, and raw
// fixtures cover layouts json.Encoder never produces plus adversarial
// lookalikes inside release-body strings.
func TestPreParserMatrix(t *testing.T) {
	t.Parallel()
	pretty := []byte(`[
  {
    "body": "release notes mentioning \"tag_name\": \"vHACK\" and \"draft\": false",
    "draft": true,
    "tag_name": "v9.9.9-alpha.1"
  },
  {
    "tag_name": "v0.1.0-alpha.1",
    "draft": false
  }
]`)
	compactReordered := []byte(`[{"draft":true,"tag_name":"v9.9.9-alpha.1"},{"draft":false,"tag_name":"v0.1.0-alpha.1"}]`)
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{"jq only", append(append([]string{}, baseTools...), "jq")},
		{"python3 only", append(append([]string{}, baseTools...), "python3")},
	} {
		for _, fixture := range []struct {
			name string
			body []byte
		}{{"pretty", pretty}, {"compact reordered", compactReordered}} {
			t.Run(tc.name+"/"+fixture.name, func(t *testing.T) {
				s := newStub(t)
				s.setRawListing([][]byte{fixture.body})
				writeRelease(s, "v0.1.0-alpha.1", false) // registers assets for the tag
				out, errOut, code := runInstall(t, s,
					[]string{"PATH=" + toolPath(t, tc.tools...)}, "--dry-run", "--pre")
				require.Equalf(t, 0, code, "stderr: %s", errOut)
				assert.Contains(t, out, "tag: v0.1.0-alpha.1",
					"draft lookalikes in body strings must not confuse the parser")
			})
		}
	}
}

func TestPreWithoutAnyParserErrors(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v0.1.0-alpha.1", false)
	_, errOut, code := runInstall(t, s,
		[]string{"PATH=" + toolPath(t, baseTools...)}, "--dry-run", "--pre")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "--pre needs jq or python3")
}

// Platform map, table-tested via a fake uname prepended to PATH.
func TestPlatformMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kernel, machine string
		want            string // "" = must be rejected
	}{
		{"Linux", "x86_64", "linux/amd64"},
		{"Linux", "amd64", "linux/amd64"},
		{"Linux", "aarch64", "linux/arm64"},
		{"Linux", "arm64", "linux/arm64"},
		{"Linux", "s390x", "linux/s390x"},
		{"Darwin", "x86_64", "darwin/amd64"},
		{"Darwin", "arm64", "darwin/arm64"},
		{"Darwin", "s390x", ""},
		{"MINGW64_NT-10.0", "x86_64", ""},
		{"Linux", "riscv64", ""},
	}
	for _, tc := range cases {
		s := newStub(t)
		writeRelease(s, "v1.2.3", false)
		s.setLatest("v1.2.3")
		env := []string{"PATH=" + fakeUname(t, tc.kernel, tc.machine) + ":" + toolPath(t, baseTools...)}
		out, errOut, code := runInstall(t, s, env, "--dry-run")
		if tc.want == "" {
			assert.NotEqualf(t, 0, code, "%s/%s must be rejected", tc.kernel, tc.machine)
			assert.Contains(t, errOut, "unsupported platform")
			continue
		}
		require.Equalf(t, 0, code, "%s/%s stderr: %s", tc.kernel, tc.machine, errOut)
		assert.Contains(t, out, "platform: "+tc.want)
	}
}

// The mv capability probe must refuse before any mutation, and must not run at
// all for a dry run (which renames nothing).
func TestMvCapabilityProbePrecedesAnyMutation(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	mvDir, mvEnv := mvWithoutNoFollow(t)
	env := append([]string{
		"PATH=" + mvDir + ":" + toolPath(t, baseTools...),
		"HOME=" + home,
	}, mvEnv...)
	before := digestTree(t, home)
	_, errOut, code := runInstall(t, s, env)
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "mv does not support the required no-follow replace")
	assert.Equal(t, before, digestTree(t, home),
		"the probe must refuse before staging, links, or the receipt exist")
	_, errOut, code = runInstall(t, s, env, "--dry-run")
	assert.Equalf(t, 0, code, "dry-run needs no mv capability; stderr: %s", errOut)
}

// TestAuthenticatedBootstrapSnippet mirrors the internal-phase
// download-then-run bootstrap in REFERENCE.md's Defensive bootstrap section
// (moved from README 2026-08-12) and runs it against a stub serving
// install.sh, asserting the token arrives as an Authorization header and
// never in argv.
func TestAuthenticatedBootstrapSnippet(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("../install.sh")
	require.NoError(t, err)
	var (
		authMu  sync.Mutex
		gotAuth string
	)
	raw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		gotAuth = r.Header.Get("Authorization")
		authMu.Unlock()
		_, _ = w.Write(script)
	}))
	defer raw.Close()
	s := newStub(t)
	// CURL_CA_BUNDLE names exactly ONE file: both stub certs go into a
	// single concatenated bundle.
	rawPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw.Certificate().Raw})
	stubPEM, err := os.ReadFile(s.caPath)
	require.NoError(t, err)
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	require.NoError(t, os.WriteFile(bundle, append(rawPEM, stubPEM...), 0o600))
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	// The REFERENCE.md defensive-bootstrap snippet with the raw URL swapped
	// for the stub's and a --dry-run appended; everything else byte-identical.
	snippet := `(
  t="$(mktemp)" || exit 1
  trap 'rc=$?; rm -f "$t"; exit "$rc"' 0
  trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM
  printf 'header = "Authorization: Bearer %s"\n' "${GH_TOKEN:-$GITHUB_TOKEN}" | \
    curl --config - --proto '=https' --tlsv1.2 -fsSL \
      -o "$t" \
      ` + raw.URL + `/install.sh \
    && GH_TOKEN="${GH_TOKEN:-$GITHUB_TOKEN}" sh "$t" --dry-run
)`
	assertSnippetMatchesDoc(t, "REFERENCE.md", `printf 'header = "Authorization: Bearer %s"`,
		snippet, func(s string) string {
			return strings.NewReplacer(
				raw.URL+"/install.sh", rawInstallURL,
				`sh "$t" --dry-run`, `sh "$t"`,
			).Replace(s)
		})
	// Recording curl wrapper: logs every argv it receives, then execs the
	// real curl — the seam proving the token travels ONLY via stdin config.
	wrapDir, argvLog, wrapEnv := recordingCurlWrapper(t)

	cmd := exec.Command("sh", "-c", snippet) // fixed test literal; no user input reaches the shell
	cmd.Env = append([]string{
		"HOME=" + t.TempDir(),
		"PATH=" + wrapDir + ":" + toolPath(t, baseTools...),
		"GH_TOKEN=test-token-123",
		"RANCHER_CLI_INSTALL_BASE_URL=" + s.srv.URL,
		"CURL_CA_BUNDLE=" + bundle,
	}, wrapEnv...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "bootstrap snippet failed: %s", out)
	authMu.Lock()
	auth := gotAuth
	authMu.Unlock()
	assert.Equal(t, "Bearer test-token-123", auth, "token must arrive as a header")
	assert.Contains(t, string(out), "tag: v1.2.3")
	assert.NotContains(t, string(out), "test-token-123",
		"the token must never appear in installer output")
	argv, err := os.ReadFile(argvLog)
	require.NoError(t, err, "the curl wrapper must have recorded invocations")
	assert.NotContains(t, string(argv), "test-token-123",
		"the token must never appear in any curl argv — stdin config only")
}

// TestBootstrapFetchFailureRunsNothing runs the ANONYMOUS README one-liner
// with an unreachable URL. The pipe form's documented trade-off: sh reads
// EOF and exits 0 (POSIX sh has no pipefail), curl's error is the only
// signal, and nothing executes — no filesystem effect anywhere.
func TestBootstrapFetchFailureRunsNothing(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	home := t.TempDir()
	snippet := `curl --proto '=https' --tlsv1.2 -fsSL https://127.0.0.1:1/install.sh | sh`
	assertSnippetMatchesDoc(t, "README.md", "curl --proto '=https' --tlsv1.2 -fsSL https://",
		snippet, func(s string) string {
			return strings.Replace(s, "https://127.0.0.1:1/install.sh", rawInstallURL, 1)
		})
	cmd := exec.Command("sh", "-c", snippet) // fixed test literal
	cmd.Env = []string{"HOME=" + home, "PATH=" + toolPath(t, baseTools...), "TMPDIR=" + tmp}
	before := digestTree(t, home)
	out, runErr := cmd.CombinedOutput()
	require.NoErrorf(t, runErr, "the pipe form exits 0 on a fetch failure — sh of empty input; output: %s", out)
	assert.Contains(t, string(out), "curl", "curl's error text is the user's only failure signal")
	assert.Equal(t, before, digestTree(t, home), "nothing may execute on a failed fetch")
	entries, readErr := os.ReadDir(tmp)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no temp residue on a failed fetch")
}

// TestBootstrapPropagatesInstallerFailure serves the real install.sh through
// the README one-liner but has it fail (grammar-rejected --version): the
// pipeline's exit status is sh's, so the installer's failure propagates.
func TestBootstrapPropagatesInstallerFailure(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("../install.sh")
	require.NoError(t, err)
	raw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(script)
	}))
	defer raw.Close()
	rawPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw.Certificate().Raw})
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(bundle, rawPEM, 0o600))
	tmp := t.TempDir()
	// The README one-liner with the raw URL swapped for the stub's and the
	// failing flag passed through sh -s --; otherwise identical.
	snippet := `curl --proto '=https' --tlsv1.2 -fsSL ` + raw.URL + `/install.sh | sh -s -- --version v1/evil`
	assertSnippetMatchesDoc(t, "README.md", "curl --proto '=https' --tlsv1.2 -fsSL https://",
		snippet, func(s string) string {
			return strings.NewReplacer(
				raw.URL+"/install.sh", rawInstallURL,
				"| sh -s -- --version v1/evil", "| sh",
			).Replace(s)
		})
	cmd := exec.Command("sh", "-c", snippet) // fixed test literal
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + toolPath(t, baseTools...),
		"TMPDIR=" + tmp, "CURL_CA_BUNDLE=" + bundle}
	out, runErr := cmd.CombinedOutput()
	require.Errorf(t, runErr, "installer failure must propagate through the pipe, output: %s", out)
	assert.Contains(t, string(out), "tag grammar")
	entries, readErr := os.ReadDir(tmp)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "the installer must clean up its work dir on failure")
}

func TestDryRunResolvesVersionFromEnv(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	out, _, code := runInstall(t, s, []string{"RANCHER_CLI_VERSION=v1.2.3"}, "--dry-run")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "tag: v1.2.3")
}

func TestVersionFlagBeatsEnv(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	out, _, code := runInstall(t, s, []string{"RANCHER_CLI_VERSION=v1.2.3"}, "--dry-run", "--version", "v1.3.0")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "tag: v1.3.0")
	assert.NotContains(t, out, "v1.2.3")
}

func TestBinDirFromEnv(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	home := t.TempDir()
	binDir := filepath.Join(home, "custom-bin")
	out, _, code := runInstall(t, s,
		append(envHome(home), "RANCHER_CLI_INSTALL_BIN_DIR="+binDir), "--dry-run", "--version", "v1.2.3")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "bin-dir: "+binDir)
}

// TestGhTokenFallback is a table-driven test covering the four distinct
// token-source scenarios. Each row is independently parallel; all current
// assertions are preserved.
//
// Row names:
//   - NoGhStaysAnonymous    — no gh binary, no env token: requests anonymous
//   - GhTokenFallback       — gh supplies a token; header set, never in argv
//   - EnvTokenBeatsGhFallback — GH_TOKEN env var takes precedence; gh not queried
//   - WrongHostDegrades     — gh exits non-zero for the host; install degrades silently
func TestGhTokenFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		extraEnv          []string
		ghScript          string // "" = no gh on PATH; logging script uses RANCHER_TEST_GH_ARGV_LOG
		wantHeader        string // expected Authorization header on the stub server
		wantOut           string // must appear in combined output (empty = skip)
		wantNoOut         string // must NOT appear in combined output (empty = skip)
		wantGhContains    string // must appear in gh argv log; %s = stub hostname (empty = skip)
		wantGhNotContains string // must NOT appear in gh argv log (empty = skip)
		wantNoTokenInArgv string // token that must not appear in any curl argv (empty = skip)
		wantNoOutMsg      string // failure message for wantNoOut assertion
	}{
		{
			// No gh binary, no token env vars: all requests are anonymous.
			name: "NoGhStaysAnonymous",
		},
		{
			// gh supplies a token via auth token; it reaches the server as a
			// header and never appears in any curl argv or installer output.
			name: "GhTokenFallback",
			ghScript: "#!/bin/sh\n" +
				"echo \"$@\" >> \"$RANCHER_TEST_GH_ARGV_LOG\"\n" +
				"case \"$1 $2\" in\n" +
				"  'auth token') echo 'stub-gh-token-123' ;;\n" +
				"  *) exit 1 ;;\n" +
				"esac\n",
			wantHeader:        "Bearer stub-gh-token-123",
			wantOut:           "using gh auth token",
			wantNoOut:         "stub-gh-token-123",
			wantNoOutMsg:      "token must never appear in installer output",
			wantGhContains:    "auth token --hostname %s",
			wantNoTokenInArgv: "stub-gh-token-123",
		},
		{
			// GH_TOKEN is set: resolve_token returns immediately; gh is never
			// queried for auth token, only by gh_capable.
			name:     "EnvTokenBeatsGhFallback",
			extraEnv: []string{"GH_TOKEN=env-token-456"},
			ghScript: "#!/bin/sh\n" +
				"echo \"$@\" >> \"$RANCHER_TEST_GH_ARGV_LOG\"\n" +
				"exit 1\n",
			wantHeader:        "Bearer env-token-456",
			wantNoOut:         "using gh auth token",
			wantNoOutMsg:      "env-var token must suppress the gh fallback notice",
			wantGhNotContains: "auth token",
		},
		{
			// gh is present but exits non-zero for the requested hostname
			// (user authenticated against a different host): install degrades
			// to anonymous without failing.
			name: "WrongHostDegrades",
			ghScript: "#!/bin/sh\n" +
				"echo \"$@\" >> \"$RANCHER_TEST_GH_ARGV_LOG\"\n" +
				"exit 1\n",
			wantHeader:     "",
			wantNoOut:      "using gh auth token",
			wantNoOutMsg:   "no fallback notice when gh could not supply a token",
			wantGhContains: "auth token --hostname %s",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeRelease(s, "v1.2.3", false)
			s.setLatest("v1.2.3")

			u, err := url.Parse(s.srv.URL)
			require.NoError(t, err)
			stubHost := u.Hostname()

			var paths []string
			var env []string
			env = append(env, tc.extraEnv...)

			var ghLog string
			if tc.ghScript != "" {
				ghLog = filepath.Join(t.TempDir(), "gh.log")
				paths = append(paths, ghStub(t, tc.ghScript))
				env = append(env, "RANCHER_TEST_GH_ARGV_LOG="+ghLog)
			}

			var argvLog string
			if tc.wantNoTokenInArgv != "" {
				wrapDir, al, wrapEnv := recordingCurlWrapper(t)
				paths = append(paths, wrapDir)
				argvLog = al
				env = append(env, wrapEnv...)
			}

			paths = append(paths, toolPath(t, baseTools...))
			env = append(env, "PATH="+strings.Join(paths, ":"))

			out, errOut, code := runInstall(t, s, env)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			combined := out + errOut

			assert.Equal(t, tc.wantHeader, s.lastAuthHeader())
			if tc.wantOut != "" {
				assert.Contains(t, combined, tc.wantOut)
			}
			if tc.wantNoOut != "" {
				assert.NotContains(t, combined, tc.wantNoOut, tc.wantNoOutMsg)
			}

			if tc.ghScript != "" {
				ghArgv, readErr := os.ReadFile(ghLog)
				require.NoError(t, readErr, "gh stub must have been invoked")
				if tc.wantGhContains != "" {
					assert.Contains(t, string(ghArgv),
						strings.Replace(tc.wantGhContains, "%s", stubHost, 1),
						"resolve_token must scope gh auth token to the install server's hostname")
				}
				if tc.wantGhNotContains != "" {
					assert.NotContains(t, string(ghArgv), tc.wantGhNotContains,
						"gh auth token must not be consulted when GH_TOKEN is set or gh exits non-zero")
				}
			}

			if tc.wantNoTokenInArgv != "" {
				argv, readErr := os.ReadFile(argvLog)
				require.NoError(t, readErr, "curl wrapper must have recorded invocations")
				assert.NotContains(t, string(argv), tc.wantNoTokenInArgv,
					"token must never appear in any curl argv — stdin config only")
			}
		})
	}
}

// The grammar has two executable homes (scripts/check-release-tag and the
// standalone install.sh); this pins them byte-identical and runs the
// conformance table through the guard script too.
func TestTagGrammarSharedSource(t *testing.T) {
	t.Parallel()
	extract := func(path string) string {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		for line := range strings.SplitSeq(string(raw), "\n") {
			if strings.HasPrefix(line, "TAG_RE=") {
				return line
			}
		}
		t.Fatalf("no TAG_RE line in %s", path)
		return ""
	}
	assert.Equal(t, extract("../scripts/check-release-tag"), extract("../install.sh"),
		"the tag grammar must stay byte-identical in both scripts")

	accept := []string{"v0.1.0", "v1.0.0", "v10.2.30", "v1.0.0-alpha.1", "v1.0.0-rc.2"}
	reject := []string{"v01.0.0", "v1.0.0-.", "v1.0.0-alpha..1", "v1.0.0-alpha-1",
		"v1.0.0+meta", "v1/evil", "1.0.0", "v1.0", "v1.0.0-alpha"}
	guard, err := filepath.Abs("../scripts/check-release-tag")
	require.NoError(t, err)
	for _, tag := range accept {
		require.NoErrorf(t, exec.Command("sh", guard, tag).Run(), "guard must accept %s", tag)
	}
	for _, tag := range reject {
		assert.Errorf(t, exec.Command("sh", guard, tag).Run(), "guard must reject %s", tag)
	}
}

// TestHostnameDerivationForGhToken verifies that resolve_token derives the
// correct hostname from BASE_URL variants: plain host:port, userinfo
// (user:pass@host), userinfo with @ in the password, and bracketed-IPv6 —
// all must call gh auth token with only the bare hostname.
func TestHostnameDerivationForGhToken(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	u, err := url.Parse(s.srv.URL)
	require.NoError(t, err)
	port := u.Port()

	cases := []struct {
		name     string
		baseURL  string
		wantHost string
	}{
		{
			name:     "plain",
			baseURL:  s.srv.URL,
			wantHost: u.Hostname(),
		},
		{
			name:     "userinfo",
			baseURL:  "https://user:pass@127.0.0.1:" + port,
			wantHost: "127.0.0.1",
		},
		{
			name:     "IPv6-bracketed",
			baseURL:  "https://[::1]:" + port,
			wantHost: "::1",
		},
		{
			// Password contains @ — only the greedy ##*@ strip reaches the
			// real host; the non-greedy #*@ form yields "ssw0rd@example.com".
			name:     "userinfo-at-in-password",
			baseURL:  "https://user:p@ssw0rd@example.com:8443/path",
			wantHost: "example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ghLog := filepath.Join(t.TempDir(), "gh.log")
			// Stub logs all calls and exits 1 so resolve_token degrades;
			// --dry-run + explicit --version skips all HTTP activity so the
			// unreachable BASE_URL variants (IPv6, userinfo) still succeed.
			ghDir := ghStub(t, "#!/bin/sh\necho \"$@\" >> \"$RANCHER_TEST_GH_ARGV_LOG\"\nexit 1\n")
			runInstall(t, s, []string{
				"PATH=" + ghDir + ":" + toolPath(t, baseTools...),
				"RANCHER_TEST_GH_ARGV_LOG=" + ghLog,
				"RANCHER_CLI_INSTALL_BASE_URL=" + tc.baseURL,
			}, "--dry-run", "--version", "v1.2.3")
			ghArgv, err := os.ReadFile(ghLog)
			require.NoError(t, err, "gh stub must have been invoked by resolve_token")
			assert.Contains(t, string(ghArgv), "auth token --hostname "+tc.wantHost,
				"resolve_token must strip userinfo, port, and brackets from BASE_URL")
		})
	}
}

// TestGhBootstrapSnippet runs the README internal-phase one-liner against a
// stub gh that serves install.sh, verifying it resolves a release and exits 0.
func TestGhBootstrapSnippet(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	// Early access ships prereleases only, and the README one-liner carries
	// --pre — the fixture mirrors that reality (--pre needs a JSON parser
	// on PATH, hence jq below).
	writeRelease(s, "v0.1.0-alpha.1", false)

	scriptPath, err := filepath.Abs("../install.sh")
	require.NoError(t, err)

	// Stub gh: serves install.sh for the README contents-API path; fails
	// for all other subcommands so resolve_token degrades to anonymous and
	// gh_capable stays false.
	ghDir := ghStub(t, "#!/bin/sh\n"+
		"case \"$1 $2\" in\n"+
		"  'api repos/rancher/rancher-cli/contents/install.sh?ref=main') cat \"$RANCHER_TEST_INSTALL_SCRIPT\" ;;\n"+
		"  *) exit 1 ;;\n"+
		"esac\n")

	// The README one-liner, byte-identical, with --dry-run appended to its
	// flags — the stub gh serves install.sh for the contents-API call;
	// RANCHER_CLI_INSTALL_BASE_URL redirects install.sh's curl calls.
	snippet := `gh api "repos/rancher/rancher-cli/contents/install.sh?ref=main" -H "Accept: application/vnd.github.raw" | sh -s -- --pre --dry-run`
	assertSnippetMatchesDoc(t, "README.md", `gh api "repos/rancher/rancher-cli/contents/install.sh?ref=main"`,
		snippet, func(s string) string { return strings.Replace(s, "| sh -s -- --pre --dry-run", "| sh -s -- --pre", 1) })

	cmd := exec.Command("sh", "-c", snippet) // fixed test literal
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"PATH=" + ghDir + ":" + toolPath(t, append(append([]string{}, baseTools...), "jq")...),
		"RANCHER_CLI_INSTALL_BASE_URL=" + s.srv.URL,
		"CURL_CA_BUNDLE=" + s.caPath,
		"RANCHER_TEST_INSTALL_SCRIPT=" + scriptPath,
	}
	out, runErr := cmd.CombinedOutput()
	require.NoErrorf(t, runErr, "README bootstrap snippet failed: %s", out)
	assert.Contains(t, string(out), "tag: v0.1.0-alpha.1")
}
