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

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// hashDir hashes each file's path, ModTime (nanoseconds), size, and contents.
// Including ModTime ensures that an atomic same-byte rewrite - which produces a
// new inode and a changed ModTime even when content is identical - is detectable
// (a content hash alone cannot detect same-byte atomic rewrites).
func hashDir(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	_ = filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		// Include path + ModTime + size in the hash so an atomic same-byte rewrite
		// is detected via the changed ModTime.
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

// No keyring call on any completion path.
// Uses config.SetKeyringObserverForTest to count actual keyring ops - unlike
// MockInitWithError alone, the observer proves keyring was structurally never
// reached, not just that errors were handled gracefully.
// Setup: keychain-pinned config, live completion ON, cluster value present so
// dispatch reaches token resolution. No env token → no mgmt token → zero
// requests. Assert zero keyring ops AND zero HTTP requests.
func TestCompletionNeverTouchesKeyring(t *testing.T) {
	// NOT parallel: mutates process-global keyring state and env.
	var keyringOps atomic.Int32
	config.SetKeyringObserverForTest(func(op string) { keyringOps.Add(1) })
	t.Cleanup(func() {
		config.SetKeyringObserverForTest(nil)
		keyring.MockInit()
	})
	keyring.MockInitWithError(fmt.Errorf("keyring accessed on completion path"))

	var hits atomic.Int32
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srvBad.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer:   "srv",
		Servers:         map[string]config.Server{"srv": {URL: srvBad.URL}},
		CredentialStore: "keychain",
		Completion:      config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.WritePin(dir, "keychain"))
	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config.
	t.Setenv(config.EnvConfigDir, dir)
	cli.SetStoredConfigForTest(t, cfg, dir)
	// Drive a cluster-dependent site with a cluster value present so dispatch
	// reaches token resolution. No env token → resolveCompletionToken returns empty.
	line := []string{"--rancher-completion-proto=2", "usage", "--cluster", "my-cluster", "--namespace", "--rancher-completion-cur=", "--generate-shell-completion"}
	cli.CaptureArgvForTest(line)
	t.Cleanup(func() { cli.CaptureArgvForTest(nil) })

	root, err := cli.BuildRoot("test")
	require.NoError(t, err)
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	root.Writer = io.Discard
	runErr := root.Run(t.Context(), cli.SanitizedArgsForTest(append([]string{"rancher"}, line...)))
	require.NoError(t, runErr, "completion must not fail when keyring is unavailable")
	assert.Empty(t, stderr.String(), "completion must not write to stderr")
	assert.Equal(t, int32(0), keyringOps.Load(), "completion must not contact the keyring")
	assert.Equal(t, int32(0), hits.Load(), "no token → no HTTP requests")
}

// No stderr from a real TAB callback (runs the real callback).
// The config must have Completion.Network=true so dispatch
// reaches live token/network paths (not the early-return for network-off).
// Two sub-paths are exercised:
//
//	(a) unreachable server with no file token → empty stdout, nil error, empty stderr
//	(b) unreadable credentials.yaml (chmod 000) → silent failure, nil error, empty stderr
func TestCompletionNoStderr(t *testing.T) {
	// NOT parallel: mutates rawArgv and env globals.
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer:   "srv",
		Servers:         map[string]config.Server{"srv": {URL: "https://example.invalid"}},
		CredentialStore: "file",
		Completion:      config.CompletionConfig{Network: true}, // must reach the live path
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Sub-path (b): write credentials.yaml then make it unreadable so
	// fileStoreTokenSilent fails silently (no stderr on read error).
	credsPath := filepath.Join(dir, "credentials.yaml")
	require.NoError(t, os.WriteFile(credsPath, []byte("srv: tok\n"), 0o600))
	require.NoError(t, os.Chmod(credsPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(credsPath, 0o600) })

	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config.
	t.Setenv(config.EnvConfigDir, dir)
	cli.SetStoredConfigForTest(t, cfg, dir)

	// Proto sentinel + words + self-prefixed empty cur field.
	// --rancher-completion-cur= (no value after =) → cursor after space, no partial typed.
	cli.CaptureArgvForTest([]string{
		"--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc",
		"--rancher-completion-cur=",
		"--generate-shell-completion",
	})

	root, err := cli.BuildRoot("test")
	require.NoError(t, err)
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	root.Writer = io.Discard
	runErr := root.Run(t.Context(), cli.SanitizedArgsForTest([]string{
		"rancher", "--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc",
		"--rancher-completion-cur=",
		"--generate-shell-completion",
	}))
	require.NoError(t, runErr, "completion must not exit nonzero")
	assert.Empty(t, stderr.String(), "no stderr on any failure path (unreadable creds or unreachable server)")
}

// At most two HTTP requests per full TAB dispatch when the clusters
// collection is one page. Drives the actual two-request path: display-name
// cluster at a --namespace site.
// rawArgv=["get","pod","--cluster","prod","--namespace","kube-"] → dispatch:
//
//	request 1: the steve clusters collection to resolve display name "prod" → "c-abc"
//	request 2: namespace list for /k8s/clusters/c-abc/api/v1/namespaces
//
// Assert exactly 2 hits (not vacuous 0 <= 2).
// A companion zero-hit test is TestNoClusterNoRequests in dispatch_test.go.
func TestCompletionAtMostTwoRequestsPerTab(t *testing.T) {
	// NOT parallel: mutates rawArgv, storedConfig, and env globals.
	keyring.MockInit()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/api/v1/namespaces") {
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"default"}},{"metadata":{"name":"kube-system"}}]}`))
		} else {
			// The steve clusters collection: one cluster with displayName "prod".
			_, _ = w.Write([]byte(`{"count":1,"revision":"1","data":[{"id":"c-abc","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}}]}`))
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config.
	t.Setenv(config.EnvConfigDir, dir)
	cli.SetStoredConfigForTest(t, cfg, dir)
	t.Setenv(config.EnvToken, "test-token")
	// Display-name cluster "prod" at --namespace site → two requests.
	// Proto sentinel + words (--namespace flag last in argv,
	// no value in args) + self-prefixed cur field for partial "kube-".
	// ParseArgTail: currentWord="kube-" non-flag, --namespace is last in args → SiteFlagValue(namespace).
	cli.CaptureArgvForTest([]string{
		"--rancher-completion-proto=2", "get", "pod", "--cluster", "prod", "--namespace",
		"--rancher-completion-cur=kube-",
		"--generate-shell-completion",
	})

	root, buildErr := cli.BuildRoot("test")
	require.NoError(t, buildErr)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = io.Discard
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_ = root.Run(ctx, cli.SanitizedArgsForTest([]string{
		"rancher", "--rancher-completion-proto=2", "get", "pod", "--cluster", "prod", "--namespace",
		"--rancher-completion-cur=kube-",
		"--generate-shell-completion",
	}))

	assert.Equal(t, int32(2), hits.Load(), "exactly 2 HTTP requests for display-name resolution + namespace list")
	assert.LessOrEqual(t, hits.Load(), int32(2), "at most 2 HTTP requests per TAB")
	assert.Contains(t, out.String(), "kube-system", "namespace candidates must include kube-system")
}

// Secrets and cloud-credentials are never completable (deny-by-default).
func TestSecretNotCompletable(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("secret")
	require.True(t, ok)
	assert.False(t, k.Completable, "secret must not be completable")
}

func TestCompletableAllowlistIsExactlyFifteenKinds(t *testing.T) {
	t.Parallel()
	var completable []string
	for _, k := range kinds.All() {
		if k.Completable {
			completable = append(completable, k.Name)
		}
	}
	sort.Strings(completable)
	assert.Equal(t,
		[]string{"app", "cluster", "clusterrepo", "deployment", "globalrole", "namespace", "node", "nodedriver", "oidcclient", "operation", "pod", "project", "provisioning-cluster", "roletemplate", "user"},
		completable, "allowlist must be exactly these fifteen kinds (oidcclient is the first Prime-capability kind)")
}

// Full-callback tests for the live-completion flag-value sites. Each
// sub-test runs the real root.Run callback using the REAL script shape:
//
//	rancher --rancher-completion-proto=2 <words-before-flag-value> <currentWord> --generate-shell-completion
//
// <words-before-flag-value> contains the flag but NOT its partial value;
// <currentWord> is the partial being typed. This is what the updated shell
// scripts send. NOT parallel: t.Setenv mutates env globals.
func TestFullCallbackFlagValueSites(t *testing.T) {
	clusterItems := `{"count":1,"revision":"1","data":[{"id":"c-abc","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}}]}`
	nsItems := `{"items":[{"metadata":{"name":"default"}},{"metadata":{"name":"kube-system"}}]}`
	nodeItems := `{"items":[{"metadata":{"name":"node-1"}}]}`
	projectItems := `{"count":1,"revision":"1","data":[{"id":"c-abc/p-abc","metadata":{"name":"p-abc"},"spec":{"clusterName":"c-abc"}}]}`
	globalRoleItems := `{"items":[{"metadata":{"name":"user-base"}}]}`

	newSrv := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.Contains(r.URL.Path, "/api/v1/namespaces"):
				_, _ = w.Write([]byte(nsItems))
			case strings.Contains(r.URL.Path, "/api/v1/nodes"):
				_, _ = w.Write([]byte(nodeItems))
			case strings.HasSuffix(r.URL.Path, "/management.cattle.io/v3/globalroles"):
				_, _ = w.Write([]byte(globalRoleItems))
			case r.URL.Path == "/v1/management.cattle.io.projects/c-abc":
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
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: true},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "tok")
		cli.SetStoredConfigForTest(t, cfg, dir)
		return dir, srv
	}

	// run simulates the script: proto sentinel + words (flag last, no value) +
	// --rancher-completion-cur=<currentWord> + --generate-shell-completion.
	// CaptureArgvForTest processes the full script-shaped slice; SanitizedArgsForTest
	// strips both protocol fields for urfave.
	run := func(t *testing.T, words []string, currentWord string) string {
		t.Helper()
		// Script-shaped request: proto sentinel + words + cur field + completion trigger.
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		cli.CaptureArgvForTest(scriptArgs)
		root, err := cli.BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		root.ErrWriter = io.Discard
		// root.Run receives sanitized args (proto sentinel stripped for urfave).
		runArgv := cli.SanitizedArgsForTest(append([]string{"rancher"}, scriptArgs...))
		_ = root.Run(t.Context(), runArgv)
		return out.String()
	}

	// Words contain flag only (no partial value); currentWord is the partial.
	// The shell does prefix-filtering on the binary's full output; the test asserts
	// the expected candidate is present in stdout (binary returns all candidates).

	t.Run("--cluster space-form partial", func(t *testing.T) {
		setup(t)
		// words=["get","pod","--cluster"], currentWord="c-" (partial cluster name)
		got := run(t, []string{"get", "pod", "--cluster"}, "c-")
		assert.Contains(t, got, "c-abc", "--cluster partial: c-abc must appear")
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
	t.Run("--global-role space-form partial (create user leaf)", func(t *testing.T) {
		setup(t)
		got := run(t, []string{"create", "user", "alice", "--global-role"}, "user-")
		assert.Contains(t, got, "user-base", "--global-role partial: user-base must appear")
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

// Candidate sanitization (charset enforcement + sort).
func TestSanitizationDropsLeadingHyphen(t *testing.T) {
	t.Parallel()
	got := cli.SanitizeCandidatesForTest([]string{"-bad", "good", "also-good"})
	assert.Equal(t, []string{"also-good", "good"}, got)
}

func TestSanitizationDropsShellMetachars(t *testing.T) {
	t.Parallel()
	got := cli.SanitizeCandidatesForTest([]string{"$(inject)", "good.name", "has space", "valid_1"})
	assert.Equal(t, []string{"good.name", "valid_1"}, got)
}

// Completion path writes nothing to disk - snapshot before/after. hashDir
// includes path + ModTime + size + content, so an atomic same-byte rewrite is
// detected via the changed ModTime. runErr is asserted nil so a failed
// invocation cannot masquerade as a no-write success.
//
// The config has Completion.Network=true and token-cache.yaml is seeded with
// one valid AND one expired exec-cred entry. The existing
// ListCachedCreds/GetCachedCred prune expired entries on read by writing the
// cache file, which completion must never do. After a full live dispatch the
// snapshot must be unchanged - proving no prune-on-read write occurred.
func TestCompletionWritesNothingToDisk(t *testing.T) {
	// NOT parallel: mutates storedConfig, rawArgv, and env globals.
	keyring.MockInit()
	dir := t.TempDir()

	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://example.invalid"}},
		Completion:    config.CompletionConfig{Network: true}, // exercise the live path
	}
	// Seed token-cache.yaml with one valid and one expired entry before writing config.
	fileStore := config.NewFileStore(dir)
	require.NoError(t, fileStore.SetCachedCred(
		config.CacheKey("k8s-ctx", "example.invalid", "c-abc"),
		"tok-valid", time.Now().Add(time.Hour),
		"k8s-ctx", "example.invalid", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "k8s-ctx", "example.invalid", "c-abc"),
	))
	require.NoError(t, fileStore.SetCachedCred(
		config.CacheKey("k8s-old", "example.invalid", "c-abc"),
		"tok-expired", time.Now().Add(-time.Hour),
		"k8s-old", "example.invalid", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "k8s-old", "example.invalid", "c-abc"),
	))

	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	// RANCHER_CLI_CONFIG_DIR makes the root Before hook load this config.
	t.Setenv(config.EnvConfigDir, dir)
	cli.SetStoredConfigForTest(t, cfg, dir)
	// Snapshot AFTER seeding but BEFORE dispatch.
	before := hashDir(t, dir)

	// Proto sentinel + words + empty currentWord (completing namespace positional).
	cli.CaptureArgvForTest([]string{
		"--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc", "--rancher-completion-cur=",
		"--generate-shell-completion",
	})
	root, err := cli.BuildRoot("test")
	require.NoError(t, err)
	root.Writer = io.Discard
	root.ErrWriter = io.Discard
	// Capture runErr: a failed invocation must not masquerade as a no-write
	// success.
	runErr := root.Run(t.Context(), cli.SanitizedArgsForTest([]string{
		"rancher", "--rancher-completion-proto=2", "get", "namespace", "--cluster", "c-abc", "--rancher-completion-cur=",
		"--generate-shell-completion",
	}))
	require.NoError(t, runErr, "completion must not exit nonzero")
	assert.Equal(t, before, hashDir(t, dir), "completion must write nothing (no prune-on-read or other writes)")
}

// TestGlobalFlagSetsAgree asserts the 4-way lockstep agreement between the
// three sources that must enumerate the same global flags:
//   - registry.go GlobalFlags (schema metadata, canonical source)
//   - internal/cli adapter.go + root.go root.Flags (live ucli flags, 11 total)
//   - argtail.go globalArgSpecs (value-classification)
//
// If any source drifts - a flag added to adapter.go but not to globalArgSpecs,
// or vice versa - this test fails immediately rather than silently misclassifying
// completions.
func TestGlobalFlagSetsAgree(t *testing.T) {
	t.Parallel()

	// Extract canonical names from runtime.GlobalFlags() schema entries.
	// Entries use formats like "--dry-run", "-o/--output", "-c/--config".
	// The canonical name is the portion after the last "--".
	schemaNames := make([]string, 0)
	for _, gf := range runtime.GlobalFlags() {
		name := gf.Name
		if _, after, found := strings.CutLast(name, "--"); found {
			schemaNames = append(schemaNames, after)
		}
	}
	sort.Strings(schemaNames)

	// Extract canonical names from globalArgSpecs (argtail.go).
	specNames := make([]string, 0)
	for _, s := range cli.GlobalArgSpecsForTest() {
		specNames = append(specNames, s.Name)
	}
	sort.Strings(specNames)

	// Extract canonical names from root.Flags (live ucli flags built by BuildRoot).
	// root.Flags = [debug, config] ++ GlobalFlags() - 12 total.
	root, err := cli.BuildRoot("test")
	require.NoError(t, err)
	rootFlagNames := make([]string, 0)
	for _, f := range root.Flags {
		rootFlagNames = append(rootFlagNames, f.Names()[0])
	}
	sort.Strings(rootFlagNames)

	assert.Equal(t, schemaNames, specNames,
		"globalArgSpecs (argtail.go) and runtime.GlobalFlags() (registry.go) must enumerate the same canonical flag names")
	assert.Equal(t, schemaNames, rootFlagNames,
		"root.Flags (adapter.go + root.go) and runtime.GlobalFlags() (registry.go) must enumerate the same canonical flag names")
}

// TestGroupLiveFlagValueCandidates covers value-taking global flags without a
// static enum (--project, --config) when network completion is enabled. The
// --project subtest runs on a line the CLI accepts, `get pods --cluster c-abc
// --project`, and asserts the leaf's live project candidates and at least one
// HTTP request; at a root or group there is no live fetch for a global flag
// (TestCompletionRejectedLineMakesNoRequest asserts the group lines that could), because the project
// list needs a cluster and only a leaf accepts --cluster. The --config subtest
// asserts no candidates and zero requests at the group level (no enum, no
// live site, the shell falls back to file completion), and the network-off
// path returns nothing with zero HTTP requests.
//
// Per-leaf flags (--cluster, --namespace, --node) are NOT in globalArgSpecs. The
// equals-form branch must not build a site for them: get --cluster=<TAB>,
// get --namespace=<TAB>, get --node=<TAB> must all produce ZERO HTTP requests
// at the group level (no kind is chosen yet, so firing a live fetch is a
// premature network request - a security assertion).
//
// NOT parallel: mutates rawArgv/rawCurrentWord globals, storedConfig, and env.
func TestGroupLiveFlagValueCandidates(t *testing.T) {
	keyring.MockInit()

	clusterItems := `{"count":1,"revision":"1","data":[{"id":"c-abc","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}}]}`
	projectItems := `{"count":1,"revision":"1","data":[{"id":"c-abc/p-xyz","metadata":{"name":"p-xyz"},"spec":{"clusterName":"c-abc"}}]}`

	newCountingSrv := func(t *testing.T, hits *atomic.Int32) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/management.cattle.io.projects/c-abc" {
				_, _ = w.Write([]byte(projectItems))
			} else {
				_, _ = w.Write([]byte(clusterItems))
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	// setupDir configures a hermetic config dir with the given server URL and network setting.
	setupDir := func(t *testing.T, srvURL string, networkOn bool) string {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srvURL}},
			Completion:    config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "tok")
		cli.SetStoredConfigForTest(t, cfg, dir)
		return dir
	}

	t.Run("get --project space-form network-on", func(t *testing.T) {
		// --project is a global flag in globalArgSpecs; --cluster c-abc in the tail
		// provides the cluster ID so the live project fetch can resolve. The line
		// names a kind (pods), as the command requires: leaf flags are not
		// accepted before the kind.
		var hits atomic.Int32
		srv := newCountingSrv(t, &hits)
		setupDir(t, srv.URL, true)
		got := runNameLikeCompletion(t, []string{"get", "pods", "--cluster", "c-abc", "--project"}, "")
		assert.Contains(t, got, "p-xyz", "get pods --project <TAB> network-on: project candidate must appear")
		assert.Greater(t, hits.Load(), int32(0), "get pods --project <TAB> network-on: must issue at least one HTTP request")
	})

	t.Run("get --project space-form group level", func(t *testing.T) {
		// At the group level no command is chosen yet, so nothing says the
		// command will take --project: no candidates and no request, even
		// with --cluster in the tail and a server that would answer.
		var hits atomic.Int32
		srv := newCountingSrv(t, &hits)
		setupDir(t, srv.URL, true)
		got := runNameLikeCompletion(t, []string{"get", "--cluster", "c-abc", "--project"}, "")
		assert.NotContains(t, got, "p-xyz", "get --project <TAB>: no project before the command is known")
		assert.Equal(t, int32(0), hits.Load(), "get --project <TAB>: zero HTTP requests at the group level")
	})

	t.Run("root --project space-form network-on", func(t *testing.T) {
		var hits atomic.Int32
		srv := newCountingSrv(t, &hits)
		setupDir(t, srv.URL, true)
		got := runNameLikeCompletion(t, []string{"--cluster", "c-abc", "--project"}, "")
		assert.NotContains(t, got, "p-xyz", "rancher --project <TAB>: no project before the command is known")
		assert.Equal(t, int32(0), hits.Load(), "rancher --project <TAB>: zero HTTP requests at the root")
	})

	t.Run("get --config space-form no-candidates", func(t *testing.T) {
		var hits atomic.Int32
		srv := newCountingSrv(t, &hits)
		setupDir(t, srv.URL, true)
		got := runNameLikeCompletion(t, []string{"get", "--config"}, "")
		assert.Empty(t, strings.TrimSpace(got), "get --config <TAB>: must produce no candidates (shell falls back to file completion)")
		assert.Equal(t, int32(0), hits.Load(), "get --config <TAB>: no live site, zero HTTP requests")
	})

	// Per-leaf flags in equals-form must not fire live HTTP requests at the
	// group level: a --cluster= token typed before any kind is chosen has no
	// leaf to resolve against, so the equals-form branch must not build a
	// SiteFlagValue for it and no cluster list may be fetched.
	t.Run("get --cluster= equals-form zero-requests", func(t *testing.T) {
		var hits atomic.Int32
		srv := newCountingSrv(t, &hits)
		setupDir(t, srv.URL, true)
		got := runNameLikeCompletion(t, []string{"get"}, "--cluster=")
		assert.Equal(t, int32(0), hits.Load(), "get --cluster=<TAB>: per-leaf flag must fire ZERO live requests at group level")
		assert.Empty(t, strings.TrimSpace(got), "get --cluster=<TAB>: must produce no candidates at group level")
	})

	t.Run("get --namespace= equals-form zero-requests", func(t *testing.T) {
		var hits atomic.Int32
		srv := newCountingSrv(t, &hits)
		setupDir(t, srv.URL, true)
		got := runNameLikeCompletion(t, []string{"get"}, "--namespace=")
		assert.Equal(t, int32(0), hits.Load(), "get --namespace=<TAB>: per-leaf flag must fire ZERO live requests at group level")
		assert.Empty(t, strings.TrimSpace(got), "get --namespace=<TAB>: must produce no candidates at group level")
	})

	t.Run("get --node= equals-form zero-requests", func(t *testing.T) {
		var hits atomic.Int32
		srv := newCountingSrv(t, &hits)
		setupDir(t, srv.URL, true)
		got := runNameLikeCompletion(t, []string{"get"}, "--node=")
		assert.Equal(t, int32(0), hits.Load(), "get --node=<TAB>: per-leaf flag must fire ZERO live requests at group level")
		assert.Empty(t, strings.TrimSpace(got), "get --node=<TAB>: must produce no candidates at group level")
	})
}
