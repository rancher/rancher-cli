package install_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stub serves both the github.com shapes (releases/latest redirect,
// releases/download assets) and the api.github.com shape (releases listing)
// under one base URL, which install.sh's RANCHER_CLI_INSTALL_BASE_URL
// override points both bases at.
type stub struct {
	srv    *httptest.Server
	caPath string // PEM of the stub's TLS cert, for CURL_CA_BUNDLE

	// mu guards everything below: tests configure the stub from the test
	// goroutine while handler goroutines read — without the mutex those are
	// data races under the mandated -race.
	mu           sync.Mutex
	releases     []map[string]any  // newest first, like the API
	rawListing   [][]byte          // raw per-page bodies; overrides releases when set
	assets       map[string][]byte // "<tag>/<name>" -> content
	latest       string            // tag the /releases/latest redirect names; "" = 404
	latestStatus int               // when non-zero, /releases/latest returns this status instead
	authHeader   string            // last Authorization header seen on asset download requests
}

func (s *stub) setLatestStatus(code int) { s.mu.Lock(); defer s.mu.Unlock(); s.latestStatus = code }

func (s *stub) setLatest(tag string)   { s.mu.Lock(); defer s.mu.Unlock(); s.latest = tag }
func (s *stub) lastAuthHeader() string { s.mu.Lock(); defer s.mu.Unlock(); return s.authHeader }
func (s *stub) setRawListing(pages [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawListing = pages
}

// setAsset and asset are the asset-store seam for the download, checksum, and
// tamper tests; the resolution tests here only need writeRelease.
func (s *stub) setAsset(key string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assets[key] = body
}

func (s *stub) asset(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.assets[key]
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{assets: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/rancher/rancher-cli/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		latest, status := s.latest, s.latestStatus
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if latest == "" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, s.srv.URL+"/rancher/rancher-cli/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("/rancher/rancher-cli/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		key := strings.TrimPrefix(r.URL.Path, "/rancher/rancher-cli/releases/download/")
		s.mu.Lock()
		s.authHeader = auth
		body, ok := s.assets[key]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/repos/rancher/rancher-cli/releases", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.rawListing != nil {
			if page <= len(s.rawListing) {
				_, _ = w.Write(s.rawListing[page-1])
			} else {
				_, _ = w.Write([]byte("[]"))
			}
			return
		}
		per := 100
		start, end := (page-1)*per, page*per
		if start > len(s.releases) {
			start = len(s.releases)
		}
		if end > len(s.releases) {
			end = len(s.releases)
		}
		_ = json.NewEncoder(w).Encode(s.releases[start:end])
	})
	// TLS, not plain HTTP: install.sh's curl runs with --proto '=https'
	// --tlsv1.2 UNCHANGED, trusting the stub via CURL_CA_BUNDLE.
	s.srv = httptest.NewTLSServer(mux)
	t.Cleanup(s.srv.Close)
	s.caPath = filepath.Join(t.TempDir(), "stub-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(s.caPath, pemBytes, 0o600))
	return s
}

// baseTools is every external tool install.sh may invoke, minus the optional
// ones tests control explicitly (jq, python3, gh).
var baseTools = []string{"sh", "curl", "uname", "mktemp", "chmod", "ln",
	"readlink", "grep", "awk", "tr", "mv", "cp", "rm", "rmdir", "mkdir",
	"date", "sleep", "cat", "sha256sum", "shasum", "dirname", "basename"}

// toolPath builds a PATH dir symlinking exactly the named tools (resolved
// from the host PATH; missing hosts' tools are skipped), so the presence or
// absence of optional tools is deterministic regardless of the host.
func toolPath(t *testing.T, tools ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		if p, err := exec.LookPath(tool); err == nil {
			require.NoError(t, os.Symlink(p, filepath.Join(dir, tool)))
		}
	}
	return dir
}

// fakeUname returns a dir whose uname reports the given kernel/machine —
// the seam for table-testing the platform map beyond the host arch.
func fakeUname(t *testing.T, kernel, machine string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n  -s) echo \"" + kernel + "\" ;;\n  -m) echo \"" + machine + "\" ;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755))
	return dir
}

// writeRelease registers a release and, when published, its three assets:
// fake "binaries" are tiny sh scripts so the epilogue's --version works.
func writeRelease(s *stub, tag string, draft bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, map[string]any{"tag_name": tag, "draft": draft})
	if draft {
		return
	}
	var sums strings.Builder
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		for _, plat := range []string{"linux-amd64", "linux-arm64", "linux-s390x", "darwin-amd64", "darwin-arm64"} {
			asset := name + "-" + plat
			body := []byte("#!/bin/sh\necho \"" + name + " version " + tag + "\"\n")
			s.assets[tag+"/"+asset] = body
			sum := sha256.Sum256(body)
			fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		}
	}
	// Direct assignment: s.mu is already held (a setAsset call here would
	// self-deadlock on the non-reentrant mutex).
	s.assets[tag+"/SHA256SUMS"] = []byte(sums.String())
}

// runInstall executes the real install.sh under sh with a hermetic HOME and
// a controlled PATH, pointed at the stub.
func runInstall(t *testing.T, s *stub, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	home := t.TempDir()
	script, err := filepath.Abs("../install.sh")
	require.NoError(t, err)
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	// Hermetic PATH by default: baseTools only, so a developer's real
	// authenticated gh (or stray jq) can never leak into a test — a capable
	// real gh would attempt live attestation calls against stub assets.
	// Tests needing optional tools pass their own PATH via extraEnv
	// (last duplicate wins).
	cmd.Env = append([]string{
		"HOME=" + home,
		// Controlled TMPDIR: install.sh's mktemp work dirs land somewhere the
		// tests can digest, so a write outside HOME (the mv probe, the download
		// work dir) is still visible to the zero-writes assertions.
		"TMPDIR=" + t.TempDir(),
		"PATH=" + toolPath(t, baseTools...),
		"RANCHER_CLI_INSTALL_BASE_URL=" + s.srv.URL,
		"CURL_CA_BUNDLE=" + s.caPath,
	}, extraEnv...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	code := 0
	if exit, ok := runErr.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if runErr != nil {
		t.Fatalf("running install.sh: %v", runErr)
	}
	return out.String(), errb.String(), code
}

// envHome overrides the hermetic HOME runInstall would otherwise generate, for
// tests that must pre-seed or inspect the installed tree (the last duplicate
// env var wins in exec).
func envHome(dir string) []string { return []string{"HOME=" + dir} }

// envTmpDir overrides the controlled TMPDIR the same way, for tests that assert
// on what install.sh wrote outside HOME.
func envTmpDir(dir string) []string { return []string{"TMPDIR=" + dir} }

// mvWithoutNoFollow returns a PATH dir whose mv rejects the no-follow replace
// flags (-T/-h) like BusyBox's does, delegating every other invocation to the
// real mv named by REAL_MV — the seam for the capability probe. The returned
// env carries REAL_MV and must be passed to runInstall.
func mvWithoutNoFollow(t *testing.T) (string, []string) {
	t.Helper()
	realMv, err := exec.LookPath("mv")
	require.NoError(t, err)
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"${1:-}\" in\n  -T|-h) exit 1 ;;\nesac\nexec \"$REAL_MV\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mv"), []byte(script), 0o755))
	return dir, []string{"REAL_MV=" + realMv}
}

// withParser widens the default hermetic PATH with the optional JSON parsers,
// for --pre tests about paging and listing exhaustion rather than about which
// parser is in use (install.sh refuses --pre outright without one).
func withParser(t *testing.T) []string {
	t.Helper()
	return []string{"PATH=" + toolPath(t, append(append([]string{}, baseTools...), "jq", "python3")...)}
}

// ghStub writes a gh executable stub script to a temp dir and returns its
// path. Callers prepend the dir to PATH. The script must start with #!/bin/sh.
func ghStub(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755))
	return dir
}

// recordingCurlWrapper builds a dir containing a curl script that logs every
// argument (one per line) to a file, then execs the real curl. The dir has a
// deliberate space in its name — guarding against broken quoting in any code
// that interpolates the path rather than passing it via env. Returns the dir
// path, the argv log path, and the env slice (RANCHER_TEST_ARGV_LOG and
// RANCHER_TEST_REAL_CURL) that wire them up.
func recordingCurlWrapper(t *testing.T) (dir, argvLog string, env []string) {
	t.Helper()
	realCurl, err := exec.LookPath("curl")
	require.NoError(t, err)
	dir = filepath.Join(t.TempDir(), "with space")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	argvLog = filepath.Join(dir, "argv.log")
	wrapper := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$RANCHER_TEST_ARGV_LOG\"\n" +
		"exec \"$RANCHER_TEST_REAL_CURL\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "curl"), []byte(wrapper), 0o755))
	env = []string{
		"RANCHER_TEST_ARGV_LOG=" + argvLog,
		"RANCHER_TEST_REAL_CURL=" + realCurl,
	}
	return
}

// docSnippet reads a markdown documentation file relative to the module
// root (one level up from the test package, like the rest of the file
// reads ../install.sh) and returns the dedented 4-space code block that
// contains anchorLine. anchorLine may match any line inside the block —
// the function expands to the full contiguous extent in both directions.
// Leading 4 spaces are stripped from every line.
//
// A failure here means the doc and the test literal drifted —
// update both together.
func docSnippet(t *testing.T, filename, anchorLine string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", filename))
	require.NoError(t, err)
	lines := strings.Split(string(b), "\n")
	anchor := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "    ") && strings.Contains(l, anchorLine) {
			anchor = i
			break
		}
	}
	require.NotEqualf(t, -1, anchor,
		"%s: no 4-space-indented line containing %q — update the test literal and the doc together",
		filename, anchorLine)
	start := anchor
	for start > 0 && strings.HasPrefix(lines[start-1], "    ") {
		start--
	}
	end := anchor + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "    ") {
		end++
	}
	out := make([]string, end-start)
	for i, l := range lines[start:end] {
		out[i] = strings.TrimPrefix(l, "    ")
	}
	return strings.Join(out, "\n")
}

// assertSnippetMatchesDoc asserts that transform(got) equals the doc block
// extracted by docSnippet. The shared failure message reminds both sides
// of the contract — doc and test literal — must move together.
func assertSnippetMatchesDoc(t *testing.T, docFile, anchorLine, got string, transform func(string) string) {
	t.Helper()
	assert.Equal(t, docSnippet(t, docFile, anchorLine), transform(got),
		"%s snippet drifted — update the test literal and the doc together", docFile)
}
