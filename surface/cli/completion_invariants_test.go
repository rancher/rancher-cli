package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	surfacecli "github.com/rancher/rancher-cli/surface/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// hashDir hashes each file's path, ModTime (nanoseconds), size, and contents.
// Including ModTime ensures that an atomic same-byte rewrite — which produces a
// new inode and a changed ModTime even when content is identical — is detectable
// (round-3 finding 5: content hash alone cannot detect same-byte atomic rewrites).
func hashDir(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	_ = filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		// Include path + ModTime + size in the hash so an atomic same-byte rewrite
		// is detected via the changed ModTime (round-3 finding 5).
		_, _ = fmt.Fprintf(h, "%s|%d|%d\n", path, fi.ModTime().UnixNano(), fi.Size())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("hashDir: reading %s: %v", path, rerr)
			return rerr
		}
		h.Write(data)
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

// AC-22: No keyring call on any completion path (round-3 finding 3).
// Uses config.SetKeyringObserverForTest to count actual keyring ops — unlike
// MockInitWithError alone, the observer proves keyring was structurally never
// reached, not just that errors were handled gracefully.
// Setup: keychain-pinned config, live completion ON, cluster value present so
// dispatch reaches token resolution. No env token → no mgmt token → zero
// requests. Assert zero keyring ops AND zero HTTP requests.
func TestAC22NoKeyringOnCompletionPath(t *testing.T) {
	// NOT parallel: mutates process-global keyring state and env (findings 14/R2-9,
	// round-3 findings 3+4).
	var keyringOps atomic.Int32
	config.SetKeyringObserverForTest(func(op string) { keyringOps.Add(1) })
	t.Cleanup(func() {
		config.SetKeyringObserverForTest(nil)
		keyring.MockInit()
	})
	keyring.MockInitWithError(fmt.Errorf("AC-22 VIOLATION: keyring accessed on completion path"))

	var hits atomic.Int32
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srvBad.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext:  "ctx",
		Contexts:        map[string]config.Context{"ctx": {Server: "srv", Cluster: "my-cluster"}},
		Servers:         map[string]config.Server{"srv": {URL: srvBad.URL}},
		CredentialStore: "keychain",
		Completion:      config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.WritePin(dir, "keychain"))
	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config (round-3 finding 4).
	t.Setenv(config.EnvConfigDir, dir)
	surfacecli.SetStoredConfigForTest(cfg, dir)
	// Drive a cluster-dependent site with a cluster value present so dispatch
	// reaches token resolution. No env token → resolveCompletionToken returns empty.
	surfacecli.SetRawArgvForTest([]string{"usage", "--cluster", "my-cluster"})

	root, err := surfacecli.BuildRoot("test")
	require.NoError(t, err)
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	root.Writer = io.Discard
	runErr := root.Run(context.Background(), []string{"rancher", "usage", "--cluster", "my-cluster", "--generate-shell-completion"})
	assert.NoError(t, runErr, "AC-22: completion must not fail when keyring is unavailable")
	assert.Empty(t, stderr.String(), "AC-22: completion must not write to stderr")
	assert.Equal(t, int32(0), keyringOps.Load(), "AC-22: completion must not contact the keyring")
	assert.Equal(t, int32(0), hits.Load(), "AC-22: no token → no HTTP requests")
}

// AC-23: No stderr from a real TAB callback (finding 13: run the real callback).
// Round-4 finding 3: config must have Completion.Network=true so dispatch
// reaches live token/network paths (not the early-return for network-off).
// Two sub-paths are exercised:
//
//	(a) unreachable server with no file token → empty stdout, nil error, empty stderr
//	(b) unreadable credentials.yaml (chmod 000) → silent failure, nil error, empty stderr
func TestAC23CompletionNoStderr(t *testing.T) {
	// NOT parallel: mutates rawArgv and env globals (finding 14, round-3 finding 4).
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext:  "ctx",
		Contexts:        map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
		Servers:         map[string]config.Server{"srv": {URL: "https://example.invalid"}},
		CredentialStore: "file",
		Completion:      config.CompletionConfig{Network: true}, // round-4: must reach live path
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Sub-path (b): write credentials.yaml then make it unreadable so
	// fileStoreTokenSilent fails silently (AC-23: no stderr on read error).
	credsPath := filepath.Join(dir, "credentials.yaml")
	require.NoError(t, os.WriteFile(credsPath, []byte("srv: tok\n"), 0o600))
	require.NoError(t, os.Chmod(credsPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(credsPath, 0o600) })

	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config (round-3 finding 4).
	t.Setenv(config.EnvConfigDir, dir)
	surfacecli.SetStoredConfigForTest(cfg, dir)

	// v2 protocol (round-5): proto sentinel + words + self-prefixed empty cur field.
	// --rancher-completion-cur= (no value after =) → cursor after space, no partial typed.
	surfacecli.CaptureArgvForTest([]string{
		"--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc",
		"--rancher-completion-cur=",
		"--generate-shell-completion",
	})

	root, err := surfacecli.BuildRoot("test")
	require.NoError(t, err)
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	root.Writer = io.Discard
	runErr := root.Run(context.Background(), surfacecli.SanitizedArgsForTest([]string{
		"rancher", "--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc",
		"--rancher-completion-cur=",
		"--generate-shell-completion",
	}))
	assert.NoError(t, runErr, "AC-23: completion must not exit nonzero")
	assert.Empty(t, stderr.String(), "AC-23: no stderr on any failure path (unreadable creds or unreachable server)")
}

// AC-24: At most two HTTP requests per full TAB dispatch (round-3 finding 4).
// Drives the actual two-request path: display-name cluster at a --namespace site.
// rawArgv=["get","pod","--cluster","prod","--namespace","kube-"] → dispatch:
//
//	request 1: cluster list to resolve display name "prod" → "c-abc"
//	request 2: namespace list for /k8s/clusters/c-abc/api/v1/namespaces
//
// Assert exactly 2 hits (not vacuous 0 <= 2).
// A companion zero-hit test is TestNoClusterNoRequests in dispatch_test.go.
func TestAC24AtMostTwoRequestsPerTAB(t *testing.T) {
	// NOT parallel: mutates rawArgv, storedConfig, and env globals (finding 14,
	// round-3 finding 4).
	keyring.MockInit()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/api/v1/namespaces") {
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"default"}},{"metadata":{"name":"kube-system"}}]}`))
		} else {
			// Cluster list: returns one cluster with displayName "prod".
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}}]}`))
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config (round-3 finding 4).
	t.Setenv(config.EnvConfigDir, dir)
	surfacecli.SetStoredConfigForTest(cfg, dir)
	t.Setenv(config.EnvToken, "test-token")
	// Display-name cluster "prod" at --namespace site → two requests.
	// v2 protocol (round-5): proto sentinel + words (--namespace flag last in argv,
	// no value in args) + self-prefixed cur field for partial "kube-".
	// ParseArgTail: currentWord="kube-" non-flag, --namespace is last in args → SiteFlagValue(namespace).
	surfacecli.CaptureArgvForTest([]string{
		"--rancher-completion-proto=2", "get", "pod", "--cluster", "prod", "--namespace",
		"--rancher-completion-cur=kube-",
		"--generate-shell-completion",
	})

	root, buildErr := surfacecli.BuildRoot("test")
	require.NoError(t, buildErr)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = io.Discard
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = root.Run(ctx, surfacecli.SanitizedArgsForTest([]string{
		"rancher", "--rancher-completion-proto=2", "get", "pod", "--cluster", "prod", "--namespace",
		"--rancher-completion-cur=kube-",
		"--generate-shell-completion",
	}))

	assert.Equal(t, int32(2), hits.Load(), "AC-24: exactly 2 HTTP requests for display-name resolution + namespace list")
	assert.LessOrEqual(t, hits.Load(), int32(2), "AC-24: at most 2 HTTP requests per TAB")
	assert.Contains(t, out.String(), "kube-system", "AC-24: namespace candidates must include kube-system")
}

// AC-25: Secrets and cloud-credentials never completable (deny-by-default).
func TestAC25SecretNotCompletable(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("secret")
	require.True(t, ok)
	assert.False(t, k.Completable, "AC-25: secret must not be completable")
}

func TestAC25CompletableAllowlistIsExactlySixKinds(t *testing.T) {
	t.Parallel()
	var completable []string
	for _, k := range kinds.All() {
		if k.Completable {
			completable = append(completable, k.Name)
		}
	}
	sort.Strings(completable)
	assert.Equal(t,
		[]string{"cluster", "deployment", "namespace", "node", "pod", "project"},
		completable, "AC-25/AC-15: allowlist must be exactly the six spec'd kinds")
}

// AC-28: Full-callback tests for the five live-completion flag-value sites
// (round-3 finding 1, round-4 finding 1+2). Each sub-test runs the real root.Run
// callback using the REAL v2 script shape:
//
//	rancher --rancher-completion-proto=2 <words-before-flag-value> <currentWord> --generate-shell-completion
//
// <words-before-flag-value> contains the flag but NOT its partial value;
// <currentWord> is the partial being typed. This is what the updated shell
// scripts send. NOT parallel: t.Setenv mutates env globals.
func TestAC28FullCallbackFlagValueSites(t *testing.T) {
	clusterItems := `{"items":[{"metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}}]}`
	nsItems := `{"items":[{"metadata":{"name":"default"}},{"metadata":{"name":"kube-system"}}]}`
	nodeItems := `{"items":[{"metadata":{"name":"node-1"}}]}`
	projectItems := `{"items":[{"metadata":{"name":"p-abc"}}]}`

	newSrv := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.Contains(r.URL.Path, "/api/v1/namespaces"):
				_, _ = w.Write([]byte(nsItems))
			case strings.Contains(r.URL.Path, "/api/v1/nodes"):
				_, _ = w.Write([]byte(nodeItems))
			case strings.Contains(r.URL.Path, "/namespaces/c-abc/projects"):
				_, _ = w.Write([]byte(projectItems))
			default:
				_, _ = w.Write([]byte(clusterItems))
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	setup := func(t *testing.T) (string, *httptest.Server) {
		t.Helper()
		srv := newSrv(t)
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentContext: "ctx",
			Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
			Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:     config.CompletionConfig{Network: true},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "tok")
		surfacecli.SetStoredConfigForTest(cfg, dir)
		return dir, srv
	}

	// run simulates the v2 script: proto sentinel + words (flag last, no value) +
	// --rancher-completion-cur=<currentWord> + --generate-shell-completion.
	// CaptureArgvForTest processes the full script-shaped slice; SanitizedArgsForTest
	// strips both protocol fields for urfave.
	run := func(t *testing.T, words []string, currentWord string) string {
		t.Helper()
		// Script-shaped request: proto sentinel + words + cur field + completion trigger.
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		surfacecli.CaptureArgvForTest(scriptArgs)
		root, err := surfacecli.BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		root.ErrWriter = io.Discard
		// root.Run receives sanitized args (proto sentinel stripped for urfave).
		runArgv := surfacecli.SanitizedArgsForTest(append([]string{"rancher"}, scriptArgs...))
		_ = root.Run(context.Background(), runArgv)
		return out.String()
	}

	// v2 test: words contain flag only (no partial value); currentWord is the partial.
	// The shell does prefix-filtering on the binary's full output; the test asserts
	// the expected candidate is present in stdout (binary returns all candidates).

	t.Run("--cluster space-form partial", func(t *testing.T) {
		setup(t)
		// words=["get","pod","--cluster"], currentWord="c-" (partial cluster name)
		got := run(t, []string{"get", "pod", "--cluster"}, "c-")
		assert.Contains(t, got, "c-abc", "--cluster partial: c-abc must appear")
	})
	t.Run("--context space-form partial", func(t *testing.T) {
		setup(t)
		got := run(t, []string{"get", "pod", "--context"}, "c-")
		assert.Contains(t, got, "c-abc", "--context partial: c-abc must appear")
	})
	t.Run("--project space-form partial", func(t *testing.T) {
		setup(t)
		// --cluster c-abc is a completed value in words; --project is the flag being completed.
		got := run(t, []string{"get", "pod", "--cluster", "c-abc", "--project"}, "p-")
		assert.Contains(t, got, "p-abc", "--project partial: p-abc must appear")
	})
	t.Run("--namespace space-form partial (get pod leaf)", func(t *testing.T) {
		setup(t)
		got := run(t, []string{"get", "pod", "--cluster", "c-abc", "--namespace"}, "kube-")
		assert.Contains(t, got, "kube-system", "--namespace partial: kube-system must appear")
	})
	t.Run("--node space-form partial (usage leaf)", func(t *testing.T) {
		setup(t)
		// currentWord "no" is a realistic prefix that matches "node-1" under shell prefix filter.
		got := run(t, []string{"usage", "--cluster", "c-abc", "--node"}, "no")
		assert.Contains(t, got, "node-1", "--node partial: node-1 must appear")
	})

	// Both-sides-of-the-ambiguity sub-tests: these prove that completed-value + empty
	// currentWord → positional, while flag-last + partial currentWord → flagValue.
	t.Run("completed cluster value + empty currentWord → positional not flagValue", func(t *testing.T) {
		setup(t)
		// words=["get","pod","--cluster","c-abc"], currentWord="" → SitePositional(0)
		// Dispatch reaches positional (pod name) site; with no pod candidates from server
		// the output is empty (no "c-abc" cluster candidate should appear here).
		got := run(t, []string{"get", "pod", "--cluster", "c-abc"}, "")
		assert.NotContains(t, got, "c-abc", "completed cluster value + empty currentWord must not complete cluster name")
	})
}

// AC-26: Candidate sanitization (charset enforcement + sort).
func TestAC26SanitizationDropsLeadingHyphen(t *testing.T) {
	t.Parallel()
	got := surfacecli.SanitizeCandidatesForTest([]string{"-bad", "good", "also-good"})
	assert.Equal(t, []string{"also-good", "good"}, got)
}

func TestAC26SanitizationDropsShellMetachars(t *testing.T) {
	t.Parallel()
	got := surfacecli.SanitizeCandidatesForTest([]string{"$(inject)", "good.name", "has space", "valid_1"})
	assert.Equal(t, []string{"good.name", "valid_1"}, got)
}

// AC-27: Completion path writes nothing to disk — snapshot before/after (round-3
// finding 5). hashDir includes path + ModTime + size + content, so an atomic
// same-byte rewrite is detected via the changed ModTime. runErr is asserted nil
// so a failed invocation cannot masquerade as a no-write success.
//
// Round-4 finding 3: config has Completion.Network=true and token-cache.yaml is
// seeded with one valid AND one expired exec-cred entry. The existing
// ListCachedCreds/GetCachedCred prune expired entries on read by writing the
// cache file (AC-6a violation). After a full live dispatch the snapshot must be
// unchanged — proving no prune-on-read write occurred.
func TestAC27CompletionWritesNothingToDisk(t *testing.T) {
	// NOT parallel: mutates storedConfig, rawArgv, and env globals (finding 14,
	// round-3 finding 4).
	keyring.MockInit()
	dir := t.TempDir()

	// Seed token-cache.yaml with one valid and one expired entry before writing config.
	fileStore := config.NewFileStore(dir)
	require.NoError(t, fileStore.SetCachedCred(
		config.CacheKey("k8s-ctx", "example.invalid", "c-abc"),
		"tok-valid", time.Now().Add(time.Hour),
		"k8s-ctx", "example.invalid", "c-abc",
	))
	require.NoError(t, fileStore.SetCachedCred(
		config.CacheKey("k8s-old", "example.invalid", "c-abc"),
		"tok-expired", time.Now().Add(-time.Hour),
		"k8s-old", "example.invalid", "c-abc",
	))

	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "c-abc"}},
		Servers:        map[string]config.Server{"srv": {URL: "https://example.invalid"}},
		Completion:     config.CompletionConfig{Network: true}, // round-4: exercise live path
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config (round-3 finding 4).
	t.Setenv(config.EnvConfigDir, dir)
	surfacecli.SetStoredConfigForTest(cfg, dir)
	// Snapshot AFTER seeding but BEFORE dispatch.
	before := hashDir(t, dir)

	// v2 protocol: proto sentinel + words + empty currentWord (completing namespace positional).
	surfacecli.CaptureArgvForTest([]string{
		"--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc", "--rancher-completion-cur=",
		"--generate-shell-completion",
	})
	root, err := surfacecli.BuildRoot("test")
	require.NoError(t, err)
	root.Writer = io.Discard
	root.ErrWriter = io.Discard
	// Capture runErr: a failed invocation must not masquerade as a no-write success
	// (round-3 finding 5).
	runErr := root.Run(context.Background(), surfacecli.SanitizedArgsForTest([]string{
		"rancher", "--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc", "--rancher-completion-cur=",
		"--generate-shell-completion",
	}))
	assert.NoError(t, runErr, "AC-27: completion must not exit nonzero")
	assert.Equal(t, before, hashDir(t, dir), "AC-27: completion must write nothing (no prune-on-read or other writes)")
}
