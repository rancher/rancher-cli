package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// regexpTemp matches the path of the temporary file a facts write uses, in
// the directory of the facts files.
var regexpTemp = regexp.MustCompile(`servers/\.[0-9a-f]+\.json-[0-9a-f]+`)

const (
	reviewPath      = "/k8s/clusters/local/apis/authentication.k8s.io/v1/selfsubjectreviews"
	k8sVersionPath  = "/k8s/clusters/local/version"
	reviewAnswer    = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"u-1"}}}`
	k8sVersionReply = `{"major":"1","minor":"31","gitVersion":"v1.31.0"}`
	pastedToken     = "id:secret"
)

// loginEnv is the recorded environment of a login against a new server whose
// CA the variable gives and that the run has no variable for: the config
// directory exists and holds nothing.
func loginEnv(t *testing.T) (dir string, srv *factsServer) {
	t.Helper()
	resetFactsRecords(t)
	dir = recordingEnv(t)
	srv = newFactsServer(t)
	t.Setenv(config.EnvCACert, srv.ca)
	return dir, srv
}

// storedEntry writes a config with the server as the current entry "prod".
func storedEntry(t *testing.T, dir string, srv *factsServer) {
	t.Helper()
	writeConfig(t, dir, serverConfig("prod", map[string]string{"prod": srv.URL}))
}

// statusRoute makes the connectivity check of auth status pass.
func statusRoute(srv *factsServer) {
	srv.handle(k8sVersionPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(k8sVersionReply))
	})
}

// loginTo runs auth login with a piped token against the server with the URL
// typed with a trailing slash, as an operator may type it.
func loginTo(t *testing.T, srv *factsServer, extra ...string) recordedRun {
	t.Helper()
	argv := append([]string{"auth", "login", "prod", "--url", srv.URL + "/", "--token-stdin"}, extra...)
	return runRecordedStdin(t, newRoot(t), pastedToken+"\n", argv...)
}

// loginStored runs auth login of the stored entry with a piped token.
func loginStored(t *testing.T, extra ...string) recordedRun {
	t.Helper()
	argv := append([]string{"auth", "login", "prod", "--token-stdin"}, extra...)
	return runRecordedStdin(t, newRoot(t), pastedToken+"\n", argv...)
}

// assertRequests asserts the paths the server saw: the command's own, in
// order, then the refresh's, in either order.
func assertRequests(t *testing.T, srv *factsServer, own []string, refresh ...string) {
	t.Helper()
	got := srv.paths()
	require.GreaterOrEqual(t, len(got), len(own), "requests: %v", got)
	assert.Equal(t, own, got[:len(own)], "the command's own requests")
	assert.ElementsMatch(t, refresh, got[len(own):], "the refresh's requests")
}

// seedInstallID stores an install id record of the server.
func seedInstallID(t *testing.T, dir string, srv *factsServer, fill func(*telemetry.Facts)) {
	t.Helper()
	seedFacts(t, dir, srv.canonical(t), fill)
}

// TestLoginAsksForTheInstallIDWithTheTokenItStored asserts a login of a
// stored entry and a login with --url, each with the directory present and
// nothing stored, send their own requests in order and then the version
// and install id requests; the install id request carries the pasted token,
// the CLI User-Agent and no impersonation, at the host root.
func TestLoginAsksForTheInstallIDWithTheTokenItStored(t *testing.T) {
	rows := map[string]func(t *testing.T, dir string, srv *factsServer) recordedRun{
		"a stored entry": func(t *testing.T, dir string, srv *factsServer) recordedRun {
			t.Helper()
			storedEntry(t, dir, srv)
			return loginStored(t)
		},
		"--url": func(t *testing.T, dir string, srv *factsServer) recordedRun {
			t.Helper()
			return loginTo(t, srv)
		},
	}
	for name, login := range rows {
		t.Run(name, func(t *testing.T) {
			dir, srv := loginEnv(t)
			got := login(t, dir, srv)
			require.Zero(t, got.res.code, got.stderr)
			assertRequests(t, srv, []string{versionPath, reviewPath}, versionPath, installIDPath)
			hits := srv.hitsOn(installIDPath)
			require.Len(t, hits, 1)
			assert.Equal(t, "Bearer "+pastedToken, hits[0].header.Get("Authorization"))
			assert.Equal(t, httpx.UserAgent(), hits[0].header.Get("User-Agent"))
			assert.Empty(t, hits[0].header.Values("Impersonate-User"))
			facts, ok := readFactsOf(t, dir, srv.canonical(t))
			require.True(t, ok)
			assert.Equal(t, goodUUID, facts.InstallID.Value)
			assert.Equal(t, "2.15.2", facts.Version.Value)
		})
	}
}

// TestLoginOfATokenNoCheckAcceptedAsksForTheVersionOnly asserts a pasted
// token stored with --no-verify over https sends the preflight and the
// refresh's version request and no install id request, and over http sends
// no request at all.
func TestLoginOfATokenNoCheckAcceptedAsksForTheVersionOnly(t *testing.T) {
	t.Run("https", func(t *testing.T) {
		dir, srv := loginEnv(t)
		storedEntry(t, dir, srv)
		got := loginStored(t, "--no-verify")
		require.Zero(t, got.res.code, got.stderr)
		assert.Equal(t, []string{versionPath, versionPath}, srv.paths())
		facts, ok := readFactsOf(t, dir, srv.canonical(t))
		require.True(t, ok)
		assert.Equal(t, "2.15.2", facts.Version.Value)
		assert.Empty(t, facts.InstallID.Value)
	})
	t.Run("http", func(t *testing.T) {
		resetFactsRecords(t)
		dir := recordingEnv(t)
		var hits atomic.Int32
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			http.NotFound(w, r)
		}))
		t.Cleanup(plain.Close)
		writeConfig(t, dir, serverConfig("prod", map[string]string{"prod": plain.URL}))
		got := loginStored(t, "--no-verify")
		require.Zero(t, got.res.code, got.stderr)
		assert.Zero(t, hits.Load(), "no request reaches the server")
		assert.Empty(t, serverFiles(t, dir))
	})
}

// TestStatusAsksForTheInstallIDWithTheTokenItChecked asserts auth status
// sends its connectivity check and its three probes, then the refresh's
// version and install id requests with the token it checked: the version is
// asked for twice in all, and once when a fresh version is stored.
func TestStatusAsksForTheInstallIDWithTheTokenItChecked(t *testing.T) {
	t.Run("nothing stored", func(t *testing.T) {
		_, srv := factsEnv(t)
		statusRoute(srv)
		got := runRecorded(t, newRoot(t), "auth", "status")
		require.Zero(t, got.res.code, got.stderr)
		paths := srv.paths()
		require.Len(t, paths, 6, "%v", paths)
		assert.Equal(t, k8sVersionPath, paths[0])
		assert.Equal(t, 1, strings.Count(strings.Join(paths[1:4], " "), versionPath), "the version probe is among the three")
		assert.ElementsMatch(t, []string{versionPath, installIDPath}, paths[4:])
		assert.Len(t, srv.hitsOn(versionPath), 2)
		hits := srv.hitsOn(installIDPath)
		require.Len(t, hits, 1)
		assert.Equal(t, "Bearer token-x:y", hits[0].header.Get("Authorization"))
	})
	t.Run("a fresh version stored", func(t *testing.T) {
		dir, srv := factsEnv(t)
		statusRoute(srv)
		seedInstallID(t, dir, srv, func(f *telemetry.Facts) { f.SetVersion("v2.15.2", telemetry.EditionCommunity, time.Now()) })
		got := runRecorded(t, newRoot(t), "auth", "status")
		require.Zero(t, got.res.code, got.stderr)
		assert.Len(t, srv.hitsOn(versionPath), 1, "the status probe only")
		assert.Len(t, srv.hitsOn(installIDPath), 1)
	})
	t.Run("the connectivity check fails", func(t *testing.T) {
		_, srv := factsEnv(t)
		srv.handle(k8sVersionPath, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) })
		got := runRecorded(t, newRoot(t), "auth", "status")
		require.NotZero(t, got.res.code)
		assert.Empty(t, srv.hitsOn(installIDPath), "a failed check registers nothing")
	})
}

// TestOnlyALoginSkipsAStoredInstallIDFailure asserts the wait after a stored
// failure of the install id record: a login asks again at once, whatever
// the failure; auth status keeps the wait; a value stored an hour ago is
// never asked for again by either; and a version failure keeps its own
// wait after a login.
func TestOnlyALoginSkipsAStoredInstallIDFailure(t *testing.T) {
	failedInstallID := func(kind telemetry.FailureKind, ago time.Duration) func(*telemetry.Facts) {
		return func(f *telemetry.Facts) { f.FailInstallID(kind, time.Now().Add(-ago)) }
	}
	goodInstallIDAgo := func(f *telemetry.Facts) { f.SetInstallID(goodUUID, time.Now().Add(-time.Hour)) }
	loginRows := []struct {
		name     string
		seed     func(*telemetry.Facts)
		install  int
		versions int
	}{
		{"forbidden 1 hour ago", failedInstallID(telemetry.FailureForbidden, time.Hour), 1, 2},
		{"network 10 minutes ago", failedInstallID(telemetry.FailureNetwork, 10*time.Minute), 1, 2},
		{"a value 1 hour ago", goodInstallIDAgo, 0, 2},
		{"a version failure 1 hour ago", func(f *telemetry.Facts) { f.FailVersion(telemetry.FailureForbidden, time.Now().Add(-time.Hour)) }, 1, 1},
	}
	for _, row := range loginRows {
		t.Run("login with "+row.name, func(t *testing.T) {
			dir, srv := loginEnv(t)
			storedEntry(t, dir, srv)
			seedInstallID(t, dir, srv, row.seed)
			got := loginStored(t)
			require.Zero(t, got.res.code, got.stderr)
			assert.Len(t, srv.hitsOn(installIDPath), row.install)
			assert.Len(t, srv.hitsOn(versionPath), row.versions)
		})
	}
	statusRows := []struct {
		name string
		seed func(*telemetry.Facts)
	}{
		{"forbidden 1 hour ago", failedInstallID(telemetry.FailureForbidden, time.Hour)},
		{"a value 1 hour ago", goodInstallIDAgo},
	}
	for _, row := range statusRows {
		t.Run("status with "+row.name, func(t *testing.T) {
			dir, srv := factsEnv(t)
			statusRoute(srv)
			seedInstallID(t, dir, srv, row.seed)
			got := runRecorded(t, newRoot(t), "auth", "status")
			require.Zero(t, got.res.code, got.stderr)
			assert.Empty(t, srv.hitsOn(installIDPath))
		})
	}
}

// treeOf returns the slash-separated path of every file and directory below
// dir, sorted.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil || rel == "." {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	}))
	sort.Strings(out)
	return out
}

// missingDir sets the config directory to one that does not exist yet.
func missingDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(recordingEnv(t), "missing")
	t.Setenv(config.EnvConfigDir, dir)
	return dir
}

// TestTheFirstLoginWritesOnlyTheFactsFile asserts the first login on a
// missing config directory, with recording on, sends the requests of a login
// and writes only the server's facts file under telemetry: no event, no
// directory id and no prune marker; a later recorded run against the same
// server records an event that carries the version, the edition and the
// install id from that file.
func TestTheFirstLoginWritesOnlyTheFactsFile(t *testing.T) {
	resetFactsRecords(t)
	dir := missingDir(t)
	srv := newFactsServer(t)
	t.Setenv(config.EnvCACert, srv.ca)

	got := loginTo(t, srv)
	require.Zero(t, got.res.code, got.stderr)
	assertRequests(t, srv, []string{versionPath, reviewPath}, versionPath, installIDPath)
	hits := srv.hitsOn(installIDPath)
	require.Len(t, hits, 1)
	assert.Equal(t, "Bearer "+pastedToken, hits[0].header.Get("Authorization"))

	factsFile := telemetry.FactsFileName(srv.canonical(t))
	var telemetryTree []string
	for _, p := range treeOf(t, dir) {
		if p == "telemetry" || strings.HasPrefix(p, "telemetry/") {
			telemetryTree = append(telemetryTree, p)
		}
	}
	assert.Equal(t, []string{"telemetry", "telemetry/servers", "telemetry/servers/" + factsFile}, telemetryTree)
	for _, p := range treeOf(t, dir) {
		assert.NotContains(t, p, "events")
		assert.NotContains(t, p, "config-directory-id")
		assert.NotContains(t, p, "pruned")
	}
	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, "2.15.2", facts.Version.Value)
	assert.Equal(t, goodUUID, facts.InstallID.Value)
	assert.Empty(t, storedRuns(t, dir))

	statusRoute(srv)
	status := runRecorded(t, newRoot(t), "auth", "status")
	require.Zero(t, status.res.code, status.stderr)
	runs := storedRuns(t, dir)
	require.Len(t, runs, 1)
	assert.Equal(t, "auth status", runs[0].Fields["io.rancher.cli.command"])
	assert.Equal(t, "2.15.2", runs[0].Fields["io.rancher.manager.version"])
	assert.Equal(t, "community", runs[0].Fields["io.rancher.manager.edition"])
	assert.Equal(t, goodUUID, runs[0].Fields["io.rancher.manager.install.id"])
}

// TestTheFirstLoginThatDoesNotSucceedOrIsNotRecordedWritesNothing asserts the
// first login writes no telemetry file when the variable turns recording
// off, when the token check answers 401, and, with --no-verify, writes the
// version only.
func TestTheFirstLoginThatDoesNotSucceedOrIsNotRecordedWritesNothing(t *testing.T) {
	newRun := func(t *testing.T) (string, *factsServer) {
		t.Helper()
		resetFactsRecords(t)
		dir := missingDir(t)
		srv := newFactsServer(t)
		t.Setenv(config.EnvCACert, srv.ca)
		return dir, srv
	}
	t.Run("recording off", func(t *testing.T) {
		dir, srv := newRun(t)
		t.Setenv(telemetry.EnvTelemetry, "false")
		got := loginTo(t, srv)
		require.Zero(t, got.res.code, got.stderr)
		assert.Equal(t, []string{versionPath, reviewPath}, srv.paths())
		assert.NoDirExists(t, filepath.Join(dir, "telemetry"))
	})
	t.Run("the token check answers 401", func(t *testing.T) {
		dir, srv := newRun(t)
		srv.handle(reviewPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`))
		})
		got := loginTo(t, srv)
		assert.Equal(t, 3, got.res.code, got.stderr)
		assert.Equal(t, []string{versionPath, reviewPath}, srv.paths())
		assert.DirExists(t, dir, "the entry's commit created the config directory")
		assert.NoDirExists(t, filepath.Join(dir, "telemetry"))
	})
	t.Run("--no-verify", func(t *testing.T) {
		dir, srv := newRun(t)
		got := loginTo(t, srv, "--no-verify")
		require.Zero(t, got.res.code, got.stderr)
		assert.Equal(t, []string{versionPath, versionPath}, srv.paths())
		facts, ok := readFactsOf(t, dir, srv.canonical(t))
		require.True(t, ok)
		assert.Equal(t, "2.15.2", facts.Version.Value)
		assert.Empty(t, facts.InstallID.Value)
		assert.Empty(t, facts.InstallID.Failure)
	})
}

// writeFactsIn writes the facts file of a server under dir and returns the
// error, for a caller that cannot fail the test from its own goroutine.
func writeFactsIn(dir, canonical string, fill func(*telemetry.Facts)) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := telemetry.OpenStore(dir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	var f telemetry.Facts
	fill(&f)
	return st.WriteFacts(canonical, f)
}

// TestTheFirstLoginKeepsAGoodValueAnotherProcessStored asserts a facts file
// another process writes during the first login is read before the refresh:
// the version it holds is fresh and is not asked for again, and an install
// id request that fails stores its failure beside the value that file holds.
func TestTheFirstLoginKeepsAGoodValueAnotherProcessStored(t *testing.T) {
	resetFactsRecords(t)
	dir := missingDir(t)
	srv := newFactsServer(t)
	t.Setenv(config.EnvCACert, srv.ca)
	canonical := srv.canonical(t)
	seeded := make(chan error, 1)
	srv.handle(reviewPath, func(w http.ResponseWriter, r *http.Request) {
		err := writeFactsIn(dir, canonical, func(f *telemetry.Facts) {
			f.SetVersion("v2.14.1", telemetry.EditionPrime, time.Now())
			f.SetInstallID(goodUUID, time.Now().Add(-25*time.Hour))
		})
		select {
		case seeded <- err:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reviewAnswer))
	})
	srv.handle(installIDPath, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) })

	got := loginTo(t, srv)
	require.Zero(t, got.res.code, got.stderr)
	select {
	case err := <-seeded:
		require.NoError(t, err, "the other process's write")
	default:
		t.Fatal("the self subject review never reached the server")
	}
	assert.Len(t, srv.hitsOn(versionPath), 1, "the preflight only")
	assert.Len(t, srv.hitsOn(installIDPath), 1)
	facts, ok := readFactsOf(t, dir, srv.canonical(t))
	require.True(t, ok)
	assert.Equal(t, "2.14.1", facts.Version.Value)
	assert.Equal(t, goodUUID, facts.InstallID.Value, "the good value stays")
	require.NotNil(t, facts.InstallID.Failure)
	assert.Equal(t, telemetry.FailureForbidden, facts.InstallID.Failure.Kind)
}

// TestTheFirstLoginReadsAndWritesTheFactsFileAndNothingElse asserts the file
// operations of the first login on a missing config directory: the hook's
// stat, the stat at the end of the run, the read of the facts file (no
// telemetry directory; no file read) and the write that creates the
// directories and the file through one temporary file.
func TestTheFirstLoginReadsAndWritesTheFactsFileAndNothingElse(t *testing.T) {
	resetFactsRecords(t)
	dir := missingDir(t)
	srv := newFactsServer(t)
	t.Setenv(config.EnvCACert, srv.ca)
	ops := observeFiles(t)
	builds := countBuilds(t)

	got := loginTo(t, srv)
	require.Zero(t, got.res.code, got.stderr)
	assert.Zero(t, *builds, "no event is built")
	shaped := make([]string, len(*ops))
	for i, op := range *ops {
		op = regexpTemp.ReplaceAllString(op, "servers/<temporary>")
		shaped[i] = op
	}
	assert.Equal(t, []string{
		"stat " + dir,
		"stat " + dir,
		"openroot .",
		"lstat telemetry",
		"openroot .",
		"lstat telemetry",
		"mkdir telemetry",
		"openroot telemetry",
		"mkdirall servers",
		"openfile servers/<temporary>",
		"write servers/<temporary>",
		"close servers/<temporary>",
		"rename servers/<temporary>",
	}, shaped)
}

// TestTheFirstLoginDecisionSaysYesForAuthLoginOnly asserts that, with
// recording on, the directory missing when the run started and present now,
// and exit code 0, the decision of the first login gives a refresh for
// auth login alone among the registered commands, does no file operation for
// any other, and gives nothing for auth login when recording is off, when
// the directory was there at the start, when the line was not parsed, when
// the run failed, when no server is named or when the directory is still
// missing at the end.
func TestTheFirstLoginDecisionSaysYesForAuthLoginOnly(t *testing.T) {
	dir := recordingEnv(t)
	const server = "https://rancher.example.com"
	withServer := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: server}}}
	setStoredConfigForTest(t, withServer, dir)
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	root := newRoot(t)
	fresh := recordingState{dir: dir, on: true}
	lineOf := func(name string) keptLine {
		return keptLine{line: parseLine(root, strings.Fields(name)), parsed: true}
	}
	ops := observeFiles(t)

	for _, c := range wr.reg.Commands() {
		name := c.Describe().Name
		got := firstLoginRefresh(lineOf(name), fresh, runResult{})
		if name != "auth login" {
			assert.Equal(t, refreshInput{}, got, name)
			continue
		}
		assert.Equal(t, server, got.server)
		assert.Equal(t, dir, got.opts.ConfigDir)
		assert.True(t, got.opts.DirExisted, "the directory exists at the end of the run")
		assert.True(t, got.start.Usable, "no telemetry directory is an empty read")
	}
	assert.Equal(t, []string{"stat " + dir, "openroot .", "lstat telemetry"}, *ops, "only auth login touches a file")

	login := lineOf("auth login")
	assert.Equal(t, refreshInput{}, firstLoginRefresh(login, recordingState{dir: dir}, runResult{}), "recording off")
	assert.Equal(t, refreshInput{}, firstLoginRefresh(login, recordingState{dir: dir, on: true, dirExisted: true}, runResult{}), "the directory was there at the start")
	assert.Equal(t, refreshInput{}, firstLoginRefresh(keptLine{line: login.line}, fresh, runResult{}), "no parse")
	assert.Equal(t, refreshInput{}, firstLoginRefresh(login, fresh, runResult{code: 1}), "the run failed")
	assert.Equal(t, refreshInput{}, firstLoginRefresh(login, recordingState{dir: filepath.Join(dir, "absent"), on: true}, runResult{}), "the directory is still missing")
	setStoredConfigForTest(t, &config.Config{Servers: map[string]config.Server{"prod": {URL: server}}}, dir)
	assert.Equal(t, refreshInput{}, firstLoginRefresh(lineOf("auth login"), fresh, runResult{}), "no server")
}

// outputRow is a run whose output and exit code must be the same with
// recording off and on: how it sets up, whether it registers a fetcher, and
// whether it starts on a missing config directory or runs auth status.
type outputRow struct {
	name      string
	registers bool
	fresh     bool
	status    bool
	setup     func(t *testing.T, dir string, srv *factsServer)
	exec      func(t *testing.T, srv *factsServer) recordedRun
}

// answerEverything makes every probe of auth status answer.
func answerEverything(srv *factsServer) {
	statusRoute(srv)
	srv.handle("/k8s/clusters/local/apis/ext.cattle.io/v1/selfusers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"apiVersion":"ext.cattle.io/v1","kind":"SelfUser","status":{"userID":"u-1"}}`))
	})
	srv.handle("/k8s/clusters/local/apis/management.cattle.io/v3/tokens/token-x", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"Token","metadata":{"name":"token-x"},"authProvider":"local","isDerived":false,"expiresAt":"","current":true}`))
	})
}

// maskedRun is what a run printed and its exit code, with the config
// directory and the server's address replaced.
type maskedRun struct {
	stdout, stderr string
	code           int
}

func (row outputRow) run(t *testing.T, recording bool, tweak func(*factsServer)) maskedRun {
	t.Helper()
	resetFactsRecords(t)
	var dir string
	if row.fresh {
		dir = missingDir(t)
	} else {
		dir = recordingEnv(t)
	}
	srv := newFactsServer(t)
	if row.status {
		srv.useServer(t)
	} else {
		t.Setenv(config.EnvCACert, srv.ca)
	}
	if row.setup != nil {
		row.setup(t, dir, srv)
	}
	if tweak != nil {
		tweak(srv)
	}
	if recording {
		t.Setenv(telemetry.EnvTelemetry, "")
	} else {
		t.Setenv(telemetry.EnvTelemetry, "false")
	}
	got := row.exec(t, srv)
	host := strings.TrimPrefix(srv.URL, "https://")
	mask := func(s string) string {
		s = strings.ReplaceAll(s, dir, "<config directory>")
		s = strings.ReplaceAll(s, srv.URL, "<server>")
		return strings.ReplaceAll(s, host, "<address>")
	}
	return maskedRun{stdout: mask(got.stdout), stderr: mask(got.stderr), code: got.res.code}
}

// TestRecordingNeverChangesWhatARunPrints asserts, for the logins and the
// status run that ask the server for facts and for those that do not, the
// standard output, the standard error and the exit code are the same with
// recording off and on, also when the install id request stalls and when it
// answers 403, and nothing reaches the process's own streams.
func TestRecordingNeverChangesWhatARunPrints(t *testing.T) {
	shrinkBudget(t, 300*time.Millisecond)
	rows := []outputRow{
		{name: "login of a stored entry", registers: true, setup: storedEntry,
			exec: func(t *testing.T, srv *factsServer) recordedRun {
				t.Helper()
				return loginStored(t)
			}},
		{name: "login with --no-verify", setup: storedEntry,
			exec: func(t *testing.T, srv *factsServer) recordedRun {
				t.Helper()
				return loginStored(t, "--no-verify")
			}},
		{name: "login the token check refuses", setup: func(t *testing.T, dir string, srv *factsServer) {
			t.Helper()
			storedEntry(t, dir, srv)
			srv.handle(reviewPath, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`))
			})
		}, exec: func(t *testing.T, srv *factsServer) recordedRun {
			t.Helper()
			return loginStored(t)
		}},
		{name: "first login on a missing directory", registers: true, fresh: true,
			exec: func(t *testing.T, srv *factsServer) recordedRun {
				t.Helper()
				return loginTo(t, srv)
			}},
		{name: "status with every probe answering", registers: true, status: true,
			setup: func(t *testing.T, dir string, srv *factsServer) {
				t.Helper()
				answerEverything(srv)
			},
			exec: func(t *testing.T, srv *factsServer) recordedRun {
				t.Helper()
				return runRecorded(t, newRoot(t), "auth", "status")
			}},
		{name: "status with probes that warn", registers: true, status: true,
			setup: func(t *testing.T, dir string, srv *factsServer) {
				t.Helper()
				statusRoute(srv)
			},
			exec: func(t *testing.T, srv *factsServer) recordedRun {
				t.Helper()
				return runRecorded(t, newRoot(t), "auth", "status")
			}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			processOutput := captureProcessStreams(t)
			off := row.run(t, false, nil)
			assert.NotEmpty(t, off.stdout+off.stderr)
			modes := map[string]func(*factsServer){"recording on": nil}
			if row.registers {
				modes["install id stalled"] = func(srv *factsServer) { srv.handle(installIDPath, stall(srv)) }
				modes["install id 403"] = func(srv *factsServer) {
					srv.handle(installIDPath, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) })
				}
			}
			for name, tweak := range modes {
				assert.Equal(t, off, row.run(t, true, tweak), name)
			}
			assert.Empty(t, processOutput(), "nothing reaches the process's own output or error")
		})
	}
}
