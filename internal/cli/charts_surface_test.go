package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/catalog"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

const (
	catalogGroupPath = "/apis/catalog.cattle.io/v1/clusterrepos"
	catalogIndexType = "/v1/catalog.cattle.io.clusterrepos/"
	downProxyPrefix  = "/k8s/clusters/c-aaa11"
	localProxyPrefix = "/k8s/clusters/local"
)

// chartsServer is one Rancher answering what `charts` reads: the cluster
// collection, the ClusterRepos of the local cluster and of c-aaa11, each
// ClusterRepo object and each ClusterRepo's index link. The ClusterRepos are
// rancher-charts (no spec.enabled), partner-charts (enabled), off-charts
// (disabled; its index answers a server error, so a command that read it
// would fail) and, when broken is set, three enabled ClusterRepos whose index
// cannot be read: unavailable-charts (503), empty-charts (500, the index
// ConfigMap "" does not exist) and unowned-charts (401). It records the
// request URIs.
type chartsServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	uris []string
}

const rancherChartsIndex = `{"apiVersion":"v1","entries":{
  "rancher-monitoring":[
    {"name":"rancher-monitoring","version":"105.0.0-rc.1","appVersion":"0.70","description":"candidate"},
    {"name":"rancher-monitoring","version":"104.1.0","appVersion":"0.65","description":"a\u001b[31mred\u0007 and\nnew"},
    {"name":"rancher-monitoring","version":"103.0.0","appVersion":"0.60","description":"old","deprecated":true}],
  "internal-chart":[
    {"name":"internal-chart","version":"1.0.0","description":"not for the UI","annotations":{"catalog.cattle.io/hidden":"true"}}],
  "retired":[
    {"name":"retired","version":"1.0.0","description":"gone","annotations":{"catalog.cattle.io/deprecated":"true"}}]}}`

const partnerChartsIndex = `{"apiVersion":"v1","entries":{
  "aaa":[{"name":"aaa","version":"1.0.0","appVersion":"1","description":"first"}]}}`

func newChartsServer(t *testing.T, broken bool) *chartsServer {
	t.Helper()
	s := &chartsServer{}
	repos := []string{
		`{"apiVersion":"catalog.cattle.io/v1","kind":"ClusterRepo","metadata":{"name":"rancher-charts"},"spec":{"url":"https://charts.example.test"}}`,
		`{"apiVersion":"catalog.cattle.io/v1","kind":"ClusterRepo","metadata":{"name":"partner-charts"},"spec":{"enabled":true}}`,
		`{"apiVersion":"catalog.cattle.io/v1","kind":"ClusterRepo","metadata":{"name":"off-charts"},"spec":{"enabled":false}}`,
	}
	repoNames := []string{"rancher-charts", "partner-charts", "off-charts"}
	failures := map[string]struct {
		status int
		body   string
	}{
		"unavailable-charts": {http.StatusServiceUnavailable, `{"type":"error","status":503,"code":"ServiceUnavailable","message":"the service is not ready"}`},
		"empty-charts":       {http.StatusInternalServerError, `{"type":"error","status":500,"code":"ServerError","message":"configmaps \"\" not found"}`},
		"unowned-charts":     {http.StatusUnauthorized, ""},
	}
	if broken {
		for _, name := range []string{"unavailable-charts", "empty-charts", "unowned-charts"} {
			repos = append(repos, `{"apiVersion":"catalog.cattle.io/v1","kind":"ClusterRepo","metadata":{"name":"`+name+`"},"spec":{"enabled":true}}`)
			repoNames = append(repoNames, name)
		}
	}
	indexes := map[string]string{"rancher-charts": rancherChartsIndex, "partner-charts": partnerChartsIndex}
	handler := func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.uris = append(s.uris, r.URL.RequestURI())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if path == steveClustersPath {
			page := steveClustersPages[r.URL.Query().Get("continue")]
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		for _, prefix := range []string{localProxyPrefix, downProxyPrefix} {
			if path == prefix+catalogGroupPath {
				_, _ = w.Write([]byte(`{"apiVersion":"catalog.cattle.io/v1","kind":"ClusterRepoList","items":[` + strings.Join(repos, ",") + `]}`))
				return
			}
		}
		for _, prefix := range []string{localProxyPrefix, downProxyPrefix} {
			name, ok := strings.CutPrefix(path, prefix+catalogGroupPath+"/")
			if !ok {
				continue
			}
			i := slices.Index(repoNames, name)
			if i < 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"type":"error","status":404,"code":"NotFound","message":"clusterrepos.catalog.cattle.io \"` + name + `\" not found"}`))
				return
			}
			_, _ = w.Write([]byte(repos[i]))
			return
		}
		for _, prefix := range []string{"", downProxyPrefix} {
			name, ok := strings.CutPrefix(path, prefix+catalogIndexType)
			if !ok {
				continue
			}
			if r.URL.RawQuery != "link=index" {
				http.Error(w, "unexpected query", http.StatusBadRequest)
				return
			}
			if body, ok := indexes[name]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
			if f, ok := failures[name]; ok && broken {
				w.WriteHeader(f.status)
				_, _ = w.Write([]byte(f.body))
				return
			}
			if name == "off-charts" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"type":"error","status":500,"code":"ServerError","message":"the index of ` + name + ` is not served"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","status":404,"code":"NotFound","message":"clusterrepos.catalog.cattle.io \"` + name + `\" not found"}`))
			return
		}
		http.NotFound(w, r)
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(handler))
	t.Cleanup(s.srv.Close)
	return s
}

// catalogRequests returns the recorded request URIs of the ClusterRepo
// collection, of each ClusterRepo object and of the index links, and drops the
// cluster collection reads.
func (s *chartsServer) catalogRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, u := range s.uris {
		if strings.Contains(u, "clusterrepos") {
			out = append(out, u)
		}
	}
	return out
}

// chartsFixture points a hermetic config directory at the TLS server, with
// its certificate as the server's CA, completion over the network on and a
// management token in the environment: one setup for a command run and a
// TAB.
func chartsFixture(t *testing.T, srv *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(certPath, pemFromServer(t, srv), 0o600))
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL, CA: certPath}},
		Completion:    config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvToken, "mgmt-token")
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
}

// runChartsCommand runs the real root with `args` against the fixture's server.
func runChartsCommand(t *testing.T, args ...string) (stdout, stderr []byte, runErr error) {
	t.Helper()
	root := grRoot(t)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	runErr = root.Run(t.Context(), append([]string{"rancher"}, args...))
	return out.Bytes(), errOut.Bytes(), runErr
}

type chartsEnvelope struct {
	Kind     string   `json:"kind"`
	Warnings []string `json:"warnings"`
	Data     []struct {
		Repository  string `json:"repository"`
		Name        string `json:"name"`
		Version     string `json:"version"`
		AppVersion  string `json:"appVersion"`
		Description string `json:"description"`
		Hidden      bool   `json:"hidden"`
		Deprecated  bool   `json:"deprecated"`
	} `json:"data"`
}

func chartsLines(env chartsEnvelope) []string {
	var out []string
	for _, it := range env.Data {
		out = append(out, it.Repository+" "+it.Name+" "+it.Version)
	}
	return out
}

// TestChartsEnvelope asserts, through the real root and a server, the
// decoded JSON envelope of a cluster listing: one line per chart in
// repository and chart name order, the newest release as the version, the
// hidden flag, the decoded text of a description with an escape sequence,
// a deprecated version and a deprecated chart left out, one warning for each
// repository whose index cannot be read (a 503, a 500 for an index that is
// not stored, a 401), and the requests the command sent: the ClusterRepo
// collection, then one index read per enabled repository in name order and
// none for the disabled one.
func TestChartsEnvelope(t *testing.T) {
	s := newChartsServer(t, true)
	chartsFixture(t, s.srv)

	stdout, stderr, err := runChartsCommand(t, "charts", "--cluster", "prod", "-o", "json")
	require.NoError(t, err, string(stderr))
	var env chartsEnvelope
	require.NoError(t, json.Unmarshal(stdout, &env))

	assert.Equal(t, "ChartList", env.Kind)
	assert.Equal(t, []string{
		"partner-charts aaa 1.0.0",
		"rancher-charts internal-chart 1.0.0",
		"rancher-charts rancher-monitoring 104.1.0",
	}, chartsLines(env))
	assert.True(t, env.Data[1].Hidden)
	assert.False(t, env.Data[0].Hidden)
	assert.Equal(t, "0.65", env.Data[2].AppVersion)
	assert.Equal(t, "a\x1b[31mred\a and\nnew", env.Data[2].Description)
	require.Len(t, env.Warnings, 3)
	assert.Contains(t, env.Warnings[0], "ClusterRepo 'empty-charts'")
	assert.Contains(t, env.Warnings[1], "ClusterRepo 'unavailable-charts'")
	assert.Contains(t, env.Warnings[2], "ClusterRepo 'unowned-charts': Rancher refused to serve its index")
	for _, line := range env.Data {
		assert.False(t, line.Deprecated)
	}
	assert.Equal(t, []string{
		downProxyPrefix + catalogGroupPath,
		downProxyPrefix + catalogIndexType + "empty-charts?link=index",
		downProxyPrefix + catalogIndexType + "partner-charts?link=index",
		downProxyPrefix + catalogIndexType + "rancher-charts?link=index",
		downProxyPrefix + catalogIndexType + "unavailable-charts?link=index",
		downProxyPrefix + catalogIndexType + "unowned-charts?link=index",
	}, s.catalogRequests())
}

// TestChartsTable asserts the text table: the five columns, no hidden
// column, and no escape or control rune of the server's description.
func TestChartsTable(t *testing.T) {
	s := newChartsServer(t, false)
	chartsFixture(t, s.srv)

	stdout, stderr, err := runChartsCommand(t, "charts", "--cluster", "c-aaa11", "--repo", "rancher-charts", "-o", "table")
	require.NoError(t, err, string(stderr))
	text := string(stdout)
	header, _, _ := strings.Cut(text, "\n")
	assert.Equal(t, []string{"REPOSITORY", "NAME", "VERSION", "APP", "VERSION", "DESCRIPTION"}, strings.Fields(header))
	assert.Contains(t, text, "a[31mred and")
	for _, r := range text {
		if r != '\n' {
			assert.False(t, r < 0x20 || r == 0x7f, "control rune %U in the table", r)
		}
	}
	assert.NotContains(t, text, "HIDDEN")
}

// TestChartsLocalAllVersions asserts the local cluster is read at /v1/...
// and --all-versions lists a chart's versions in the order Rancher serves
// them, the deprecated one left out.
func TestChartsLocalAllVersions(t *testing.T) {
	s := newChartsServer(t, false)
	chartsFixture(t, s.srv)

	stdout, stderr, err := runChartsCommand(t, "charts", "--cluster", "local", "--repo", "rancher-charts", "--all-versions", "-o", "json")
	require.NoError(t, err, string(stderr))
	var env chartsEnvelope
	require.NoError(t, json.Unmarshal(stdout, &env))
	assert.Equal(t, []string{
		"rancher-charts internal-chart 1.0.0",
		"rancher-charts rancher-monitoring 105.0.0-rc.1",
		"rancher-charts rancher-monitoring 104.1.0",
	}, chartsLines(env))
	assert.Equal(t, []string{
		localProxyPrefix + catalogGroupPath + "/rancher-charts",
		catalogIndexType + "rancher-charts?link=index",
	}, s.catalogRequests())
}

// TestChartsIncludeDeprecated asserts --include-deprecated lists the
// deprecated chart and version, marked in JSON, and adds the DEPRECATED
// column to the table.
func TestChartsIncludeDeprecated(t *testing.T) {
	s := newChartsServer(t, false)
	chartsFixture(t, s.srv)

	stdout, stderr, err := runChartsCommand(t, "charts", "--cluster", "local", "--repo", "rancher-charts", "--all-versions", "--include-deprecated", "-o", "json")
	require.NoError(t, err, string(stderr))
	var env chartsEnvelope
	require.NoError(t, json.Unmarshal(stdout, &env))
	assert.Equal(t, []string{
		"rancher-charts internal-chart 1.0.0",
		"rancher-charts rancher-monitoring 105.0.0-rc.1",
		"rancher-charts rancher-monitoring 104.1.0",
		"rancher-charts rancher-monitoring 103.0.0",
		"rancher-charts retired 1.0.0",
	}, chartsLines(env))
	assert.Equal(t, []bool{false, false, false, true, true}, []bool{env.Data[0].Deprecated, env.Data[1].Deprecated, env.Data[2].Deprecated, env.Data[3].Deprecated, env.Data[4].Deprecated})

	stdout, stderr, err = runChartsCommand(t, "charts", "--cluster", "local", "--repo", "rancher-charts", "--include-deprecated", "-o", "table")
	require.NoError(t, err, string(stderr))
	header, _, _ := strings.Cut(string(stdout), "\n")
	assert.Equal(t, []string{"REPOSITORY", "NAME", "VERSION", "APP", "VERSION", "DESCRIPTION", "DEPRECATED"}, strings.Fields(header))
	assert.Contains(t, string(stdout), "retired")
}

type chartsErrorEnvelope struct {
	Data struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Hint      string `json:"hint"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	} `json:"data"`
}

// TestChartsRepoErrors asserts a failed --repo read is the command's error:
// a missing repository is NOT_FOUND with the command that lists the
// repositories of the cluster as typed; a disabled one is VALIDATION with exit
// code 2, no retry and the command that shows it, and the command reads no
// index for it; a 503 and a 500 for an index that is not stored are
// retryable GENERIC errors with no hint; a 401 says Rancher refused to serve
// the index and sends no one to sign in; and each names the repository.
func TestChartsRepoErrors(t *testing.T) {
	s := newChartsServer(t, true)
	chartsFixture(t, s.srv)

	for _, tc := range []struct {
		repo      string
		code      string
		exit      int
		message   string
		hint      string
		retryable bool
	}{
		{"nope", "NOT_FOUND", 4, "ClusterRepo 'nope' not found", "get clusterrepos --cluster 'prod'", false},
		{"off-charts", "VALIDATION", 2, "ClusterRepo 'off-charts' is disabled", "get clusterrepo 'off-charts' --cluster 'prod'", false},
		{"unavailable-charts", "GENERIC", 1, "ClusterRepo 'unavailable-charts'", "", true},
		{"empty-charts", "GENERIC", 1, "ClusterRepo 'empty-charts'", "", true},
		{"unowned-charts", "GENERIC", 1, "ClusterRepo 'unowned-charts'", "", false},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			stdout, stderr, err := runChartsCommand(t, "charts", "--cluster", "prod", "--repo", tc.repo, "-o", "json")
			require.Error(t, err)
			coder, ok := errors.AsType[ucli.ExitCoder](err)
			require.True(t, ok, "the error carries an exit code")
			assert.Equal(t, tc.exit, coder.ExitCode())
			assert.Empty(t, stdout)
			var env chartsErrorEnvelope
			require.NoError(t, json.Unmarshal(stderr, &env))
			assert.Equal(t, tc.code, env.Data.Error.Code)
			assert.Contains(t, env.Data.Error.Message, tc.message)
			assert.Equal(t, tc.retryable, env.Data.Error.Retryable)
			if tc.repo == "off-charts" || tc.repo == "nope" {
				for _, uri := range s.catalogRequests() {
					assert.NotContains(t, uri, "/"+tc.repo+"?link=", "no index read for %s", tc.repo)
				}
			}
			if tc.repo == "unowned-charts" {
				assert.Contains(t, env.Data.Error.Message, "Rancher refused to serve its index")
			}
			if tc.hint != "" {
				assert.Contains(t, env.Data.Error.Hint, tc.hint)
			} else {
				assert.Empty(t, env.Data.Error.Hint)
			}
		})
	}
}

// TestChartsRepoSuggestionsAreAccepted asserts each suggestion of --repo is
// accepted by the command on the same server: no suggestion is refused, and
// the disabled repository, which the command refuses, is not offered.
func TestChartsRepoSuggestionsAreAccepted(t *testing.T) {
	s := newChartsServer(t, false)
	chartsFixture(t, s.srv)

	got := runCompletion(t, []string{"charts", "--cluster", "c-aaa11", "--repo"}, "")
	require.Equal(t, []string{"partner-charts", "rancher-charts"}, got)
	for _, repo := range got {
		_, _, err := runChartsCommand(t, "charts", "--cluster", "c-aaa11", "--repo", repo, "-o", "json")
		require.NoError(t, err, "suggestion %q", repo)
	}
	_, _, err := runChartsCommand(t, "charts", "--cluster", "c-aaa11", "--repo", "off-charts", "-o", "json")
	require.Error(t, err, "the command refuses a disabled repository, so offering it would fail this test")
}

// TestRepoSuggestionsAreClusterRepoNames asserts every suggestion of --repo,
// for charts and for app install, passes the name rule the commands apply
// before any request, so the command never refuses what completion offers.
func TestRepoSuggestionsAreClusterRepoNames(t *testing.T) {
	s := newChartsServer(t, true)
	chartsFixture(t, s.srv)

	for name, words := range map[string][]string{
		"charts":      {"charts", "--cluster", "c-aaa11", "--repo"},
		"app install": {"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo"},
	} {
		got := runCompletion(t, words, "")
		require.NotEmpty(t, got, name)
		for _, repo := range got {
			assert.True(t, kinds.ClusterRepoName(repo), "%s offers %q, which is not a ClusterRepo name", name, repo)
		}
	}
}

// chartsFlagCompletion says how each value-taking flag of `charts`
// completes: a live source (the command line before the flag is the row's
// words) or a reason it has none. The rows are checked against the
// command's own descriptor.
var chartsFlagCompletion = map[string]struct {
	words  []string
	reason string
}{
	"cluster": {words: []string{"charts"}},
	"repo":    {words: []string{"charts", "--cluster", "c-aaa11"}},
}

// TestChartsValueFlagsHaveACompletion asserts every value-taking flag the
// descriptor of `charts` declares has a row naming a live completion that
// offers candidates, or a reason it has none; a flag declared without a row
// fails.
func TestChartsValueFlagsHaveACompletion(t *testing.T) {
	s := newChartsServer(t, false)
	chartsFixture(t, s.srv)

	declared := map[string]bool{}
	for _, f := range catalog.NewCharts(nil, nil).Describe().Flags {
		if f.Type == "bool" {
			continue
		}
		declared[f.Name] = true
		row, ok := chartsFlagCompletion[f.Name]
		require.True(t, ok, "--%s takes a value and has no completion row", f.Name)
		if row.words == nil {
			assert.NotEmpty(t, row.reason, "--%s has neither a live source nor a reason", f.Name)
			continue
		}
		got := runCompletion(t, append(slices.Clone(row.words), "--"+f.Name), "")
		assert.NotEmpty(t, got, "--%s offers no candidates", f.Name)
	}
	for name := range chartsFlagCompletion {
		assert.True(t, declared[name], "row for --%s: the command declares no such flag", name)
	}
}
