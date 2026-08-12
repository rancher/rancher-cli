package install_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestDryRunNoStableRelease(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	_, errOut, code := runInstall(t, s, nil, "--dry-run")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "no stable release yet")
}

func TestPreSkipsDraftsAcrossPages(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	for i := 0; i < 100; i++ { // a full first page of drafts
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

// TestAuthenticatedBootstrapSnippet extracts the internal-phase bootstrap
// from the README and runs it against a stub serving install.sh, asserting
// the token arrives as an Authorization header and never in argv.
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

	// The README snippet with the raw URL swapped for the stub's and a
	// --dry-run appended; everything else byte-identical.
	snippet := `(
  t="$(mktemp)" || exit 1
  trap 'rc=$?; rm -f "$t"; exit "$rc"' 0
  trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM
  printf 'header = "Authorization: Bearer %s"\n' "$GH_TOKEN" | \
    curl --config - --proto '=https' --tlsv1.2 -fsSL \
      -o "$t" \
      ` + raw.URL + `/install.sh \
    && GH_TOKEN="$GH_TOKEN" sh "$t" --dry-run
)`
	// Recording curl wrapper: logs every argv it receives, then execs the
	// real curl — the seam proving the token travels ONLY via stdin config.
	// Paths reach the wrapper via ENV, quoted at expansion: interpolating
	// them into the script source would break under a space-bearing TMPDIR
	// or curl path (which the deliberately space-bearing wrapDir pins).
	realCurl, err := exec.LookPath("curl")
	require.NoError(t, err)
	wrapDir := filepath.Join(t.TempDir(), "with space")
	require.NoError(t, os.MkdirAll(wrapDir, 0o755))
	argvLog := filepath.Join(wrapDir, "argv.log")
	wrapper := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$RANCHER_TEST_ARGV_LOG\"\n" +
		"exec \"$RANCHER_TEST_REAL_CURL\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(wrapDir, "curl"), []byte(wrapper), 0o755))

	cmd := exec.Command("sh", "-c", snippet) // fixed test literal; no user input reaches the shell
	cmd.Env = []string{"HOME=" + t.TempDir(),
		"PATH=" + wrapDir + ":" + toolPath(t, baseTools...),
		"RANCHER_TEST_ARGV_LOG=" + argvLog,
		"RANCHER_TEST_REAL_CURL=" + realCurl,
		"GH_TOKEN=test-token-123",
		"RANCHER_CLI_INSTALL_BASE_URL=" + s.srv.URL,
		"CURL_CA_BUNDLE=" + bundle}
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

// The grammar has two executable homes (scripts/check-release-tag and the
// standalone install.sh); this pins them byte-identical and runs the
// conformance table through the guard script too.
func TestTagGrammarSharedSource(t *testing.T) {
	t.Parallel()
	extract := func(path string) string {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, line := range strings.Split(string(raw), "\n") {
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
		assert.NoErrorf(t, exec.Command("sh", guard, tag).Run(), "guard must accept %s", tag)
	}
	for _, tag := range reject {
		assert.Errorf(t, exec.Command("sh", guard, tag).Run(), "guard must reject %s", tag)
	}
}
