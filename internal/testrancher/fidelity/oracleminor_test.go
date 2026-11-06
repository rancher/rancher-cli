package fidelity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dockerStubScript stands in for docker. Every call is logged. Only the calls
// the script makes before the lock are answered, and the slot reads only when
// the case supplies their output; anything else exits 97. Like real docker,
// inspect answers only when it is given at least one name, and image inspect
// answers 0 only for the references in STUB_PRESENT.
const dockerStubScript = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
case "$1" in
info) exit 0 ;;
version)
  if [ "$2" = "--format" ]; then printf '%s\n' "$STUB_PLATFORM"; exit 0; fi ;;
ps)
  if [ "$*" = "ps -aq" ] && [ -n "$STUB_PS" ]; then cat "$STUB_PS"; exit 0; fi ;;
inspect)
  if [ "$2" = "-f" ] && [ "$#" -ge 4 ] && [ -n "$STUB_INSPECT" ]; then cat "$STUB_INSPECT"; exit 0; fi ;;
image)
  if [ "$2" = "inspect" ]; then
    for ref in $STUB_PRESENT; do
      if [ "$3" = "$ref" ]; then exit 0; fi
    done
  fi ;;
network)
  case "$2" in
  ls)
    if [ "$3" = "-q" ] && [ -n "$STUB_NETLS" ]; then cat "$STUB_NETLS"; exit 0; fi ;;
  inspect)
    if [ "$3" = "-f" ] && [ "$#" -ge 5 ] && [ -n "$STUB_NETINSPECT" ]; then cat "$STUB_NETINSPECT"; exit 0; fi ;;
  esac ;;
esac
exit 97
`

// xargsStubScript behaves like GNU xargs: without -r it runs the command once
// even when its input is empty. BSD xargs skips empty input whatever the
// flags, so this makes a missing -r fail on every host.
const xargsStubScript = `#!/bin/sh
skip=0
if [ "$1" = "-r" ]; then skip=1; shift; fi
names=$(cat)
if [ -z "$names" ] && [ "$skip" = 1 ]; then exit 0; fi
# shellcheck disable=SC2086
exec "$@" $names
`

const (
	listMediaType     = "application/vnd.docker.distribution.manifest.list.v2+json"
	manifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	registryToken     = "test-token"
)

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type registryRequest struct {
	Method        string
	Path          string
	Authorization string
}

type publishedImage struct {
	ListDigest      string
	ArmDigest       string
	ArmConfigDigest string
}

// fakeRegistry serves the subset of the registry API the script reads: the
// 401 challenge, a token endpoint, manifests by tag and by digest, and blobs
// through a redirect to a second path.
type fakeRegistry struct {
	server    *httptest.Server
	once      sync.Once
	mu        sync.Mutex
	requests  []registryRequest
	tags      map[string]string
	manifests map[string][]byte
	blobs     map[string][]byte
	// served replaces the body served at a request path.
	served map[string][]byte
	// status forces the status of an authenticated request to a path.
	status  map[string]int
	noToken bool
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	r := &fakeRegistry{
		tags:      map[string]string{},
		manifests: map[string][]byte{},
		blobs:     map[string][]byte{},
		served:    map[string][]byte{},
		status:    map[string]int{},
	}
	r.server = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

// start begins serving once the case has filled the registry, so the
// handler never races the setup.
func (r *fakeRegistry) start() {
	r.once.Do(r.server.Start)
}

func (r *fakeRegistry) record(method, path, auth string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, registryRequest{Method: method, Path: path, Authorization: auth})
}

func (r *fakeRegistry) recorded() []registryRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func (r *fakeRegistry) countPrefix(method, prefix string) int {
	n := 0
	for _, req := range r.recorded() {
		if req.Method == method && strings.HasPrefix(req.Path, prefix) {
			n++
		}
	}
	return n
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	auth := req.Header.Get("Authorization")
	r.record(req.Method, req.URL.Path, auth)
	path := req.URL.Path

	if path == "/token" {
		if r.noToken {
			_, _ = w.Write([]byte(`{"access_token":"not-the-field"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"token":%q}`, registryToken)
		return
	}
	if digest, ok := strings.CutPrefix(path, "/cdn/"); ok {
		body, found := r.blobs[digest]
		if replaced, ok := r.served[path]; ok {
			body, found = replaced, true
		}
		if !found {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
		return
	}
	if forced, ok := r.status[path]; ok && auth != "" {
		w.WriteHeader(forced)
		return
	}

	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	var repo, kind, ref string
	if before, after, found := strings.Cut(rest, "/manifests/"); found {
		repo, kind, ref = before, "manifests", after
	} else if before, after, found := strings.Cut(rest, "/blobs/"); found {
		repo, kind, ref = before, "blobs", after
	} else {
		http.NotFound(w, req)
		return
	}
	if auth != "Bearer "+registryToken {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(
			`Bearer realm="%s/token",service="registry.test",scope="repository:%s:pull"`, r.server.URL, repo))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if kind == "blobs" {
		http.Redirect(w, req, "/cdn/"+ref, http.StatusTemporaryRedirect)
		return
	}
	digest := ref
	if !strings.HasPrefix(ref, "sha256:") {
		var found bool
		if digest, found = r.tags[repo+":"+ref]; !found {
			http.NotFound(w, req)
			return
		}
	}
	body, found := r.manifests[repo+"@"+digest]
	if replaced, ok := r.served[path]; ok {
		body, found = replaced, true
	}
	if !found {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Docker-Content-Digest", digest)
	if strings.Contains(string(body), `"manifests"`) {
		w.Header().Set("Content-Type", listMediaType)
	} else {
		w.Header().Set("Content-Type", manifestMediaType)
	}
	if req.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func (r *fakeRegistry) addBlob(body []byte) string {
	digest := digestOf(body)
	r.blobs[digest] = body
	return digest
}

func (r *fakeRegistry) addManifest(repo string, body []byte) string {
	digest := digestOf(body)
	r.manifests[repo+"@"+digest] = body
	return digest
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	return body
}

func imageManifest(t *testing.T, configDigest string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     manifestMediaType,
		"config":        map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": configDigest, "size": 1},
		"layers":        []any{},
	})
}

func imageConfig(t *testing.T, env []string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{"config": map[string]any{"Env": env}})
}

// publish serves repo:tag as a manifest list with an amd64 entry that carries
// other values than the arm64 entry, and an attestation entry no read may
// pick. Empty versions leave the variable out of the environment.
func (r *fakeRegistry) publish(t *testing.T, repo, tag, serverVersion, k3sVersion string) publishedImage {
	t.Helper()
	var env []string
	env = append(env, "PATH=/usr/bin")
	if serverVersion != "" {
		env = append(env, "CATTLE_SERVER_VERSION="+serverVersion)
	}
	if k3sVersion != "" {
		env = append(env, "CATTLE_K3S_VERSION="+k3sVersion)
	}
	decoy := []string{"CATTLE_SERVER_VERSION=v9.9-decoy", "CATTLE_K3S_VERSION=v9.9.9+k3s9"}

	armConfig := r.addBlob(imageConfig(t, env))
	amdConfig := r.addBlob(imageConfig(t, decoy))
	armDigest := r.addManifest(repo, imageManifest(t, armConfig))
	amdDigest := r.addManifest(repo, imageManifest(t, amdConfig))

	entry := func(digest, osName, arch string) map[string]any {
		return map[string]any{
			"mediaType": manifestMediaType, "digest": digest, "size": 1,
			"platform": map[string]any{"os": osName, "architecture": arch},
		}
	}
	list := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     listMediaType,
		"manifests": []any{
			entry(amdDigest, "linux", "amd64"),
			entry(armDigest, "linux", "arm64"),
			entry("sha256:"+strings.Repeat("0", 64), "unknown", "unknown"),
		},
	})
	listDigest := r.addManifest(repo, list)
	r.tags[repo+":"+tag] = listDigest
	return publishedImage{ListDigest: listDigest, ArmDigest: armDigest, ArmConfigDigest: armConfig}
}

type slotStub struct {
	ps         string
	inspect    string
	netLS      string
	netInspect string
}

// scenario is one run of the script with an isolated environment: a docker
// stub first on PATH, no Docker daemon reachable, a stub registry, a local git
// repository and a temporary home and state directory.
type scenario struct {
	t         *testing.T
	script    string
	home      string
	stateHome string
	stubDir   string
	logPath   string
	gitConfig string
	repoDir   string
	repoURL   string
	reg       *fakeRegistry
	env       map[string]string
	platform  string
	slot      *slotStub
	// minor is the Rancher minor the case boots; default 2.16.
	minor string
	// present lists the image references docker image inspect finds.
	present []string
	// k3sRef is the k3s image reference publishDefaults served.
	k3sRef string
	// tmpDir is the TMPDIR of the run; the script removes what it makes there.
	tmpDir string
	// pinRewrites replaces the named top-level pin lines in the copy that
	// slotScript writes.
	pinRewrites map[string]string
	// copyRoot is the repository root of the script copy slotScript writes.
	copyRoot string
	// helm is set by newHelmCheckScenario and carries what the helm rows use.
	helm *helmFixture
}

type runResult struct {
	stdout string
	stderr string
	code   int
	log    []string
}

func realScriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "e2e-fidelity"))
	require.NoError(t, err)
	return path
}

func newScenario(t *testing.T) *scenario {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("scripts/e2e-fidelity is a bash script")
	}
	root := t.TempDir()
	s := &scenario{
		t:         t,
		script:    realScriptPath(t),
		home:      filepath.Join(root, "home"),
		stateHome: filepath.Join(root, "state"),
		stubDir:   filepath.Join(root, "stub"),
		logPath:   filepath.Join(root, "docker.log"),
		gitConfig: filepath.Join(root, "gitconfig"),
		repoDir:   filepath.Join(root, "rancher.git"),
		reg:       newFakeRegistry(t),
		env:       map[string]string{},
		platform:  "linux/arm64",
		minor:     "2.16",
	}
	s.tmpDir = filepath.Join(root, "tmp")
	for _, dir := range []string{s.home, s.stateHome, s.stubDir, s.repoDir, s.tmpDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, os.WriteFile(s.gitConfig, nil, 0o600))
	require.NoError(t, os.WriteFile(s.logPath, nil, 0o600))
	testfs.WriteExecutable(t, filepath.Join(s.stubDir, "docker"), []byte(dockerStubScript))
	testfs.WriteExecutable(t, filepath.Join(s.stubDir, "xargs"), []byte(xargsStubScript))
	for name, script := range map[string]string{
		"curl": curlStubScript, "go": goStubScript, "sleep": sleepStubScript, "openssl": opensslStubScript, "df": dfStubScript,
	} {
		testfs.WriteExecutable(t, filepath.Join(s.stubDir, name), []byte(script))
	}
	for _, name := range []string{"curl.log", "curl.table", "curl.argv", "go.log"} {
		require.NoError(t, os.WriteFile(filepath.Join(s.stubDir, name), nil, 0o600))
	}
	s.repoURL = "file://" + s.repoDir
	s.git("init", "-q")
	s.git("commit", "-q", "--allow-empty", "-m", "initial")
	return s
}

func (s *scenario) git(args ...string) {
	s.t.Helper()
	full := append([]string{
		"-C", s.repoDir,
		"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.CommandContext(s.t.Context(), "git", full...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + s.home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + s.gitConfig,
	}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(s.t, err, "git %v: %s", args, out)
}

func (s *scenario) addBranch() { s.t.Helper(); s.git("branch", "release/v2.16") }
func (s *scenario) addTag()    { s.t.Helper(); s.git("tag", "v2.16.0") }

// publishDefaults serves the main branch image of 2.16 and the k3s image its
// environment names.
func (s *scenario) publishDefaults() publishedImage {
	s.t.Helper()
	k3s := s.reg.publish(s.t, "rancher/k3s", "v1.37.1-k3s1", "", "")
	s.k3sRef = "rancher/k3s:v1.37.1-k3s1@" + k3s.ListDigest
	return s.reg.publish(s.t, "rancher/rancher", "head", "v"+s.minor+"-abc123-head", "v1.37.1+k3s1")
}

func (s *scenario) environ() []string {
	s.reg.start()
	env := []string{
		"PATH=" + s.stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + s.home,
		"XDG_STATE_HOME=" + s.stateHome,
		"DOCKER_HOST=unix:///nonexistent",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + s.gitConfig,
		"E2E_FIDELITY_REGISTRY_URL=" + s.reg.server.URL,
		"E2E_FIDELITY_RANCHER_REPO=" + s.repoURL,
		"STUB_LOG=" + s.logPath,
		"STUB_PLATFORM=" + s.platform,
		"STUB_PRESENT=" + strings.Join(s.present, " "),
		"TMPDIR=" + s.tmpDir,
		"STUB_CURL_LOG=" + filepath.Join(s.stubDir, "curl.log"),
		"STUB_CURL_TABLE=" + filepath.Join(s.stubDir, "curl.table"),
		"STUB_CURL_ARGV=" + filepath.Join(s.stubDir, "curl.argv"),
		"STUB_GO_LOG=" + filepath.Join(s.stubDir, "go.log"),
	}
	env = append(env, s.helmEnviron()...)
	if s.slot != nil {
		dir := s.t.TempDir()
		for _, f := range []struct{ name, key, content string }{
			{"ps", "STUB_PS", s.slot.ps},
			{"inspect", "STUB_INSPECT", s.slot.inspect},
			{"netls", "STUB_NETLS", s.slot.netLS},
			{"netinspect", "STUB_NETINSPECT", s.slot.netInspect},
		} {
			path := filepath.Join(dir, f.name)
			require.NoError(s.t, os.WriteFile(path, []byte(f.content), 0o600))
			env = append(env, f.key+"="+path)
		}
	}
	for k, v := range s.env {
		env = append(env, k+"="+v)
	}
	return env
}

// stubbedTools are the commands every run stubs: nothing the script runs can
// reach a real Docker daemon, a real server or the host's disk and clock.
var stubbedTools = []string{"docker", "curl", "go", "sleep", "openssl", "df"}

// run executes the script after checking that every stubbed tool resolves to
// its stub, so a harness slip fails before the script can reach a real one.
// After the run it asserts that the script removed what it made under TMPDIR
// and that the curl stub denied no request.
func (s *scenario) run() runResult {
	s.t.Helper()
	if s.helm != nil {
		require.NotEmpty(s.t, s.copyRoot, "a helm row runs a copy of the script: call slotScript")
	}
	env := s.environ()
	probe := exec.CommandContext(s.t.Context(), "bash", "-c", "command -v "+strings.Join(stubbedTools, " "))
	probe.Env = env
	probe.Dir = s.home
	found, err := probe.Output()
	require.NoError(s.t, err)
	var want []string
	for _, name := range stubbedTools {
		want = append(want, filepath.Join(s.stubDir, name))
	}
	require.Equal(s.t, want, strings.Fields(string(found)),
		"every stubbed tool must resolve to its stub before the script runs")

	cmd := exec.CommandContext(s.t.Context(), "bash", s.script)
	cmd.Env = env
	cmd.Dir = s.home
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.Truef(s.t, ok, "running the script: %v", err)
		code = exitErr.ExitCode()
	}
	raw, err := os.ReadFile(s.logPath)
	require.NoError(s.t, err)
	var log []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			log = append(log, line)
		}
	}
	left, err := os.ReadDir(s.tmpDir)
	require.NoError(s.t, err)
	require.Empty(s.t, left, "the script must remove what it made under TMPDIR")
	for _, line := range s.curlLog() {
		require.NotContains(s.t, line, "denied ", "the curl stub denied a request: %v", s.curlLog())
	}
	return runResult{stdout: stdout.String(), stderr: stderr.String(), code: code, log: log}
}

func (r runResult) commands() []string {
	var names []string
	for _, line := range r.log {
		names = append(names, strings.Fields(line)[0])
	}
	return names
}

func (s *scenario) requireNoStateCreated() {
	s.t.Helper()
	entries, err := os.ReadDir(s.stateHome)
	require.NoError(s.t, err)
	require.Empty(s.t, entries, "a refusal must create nothing under the state home")
	_, err = os.Stat(filepath.Join(s.stateHome, "rancher-cli-e2e-fidelity-v2.16"))
	require.True(s.t, os.IsNotExist(err))
}

// requireRefused asserts exit 1 with the message on stderr, no stub call
// beyond the given commands, and no state root.
func (s *scenario) requireRefused(res runResult, message string, commands ...string) {
	s.t.Helper()
	require.Equalf(s.t, 1, res.code, "stderr: %s", res.stderr)
	require.Contains(s.t, res.stderr, message)
	require.Equal(s.t, commands, res.commands())
	s.requireNoStateCreated()
}

func decodePrinted(t *testing.T, stdout string) map[string]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	require.Lenf(t, lines, 1, "stdout must be one JSON line: %q", stdout)
	var got map[string]string
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &got))
	return got
}

const sampleDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestOracleMinorRefusesBeforeDocker asserts that every refusal of the
// variable combinations and of the minor itself exits 1 with its message
// before any docker call and before any state is created.
func TestOracleMinorRefusesBeforeDocker(t *testing.T) {
	validUpcoming := "rancher/rancher:v2.16-abc-head@" + sampleDigest
	validK3s := "rancher/k3s:v1.34.4-k3s1@" + sampleDigest
	type tc struct {
		name string
		env  map[string]string
		want string
	}
	cases := []tc{
		{"head set", map[string]string{"E2E_FIDELITY_HEAD": "1"}, "E2E_FIDELITY_HEAD is no longer read; set E2E_FIDELITY_MINOR=<major>.<minor> (for example 2.16)"},
		{"head set with a minor", map[string]string{"E2E_FIDELITY_HEAD": "1", "E2E_FIDELITY_MINOR": "2.16"}, "E2E_FIDELITY_HEAD is no longer read"},
		{"prime with a minor", map[string]string{"E2E_FIDELITY_PRIME": "1", "E2E_FIDELITY_MINOR": "2.16"}, "E2E_FIDELITY_PRIME and E2E_FIDELITY_MINOR select different oracles; set one"},
		{"minor with a v prefix", map[string]string{"E2E_FIDELITY_MINOR": "v2.16"}, "E2E_FIDELITY_MINOR=v2.16 is not <major>.<minor>; for example 2.16"},
		{"minor without a dot", map[string]string{"E2E_FIDELITY_MINOR": "2"}, "E2E_FIDELITY_MINOR=2 is not <major>.<minor>"},
		{"minor with a patch", map[string]string{"E2E_FIDELITY_MINOR": "2.16.0"}, "E2E_FIDELITY_MINOR=2.16.0 is not <major>.<minor>"},
		{"minor with a letter", map[string]string{"E2E_FIDELITY_MINOR": "2.x"}, "E2E_FIDELITY_MINOR=2.x is not <major>.<minor>"},
		{"minor with a leading zero", map[string]string{"E2E_FIDELITY_MINOR": "02.16"}, "E2E_FIDELITY_MINOR=02.16 is not <major>.<minor>"},
		{"minor with no slot", map[string]string{"E2E_FIDELITY_MINOR": "2.99"}, "E2E_FIDELITY_MINOR=2.99 has no slot in scripts/e2e-fidelity; slots: 2.16 2.17"},
	}
	for _, variable := range []struct{ name, value string }{
		{"E2E_FIDELITY_UPCOMING_IMAGE", validUpcoming},
		{"E2E_FIDELITY_K3S_IMAGE", validK3s},
		{"E2E_FIDELITY_PRINT_IMAGES", "1"},
	} {
		cases = append(cases,
			tc{variable.name + " alone", map[string]string{variable.name: variable.value},
				variable.name + " is set without E2E_FIDELITY_MINOR; the community and Prime profiles run only their pinned images"},
			tc{variable.name + " with prime", map[string]string{variable.name: variable.value, "E2E_FIDELITY_PRIME": "1"},
				variable.name + " is set without E2E_FIDELITY_MINOR"},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.env = c.env
			res := s.run()
			s.requireRefused(res, c.want)
			assert.Empty(t, s.reg.recorded())
		})
	}
	t.Run("E2E_FIDELITY_PRINT_IMAGES=0 alone is not refused", func(t *testing.T) {
		s := newScenario(t)
		s.env = map[string]string{"E2E_FIDELITY_PRINT_IMAGES": "0"}
		res := s.run()
		assert.NotContains(t, res.stderr, "E2E_FIDELITY_PRINT_IMAGES is set without E2E_FIDELITY_MINOR")
		assert.Equal(t, 97, res.code, "the run goes on to the community profile and stops at the docker stub")
	})
}

// TestOracleMinorOverrideFormat asserts that both image overrides refuse a
// value that is not a repository, a tag and a 64 hex digest, each with the
// message of the rule it breaks (tag, digest, and for the upcoming override
// the registry host), and that a valid k3s override on another registry is
// accepted.
func TestOracleMinorOverrideFormat(t *testing.T) {
	hex64 := strings.TrimPrefix(sampleDigest, "sha256:")
	noTag := func(variable, value, form string) string {
		return variable + "=" + value + " has no tag; write " + form
	}
	noDigest := func(variable, value, form string) string {
		return variable + "=" + value + " does not end in @sha256: and 64 lowercase hex digits; write " + form
	}
	noHost := func(value string) string {
		return "E2E_FIDELITY_UPCOMING_IMAGE=" + value + " must be written without a registry host, as <namespace>/<repo>:<tag>@sha256:<64 hex>; the script reads Docker Hub only"
	}
	const k3sForm = "<repo>:<tag>@sha256:<64 hex>"
	const upcomingForm = "<namespace>/<repo>:<tag>@sha256:<64 hex>"
	type tc struct {
		name     string
		variable string
		value    string
		want     string
	}
	var cases []tc
	for _, bad := range []struct {
		name, suffix string
		message      func(variable, value, form string) string
	}{
		{"no tag", "@sha256:" + hex64, noTag},
		{"no digest", ":v1", noDigest},
		{"short digest", ":v1@sha256:" + hex64[:63], noDigest},
		{"uppercase hex", ":v1@sha256:" + strings.ToUpper(hex64), noDigest},
		{"sha512", ":v1@sha512:" + hex64, noDigest},
		{"trailing space", ":v1@sha256:" + hex64 + " ", noDigest},
	} {
		k3s := "rancher/k3s" + bad.suffix
		upcoming := "rancher/rancher" + bad.suffix
		cases = append(cases,
			tc{"k3s " + bad.name, "E2E_FIDELITY_K3S_IMAGE", k3s, bad.message("E2E_FIDELITY_K3S_IMAGE", k3s, k3sForm)},
			tc{"upcoming " + bad.name, "E2E_FIDELITY_UPCOMING_IMAGE", upcoming, bad.message("E2E_FIDELITY_UPCOMING_IMAGE", upcoming, upcomingForm)},
		)
	}
	cases = append(cases,
		tc{"k3s name with an invalid character", "E2E_FIDELITY_K3S_IMAGE", "Rancher/k3s:v1@sha256:" + hex64,
			"E2E_FIDELITY_K3S_IMAGE=Rancher/k3s:v1@sha256:" + hex64 + " is not " + k3sForm},
		tc{"k3s registry with a port and no tag", "E2E_FIDELITY_K3S_IMAGE", "registry.example.com:5000/k3s@sha256:" + hex64,
			noTag("E2E_FIDELITY_K3S_IMAGE", "registry.example.com:5000/k3s@sha256:"+hex64, k3sForm)},
	)
	for _, host := range []string{"registry.example.com/rancher/rancher", "docker.io/rancher/rancher", "localhost/rancher", "rancher"} {
		value := host + ":v1@sha256:" + hex64
		cases = append(cases, tc{"upcoming " + host, "E2E_FIDELITY_UPCOMING_IMAGE", value, noHost(value)})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", c.variable: c.value}
			res := s.run()
			s.requireRefused(res, c.want)
			assert.Empty(t, s.reg.recorded())
		})
	}

	t.Run("k3s override on another registry is accepted", func(t *testing.T) {
		s := newScenario(t)
		s.publishDefaults()
		value := "registry.example.com:5000/team/k3s:v1.34.4-k3s1@sha256:" + hex64
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "1", "E2E_FIDELITY_K3S_IMAGE": value}
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		assert.Equal(t, value, decodePrinted(t, res.stdout)["k3sImage"])
	})
}

// TestOracleMinorChoosesTheImage asserts the image choice in print mode: the
// main branch image without a release branch, the release branch image with
// one, the overrides verbatim, the platform entry the docker server reports,
// the k3s image from the image's environment, and one JSON line on stdout.
func TestOracleMinorChoosesTheImage(t *testing.T) {
	printEnv := func(extra map[string]string) map[string]string {
		env := map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "1"}
		maps.Copy(env, extra)
		return env
	}

	t.Run("main without a release branch", func(t *testing.T) {
		s := newScenario(t)
		image := s.publishDefaults()
		s.env = printEnv(nil)
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, "2.16", got["minor"])
		assert.Equal(t, "main", got["source"])
		assert.Equal(t, "rancher/rancher:head@"+image.ListDigest, got["oracleImage"])
		assert.Equal(t, "v2.16-abc123-head", got["serverVersion"])
		assert.Equal(t, "image", got["k3sSource"])
		assert.Equal(t, "v1.37.1+k3s1", got["k3sVersion"])
		assert.Regexp(t, `^rancher/k3s:v1\.37\.1-k3s1@sha256:[0-9a-f]{64}$`, got["k3sImage"])
		assert.Equal(t, []string{"info", "version"}, res.commands())
		assert.Contains(t, res.stderr, "==> 2.16 oracle image rancher/rancher:head@"+image.ListDigest)
		assert.Contains(t, res.stderr, "(from CATTLE_K3S_VERSION v1.37.1+k3s1)")
		s.requireNoStateCreated()

		// The list and the platform manifest are the only manifest GETs on the
		// Rancher image, and the k3s image is read by HEAD alone.
		assert.Equal(t, 2, s.reg.countPrefix(http.MethodGet, "/v2/rancher/rancher/manifests/"))
		assert.Zero(t, s.reg.countPrefix(http.MethodGet, "/v2/rancher/k3s/"))
		// The blob request carries the token; its redirect target does not.
		var blobAuth, cdnAuth []string
		for _, req := range s.reg.recorded() {
			switch {
			case strings.HasPrefix(req.Path, "/v2/rancher/rancher/blobs/"):
				blobAuth = append(blobAuth, req.Authorization)
			case strings.HasPrefix(req.Path, "/cdn/"):
				cdnAuth = append(cdnAuth, req.Authorization)
			}
		}
		assert.Equal(t, []string{"Bearer " + registryToken}, blobAuth)
		assert.Equal(t, []string{""}, cdnAuth)
	})

	t.Run("release branch", func(t *testing.T) {
		s := newScenario(t)
		s.addBranch()
		s.publishDefaults()
		image := s.reg.publish(s.t, "rancher/rancher", "v2.16-head", "v2.16-def456-head", "v1.37.1+k3s1")
		s.env = printEnv(nil)
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, "release-branch", got["source"])
		assert.Equal(t, "rancher/rancher:v2.16-head@"+image.ListDigest, got["oracleImage"])
		assert.Equal(t, "v2.16-def456-head", got["serverVersion"])
	})

	t.Run("upcoming override", func(t *testing.T) {
		s := newScenario(t)
		s.publishDefaults()
		image := s.reg.publish(s.t, "rancher/rancher", "v2.16-0123abc-head", "v2.16-0123abc-head", "v1.37.1+k3s1")
		value := "rancher/rancher:v2.16-0123abc-head@" + image.ListDigest
		s.env = printEnv(map[string]string{"E2E_FIDELITY_UPCOMING_IMAGE": value})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, "override", got["source"])
		assert.Equal(t, value, got["oracleImage"])
		assert.Equal(t, "v2.16-0123abc-head", got["serverVersion"])
		for _, req := range s.reg.recorded() {
			assert.NotContains(t, req.Path, "/manifests/head", "an override reads no tag")
		}
	})

	t.Run("k3s override", func(t *testing.T) {
		s := newScenario(t)
		s.publishDefaults()
		value := "rancher/k3s:v1.34.4-k3s1@" + sampleDigest
		s.env = printEnv(map[string]string{"E2E_FIDELITY_K3S_IMAGE": value})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, value, got["k3sImage"])
		assert.Equal(t, "override", got["k3sSource"])
		assert.Empty(t, got["k3sVersion"])
		for _, req := range s.reg.recorded() {
			assert.NotContains(t, req.Path, "/v2/rancher/k3s/", "a k3s override reads no registry")
		}
		assert.Contains(t, res.stderr, "(from E2E_FIDELITY_K3S_IMAGE)")
	})
}

// TestOracleMinorRefusesAnUnusableImage asserts that every unusable image or
// failed read ends the run with one error line before any state is created
// and before any docker call past the platform read, and that a release
// candidate override of the minor is accepted.
func TestOracleMinorRefusesAnUnusableImage(t *testing.T) {
	infoOnly := []string{"info"}
	infoVersion := []string{"info", "version"}
	rancherPath := "/v2/rancher/rancher/manifests/"
	type tc struct {
		name     string
		setup    func(s *scenario)
		want     func(s *scenario) string
		commands []string
	}
	plain := func(message string) func(*scenario) string {
		return func(*scenario) string { return message }
	}
	cases := []tc{
		{"head of another minor", func(s *scenario) {
			s.reg.publish(s.t, "rancher/k3s", "v1.37.1-k3s1", "", "")
			s.reg.publish(s.t, "rancher/rancher", "head", "v2.17-abc123-head", "v1.37.1+k3s1")
		}, plain("rancher/rancher:head is CATTLE_SERVER_VERSION=v2.17-abc123-head, not 2.16, and release/v2.16 does not exist; set E2E_FIDELITY_UPCOMING_IMAGE to a 2.16 build"), infoVersion},
		{"release branch without its head image", func(s *scenario) {
			s.addBranch()
			s.publishDefaults()
		}, plain("release/v2.16 exists but rancher/rancher:v2.16-head is not published; set E2E_FIDELITY_UPCOMING_IMAGE to a build from the branch"), infoVersion},
		{"override of another minor", func(s *scenario) {
			s.publishDefaults()
			image := s.reg.publish(s.t, "rancher/rancher", "v2.15.2", "v2.15.2", "v1.36.5+k3s1")
			s.env["E2E_FIDELITY_UPCOMING_IMAGE"] = "rancher/rancher:v2.15.2@" + image.ListDigest
		}, plain("E2E_FIDELITY_UPCOMING_IMAGE is CATTLE_SERVER_VERSION=v2.15.2, not 2.16"), infoVersion},
		{"GA tag", func(s *scenario) {
			s.publishDefaults()
			s.addTag()
		}, plain("v2.16.0 is tagged on rancher/rancher; 2.16 is released and is not an upcoming minor"), infoOnly},
		{"GA tag with an override", func(s *scenario) {
			s.publishDefaults()
			s.addTag()
			image := s.reg.publish(s.t, "rancher/rancher", "v2.16-0123abc-head", "v2.16-0123abc-head", "v1.37.1+k3s1")
			s.env["E2E_FIDELITY_UPCOMING_IMAGE"] = "rancher/rancher:v2.16-0123abc-head@" + image.ListDigest
		}, plain("v2.16.0 is tagged on rancher/rancher; 2.16 is released and is not an upcoming minor"), infoOnly},
		{"unreadable repository", func(s *scenario) {
			s.publishDefaults()
			s.repoURL = "file://" + filepath.Join(s.home, "missing.git")
		}, func(s *scenario) string {
			return "could not read branches and tags from " + s.repoURL + " (git ls-remote exit 128); a failed read is never taken as no branch"
		}, infoOnly},
		{"no k3s version", func(s *scenario) {
			s.reg.publish(s.t, "rancher/rancher", "head", "v2.16-abc123-head", "")
		}, func(s *scenario) string {
			return `has no usable CATTLE_K3S_VERSION (""); set E2E_FIDELITY_K3S_IMAGE`
		}, infoVersion},
		{"malformed k3s version", func(s *scenario) {
			s.reg.publish(s.t, "rancher/rancher", "head", "v2.16-abc123-head", "1.37.1")
		}, plain(`has no usable CATTLE_K3S_VERSION ("1.37.1"); set E2E_FIDELITY_K3S_IMAGE`), infoVersion},
		{"k3s tag not published", func(s *scenario) {
			s.reg.publish(s.t, "rancher/rancher", "head", "v2.16-abc123-head", "v1.99.9+k3s9")
		}, plain("rancher/k3s:v1.99.9-k3s9 (from CATTLE_K3S_VERSION v1.99.9+k3s9) is not published; set E2E_FIDELITY_K3S_IMAGE"), infoVersion},
		{"registry rate limit", func(s *scenario) {
			s.publishDefaults()
			s.reg.status[rancherPath+"head"] = http.StatusTooManyRequests
		}, func(s *scenario) string {
			return "could not read rancher/rancher:head from " + s.reg.server.URL + " (HTTP 429)"
		}, infoVersion},
		{"token response without a token", func(s *scenario) {
			s.publishDefaults()
			s.reg.noToken = true
		}, plain("(the token response has no token)"), infoVersion},
		{"manifest list digest mismatch", func(s *scenario) {
			image := s.publishDefaults()
			s.reg.served[rancherPath+image.ListDigest] = append(slices.Clone(s.reg.manifests["rancher/rancher@"+image.ListDigest]), ' ')
		}, nil, infoVersion},
		{"platform manifest digest mismatch", func(s *scenario) {
			image := s.publishDefaults()
			s.reg.served[rancherPath+image.ArmDigest] = append(slices.Clone(s.reg.manifests["rancher/rancher@"+image.ArmDigest]), ' ')
		}, nil, infoVersion},
		{"config blob digest mismatch", func(s *scenario) {
			image := s.publishDefaults()
			s.reg.served["/cdn/"+image.ArmConfigDigest] = append(slices.Clone(s.reg.blobs[image.ArmConfigDigest]), ' ')
		}, nil, infoVersion},
		{"no entry for the platform", func(s *scenario) {
			s.publishDefaults()
			s.platform = "linux/s390x"
		}, plain("no manifest for platform linux/s390x"), infoVersion},
	}
	digestMessages := map[string]func(image publishedImage) string{
		"manifest list digest mismatch": func(i publishedImage) string {
			return "manifest list " + i.ListDigest + " does not match its digest (got sha256:"
		},
		"platform manifest digest mismatch": func(i publishedImage) string {
			return "manifest " + i.ArmDigest + " does not match its digest (got sha256:"
		},
		"config blob digest mismatch": func(i publishedImage) string {
			return "config blob " + i.ArmConfigDigest + " does not match its digest (got sha256:"
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "1"}
			c.setup(s)
			res := s.run()
			var want string
			if c.want != nil {
				want = c.want(s)
			} else {
				want = digestMessages[c.name](publishedImage{
					ListDigest:      s.reg.tags["rancher/rancher:head"],
					ArmDigest:       s.armDigest("rancher/rancher", "head"),
					ArmConfigDigest: s.armConfigDigest("rancher/rancher", "head"),
				})
			}
			s.requireRefused(res, want, c.commands...)
			assert.Empty(t, res.stdout)
		})
	}

	t.Run("release candidate override of the minor is accepted", func(t *testing.T) {
		s := newScenario(t)
		s.publishDefaults()
		image := s.reg.publish(s.t, "rancher/rancher", "v2.16.0-rc1", "v2.16.0-rc1", "v1.37.1+k3s1")
		value := "rancher/rancher:v2.16.0-rc1@" + image.ListDigest
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "1", "E2E_FIDELITY_UPCOMING_IMAGE": value}
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, value, got["oracleImage"])
		assert.Equal(t, "v2.16.0-rc1", got["serverVersion"])
	})
}

// armDigest and armConfigDigest read the digests the registry holds for the
// arm64 entry of a published tag, for the cases that corrupt one body.
func (s *scenario) armDigest(repo, tag string) string {
	s.t.Helper()
	var list struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	require.NoError(s.t, json.Unmarshal(s.reg.manifests[repo+"@"+s.reg.tags[repo+":"+tag]], &list))
	for _, m := range list.Manifests {
		if m.Platform.Architecture == "arm64" {
			return m.Digest
		}
	}
	s.t.Fatalf("no arm64 entry for %s:%s", repo, tag)
	return ""
}

func (s *scenario) armConfigDigest(repo, tag string) string {
	s.t.Helper()
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	require.NoError(s.t, json.Unmarshal(s.reg.manifests[repo+"@"+s.armDigest(repo, tag)], &manifest))
	return manifest.Config.Digest
}

// slotScript copies the script to <tmp>/scripts/e2e-fidelity, so the copy's
// repository root is <tmp>, with every slot port row it can reach replaced by
// one port the test chose free: the check never binds a real slot port, and a
// kept pair on the developer's host cannot fail it. It also rewrites the pin
// lines named in pinRewrites. Each rewrite asserts it replaced exactly one
// line, and no non-comment line of the copy names the port of the community
// or helm-check slot.
func (s *scenario) slotScript() int {
	s.t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(s.t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(s.t, listener.Close())

	raw, err := os.ReadFile(realScriptPath(s.t))
	require.NoError(s.t, err)
	text := string(raw)
	rows := []string{`HOST_PORT="8443"`, `HOST_PORT="8447"`}
	if s.minor != "" {
		row := map[string]string{"2.16": `HOST_PORT="8445"`, "2.17": `HOST_PORT="8446"`}[s.minor]
		require.NotEmpty(s.t, row, "no slot row known for %s", s.minor)
		rows = append(rows, row)
	}
	for _, row := range rows {
		require.Equal(s.t, 1, strings.Count(text, row), "the %s row changed shape; update the replacement", row)
		text = strings.Replace(text, row, fmt.Sprintf(`HOST_PORT="%d"`, port), 1)
	}
	for name, value := range s.pinRewrites {
		line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `="[^"]*"$`)
		require.Len(s.t, line.FindAllStringIndex(text, -1), 1, "the %s line changed shape; update the replacement", name)
		text = line.ReplaceAllLiteralString(text, name+`="`+value+`"`)
	}
	literal := regexp.MustCompile(`\b844[37]\b`)
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		require.Falsef(s.t, literal.MatchString(line), "line %d of the copy names a real slot port: %s", i+1, line)
	}
	s.copyRoot = s.t.TempDir()
	copyPath := filepath.Join(s.copyRoot, "scripts", "e2e-fidelity")
	require.NoError(s.t, os.MkdirAll(filepath.Dir(copyPath), 0o755))
	testfs.WriteExecutable(s.t, copyPath, []byte(text))
	s.script = copyPath
	return port
}

// TestOracleMinorRefusesAHeldSlot asserts the pre-boot check: a port or subnet
// another container or network holds is refused with the holder named, only
// another minor's pair gets the remove advice, the minor's own container and
// network are not holders, and a case that passes the check reaches the pull
// before the lock without any removal or boot.
func TestOracleMinorRefusesAHeldSlot(t *testing.T) {
	stateRoot := func(s *scenario, minor string) string {
		return filepath.Join(s.stateHome, "rancher-cli-e2e-fidelity-v"+minor)
	}
	removeAdvice := func(s *scenario, minor string) string {
		pair := "rancher-cli-e2e-fidelity-v" + minor
		return "; that is the " + minor + " pair: remove containers " + pair + " and " + pair + "-k3s, network " +
			pair + "-net and state root " + stateRoot(s, minor)
	}
	type tc struct {
		name string
		stub func(port int) slotStub
		want func(s *scenario, port int) string
		// notWant is a text the refusal must not contain.
		notWant string
	}
	cases := []tc{
		{"port held by a running container", func(port int) slotStub {
			return slotStub{ps: "c1\n", inspect: fmt.Sprintf("/web %d \n", port)}
		}, func(s *scenario, port int) string {
			return fmt.Sprintf("port %d for 2.16 is held by container web\n", port)
		}, "that is the"},
		{"port held by a stopped container", func(port int) slotStub {
			return slotStub{ps: "c1\nc2\n", inspect: fmt.Sprintf("/idle-web %d \n/other \n", port)}
		}, func(s *scenario, port int) string {
			return fmt.Sprintf("port %d for 2.16 is held by container idle-web\n", port)
		}, "that is the"},
		{"port held by another minor's pair", func(port int) slotStub {
			return slotStub{ps: "c1\n", inspect: fmt.Sprintf("/rancher-cli-e2e-fidelity-v2.14 %d \n", port)}
		}, func(s *scenario, port int) string {
			return fmt.Sprintf("port %d for 2.16 is held by container rancher-cli-e2e-fidelity-v2.14", port) + removeAdvice(s, "2.14")
		}, ""},
		{"subnet held by another minor's pair", func(int) slotStub {
			return slotStub{ps: "", netLS: "n1\n", netInspect: "rancher-cli-e2e-fidelity-v2.14-net 172.30.3.0/24 \n"}
		}, func(s *scenario, port int) string {
			return "subnet 172.30.3.0/24 for 2.16 overlaps network rancher-cli-e2e-fidelity-v2.14-net (172.30.3.0/24)" + removeAdvice(s, "2.14")
		}, ""},
		{"port held by the community pair", func(port int) slotStub {
			return slotStub{ps: "c1\n", inspect: fmt.Sprintf("/rancher-cli-e2e-fidelity %d \n", port)}
		}, func(s *scenario, port int) string {
			return fmt.Sprintf("port %d for 2.16 is held by container rancher-cli-e2e-fidelity, the community pair\n", port)
		}, "remove"},
		{"subnet overlap with another network", func(int) slotStub {
			return slotStub{ps: "", netLS: "n1\nn2\n", netInspect: "bridge 172.17.0.0/16 \nlab-net 172.30.3.128/25 fd00::/64 \n"}
		}, func(s *scenario, port int) string {
			return "subnet 172.30.3.0/24 for 2.16 overlaps network lab-net (172.30.3.128/25)\n"
		}, "that is the"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.publishDefaults()
			port := s.slotScript()
			stub := c.stub(port)
			s.slot = &stub
			s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
			res := s.run()
			require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
			assert.Contains(t, res.stderr, "error: "+c.want(s, port))
			if c.notWant != "" {
				assert.NotContains(t, res.stderr, c.notWant)
			}
			for _, command := range res.commands() {
				assert.Contains(t, []string{"info", "version", "ps", "inspect", "network"}, command)
			}
			s.requireNoStateCreated()
		})
	}

	passes := []struct {
		name string
		stub func(port int) slotStub
	}{
		{"own container and own network are not holders", func(port int) slotStub {
			return slotStub{
				ps:         "c1\nc2\n",
				inspect:    fmt.Sprintf("/rancher-cli-e2e-fidelity-v2.16 %d \n/rancher-cli-e2e-fidelity-v2.16-k3s \n", port),
				netLS:      "n1\n",
				netInspect: "rancher-cli-e2e-fidelity-v2.16-net 172.30.3.0/24 \n",
			}
		}},
		{"no containers and no networks", func(int) slotStub { return slotStub{} }},
	}
	for _, c := range passes {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			image := s.publishDefaults()
			port := s.slotScript()
			stub := c.stub(port)
			s.slot = &stub
			s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
			res := s.run()
			// The stub answers neither the image check nor the pull, so the
			// run ends at the pull refusal: nothing was removed or booted.
			require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
			assert.Contains(t, res.stderr, "error: could not pull rancher/rancher:head@"+image.ListDigest+"; nothing was removed")
			assertNoRemovalOrBoot(t, res)
			index := slices.IndexFunc(res.log, func(line string) bool { return strings.HasPrefix(line, "image inspect ") })
			require.GreaterOrEqual(t, index, 0, "log: %v", res.log)
			assert.Equal(t, "image inspect rancher/rancher:head@"+image.ListDigest, res.log[index])
			assert.Equal(t, "pull -q rancher/rancher:head@"+image.ListDigest, res.log[index+1])
			assert.Len(t, res.log, index+2)
			s.requireNoStateCreated()
			if c.name == "no containers and no networks" {
				// With nothing to inspect, no inspect call is made at all.
				for _, line := range res.log {
					assert.NotContains(t, line, "inspect -f", "log: %v", res.log)
				}
			}
		})
	}

	// The pull covers both images, and an image that is already present is
	// not pulled.
	t.Run("only the k3s image is missing", func(t *testing.T) {
		s := newScenario(t)
		image := s.publishDefaults()
		oracleRef := "rancher/rancher:head@" + image.ListDigest
		s.present = []string{oracleRef}
		s.slotScript()
		s.slot = &slotStub{}
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: could not pull "+s.k3sRef+"; nothing was removed")
		assert.NotContains(t, res.log, "pull -q "+oracleRef)
		assert.Contains(t, res.log, "pull -q "+s.k3sRef)
		assertNoRemovalOrBoot(t, res)
		s.requireNoStateCreated()
	})
	t.Run("both images present are not pulled", func(t *testing.T) {
		s := newScenario(t)
		image := s.publishDefaults()
		s.present = []string{"rancher/rancher:head@" + image.ListDigest, s.k3sRef}
		s.slotScript()
		s.slot = &slotStub{}
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
		res := s.run()
		// The stub then answers nothing the boot needs, so the run fails
		// past the pull step; what matters is that no pull was made.
		assert.NotContains(t, res.stderr, "could not pull")
		for _, line := range res.log {
			assert.NotContains(t, line, "pull ", "log: %v", res.log)
		}
		assert.Contains(t, res.log, "image inspect "+s.k3sRef)
	})
	t.Run("print images set to 0 does not print", func(t *testing.T) {
		s := newScenario(t)
		image := s.publishDefaults()
		s.slotScript()
		s.slot = &slotStub{}
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "0"}
		res := s.run()
		assert.Empty(t, res.stdout)
		assert.Contains(t, res.stderr, "error: could not pull rancher/rancher:head@"+image.ListDigest)
	})

	// The 2.17 row: its subnet is read from the script, its port is the
	// test's replacement (the replacement fails if the row changes shape).
	// A listener on the wildcard address is not covered: whether a bind on
	// 127.0.0.1 then fails depends on the platform.
	t.Run("2.17 row", func(t *testing.T) {
		for _, c := range []struct {
			name string
			stub func(port int) slotStub
			want func(port int) string
		}{
			{"port held", func(port int) slotStub {
				return slotStub{ps: "c1\n", inspect: fmt.Sprintf("/web %d \n", port)}
			}, func(port int) string { return fmt.Sprintf("port %d for 2.17 is held by container web\n", port) }},
			{"subnet held", func(int) slotStub {
				return slotStub{netLS: "n1\n", netInspect: "lab-net 172.30.4.0/24 \n"}
			}, func(int) string { return "subnet 172.30.4.0/24 for 2.17 overlaps network lab-net (172.30.4.0/24)\n" }},
			{"own pair is not a holder", func(port int) slotStub {
				return slotStub{
					ps:         "c1\n",
					inspect:    fmt.Sprintf("/rancher-cli-e2e-fidelity-v2.17 %d \n", port),
					netLS:      "n1\n",
					netInspect: "rancher-cli-e2e-fidelity-v2.17-net 172.30.4.0/24 \n",
				}
			}, func(int) string { return "could not pull rancher/rancher:head@" }},
		} {
			t.Run(c.name, func(t *testing.T) {
				s := newScenario(t)
				s.minor = "2.17"
				s.publishDefaults()
				port := s.slotScript()
				stub := c.stub(port)
				s.slot = &stub
				s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.17"}
				res := s.run()
				require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
				assert.Contains(t, res.stderr, "error: "+c.want(port))
				assertNoRemovalOrBoot(t, res)
			})
		}
	})

	t.Run("listener outside docker", func(t *testing.T) {
		s := newScenario(t)
		s.publishDefaults()
		port := s.slotScript()
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		s.slot = &slotStub{}
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, fmt.Sprintf(
			"error: port %d for 2.16 is in use on 127.0.0.1 outside Docker (checked twice, 2 s apart); a pair removed in the last minute can leave the port in TIME_WAIT", port))
		assertNoRemovalOrBoot(t, res)
		s.requireNoStateCreated()
	})
}

func assertNoRemovalOrBoot(t *testing.T, res runResult) {
	t.Helper()
	for _, line := range res.log {
		for _, prefix := range []string{"rm ", "run ", "network create", "network rm"} {
			assert.Falsef(t, strings.HasPrefix(line, prefix), "docker call %q must not happen: %v", line, res.log)
		}
	}
}

// fnHarness runs one shell function, extracted from scripts/e2e-fidelity
// together with its two time variables, against a curl stub that answers each
// call from a numbered response file (the file named default answers any
// further call). It overrides the two time variables in the extracted shell
// text only; the script itself reads no variable for them.
//
// Time in the run is fake: the function's own sleep moves bash's SECONDS
// forward and takes no real time, and SECONDS never ticks on its own, so the
// number of attempts depends on the loop and never on how long an attempt
// takes.
type fnHarness struct {
	dir string
	// limitVar and waitVar name the two time variables, fn the function.
	limitVar, waitVar, fn string
	// firstCallDelay, when set, is the time in seconds the curl stub waits
	// before it answers its first call.
	firstCallDelay string
}

// fakeClock is the shell text that run puts ahead of the extracted function.
// Unsetting SECONDS removes its special property, so it is an ordinary
// variable that only sleep changes.
const fakeClock = `unset SECONDS
SECONDS=0
sleep() { SECONDS=$((SECONDS + $1)); }
`

func newFnHarness(t *testing.T, limitVar, waitVar, fn string, responses map[string]string) *fnHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("scripts/e2e-fidelity is a bash script")
	}
	h := &fnHarness{dir: t.TempDir(), limitVar: limitVar, waitVar: waitVar, fn: fn}
	stub := `#!/bin/sh
n=$(cat "$STUB_DIR/count" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > "$STUB_DIR/count"
if [ "$n" = 1 ] && [ -n "${STUB_FIRST_DELAY:-}" ]; then sleep "$STUB_FIRST_DELAY"; fi
printf '%s\n' "$*" >> "$STUB_DIR/argv"
cat > "$STUB_DIR/stdin.$n"
if [ -f "$STUB_DIR/resp.$n" ]; then cat "$STUB_DIR/resp.$n"; else cat "$STUB_DIR/resp.default"; fi
`
	testfs.WriteExecutable(t, filepath.Join(h.dir, "curl"), []byte(stub))
	for name, body := range responses {
		require.NoError(t, os.WriteFile(filepath.Join(h.dir, "resp."+name), []byte(body), 0o600))
	}
	return h
}

func (h *fnHarness) run(t *testing.T, limit, wait int) (stdout, stderr string, code int) {
	t.Helper()
	raw, err := os.ReadFile(realScriptPath(t))
	require.NoError(t, err)
	script := string(raw)
	start := strings.Index(script, "\n"+h.limitVar+"=")
	require.GreaterOrEqual(t, start, 0, "%s not found in the script", h.limitVar)
	start++
	fnStart := strings.Index(script[start:], h.fn+"() {")
	require.GreaterOrEqual(t, fnStart, 0, "%s not found in the script", h.fn)
	end := strings.Index(script[start+fnStart:], "\n}\n")
	require.GreaterOrEqual(t, end, 0, "end of %s not found", h.fn)
	extracted := script[start : start+fnStart+end+3]

	program := "set -euo pipefail\n" +
		fakeClock +
		"BASE_URL=http://127.0.0.1:1 BOOTSTRAP_PW=bootstrap-secret-pw\n" +
		"CA_FILE=" + filepath.Join(h.dir, "ca.pem") + "\n" +
		extracted +
		fmt.Sprintf("%s=%d\n%s=%d\n", h.limitVar, limit, h.waitVar, wait) +
		h.fn + "\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", program)
	cmd.Env = []string{"PATH=" + h.dir + string(os.PathListSeparator) + os.Getenv("PATH"), "STUB_DIR=" + h.dir}
	if h.firstCallDelay != "" {
		cmd.Env = append(cmd.Env, "STUB_FIRST_DELAY="+h.firstCallDelay)
	}
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.Truef(t, ok, "running the harness: %v", err)
		code = exitErr.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func (h *fnHarness) calls(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.dir, "count"))
	require.NoError(t, err)
	var n int
	_, err = fmt.Sscanf(string(raw), "%d", &n)
	require.NoError(t, err)
	return n
}

// TestOracleMinorLoginRetries asserts that the admin login is retried while
// the answer is empty, not JSON or not a string token, prints the token once
// one arrives, bounds each attempt with a curl timeout, and keeps the
// password off the curl command line.
func TestOracleMinorLoginRetries(t *testing.T) {
	h := newFnHarness(t, "LOGIN_LIMIT_SECONDS", "LOGIN_WAIT_SECONDS", "login_admin", map[string]string{
		"1":       "\n000",
		"2":       "<html>bad gateway</html>\n502",
		"3":       "{\"message\":\"starting\"}\n200",
		"4":       "{\"token\":null}\n200",
		"default": "{\"token\":\"token-from-login\"}\n200",
	})
	stdout, stderr, code := h.run(t, 60, 0)
	require.Equalf(t, 0, code, "stderr: %s", stderr)
	assert.Equal(t, "token-from-login\n", stdout)
	assert.Equal(t, 5, h.calls(t))

	argv, err := os.ReadFile(filepath.Join(h.dir, "argv"))
	require.NoError(t, err)
	assert.NotContains(t, string(argv), "bootstrap-secret-pw", "the password must not be on the curl command line")
	assert.Contains(t, string(argv), "--max-time", "each attempt is bounded")
	for i := 1; i <= 5; i++ {
		body, err := os.ReadFile(filepath.Join(h.dir, fmt.Sprintf("stdin.%d", i)))
		require.NoError(t, err)
		assert.Contains(t, string(body), `"password":"bootstrap-secret-pw"`)
	}
}

// TestOracleMinorLoginGivesUp asserts that a login that never returns a token
// ends with one plain error line naming the last HTTP status, after the time
// limit and not before, and without printing the response body or a
// traceback.
func TestOracleMinorLoginGivesUp(t *testing.T) {
	h := newFnHarness(t, "LOGIN_LIMIT_SECONDS", "LOGIN_WAIT_SECONDS", "login_admin", map[string]string{
		"default": "{\"message\":\"leak-marker\"}\n503",
	})
	stdout, stderr, code := h.run(t, 2, 1)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Equal(t, "error: the admin login did not return a token within 2 s (last HTTP status 503)\n", stderr)
	assert.NotContains(t, stderr, "leak-marker")
	assert.NotContains(t, stderr, "Traceback")
	assert.Equal(t, 3, h.calls(t), "calls at 0, 1 and 2 s, then the limit of 2 s is reached")
}

func testCertificatePEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestOracleMinorCARetries asserts that the CA read is retried while the
// endpoint answers something that is not a certificate (a plain text message,
// a PEM header over garbage), and that only the certificate is written to the
// CA file.
func TestOracleMinorCARetries(t *testing.T) {
	cert := testCertificatePEM(t)
	h := newFnHarness(t, "CA_LIMIT_SECONDS", "CA_WAIT_SECONDS", "fetch_ca", map[string]string{
		"1":       "API Aggregation not ready\n200",
		"2":       "\n000",
		"3":       "-----BEGIN CERTIFICATE-----\nnot-base64!\n-----END CERTIFICATE-----\n\n200",
		"default": cert + "\n200",
	})
	_, stderr, code := h.run(t, 60, 0)
	require.Equalf(t, 0, code, "stderr: %s", stderr)
	assert.Equal(t, 4, h.calls(t))
	written, err := os.ReadFile(filepath.Join(h.dir, "ca.pem"))
	require.NoError(t, err)
	assert.Equal(t, cert, string(written))
	argv, err := os.ReadFile(filepath.Join(h.dir, "argv"))
	require.NoError(t, err)
	assert.Contains(t, string(argv), "--max-time", "each attempt is bounded")
}

// TestOracleMinorCAGivesUp asserts that a CA read that never returns a
// certificate ends with one plain error line naming the last HTTP status, or
// saying there was no HTTP answer, after the time limit, and leaves no CA
// file.
func TestOracleMinorCAGivesUp(t *testing.T) {
	h := newFnHarness(t, "CA_LIMIT_SECONDS", "CA_WAIT_SECONDS", "fetch_ca", map[string]string{
		"default": "API Aggregation not ready\n503",
	})
	_, stderr, code := h.run(t, 2, 1)
	assert.Equal(t, 1, code)
	assert.Equal(t, "error: http://127.0.0.1:1/cacerts did not return a certificate within 2 s (last HTTP status 503)\n", stderr)
	assert.Equal(t, 3, h.calls(t), "calls at 0, 1 and 2 s, then the limit of 2 s is reached")
	_, err := os.Stat(filepath.Join(h.dir, "ca.pem"))
	assert.True(t, os.IsNotExist(err), "no CA file may be left behind")

	none := newFnHarness(t, "CA_LIMIT_SECONDS", "CA_WAIT_SECONDS", "fetch_ca", map[string]string{"default": "000"})
	_, stderr, code = none.run(t, 1, 1)
	assert.Equal(t, 1, code)
	assert.Equal(t, "error: http://127.0.0.1:1/cacerts did not return a certificate within 1 s (no HTTP answer)\n", stderr)
	assert.Equal(t, 2, none.calls(t), "calls at 0 and 1 s, then the limit of 1 s is reached")
}

// TestOracleMinorLoginRefused asserts that a 401 or 403 ends the login at once
// with its own line, without waiting for the time limit.
func TestOracleMinorLoginRefused(t *testing.T) {
	for _, status := range []string{"401", "403"} {
		t.Run(status, func(t *testing.T) {
			h := newFnHarness(t, "LOGIN_LIMIT_SECONDS", "LOGIN_WAIT_SECONDS", "login_admin", map[string]string{
				"default": "{\"message\":\"leak-marker\"}\n" + status,
			})
			stdout, stderr, code := h.run(t, 60, 60)
			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Equal(t, "error: the admin login was refused with HTTP status "+status+"\n", stderr)
			assert.Equal(t, 1, h.calls(t))
		})
	}
}

// TestOracleMinorLoginNoAnswer asserts that a login that never gets an HTTP
// answer says so instead of printing the status 000.
func TestOracleMinorLoginNoAnswer(t *testing.T) {
	h := newFnHarness(t, "LOGIN_LIMIT_SECONDS", "LOGIN_WAIT_SECONDS", "login_admin", map[string]string{
		"default": "000",
	})
	_, stderr, code := h.run(t, 1, 1)
	assert.Equal(t, 1, code)
	assert.Equal(t, "error: the admin login did not return a token within 1 s (no HTTP answer)\n", stderr)
	assert.Equal(t, 2, h.calls(t), "calls at 0 and 1 s, then the limit of 1 s is reached")
}

// clockFunctions lists every function of scripts/e2e-fidelity that loops on
// bash's SECONDS, with the variables that hold its time limit and its wait.
var clockFunctions = []struct{ fn, limitVar, waitVar string }{
	{"login_admin", "LOGIN_LIMIT_SECONDS", "LOGIN_WAIT_SECONDS"},
	{"fetch_ca", "CA_LIMIT_SECONDS", "CA_WAIT_SECONDS"},
}

// TestOracleMinorRetryClockIgnoresSlowCalls asserts that the number of
// attempts of each function in clockFunctions follows the loop alone: a first
// curl call that takes 2.2 s of real time, longer than the whole limit of 2 s,
// still leaves the calls at 0, 1 and 2 s of the fake clock, three in all. On
// the real clock that call would use up the limit and end the loop after one.
func TestOracleMinorRetryClockIgnoresSlowCalls(t *testing.T) {
	for _, c := range clockFunctions {
		t.Run(c.fn, func(t *testing.T) {
			t.Parallel()
			h := newFnHarness(t, c.limitVar, c.waitVar, c.fn, map[string]string{"default": "000"})
			h.firstCallDelay = "2.2"
			_, stderr, code := h.run(t, 2, 1)
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr, "within 2 s (no HTTP answer)")
			assert.Equal(t, 3, h.calls(t), "the slow first call must not use up the limit")
		})
	}
}

var (
	secondsWord = regexp.MustCompile(`\bSECONDS\b`)

	// otherClockRead matches the other ways a shell function can read the
	// real time: date with a format, EPOCHSECONDS, EPOCHREALTIME and
	// printf's %(...)T.
	otherClockRead = regexp.MustCompile(`\bEPOCHSECONDS\b|\bEPOCHREALTIME\b|\bdate\b[^\n]*\+|\bprintf\s[^\n]*%\([^)]*\)T`)

	// functionStart matches the first line of a shell function definition:
	// name() { or name () { or function name { or function name() {, with
	// any indentation. Group 1 or 2 is the name, group 3 the rest of the
	// line. A brace on the next line, a subshell body f() ( and names with
	// - or . are not recognized.
	functionStart = regexp.MustCompile(`^\s*(?:function\s+([A-Za-z_]\w*)\s*(?:\(\))?|([A-Za-z_]\w*)\s*\(\))\s*\{(.*)$`)
)

// otherClockReaderReasons holds the functions of scripts/e2e-fidelity that the
// scan flags for another way of reading the time than SECONDS (a log
// timestamp, for example) and that are not in clockFunctions, each with the
// reason it needs no fake clock. A function that reads SECONDS cannot be
// listed here; the test fails if one is.
var otherClockReaderReasons = map[string]string{}

// clockReaders returns the sorted names of the shell functions of a script
// that read the clock, and the sorted names of those among them that read
// SECONDS. SECONDS counts anywhere, and "(top level)" stands for a line
// outside any function; the other ways to read the time (date with a format,
// EPOCHSECONDS, EPOCHREALTIME, printf %(...)T) count only inside a function.
// Comment lines and variables that only end in SECONDS (LOGIN_LIMIT_SECONDS)
// do not count. A function ends at a line that holds only a closing brace at
// the indentation of its definition, so an inner { ...; } group does not end
// it.
func clockReaders(script string) (all, seconds []string) {
	found := map[string]bool{}
	foundSeconds := map[string]bool{}
	current, indent := "", ""
	for line := range strings.SplitSeq(script, "\n") {
		line = strings.TrimRight(line, "\r")
		text := line
		closes := false
		if current == "" {
			if m := functionStart.FindStringSubmatch(line); m != nil {
				current = m[1] + m[2]
				text = m[3]
				indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				trimmed := strings.TrimRight(text, " \t")
				closes = trimmed == "}" || strings.HasSuffix(trimmed, " }") || strings.HasSuffix(trimmed, ";}")
			}
		} else if strings.TrimRight(line, " \t") == indent+"}" {
			closes = true
		}
		if !strings.HasPrefix(strings.TrimSpace(text), "#") {
			switch {
			case secondsWord.MatchString(text):
				name := current
				if name == "" {
					name = "(top level)"
				}
				found[name] = true
				foundSeconds[name] = true
			case current != "" && otherClockRead.MatchString(text):
				found[current] = true
			}
		}
		if closes {
			current = ""
		}
	}
	return slices.Sorted(maps.Keys(found)), slices.Sorted(maps.Keys(foundSeconds))
}

// TestOracleMinorClockReadersScan asserts that the scan finds each way a
// shell function reads the clock, under each way of writing the function
// definition, and ignores top-level date calls, comments and variables that
// only end in SECONDS.
func TestOracleMinorClockReadersScan(t *testing.T) {
	cases := []struct {
		name, script string
		want         []string
	}{
		{"seconds", "timed() {\n  local d=$((SECONDS + LIMIT_SECONDS))\n}\n", []string{"timed"}},
		{"seconds braces and CRLF", "timed() {\r\n  echo ${SECONDS}\r\n}\r\n", []string{"timed"}},
		{"seconds at top level", "echo $SECONDS\n", []string{"(top level)"}},
		{"date with a format", "poll() {\n  local end=$(( $(date +%s) + 60 ))\n}\n", []string{"poll"}},
		{"EPOCHSECONDS", "poll() {\n  local end=$((EPOCHSECONDS + 60))\n}\n", []string{"poll"}},
		{"EPOCHREALTIME", "poll() {\n  echo \"${EPOCHREALTIME}\"\n}\n", []string{"poll"}},
		{"printf time format", "poll() {\n  printf '%(%s)T' -1\n}\n", []string{"poll"}},
		{"one line", "poll() { local end=$((SECONDS + 1)); }\nquiet() {\n  echo hi\n}\n", []string{"poll"}},
		{"space before the parentheses", "poll () {\n  echo $(date +%s)\n}\n", []string{"poll"}},
		{"function keyword", "function poll {\n  echo $SECONDS\n}\n", []string{"poll"}},
		{"function keyword and parentheses", "function poll() {\n  echo $SECONDS\n}\n", []string{"poll"}},
		{"indented definition", "if true; then\n  poll() {\n    echo $SECONDS\n  }\nfi\n", []string{"poll"}},
		{"inner group then date", "f() {\n  a || {\n    echo x\n  }\n  local e=$(( $(date +%s) + 5 ))\n}\n", []string{"f"}},
		{"one-line close without a space", "f() { echo;}\ng() {\n  local e=$(( $(date +%s) + 5 ))\n}\n", []string{"g"}},
		{"close with trailing whitespace", "f() {\n  echo\n}  \ng() {\n  local e=$(( $(date +%s) + 5 ))\n}\n", []string{"g"}},
		{"inner group then SECONDS", "f() {\n  a || {\n    echo x\n  }\n  local e=$((SECONDS + 5))\n}\n", []string{"f"}},
		{"start line that continues", "f() { x=${y}\n  local e=$(date +%s)\n}\ng() {\n  echo hi\n}\n", []string{"f"}},
		{"date at top level", "STARTED=\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"\n", nil},
		{"names and comments", "LIMIT_SECONDS=5\n# the loop reads SECONDS\nquiet() {\n  # date +%s\n  echo \"${LIMIT_SECONDS}\"\n}\n", nil},
		{"two readers", "a() {\n  echo $SECONDS\n}\nb() { date +%s; }\n", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			all, _ := clockReaders(c.script)
			assert.Equal(t, c.want, all)
		})
	}
}

// TestOracleMinorClockFunctionsMatchTheScriptsClockReaders asserts that the
// functions of scripts/e2e-fidelity that read the clock are exactly the ones
// in clockFunctions (or in otherClockReaderReasons with a reason), which run
// through the fake clock and the slow-call test; and that no other test file
// names them. shellFunction refuses a clock function by name, because its
// callers have no fake clock.
func TestOracleMinorClockFunctionsMatchTheScriptsClockReaders(t *testing.T) {
	raw, err := os.ReadFile(realScriptPath(t))
	require.NoError(t, err)
	var declared []string
	for _, c := range clockFunctions {
		declared = append(declared, c.fn)
	}
	slices.Sort(declared)
	all, seconds := clockReaders(string(raw))
	for _, name := range seconds {
		_, listed := otherClockReaderReasons[name]
		assert.Falsef(t, listed, "%s reads SECONDS and cannot be listed in otherClockReaderReasons", name)
	}
	var readers []string
	for _, name := range all {
		if reason, ok := otherClockReaderReasons[name]; ok && !slices.Contains(seconds, name) {
			assert.NotEmptyf(t, reason, "%s needs a reason", name)
			continue
		}
		readers = append(readers, name)
	}
	assert.Equal(t, declared, readers,
		"a function that reads the clock belongs in clockFunctions, which means extending the table and the slow-call test; a false positive gets a row with a reason in otherClockReaderReasons")

	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	for _, file := range files {
		if file == "oracleminor_test.go" {
			continue
		}
		body, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, fn := range declared {
			assert.NotContainsf(t, string(body), fn,
				"%s names %s; clock functions are run only through newFnHarness in oracleminor_test.go", file, fn)
		}
	}
}

// shellFunction returns the text of one top-level function of the script. It
// refuses a function in clockFunctions, which only fnHarness.run may run: a
// program built from the returned text has the real SECONDS.
func shellFunction(t *testing.T, name string) string {
	t.Helper()
	for _, c := range clockFunctions {
		require.NotEqualf(t, c.fn, name, "%s reads the clock; run it through newFnHarness", name)
	}
	raw, err := os.ReadFile(realScriptPath(t))
	require.NoError(t, err)
	script := string(raw)
	start := strings.Index(script, "\n"+name+"() {")
	require.GreaterOrEqual(t, start, 0, "%s not found in the script", name)
	end := strings.Index(script[start+1:], "\n}\n")
	require.GreaterOrEqual(t, end, 0, "end of %s not found", name)
	return script[start+1 : start+1+end+3]
}

// requireRemovalsRemoveVolumes asserts that every container removal in the
// docker stub's log is forced and removes the container's anonymous volumes,
// and that there was at least one.
func requireRemovalsRemoveVolumes(t *testing.T, log []string) {
	t.Helper()
	removals := 0
	for _, line := range log {
		if !strings.HasPrefix(line, "rm ") {
			continue
		}
		removals++
		assert.Truef(t, strings.HasPrefix(line, "rm -f -v "), "container removal without -v: %q", line)
	}
	assert.NotZero(t, removals, "no container removal in the log: %v", log)
}

// TestOracleMinorRemovesContainersWithVolumes asserts that every container
// removal the script makes passes -v, so the anonymous volumes of the Rancher
// and k3s images are not left behind: the fresh boot removal and the cleanup
// trap, the downstream rebuild, and the downstream boot. A scan of the source
// covers the remaining lines.
func TestOracleMinorRemovesContainersWithVolumes(t *testing.T) {
	t.Run("fresh boot and cleanup", func(t *testing.T) {
		s := newScenario(t)
		image := s.publishDefaults()
		s.present = []string{"rancher/rancher:head@" + image.ListDigest, s.k3sRef}
		s.slotScript()
		s.slot = &slotStub{}
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
		res := s.run()
		// The stub answers nothing the boot needs, so the run ends right
		// after the removals; both the fresh boot and the trap removed.
		count := 0
		for _, line := range res.log {
			if strings.HasPrefix(line, "rm ") {
				count++
			}
		}
		assert.Equal(t, 2, count, "log: %v", res.log)
		requireRemovalsRemoveVolumes(t, res.log)
	})

	for _, name := range []string{"rebuild_downstream", "boot_k3s"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("scripts/e2e-fidelity is a bash script")
			}
			dir := t.TempDir()
			logPath := filepath.Join(dir, "docker.log")
			require.NoError(t, os.WriteFile(logPath, nil, 0o600))
			testfs.WriteExecutable(t, filepath.Join(dir, "docker"), []byte(dockerStubScript))
			program := "set -euo pipefail\n" +
				"K3S_CONTAINER=k3s-case NETWORK=net-case K3S_IP=10.0.0.3 K3S_IMAGE=k3s-image BASE_URL=http://127.0.0.1:1 DOWNSTREAM_NAME=downstream\n" +
				"api() { echo 404; }\nprime_ip_san() { :; }\nboot_k3s() { :; }\nimport_downstream() { :; }\nwrite_state() { :; }\n" +
				shellFunction(t, name) + name + "\n"
			cmd := exec.CommandContext(t.Context(), "bash", "-c", program)
			cmd.Env = []string{
				"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"STUB_LOG=" + logPath, "DOCKER_HOST=unix:///nonexistent",
			}
			_ = cmd.Run()
			raw, err := os.ReadFile(logPath)
			require.NoError(t, err)
			log := strings.Split(strings.TrimSpace(string(raw)), "\n")
			requireRemovalsRemoveVolumes(t, log)
		})
	}

	t.Run("source", func(t *testing.T) {
		raw, err := os.ReadFile(realScriptPath(t))
		require.NoError(t, err)
		// Any spelling of a container removal: docker rm, docker container rm,
		// extra spaces, or a removal continued on the next line.
		removal := regexp.MustCompile(`docker\s+(container\s+)?rm\b`)
		for i, line := range strings.Split(string(raw), "\n") {
			if removal.MatchString(line) {
				assert.Containsf(t, line, "docker rm -f -v", "scripts/e2e-fidelity:%d removes a container without -v", i+1)
				assert.Falsef(t, strings.HasSuffix(strings.TrimSpace(line), "\\"), "scripts/e2e-fidelity:%d continues a removal on the next line", i+1)
			}
		}
	})
}
