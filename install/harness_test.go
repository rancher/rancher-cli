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
	// goroutine while handler goroutines read - without the mutex those are
	// data races under the mandated -race.
	mu               sync.Mutex
	releases         []map[string]any  // newest first, like the API
	rawListing       [][]byte          // raw per-page bodies; overrides releases when set
	assets           map[string][]byte // "<tag>/<name>" -> content (browser download path)
	assetByID        map[string][]byte // "<numeric-id>" -> content (API asset path)
	releaseJSON      map[string][]byte // "<tag>" -> single-release JSON bytes
	nextAssetID      int64             // counter for allocating numeric asset ids
	latest           string            // tag the /releases/latest redirect names; "" = 404
	latestStatus     int               // when non-zero, /releases/latest returns this status instead
	authHeader       string            // last Authorization header seen on API asset requests
	cdnAuthHeader    string            // last Authorization header seen on CDN asset requests
	private          bool              // when true, browser download paths return 404
	assetRedirect    bool              // when true, API asset endpoint 302s to /cdn/assets/<id>
	releaseTagStatus int               // when non-zero, /repos/.../releases/tags/ returns this status
	apiAssetRequests int               // count of requests to the API asset endpoint
	cdnAssetRequests int               // count of requests to the CDN asset endpoint
}

func (s *stub) setLatestStatus(code int) { s.mu.Lock(); defer s.mu.Unlock(); s.latestStatus = code }

func (s *stub) setLatest(tag string)      { s.mu.Lock(); defer s.mu.Unlock(); s.latest = tag }
func (s *stub) lastAuthHeader() string    { s.mu.Lock(); defer s.mu.Unlock(); return s.authHeader }
func (s *stub) lastCDNAuthHeader() string { s.mu.Lock(); defer s.mu.Unlock(); return s.cdnAuthHeader }
func (s *stub) setPrivate(v bool)         { s.mu.Lock(); defer s.mu.Unlock(); s.private = v }
func (s *stub) setAssetRedirect(v bool)   { s.mu.Lock(); defer s.mu.Unlock(); s.assetRedirect = v }
func (s *stub) apiAssetRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiAssetRequests
}
func (s *stub) cdnAssetRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cdnAssetRequests
}
func (s *stub) setReleaseTagStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseTagStatus = code
}
func (s *stub) setReleaseJSON(tag string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseJSON[tag] = b
}
func (s *stub) releaseJSONFor(tag string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.releaseJSON[tag]
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
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
	s := &stub{
		assets:      map[string][]byte{},
		assetByID:   map[string][]byte{},
		releaseJSON: map[string][]byte{},
		nextAssetID: 1000,
	}
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
		priv := s.private
		body, ok := s.assets[key]
		s.mu.Unlock()
		if priv {
			// Private/internal repos: browser download URLs return 404 even with
			// a valid token. The API asset endpoint is the only path that works.
			http.NotFound(w, r)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	// Single-release JSON endpoint: used by fetch_asset_ids to resolve numeric
	// asset ids. releaseTagStatus overrides with a specific HTTP status code,
	// e.g. 403 to simulate a scoped token that cannot read the repo metadata.
	mux.HandleFunc("/repos/rancher/rancher-cli/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/repos/rancher/rancher-cli/releases/tags/")
		s.mu.Lock()
		status := s.releaseTagStatus
		body := s.releaseJSON[tag]
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	// API asset download endpoint: serves asset bytes by numeric id and records
	// the Authorization header. When assetRedirect is set it returns a genuine
	// 302 to the /cdn/assets/<id> path so fetch_asset's two-step redirect logic
	// is exercised; otherwise it returns 200 directly (the original mode).
	mux.HandleFunc("/repos/rancher/rancher-cli/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/repos/rancher/rancher-cli/releases/assets/")
		auth := r.Header.Get("Authorization")
		s.mu.Lock()
		s.authHeader = auth
		s.apiAssetRequests++
		redirect := s.assetRedirect
		body, ok := s.assetByID[id]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if redirect {
			http.Redirect(w, r, s.srv.URL+"/cdn/assets/"+id, http.StatusFound)
			return
		}
		_, _ = w.Write(body)
	})
	// CDN asset endpoint: serves asset bytes after a 302 redirect from the API
	// asset endpoint. Records its Authorization header so tests can assert the
	// token is never forwarded to the CDN host.
	mux.HandleFunc("/cdn/assets/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/cdn/assets/")
		auth := r.Header.Get("Authorization")
		s.mu.Lock()
		s.cdnAuthHeader = auth
		s.cdnAssetRequests++
		body, ok := s.assetByID[id]
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

// fakeUname returns a dir whose uname reports the given kernel/machine -
// the seam for table-testing the platform map beyond the host arch.
func fakeUname(t *testing.T, kernel, machine string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n  -s) echo \"" + kernel + "\" ;;\n  -m) echo \"" + machine + "\" ;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755))
	return dir
}

// stubAsset is one release binary the stub serves: its asset name and bytes.
type stubAsset struct {
	name string
	body []byte
}

// writeRelease registers a release and, when published, its three assets:
// fake "binaries" are tiny sh scripts so the epilogue's --version works.
// It also populates assetByID (for the API asset endpoint) and releaseJSON
// (for the single-release metadata endpoint) so private-mode installs work.
func writeRelease(s *stub, tag string, draft bool) {
	var assets []stubAsset
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		for _, plat := range installerPlatforms {
			assets = append(assets, stubAsset{
				name: name + "-" + plat,
				body: []byte("#!/bin/sh\necho \"" + name + " version " + tag + "\"\n"),
			})
		}
	}
	registerRelease(s, tag, draft, assets)
}

// registerRelease stores a release's binaries under both download shapes,
// generates its SHA256SUMS, and adds it to the metadata endpoints.
func registerRelease(s *stub, tag string, draft bool, binaries []stubAsset) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if draft {
		s.releases = append(s.releases, map[string]any{"tag_name": tag, "draft": draft})
		return
	}

	// releaseAsset mirrors the fields the awk extractor needs: url before name
	// (Go struct fields marshal in declaration order, ensuring the right sequence
	// for the proven awk).
	type releaseAsset struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}

	var apiAssets []releaseAsset
	var sums strings.Builder

	for _, a := range binaries {
		s.assets[tag+"/"+a.name] = a.body
		sum := sha256.Sum256(a.body)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), a.name)

		id := s.nextAssetID
		s.nextAssetID++
		s.assetByID[strconv.FormatInt(id, 10)] = a.body
		apiAssets = append(apiAssets, releaseAsset{
			URL:  s.srv.URL + "/repos/rancher/rancher-cli/releases/assets/" + strconv.FormatInt(id, 10),
			Name: a.name,
			ID:   id,
		})
	}

	sumsBody := []byte(sums.String())
	// Direct assignment: s.mu is already held (a setAsset call here would
	// self-deadlock on the non-reentrant mutex).
	s.assets[tag+"/SHA256SUMS"] = sumsBody

	sumsID := s.nextAssetID
	s.nextAssetID++
	s.assetByID[strconv.FormatInt(sumsID, 10)] = sumsBody
	apiAssets = append(apiAssets, releaseAsset{
		URL:  s.srv.URL + "/repos/rancher/rancher-cli/releases/assets/" + strconv.FormatInt(sumsID, 10),
		Name: "SHA256SUMS",
		ID:   sumsID,
	})

	// Single-release JSON: used by fetch_asset_ids to extract numeric ids.
	// The outer struct uses field declaration order (tag_name, draft, assets)
	// so the JSON is deterministic and the awk finds url before name within
	// each asset object.
	type releaseBody struct {
		TagName string         `json:"tag_name"`
		Draft   bool           `json:"draft"`
		Assets  []releaseAsset `json:"assets"`
	}
	relJSON, _ := json.MarshalIndent(releaseBody{TagName: tag, Draft: draft, Assets: apiAssets}, "", "  ")
	s.releaseJSON[tag] = relJSON

	// Listing entry: include assets so _extract in resolve_pre_tag returns a
	// full release object that fetch_asset_ids can extract ids from (mirrors
	// real GitHub API which includes assets in listing responses).
	s.releases = append(s.releases, map[string]any{
		"tag_name": tag,
		"draft":    draft,
		"assets":   apiAssets,
	})
}

// runInstall executes the real install.sh under sh with a hermetic HOME and
// a controlled PATH, pointed at the stub.
func runInstall(t *testing.T, s *stub, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	home := t.TempDir()
	script, err := filepath.Abs("../install.sh")
	require.NoError(t, err)
	// A fake tool script that interpolates a host path instead of expanding a
	// quoted env var mangles the separators and writes relative to the CWD;
	// comparing the package directory before and after catches the stray file.
	// Parallel tests share this directory, so the failure can surface in a
	// concurrently running test's comparison - the diff still names the stray
	// file, which is what identifies the offending fake.
	before := packageDirNames(t)
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	// Hermetic PATH by default: baseTools only, so a developer's real
	// authenticated gh (or stray jq) can never leak into a test - a capable
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
	assert.Equal(t, before, packageDirNames(t),
		"install.sh run left stray files in the package directory")
	return out.String(), errb.String(), code
}

// packageDirNames returns the entry names of the test package directory, in
// ReadDir's sorted order.
func packageDirNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
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
// real mv named by REAL_MV - the seam for the capability probe. The returned
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
// deliberate space in its name - guarding against broken quoting in any code
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
// contains anchorLine. anchorLine may match any line inside the block -
// the function expands to the full contiguous extent in both directions.
// Leading 4 spaces are stripped from every line.
//
// A failure here means the doc and the test literal drifted -
// update both together.
func docSnippet(t *testing.T, filename, anchorLine string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", filename))
	require.NoError(t, err)
	// A checkout with CRLF line endings must read the same as one with LF.
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	anchor := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "    ") && strings.Contains(l, anchorLine) {
			anchor = i
			break
		}
	}
	require.NotEqualf(t, -1, anchor,
		"%s: no 4-space-indented line containing %q - update the test literal and the doc together",
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
// of the contract - doc and test literal - must move together.
func assertSnippetMatchesDoc(t *testing.T, docFile, anchorLine, got string, transform func(string) string) {
	t.Helper()
	assert.Equal(t, docSnippet(t, docFile, anchorLine), transform(got),
		"%s snippet drifted - update the test literal and the doc together", docFile)
}
