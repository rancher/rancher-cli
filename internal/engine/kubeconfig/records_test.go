package kubeconfig

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

const recordsURL = "https://r.example.com"

// countingStore counts the stored-login reads made through it and fails
// the ones named in fail.
type countingStore struct {
	config.CredStore
	reads *atomic.Int32
	fail  error
}

func (c countingStore) GetToken(ref config.ServerRef) (string, error) {
	c.reads.Add(1)
	if c.fail != nil {
		return "", c.fail
	}
	return c.CredStore.GetToken(ref)
}

// recordsSetup is one directory for a kubeconfig run: its config, the
// entries with a stored login, each recording its owner as u-<entry>
// unless unowned and the auth config in authConfig when it is set, and the
// session's variables.
type recordsSetup struct {
	cfg        config.Config
	loggedIn   []string
	unowned    bool
	authConfig string
	envURL     string
	envCA      string
	envToken   bool
	envStore   string
	failRead   error
	cluster    string
	exec       string
}

// runRecords runs kubeconfig for s and returns the exec entry, the
// notices as the invocation prints them (remedies expanded for the
// directory), the directory recorded, and the result's error.
func runRecords(t *testing.T, s recordsSetup) (*clientcmdapi.ExecConfig, []string, string, error) {
	t.Helper()
	t.Setenv(config.EnvURL, s.envURL)
	t.Setenv(config.EnvCACert, s.envCA)
	t.Setenv(config.EnvToken, "")
	if s.envToken {
		t.Setenv(config.EnvToken, "env-token")
	}
	t.Setenv(config.EnvCredentialStore, s.envStore)
	dir := t.TempDir()
	cfg := s.cfg
	if !s.unowned {
		servers := maps.Clone(cfg.Servers)
		for _, e := range s.loggedIn {
			entry := servers[e]
			entry.TokenUserID = "u-" + e
			servers[e] = entry
		}
		cfg.Servers = servers
	}
	if s.authConfig != "" {
		servers := maps.Clone(cfg.Servers)
		for _, e := range s.loggedIn {
			entry := servers[e]
			entry.TokenAuthConfig = s.authConfig
			servers[e] = entry
		}
		cfg.Servers = servers
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	file := config.NewFileStore(dir)
	for _, e := range s.loggedIn {
		require.NoError(t, file.SetToken(config.ServerRef{Name: e, URL: cfg.Servers[e].URL}, "tok-"+e))
	}
	loaded, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	lazy, err := config.NewLazyStore(loaded, dir, false)
	require.NoError(t, err)
	var reads atomic.Int32
	store := config.CredStore(lazy)
	if s.failRead != nil {
		store = countingStore{CredStore: lazy, reads: &reads, fail: s.failRead}
	}
	cluster := s.cluster
	if cluster == "" {
		cluster = "c-1"
	}
	in := map[string]string{"cluster": cluster}
	if s.exec != "" {
		in["exec-command"] = s.exec
	}
	var notices bytes.Buffer
	io := runtime.IO{RemedyDir: config.CanonicalDir(dir)}
	p := ui.New(&notices, nil, true, false).WithNoticeRewrite(io.ExpandText)
	res, err := newTestCommand(dir, store, stubFactory{}).(runtime.ReadCommand).Execute(t.Context(), execInput{s: in}, p)
	var lines []string
	if out := strings.TrimSuffix(notices.String(), "\n"); out != "" {
		lines = strings.Split(out, "\n")
	}
	if err != nil {
		require.Nil(t, res.RawBytes, "a refused run writes nothing")
		return nil, lines, config.CanonicalDir(dir), err
	}
	kc := loadKubeconfig(t, res)
	require.Len(t, kc.AuthInfos, 1)
	for _, ai := range kc.AuthInfos {
		return ai.Exec, lines, config.CanonicalDir(dir), nil
	}
	return nil, lines, "", nil
}

func prodCurrent(extra map[string]config.Server) config.Config {
	servers := map[string]config.Server{"prod": {URL: recordsURL}}
	maps.Copy(servers, extra)
	return config.Config{CurrentServer: "prod", Servers: servers}
}

// envValue returns the exec entry's value for name, and whether it is set.
func envValue(e *clientcmdapi.ExecConfig, name string) (string, bool) {
	for _, v := range e.Env {
		if v.Name == name {
			return v.Value, true
		}
	}
	return "", false
}

// TestKubeconfigRecordsTheDirectoryAndTheStoreVariable asserts the exec
// entry's env: the directory, absolute, always; the store variable empty
// on a directory that declares its store whatever is set, and on an
// undeclared one the value set when the command ran, else empty; nothing
// else. A value the declaration contradicts without RANCHER_CLI_TOKEN is
// refused at the credential read and writes nothing; with it the file is
// written with the empty value.
func TestKubeconfigRecordsTheDirectoryAndTheStoreVariable(t *testing.T) {
	keyring.MockInit()
	declared := prodCurrent(nil)
	declared.CredentialStore = "file"
	for _, tc := range []struct {
		name   string
		setup  recordsSetup
		store  string
		refuse string
	}{
		{name: "declared, variable unset", setup: recordsSetup{cfg: declared, loggedIn: []string{"prod"}}, store: ""},
		{name: "declared, variable agrees", setup: recordsSetup{cfg: declared, loggedIn: []string{"prod"}, envStore: "file"}, store: ""},
		{name: "undeclared, variable unset", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}}, store: ""},
		{name: "undeclared, variable set", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}, envStore: "file"}, store: "file"},
		{name: "declared, variable contradicts, no token", setup: recordsSetup{cfg: declared, loggedIn: []string{"prod"}, envStore: "keychain"},
			refuse: "this config directory stores credentials in \"file\""},
		{name: "declared, variable contradicts, with the token", setup: recordsSetup{cfg: declared, loggedIn: []string{"prod"}, envStore: "keychain", envToken: true}, store: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, dir, err := runRecords(t, tc.setup)
			if tc.refuse != "" {
				require.ErrorContains(t, err, tc.refuse)
				require.ErrorIs(t, err, config.ErrCredentialStoreConflict)
				return
			}
			require.NoError(t, err)
			require.Len(t, e.Env, 2, "the directory and the store variable, nothing else")
			v, ok := envValue(e, config.EnvConfigDir)
			require.True(t, ok)
			assert.Equal(t, dir, v)
			assert.True(t, filepath.IsAbs(v))
			v, ok = envValue(e, config.EnvCredentialStore)
			require.True(t, ok, "the store variable is always set, so kubectl's own never reaches the plugin")
			assert.Equal(t, tc.store, v)
		})
	}
}

// TestKubeconfigRecordsARelativeDirectoryAbsolute asserts every spelling of
// a relative directory records the one absolute path, so kubectl's working
// directory does not matter.
func TestKubeconfigRecordsARelativeDirectoryAbsolute(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	wd := t.TempDir()
	t.Chdir(wd)
	realWD, err := os.Getwd()
	require.NoError(t, err)
	cfg := prodCurrent(nil)
	cfg.SetPath(config.FilePath("acct"))
	require.NoError(t, cfg.Write())
	want := filepath.Join(realWD, "acct")
	for _, spelling := range []string{"acct", "./acct", "x/../acct", "acct" + string(os.PathSeparator)} {
		res, err := newTestCommand(spelling, config.NewFileStore(spelling), stubFactory{}).(runtime.ReadCommand).
			Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1"}}, nil)
		require.NoError(t, err, spelling)
		v, _ := envValue(loadKubeconfig(t, res).AuthInfos["prod-c-1"].Exec, config.EnvConfigDir)
		assert.Equal(t, want, v, spelling)
	}
}

// TestKubeconfigNoticesFollowTheExecPluginsLookup asserts one notice per
// outcome of the exec plugin's own lookup, by value, with every remedy as
// the invocation prints it: the session's own login says nothing more;
// another login - under RANCHER_CLI_TOKEN, the plugin's login entry, or an
// entry literally named <current>-<id> - is named; none answering names
// the ways to get one; the plugin's refusal is relayed with its remedies;
// a failed read is a notice and the file is still written. The
// machine-specific line closes every run.
func TestKubeconfigNoticesFollowTheExecPluginsLookup(t *testing.T) {
	keyring.MockInit()
	plugin := config.Server{URL: recordsURL, Provenance: clierr.ProvenanceExecLogin}
	for _, tc := range []struct {
		name  string
		setup recordsSetup
		want  func(dir string) string
	}{
		{name: "the session's own login", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}},
			want: func(string) string { return "" }},
		{name: "the stored login under RANCHER_CLI_TOKEN", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}, envToken: true},
			want: func(string) string {
				return "this kubeconfig authenticates through the stored login of 'prod', not through RANCHER_CLI_TOKEN"
			}},
		{name: "the plugin's login entry under RANCHER_CLI_TOKEN", setup: recordsSetup{cfg: withReuse(prodCurrent(map[string]config.Server{"exec-login-r": plugin})), loggedIn: []string{"exec-login-r"}, envToken: true},
			want: func(string) string {
				return "this kubeconfig authenticates through the exec plugin's own login entry 'exec-login-r', not through RANCHER_CLI_TOKEN"
			}},
		{name: "an entry named <current>-<id> with its own login", setup: recordsSetup{cfg: prodCurrent(map[string]config.Server{"prod-c-1": {URL: recordsURL}}), loggedIn: []string{"prod", "prod-c-1"}},
			want: func(string) string {
				return "this kubeconfig authenticates through the stored login of 'prod-c-1', not through the login of 'prod'"
			}},
		{name: "an entry named <current>-<id> answered by the plugin's login entry", setup: recordsSetup{cfg: withReuse(prodCurrent(map[string]config.Server{"prod-c-1": {URL: recordsURL}, "exec-login-r": plugin})), loggedIn: []string{"prod", "exec-login-r"}},
			want: func(string) string {
				return "this kubeconfig authenticates through the exec plugin's own login entry 'exec-login-r', not through the login of 'prod'"
			}},
		{name: "an entry named <current>-<id> that nothing answers for", setup: recordsSetup{cfg: prodCurrent(map[string]config.Server{"prod-c-1": {URL: recordsURL}}), loggedIn: []string{"prod"}},
			want: func(dir string) string {
				return "no stored login in " + dir + " answers for this kubeconfig: kubectl will sign in on first use when it has a terminal; " +
					"for unattended use pipe an API token to rancher --config '" + dir + "' auth login 'prod-c-1' --url '" + recordsURL + "' --token-stdin first, " +
					"or use rancher --config '" + dir + "' create kubeconfig --clusters 'c-1' --apply --yes for a kubeconfig that carries its own token"
			}},
		{name: "an environment-only session with nothing stored", setup: recordsSetup{envURL: "https://env.example.com", envToken: true},
			want: func(dir string) string {
				return "no stored login in " + dir + " answers for this kubeconfig: kubectl will sign in on first use when it has a terminal; " +
					"for unattended use pipe an API token to rancher --config '" + dir + "' auth login 'env.example.com' --url 'https://env.example.com' --token-stdin first, " +
					"or use rancher --config '" + dir + "' create kubeconfig --clusters 'c-1' --apply --yes for a kubeconfig that carries its own token"
			}},
		{name: "an entry named <current>-<id> at another URL", setup: recordsSetup{cfg: prodCurrent(map[string]config.Server{"prod-c-1": {URL: "https://other.example.com"}}), loggedIn: []string{"prod"}},
			want: func(dir string) string {
				return "kubectl's exec plugin will refuse this kubeconfig: the exec entry names server entry 'prod-c-1', which is registered at https://other.example.com, but this call runs for " + recordsURL +
					"; to use 'prod-c-1', regenerate the kubeconfig from it with rancher --config '" + dir + "' config use-server 'prod-c-1' (this switches the directory's current server; switch back with rancher --config '" + dir + "' config use-server 'prod'), " +
					"then rancher --config '" + dir + "' auth login 'prod-c-1' (if it has no login), then rancher --config '" + dir + "' kubeconfig --cluster 'c-1'; " +
					"to use " + recordsURL + ", regenerate it from an entry registered there with rancher --config '" + dir + "' config use-server <entry> (this switches the directory's current server; switch back with rancher --config '" + dir + "' config use-server 'prod'), " +
					"then rancher --config '" + dir + "' kubeconfig --cluster 'c-1'"
			}},
		{name: "a tie in an environment-only session", setup: recordsSetup{envURL: recordsURL, envToken: true,
			cfg: config.Config{Servers: map[string]config.Server{"a": {URL: recordsURL}, "b": {URL: recordsURL}}}, loggedIn: []string{"a", "b"}},
			want: func(dir string) string {
				return "kubectl's exec plugin will refuse this kubeconfig: more than one server entry for " + recordsURL + " has a stored login ('a', 'b') and this exec entry names none of them; " +
					"give the exec entry --name <entry>-c-1, or regenerate the kubeconfig from the entry you want with rancher --config '" + dir + "' config use-server <entry> (this sets the directory's current server, which is unset now), " +
					"then rancher --config '" + dir + "' kubeconfig --cluster 'c-1'"
			}},
		{name: "a store that cannot be read, under RANCHER_CLI_TOKEN", setup: recordsSetup{cfg: prodCurrent(nil), envToken: true, failRead: errors.New("keychain locked")},
			want: func(string) string {
				return "could not check the stored login for 'prod' (keychain locked); kubectl's exec plugin fails the same way until it can"
			}},
		{name: "a store that cannot be read, for a --name that identifies no entry", setup: recordsSetup{envURL: recordsURL, envToken: true,
			cfg: config.Config{Servers: map[string]config.Server{"a": {URL: recordsURL}}}, failRead: errors.New("keychain locked")},
			want: func(string) string {
				return "could not check the stored login for 'r.example.com-c-1' (keychain locked); kubectl's exec plugin fails the same way until it can"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, lines, dir, err := runRecords(t, tc.setup)
			require.NoError(t, err)
			require.NotNil(t, e)
			want := tc.want(dir)
			store, _ := envValue(e, config.EnvCredentialStore)
			machine := machineNotice(dir, store, "rancher", false)
			if want == "" {
				assert.Equal(t, []string{machine}, lines)
				return
			}
			assert.Equal(t, []string{want, machine}, lines)
		})
	}
}

func withReuse(c config.Config) config.Config {
	c.TokenReuse = true
	return c
}

// TestKubeconfigReusesTheCredentialEffectiveRead asserts the common case -
// the session's current server, its credential read by Effective - reads
// the store once, not twice: the lookup takes Effective's answer for that
// entry. Under RANCHER_CLI_TOKEN nothing was read, and the lookup reads.
func TestKubeconfigReusesTheCredentialEffectiveRead(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCACert, "")
	t.Setenv(config.EnvCredentialStore, "")
	for _, tc := range []struct {
		name     string
		envToken bool
		reads    int32
	}{
		{name: "Effective read it", reads: 1},
		{name: "RANCHER_CLI_TOKEN, nothing read before", envToken: true, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvToken, "")
			if tc.envToken {
				t.Setenv(config.EnvToken, "env-token")
			}
			dir := writeTestConfig(t, "")
			writeLoggedIn(t, dir)
			var reads atomic.Int32
			store := countingStore{CredStore: config.NewFileStore(dir), reads: &reads}
			_, err := newTestCommand(dir, store, stubFactory{}).(runtime.ReadCommand).Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1"}}, ui.Discard())
			require.NoError(t, err)
			assert.Equal(t, tc.reads, reads.Load())
		})
	}
}

// TestKubeconfigLookupReadsTheDeclaredStore asserts the lookup reads the
// store the plugin will open - the declared one, since the exec entry sets
// the variable empty there - when the variable this command ran with
// names another.
func TestKubeconfigLookupReadsTheDeclaredStore(t *testing.T) {
	keyring.MockInit()
	cfg := prodCurrent(nil)
	cfg.CredentialStore = "file"
	e, lines, dir, err := runRecords(t, recordsSetup{cfg: cfg, loggedIn: []string{"prod"}, envStore: "keychain", envToken: true})
	require.NoError(t, err)
	store, _ := envValue(e, config.EnvCredentialStore)
	assert.Empty(t, store)
	assert.Equal(t, []string{
		"this kubeconfig authenticates through the stored login of 'prod', not through RANCHER_CLI_TOKEN",
		machineNotice(dir, "", "rancher", false),
	}, lines)
}

// TestKubeconfigInstallHintAndMachineNotice asserts installHint - the
// install URL for the default command; for another, its printable form and
// the steps that write the file again from the entry it was written from,
// the selection first even for the current server - and the
// machine-specific line naming the directory, a store value that is not
// empty, and a path command, never a bare one.
func TestKubeconfigInstallHintAndMachineNotice(t *testing.T) {
	cfg := prodCurrent(nil)
	for _, tc := range []struct {
		name, exec, envStore string
		hint                 func(dir, cmd string) string
		notice               func(dir, cmd string) string
	}{
		{name: "the default command", hint: func(string, string) string { return "install the rancher CLI: https://github.com/rancher/rancher-cli" },
			notice: func(dir, _ string) string {
				return "this kubeconfig records this machine's config directory " + dir + "; it works for this user on this machine only"
			}},
		{name: "a bare name", exec: "rancher-dev",
			hint: func(dir, _ string) string {
				return "rancher-dev was not found; restore it, or regenerate this kubeconfig from entry 'prod': rancher --config '" + dir +
					"' config use-server 'prod' (this switches the directory's current server), then rancher --config '" + dir + "' kubeconfig --cluster 'c-1'"
			},
			notice: func(dir, _ string) string {
				return "this kubeconfig records this machine's config directory " + dir + "; it works for this user on this machine only"
			}},
		{name: "a path and a store value", exec: "/opt/bin/rancher", envStore: "file",
			hint: func(dir, cmd string) string {
				return cmd + " was not found; restore it, or regenerate this kubeconfig from entry 'prod': rancher --config '" + dir +
					"' config use-server 'prod' (this switches the directory's current server), then rancher --config '" + dir + "' kubeconfig --cluster 'c-1'"
			},
			notice: func(dir, cmd string) string {
				return "this kubeconfig records this machine's config directory " + dir + ", credential store file and command " + cmd + "; it works for this user on this machine only"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, lines, dir, err := runRecords(t, recordsSetup{cfg: cfg, loggedIn: []string{"prod"}, exec: tc.exec, envStore: tc.envStore})
			require.NoError(t, err)
			assert.Equal(t, tc.hint(dir, e.Command), e.InstallHint)
			require.NotEmpty(t, lines)
			assert.Equal(t, tc.notice(dir, e.Command), lines[len(lines)-1])
		})
	}
}

// TestKubeconfigHostileCharacters asserts a command and a directory
// holding a newline, an escape and a bidi character reach the file exactly
// and the terminal only printable: the notices and installHint carry no
// control or format character, and for a directory that cannot be printed
// no command is offered at all.
func TestKubeconfigHostileCharacters(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("Windows refuses control characters in file names")
	}
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	hostile := "a\nb\x1b[31m\u202e"
	dir := filepath.Join(t.TempDir(), "d"+hostile)
	cfg := prodCurrent(nil)
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	cmdPath := filepath.Join(t.TempDir(), "bin"+hostile)

	var notices bytes.Buffer
	io := runtime.IO{RemedyDir: dir}
	p := ui.New(&notices, nil, true, false).WithNoticeRewrite(io.ExpandText)
	res, err := newTestCommand(dir, config.NewFileStore(dir), stubFactory{}).(runtime.ReadCommand).
		Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1", "exec-command": cmdPath}}, p)
	require.NoError(t, err)
	e := loadKubeconfig(t, res).AuthInfos["prod-c-1"].Exec
	assert.Equal(t, cmdPath, e.Command, "the command is written exactly")
	v, _ := envValue(e, config.EnvConfigDir)
	assert.Equal(t, dir, v, "the directory is written exactly")
	for _, s := range []string{e.InstallHint, notices.String()} {
		assert.NotContains(t, s, "\x1b")
		assert.NotContains(t, s, "\u202e")
		assert.NotContains(t, s, clierr.RemedyMarker)
	}
	assert.NotContains(t, e.InstallHint, "--config", "no command names a directory it cannot print")
	assert.Contains(t, e.InstallHint, runtime.UnprintableConfigDirNote)
	assert.Contains(t, notices.String(), "kubectl will sign in on first use when it has a terminal; "+runtime.UnprintableConfigDirNote+"\n",
		"no command is offered for a directory that cannot be printed")
	assert.NotContains(t, notices.String(), "rancher ")
	assert.Contains(t, notices.String(), "(its path has characters a terminal cannot show)")
}

// TestKubeconfigEntryNameCannotInjectTheDirectory asserts a server entry
// whose name holds the remedy marker gains no --config of its own in a
// notice: quoting removes the marker from the name, so only the refusal's
// own two commands name the directory.
func TestKubeconfigEntryNameCannotInjectTheDirectory(t *testing.T) {
	name := "b" + clierr.Rancher + " auth"
	_, lines, dir, err := runRecords(t, recordsSetup{envURL: recordsURL, envToken: true,
		cfg: config.Config{Servers: map[string]config.Server{"a": {URL: recordsURL}, name: {URL: recordsURL}}}, loggedIn: []string{"a", name}})
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "('a', 'brancher auth')")
	assert.Equal(t, 2, strings.Count(lines[0], "rancher --config '"+dir+"'"))
	assert.Equal(t, 2, strings.Count(lines[0], "--config"))
}

// TestKubeconfigOmittedNoticeRelaysRemediesForTheDirectory asserts a reason
// in the omitted-contexts notice that carries a remedy - a management read
// that timed out names rancher inspect cluster - names the directory like
// every other remedy the command prints, and stays printable.
func TestKubeconfigOmittedNoticeRelaysRemediesForTheDirectory(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	dir := writeTestConfig(t, "")
	writeLoggedIn(t, dir)
	objects, lists := aceObjects("c-abc", "ace.example.test", testCAPEM(t))
	delete(objects, "multi-cluster-management")
	timedOut := clierr.Retryable(clierr.CodeGeneric,
		"context deadline exceeded\x1b[31m - the server may be slow (check with: "+clierr.Rancher+" inspect cluster <cluster-id>)")
	var notices bytes.Buffer
	io := runtime.IO{RemedyDir: config.CanonicalDir(dir)}
	p := ui.New(&notices, nil, true, false).WithNoticeRewrite(io.ExpandText)
	_, err := newTestCommand(dir, config.NewFileStore(dir), stubFactory{objects: objects, lists: lists,
		getErrs: map[string]error{"multi-cluster-management": timedOut}}).(runtime.ReadCommand).
		Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-abc"}}, p)
	require.NoError(t, err)
	got, _, _ := strings.Cut(notices.String(), "\n")
	assert.Contains(t, got, "direct endpoint contexts omitted: the multi-cluster-management feature is not readable")
	assert.Contains(t, got, "(check with: rancher --config '"+config.CanonicalDir(dir)+"' inspect cluster <cluster-id>)")
	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, clierr.RemedyMarker)
}

// TestKubeconfigInstallHintForAnEnvironmentOnlySession asserts the steps
// installHint names when the session had no entry to select: the
// variables it ran with - the CA's too when the file carries one - and the
// kubeconfig command, with no use-server and no empty entry.
func TestKubeconfigInstallHintForAnEnvironmentOnlySession(t *testing.T) {
	for _, tc := range []struct {
		name, ca string
		want     func(dir string) string
	}{
		{name: "no CA", want: func(dir string) string {
			return "rancher-dev was not found; restore it, or regenerate this kubeconfig with RANCHER_CLI_URL set to 'https://env.example.com' and " +
				"RANCHER_CLI_TOKEN set: rancher --config '" + dir + "' kubeconfig --cluster 'c-1'"
		}},
		{name: "a CA from RANCHER_CLI_CACERT", ca: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n", want: func(dir string) string {
			return "rancher-dev was not found; restore it, or regenerate this kubeconfig with RANCHER_CLI_URL set to 'https://env.example.com', " +
				"RANCHER_CLI_CACERT set to the CA this kubeconfig carries and RANCHER_CLI_TOKEN set: rancher --config '" + dir + "' kubeconfig --cluster 'c-1'"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvURL, "https://env.example.com")
			t.Setenv(config.EnvToken, "env-token")
			t.Setenv(config.EnvCACert, tc.ca)
			t.Setenv(config.EnvCredentialStore, "")
			dir := t.TempDir()
			res, err := newTestCommand(dir, config.NewFileStore(dir), stubFactory{}).(runtime.ReadCommand).
				Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1", "exec-command": "rancher-dev"}}, nil)
			require.NoError(t, err)
			e := loadKubeconfig(t, res).AuthInfos["env.example.com-c-1"].Exec
			assert.Equal(t, tc.want(config.CanonicalDir(dir)), e.InstallHint)
			assert.NotContains(t, e.InstallHint, "use-server")
		})
	}
}

// TestKubeconfigInstallHintIsPrintable asserts installHint, which kubectl
// prints itself, carries no control or format character from any value in
// it - the cluster id the server returned, not only the command.
func TestKubeconfigInstallHintIsPrintable(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	dir := writeTestConfig(t, "")
	writeLoggedIn(t, dir)
	res, err := newTestCommand(dir, config.NewFileStore(dir), stubFactory{resolved: map[string]string{"web": "c\x1b[31m-1\u202e"}}).(runtime.ReadCommand).
		Execute(t.Context(), execInput{s: map[string]string{"cluster": "web", "exec-command": "rancher-dev"}}, nil)
	require.NoError(t, err)
	kc := loadKubeconfig(t, res)
	require.Len(t, kc.AuthInfos, 1)
	for _, ai := range kc.AuthInfos {
		assert.Contains(t, ai.Exec.InstallHint, "kubeconfig --cluster 'c[31m-1'")
		assert.NotContains(t, ai.Exec.InstallHint, "\x1b")
		assert.NotContains(t, ai.Exec.InstallHint, "\u202e")
	}
}

// TestKubeconfigRefusesADirectoryItCannotMakeAbsolute asserts a directory
// that cannot be made absolute (a removed working directory, where getcwd
// fails) is refused before anything is written or requested: an exec
// entry with an empty directory would send the plugin to another one.
func TestKubeconfigRefusesADirectoryItCannotMakeAbsolute(t *testing.T) {
	dir := writeTestConfig(t, "")
	prev := canonicalDir
	canonicalDir = func(string) string { return "" }
	t.Cleanup(func() { canonicalDir = prev })
	res, err := newTestCommand(dir, config.NewFileStore(dir), stubFactory{resolveErr: errors.New("no request may be made")}).(runtime.ReadCommand).
		Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1"}}, nil)
	require.ErrorContains(t, err, "the config directory "+clierr.ShellQuote(dir)+" cannot be made absolute")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Nil(t, res.RawBytes)
}
