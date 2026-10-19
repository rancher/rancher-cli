package kubeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// userIDArg is the --user-id the exec entry's arguments carry right after
// its --name, "" when none follows it.
func userIDArg(t *testing.T, args []string) string {
	t.Helper()
	i := slices.Index(args, "--name")
	require.GreaterOrEqual(t, i, 0, "the exec entry names --name")
	if i+3 < len(args) && args[i+2] == "--user-id" {
		return args[i+3]
	}
	assert.NotContains(t, args, "--user-id", "--user-id, when written, follows --name")
	return ""
}

// TestKubeconfigWritesTheAnsweringLoginsOwner asserts the exec entry
// names, with --user-id after --name, the owner the login that answers the
// plugin's lookup records - which is not the current entry's when an entry
// named <current>-<id> answers - and that a login recording no owner, or
// one --user-id would refuse, writes none and says how to get one: a login
// of the entry, of the entry the plugin login entry answered for, or of a
// new entry when no user entry is the call's. Every other outcome of the
// lookup writes none and keeps its notice.
func TestKubeconfigWritesTheAnsweringLoginsOwner(t *testing.T) {
	keyring.MockInit()
	plugin := config.Server{URL: recordsURL, Provenance: clierr.ProvenanceExecLogin}
	pemCA := string(testCAPEM(t))
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(pemCA), 0o600))
	pluginOnly := withReuse(config.Config{Servers: map[string]config.Server{"exec-login-r": plugin}})
	for _, tc := range []struct {
		name  string
		setup recordsSetup
		owner string
		// notice is the notice about the owner, "" for none.
		notice func(dir string) string
		// lines is how many notices the run prints, the machine line included.
		lines int
	}{
		{name: "the session's own login", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}}, owner: "u-prod", lines: 1},
		{name: "an entry named <current>-<id> answers", setup: recordsSetup{cfg: prodCurrent(map[string]config.Server{"prod-c-1": {URL: recordsURL}}), loggedIn: []string{"prod", "prod-c-1"}},
			owner: "u-prod-c-1", lines: 2},
		{name: "the session's own login records no owner", setup: recordsSetup{cfg: prodCurrent(nil), loggedIn: []string{"prod"}, unowned: true}, lines: 2,
			notice: func(dir string) string {
				return "this kubeconfig names no Rancher user: the stored login of 'prod' records no owner; the exec plugin does not check the user; " +
					"pipe an API token to rancher --config '" + dir + "' auth login 'prod' --token-stdin, then run rancher --config '" + dir + "' kubeconfig --cluster 'c-1' again"
			}},
		{name: "the plugin login entry an entry fell back to records no owner",
			setup: recordsSetup{cfg: withReuse(prodCurrent(map[string]config.Server{"prod-c-1": {URL: recordsURL}, "exec-login-r": plugin})), loggedIn: []string{"prod", "exec-login-r"}, unowned: true},
			lines: 3,
			notice: func(dir string) string {
				return "this kubeconfig names no Rancher user: the stored login of the exec plugin's own login entry 'exec-login-r' records no owner; the exec plugin does not check the user; " +
					"pipe an API token to rancher --config '" + dir + "' auth login 'prod-c-1' --token-stdin, then run rancher --config '" + dir + "' kubeconfig --cluster 'c-1' again"
			}},
		{name: "a plugin login entry no entry is named for records no owner",
			setup: recordsSetup{envURL: recordsURL, envToken: true, cfg: withReuse(config.Config{Servers: map[string]config.Server{"exec-login-r": plugin}}), loggedIn: []string{"exec-login-r"}, unowned: true},
			lines: 3,
			notice: func(dir string) string {
				return "this kubeconfig names no Rancher user: the stored login of the exec plugin's own login entry 'exec-login-r' records no owner; the exec plugin does not check the user; " +
					"remove 'exec-login-r' with rancher --config '" + dir + "' auth logout 'exec-login-r', then rancher --config '" + dir + "' config delete-server 'exec-login-r', " +
					"then run rancher --config '" + dir + "' kubeconfig --cluster 'c-1' again"
			}},
		{name: "a plugin login entry no entry is named for records no owner, the session's CA a path",
			setup: recordsSetup{envURL: recordsURL, envToken: true, envCA: caPath, cfg: pluginOnly, loggedIn: []string{"exec-login-r"}, unowned: true},
			lines: 3,
			notice: func(dir string) string {
				return "this kubeconfig names no Rancher user: the stored login of the exec plugin's own login entry 'exec-login-r' records no owner; the exec plugin does not check the user; " +
					"remove 'exec-login-r' with rancher --config '" + dir + "' auth logout 'exec-login-r', then rancher --config '" + dir + "' config delete-server 'exec-login-r', " +
					"then run rancher --config '" + dir + "' kubeconfig --cluster 'c-1' again"
			}},
		{name: "a plugin login entry no entry is named for records no owner, the session's CA inline",
			setup: recordsSetup{envURL: recordsURL, envToken: true, envCA: pemCA, cfg: pluginOnly, loggedIn: []string{"exec-login-r"}, unowned: true},
			lines: 3,
			notice: func(dir string) string {
				return "this kubeconfig names no Rancher user: the stored login of the exec plugin's own login entry 'exec-login-r' records no owner; the exec plugin does not check the user; " +
					"remove 'exec-login-r' with rancher --config '" + dir + "' auth logout 'exec-login-r', then rancher --config '" + dir + "' config delete-server 'exec-login-r', " +
					"then run rancher --config '" + dir + "' kubeconfig --cluster 'c-1' again"
			}},
		{name: "an owner --user-id would refuse", setup: recordsSetup{cfg: prodCurrent(map[string]config.Server{"prod": {URL: recordsURL, TokenUserID: "U/1"}}), loggedIn: []string{"prod"}, unowned: true},
			lines: 2,
			notice: func(string) string {
				return "this kubeconfig names no Rancher user: the stored login of 'prod' records the owner 'U/1', which --user-id does not accept; the exec plugin does not check the user"
			}},
		{name: "no stored login answers", setup: recordsSetup{cfg: prodCurrent(nil)}, lines: 2},
		{name: "the plugin refuses", setup: recordsSetup{cfg: prodCurrent(map[string]config.Server{"prod-c-1": {URL: "https://other.example.com"}}), loggedIn: []string{"prod"}}, lines: 2},
		{name: "the store cannot be read", setup: recordsSetup{cfg: prodCurrent(nil), envToken: true, failRead: errors.New("keychain locked")}, lines: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, lines, dir, err := runRecords(t, tc.setup)
			require.NoError(t, err)
			assert.Equal(t, tc.owner, userIDArg(t, e.Args))
			assert.Len(t, lines, tc.lines)
			if tc.notice != nil {
				assert.Contains(t, lines, tc.notice(dir))
			}
			for _, l := range lines {
				if tc.notice == nil {
					assert.NotContains(t, l, "names no Rancher user")
				}
			}
		})
	}
}

// TestKubeconfigWritesTheOwnerWithoutAPrompter asserts the lookup that
// decides --user-id runs when there is no prompter to print a notice to.
func TestKubeconfigWritesTheOwnerWithoutAPrompter(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	dir := writeTestConfig(t, "")
	writeLoggedIn(t, dir)
	res, err := newTestCommand(dir, config.NewFileStore(dir), stubFactory{}).(runtime.ReadCommand).
		Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, loggedInOwner, userIDArg(t, loadKubeconfig(t, res).AuthInfos["prod-c-1"].Exec.Args))
}

// TestKubeconfigRefusesANameAuthGetTokenRefuses asserts a --name that
// auth get-token would refuse - an environment-only session's host in the
// namespace of the --user-id cache slots, or of the legacy shim's - is
// never written: the run fails and names a login under an entry of
// another name, with the session's CA as a path, or the clause asking for
// one, naming RANCHER_CLI_CACERT as the holder, when the CA is inline.
func TestKubeconfigRefusesANameAuthGetTokenRefuses(t *testing.T) {
	keyring.MockInit()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	pemCA := string(testCAPEM(t))
	require.NoError(t, os.WriteFile(caPath, []byte(pemCA), 0o600))
	for _, tc := range []struct {
		name, url, prefix, ca, caFlag, caClause string
	}{
		{"user-id:", "https://user-id:8443", "user-id:", "", "", ""},
		{"legacy:", "https://legacy:8443", "legacy:", "", "", ""},
		{"a CA path", "https://user-id:8443", "user-id:", caPath, " --cacert '" + caPath + "'", ""},
		{"an inline CA", "https://user-id:8443", "user-id:", pemCA, "", ", adding --cacert with the path of a file that holds the server's CA certificate, which RANCHER_CLI_CACERT carries inline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, dir, err := runRecords(t, recordsSetup{envURL: tc.url, envToken: true, envCA: tc.ca})
			require.Nil(t, e, "nothing is written")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			host := tc.url[len("https://"):]
			assert.Equal(t, "the kubeconfig's --name '"+host+"-c-1' would start with '"+tc.prefix+"', which auth get-token refuses; "+
				"log in with a server entry of another name: pipe an API token to rancher --config '"+dir+"' auth login <name> --url '"+tc.url+"'"+tc.caFlag+" --token-stdin, "+
				"with a name of your choice for <name>"+tc.caClause+", then rancher --config '"+dir+"' config use-server <name>, then rancher --config '"+dir+"' kubeconfig --cluster 'c-1'",
				runtime.IO{RemedyDir: dir}.ExpandText(ce.Message))
		})
	}
}

// TestKubeconfigNamesNoLoginOfAPluginLoginEntryName asserts the notice for
// a login that records no owner never names auth login on a name in the
// exec plugin's own login entry namespace, which auth login refuses
// whatever the entry's provenance: an entry carrying such a name, as its
// own login's entry or as the entry a plugin login entry answered for, is
// removed instead.
func TestKubeconfigNamesNoLoginOfAPluginLoginEntryName(t *testing.T) {
	keyring.MockInit()
	plugin := config.Server{URL: recordsURL, Provenance: clierr.ProvenanceExecLogin}
	for _, tc := range []struct {
		name  string
		setup recordsSetup
	}{
		{"the entry's own login", recordsSetup{cfg: config.Config{CurrentServer: "exec-login-hand", Servers: map[string]config.Server{"exec-login-hand": {URL: recordsURL}}},
			loggedIn: []string{"exec-login-hand"}, unowned: true}},
		{"the entry a plugin login entry answered for", recordsSetup{cfg: withReuse(config.Config{CurrentServer: "exec-login-hand", Servers: map[string]config.Server{
			"exec-login-hand": {URL: recordsURL}, "exec-login-r": plugin}}), loggedIn: []string{"exec-login-r"}, unowned: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, lines, dir, err := runRecords(t, tc.setup)
			require.NoError(t, err)
			var notice string
			for _, l := range lines {
				if strings.Contains(l, "names no Rancher user") {
					notice = l
				}
			}
			require.NotEmpty(t, notice, "the login records no owner, so the notice is printed")
			assert.NotContains(t, notice, "auth login 'exec-login-")
			entry := tc.setup.cfg.CurrentServer
			if len(tc.setup.loggedIn) == 1 {
				entry = tc.setup.loggedIn[0]
			}
			assert.Contains(t, notice, "remove '"+entry+"' with rancher --config '"+dir+"' auth logout '"+entry+"', then rancher --config '"+dir+"' config delete-server '"+entry+"'")
		})
	}
}
