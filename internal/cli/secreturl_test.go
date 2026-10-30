package cli

import (
	"cmp"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// urlCanary is the password the URL rows put in a server URL's userinfo. It
// is mixed case: an identity is lowercased, and the check looks for both.
const urlCanary = "CaNaRy-9x"

// The forms of a server URL carrying the canary, each of which a command
// refuses: dialable, with a bad port, schemeless, and with a user name only.
const (
	canaryURL        = "https://u:" + urlCanary + "@{host}"
	canaryURLBadPort = "https://u:" + urlCanary + "@127.0.0.1:abc"
	canaryURLNoHost  = "u:" + urlCanary + "@127.0.0.1:1"
)

// secretServerURL is the URL of the stub server of the current row.
var secretServerURL string

// secretSeen records the requests the stub server of the current row saw.
var secretSeen seenRequests

type seenRequests struct {
	mu    sync.Mutex
	paths []string
}

func (s *seenRequests) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = nil
}

func (s *seenRequests) add(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, p)
}

func (s *seenRequests) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// secretStored is the one server entry a row's config.yaml holds, named x and
// the current server unless noCurrent is set.
type secretStored struct {
	// name is the entry's name; empty stands for x.
	name string
	// url is the entry's URL as a hand edit could leave it; {host} stands for
	// the stub's address.
	url string
	// stubCA saves the stub's CA on the entry, so an unguarded dial reaches
	// the stub.
	stubCA bool
	// token records a token the CLI created for the entry and stores a login
	// for it, so a revoke has something to send.
	token bool
	// provenance, when set, is the entry's provenance.
	provenance clierr.Provenance
	// records are superseded token records of the entry.
	records   []config.SupersededToken
	noCurrent bool
	// credentialStore is the config.yaml credentialStore value.
	credentialStore string
}

// secretFiller returns the function that replaces the placeholders of a row
// with the stub's values: {url} and {host} stand for the stub's address and
// {pem} for its certificate.
func secretFiller(t *testing.T, srv *httptest.Server) func(string) string {
	t.Helper()
	pem := string(pemFromServer(t, srv))
	return func(w string) string {
		w = strings.ReplaceAll(w, "{url}", srv.URL)
		w = strings.ReplaceAll(w, "{host}", strings.TrimPrefix(srv.URL, "https://"))
		return strings.ReplaceAll(w, "{pem}", pem)
	}
}

func fillAll(fill func(string) string, words []string) []string {
	out := make([]string, 0, len(words)+2)
	for _, w := range words {
		out = append(out, fill(w))
	}
	return out
}

// writeSecretStored writes config.yaml, and a stored login when the entry has
// a recorded token, into dir.
func writeSecretStored(t *testing.T, dir string, st secretStored, fill func(string) string) {
	t.Helper()
	e := config.Server{URL: fill(st.url)}
	if st.stubCA {
		e.CA = fill("{pem}")
	}
	for _, rec := range st.records {
		rec.Server = fill(rec.Server)
		e.SupersededTokens = append(e.SupersededTokens, rec)
	}
	e.Provenance = st.provenance
	if st.token {
		e.Provenance = cmp.Or(st.provenance, clierr.ProvenanceDerived)
		e.TokenRef = &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
		e.TokenUserID = "u-abc12"
	}
	name := cmp.Or(st.name, "x")
	cfg := &config.Config{Servers: map[string]config.Server{name: e}, CredentialStore: st.credentialStore}
	if !st.noCurrent {
		cfg.CurrentServer = name
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	if st.token {
		require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: name}, "ext/token-abc12:secret"))
	}
}

// prepareSecretRow writes the config a row asks for and runs its seed.
func prepareSecretRow(t *testing.T, srv *httptest.Server, row secretRow, fill func(string) string) {
	t.Helper()
	dir := os.Getenv(config.EnvConfigDir)
	if row.stored != nil {
		writeSecretStored(t, dir, *row.stored, fill)
	}
	if row.seed != nil {
		row.seed(t, dir, srv.URL)
	}
}

// seedHit is the seed of a row that must be served from the exec plugin's
// cache: the cached credentials of a server entry at the stub's usable URL.
func seedHit(tb testing.TB, dir, serverURL string) {
	tb.Helper()
	seedCachedHit(tb, dir, serverURL)
}

// secretURLRows are the rows of the server URL inputs: one per route that
// carries a URL into a command, each refused, plus the rows that must not be
// refused.
func secretURLRows() []secretRow {
	const (
		validation = string(clierr.CodeValidation)
		env        = "RANCHER_CLI_URL"
	)
	// refusal is the start of every refusal of a URL that cannot be used.
	refusal := func(source, reason string) string {
		return "the server URL in " + source + " cannot be used: " + reason
	}
	const (
		userinfo = "it holds a user name or password"
		badPort  = "it is not a valid URL"
		noHost   = "it must be absolute (scheme://host)"
		entry    = "server entry 'x'"
		// plain is the repair of a URL typed in a flag or set in the variable.
		plain = "; give a URL like https://host, without a user name or password"
		// kubectl is what a typed --server of the plugin commands adds.
		kubectl = "; when kubectl runs this command, the URL is the --server argument of the kubeconfig's exec block"
		// update is the repair of an ordinary stored entry.
		update = "; change the entry's URL with rancher config update-server 'x' --url <url>"
	)
	var rows []secretRow
	add := func(r ...secretRow) { rows = append(rows, r...) }
	forms := []struct{ name, url, why string }{
		{"userinfo", canaryURL, userinfo},
		{"a bad port", canaryURLBadPort, badPort},
		{"no scheme", canaryURLNoHost, noHost},
	}
	// ApplyDefaultScheme gives a schemeless --server its scheme, so the
	// userinfo is what is refused.
	serverReason := func(why string) string {
		if why == noHost {
			return userinfo
		}
		return why
	}
	row := func(r secretRow) secretRow {
		r.canary, r.code = urlCanary, validation
		return r
	}
	// A credential store the process cannot open fails while the command's
	// dependencies are built, before the command runs.
	genericRow := func(r secretRow) secretRow {
		r.code = string(clierr.CodeGeneric)
		return r
	}

	for _, f := range forms {
		add(
			row(secretRow{env: env, name: "get cluster, the variable holds " + f.name, argv: []string{"get", "cluster"},
				setEnv: map[string]string{env: f.url}, message: refusal(env, f.why) + plain}),
			row(secretRow{env: env, name: "get node, the root client, the variable holds " + f.name, argv: []string{"get", "node", "--cluster", "c-1"},
				setEnv: map[string]string{env: f.url}, message: refusal(env, f.why) + plain}),
			row(secretRow{env: env, name: "auth status, the version probe, the variable holds " + f.name, argv: []string{"auth", "status"},
				setEnv: map[string]string{env: f.url}, message: refusal(env, f.why) + plain}),
			row(secretRow{env: env, name: "auth get-token, a cache miss, the variable holds " + f.name, argv: []string{"auth", "get-token", "--cluster", "c-1"},
				setEnv: map[string]string{env: f.url}, message: refusal(env, f.why) + plain}),
			row(secretRow{flag: "server", name: "auth get-token, a cache miss, --server holds " + f.name,
				argv: []string{"auth", "get-token", "--server", f.url, "--cluster", "c-1"}, message: refusal("--server", serverReason(f.why)) + plain + kubectl}),
			row(secretRow{flag: "server", name: "token, a cache miss, --server holds " + f.name,
				argv: []string{"token", "--server", f.url, "--user", "kube", "--cluster", "c-1"}, message: refusal("--server", serverReason(f.why)) + plain + kubectl}),
			row(secretRow{flag: "url", name: "config add-server, --url holds " + f.name, argv: []string{"config", "add-server", "x", "--url", f.url},
				message: refusal("--url", f.why) + plain}),
			row(secretRow{flag: "url", name: "auth login, --url holds " + f.name, argv: []string{"auth", "login", "x", "--token-stdin", "--url", f.url},
				stdin: "id:y", message: refusal("--url", f.why) + plain}),
			row(secretRow{flag: "url", name: "config update-server, --url holds " + f.name, argv: []string{"config", "update-server", "x", "--url", f.url},
				before: [][]string{strings.Fields("config add-server x --url {url} --cacert {pem}")}, message: refusal("--url", f.why) + plain}),
			row(secretRow{env: env, name: "get cluster, a stored entry holds " + f.name, argv: []string{"get", "cluster"},
				setEnv: map[string]string{env: ""}, stored: &secretStored{url: f.url}, message: refusal(entry, f.why) + update}),
			row(secretRow{flag: "server", name: "auth get-token, --name, a stored entry holds " + f.name, argv: []string{"auth", "get-token", "--name", "x", "--cluster", "c-1"},
				setEnv: map[string]string{env: ""}, stored: &secretStored{url: f.url}, message: refusal(entry, f.why) + "; change the entry's URL with rancher",
				contains: []string{"config update-server 'x' --url <url>"}}),
			row(secretRow{flag: "url", name: "auth login x, a stored entry holds " + f.name, argv: []string{"auth", "login", "x", "--token-stdin"},
				stdin: "id:y", stored: &secretStored{url: f.url}, message: refusal(entry, f.why) + update}),
			row(secretRow{flag: "url", name: "auth login x --no-verify, a stored entry holds " + f.name, argv: []string{"auth", "login", "x", "--no-verify", "--token-stdin"},
				stdin: "id:y", stored: &secretStored{url: f.url}, message: refusal(entry, f.why) + update}),
			row(secretRow{flag: "url", name: "auth login, the current entry holds " + f.name, argv: []string{"auth", "login", "--no-verify", "--token-stdin"},
				stdin: "id:y", stored: &secretStored{url: f.url}, message: refusal(entry, f.why) + update}),
			row(secretRow{flag: "url", name: "config update-server --cacert, a stored entry holds " + f.name, argv: []string{"config", "update-server", "x", "--cacert", "{pem}"},
				stored: &secretStored{url: f.url}, message: refusal(entry, f.why) + update}),
		)
	}

	hit := func(name string, argv, extraEnv []string, message string, flag, envName string) secretRow {
		r := row(secretRow{flag: flag, env: envName, name: name, argv: argv, seed: seedHit, message: message})
		if len(extraEnv) == 2 {
			r.setEnv = map[string]string{extraEnv[0]: extraEnv[1]}
		}
		return r
	}
	add(
		hit("auth get-token, a cache hit, --server holds userinfo", []string{"auth", "get-token", "--name", "prod", "--server", canaryURL, "--cluster", "c-1"}, nil,
			refusal("--server", userinfo)+plain+kubectl, "server", ""),
		hit("auth get-token, a cache hit, the variable holds userinfo", []string{"auth", "get-token", "--name", "prod", "--cluster", "c-1"}, []string{env, canaryURL},
			refusal(env, userinfo)+plain, "", env),
		hit("token, a cache hit, --server holds userinfo", []string{"token", "--server", canaryURL, "--user", "kube", "--cluster", "c-1"}, nil,
			refusal("--server", userinfo)+plain+kubectl, "server", ""),
	)

	// The name collision and CONFLICT refusals keep their own codes. The
	// stored URL differs from the typed one only by its user information:
	// they name the entry's URL as the one that cannot be used, and never
	// show a URL that reads like the typed one.
	derived := "the name was derived from the host of --url; change that entry's URL before a login, or pass an explicit name to add this server as a separate entry"
	add(
		secretRow{flag: "url", name: "config add-server on a stored entry with userinfo, with another URL", argv: []string{"config", "add-server", "x", "--url", "{url}", "--cacert", "{pem}"},
			stored: &secretStored{url: canaryURL}, canary: urlCanary, code: string(clierr.CodeConflict), message: refusal(entry, userinfo) + update, forbid: []string{"already points at"}},
		secretRow{flag: "url", name: "auth login x on a stored entry with userinfo, with another URL", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}"},
			stdin: "id:y", stored: &secretStored{url: canaryURL}, canary: urlCanary, code: validation, message: refusal(entry, userinfo) + update, forbid: []string{"is registered at"}},
		secretRow{flag: "url", name: "auth login with a derived name on a stored entry with userinfo", argv: []string{"auth", "login", "--token-stdin", "--url", "{url}"},
			stdin: "id:y", stored: &secretStored{name: "127.0.0.1", url: canaryURL}, canary: urlCanary, code: validation,
			message: refusal("server entry '127.0.0.1'", userinfo) + "; " + derived, forbid: []string{"is already registered at", "config update-server"}},
	)

	// An entry the exec plugin made for itself cannot be repaired with
	// update-server (it refuses to change that URL): the repair is to remove
	// the entry.
	pluginEntry := "server entry 'exec-login-abc'"
	pluginRepair := "; run rancher auth logout 'exec-login-abc' if it holds a login, then remove the entry with rancher config delete-server 'exec-login-abc'"
	add(
		secretRow{env: env, name: "get cluster, the only entry is the plugin's own and holds a bad port", argv: []string{"get", "cluster"},
			setEnv: map[string]string{env: ""}, stored: &secretStored{name: "exec-login-abc", url: canaryURLBadPort, noCurrent: true}, canary: urlCanary,
			code: string(clierr.CodeNotLoggedIn), message: "not logged in - no current server; the only server entry, 'exec-login-abc', is the exec plugin's own login entry, which auth login refuses; " +
				refusal(pluginEntry, badPort) + pluginRepair, forbid: []string{"update-server"}},
		secretRow{flag: "url", name: "config update-server --cacert, the plugin's own entry holds a bad port", argv: []string{"config", "update-server", "exec-login-abc", "--cacert", "{pem}"},
			stored: &secretStored{name: "exec-login-abc", url: canaryURLBadPort}, canary: urlCanary, code: validation,
			message: refusal(pluginEntry, badPort) + pluginRepair, forbid: []string{"update-server"}},
	)

	// A URL with a scheme other than http or https is refused with the same
	// form, for each source.
	const scheme = "its scheme is not http or https"
	const useHTTP = "; use http:// or https://"
	add(
		row(secretRow{flag: "url", name: "config add-server, --url holds an ftp URL", argv: []string{"config", "add-server", "x", "--url", "ftp://127.0.0.1:1"},
			message: refusal("--url", scheme) + useHTTP}),
		row(secretRow{flag: "url", name: "auth login, --url holds an ftp URL", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "ftp://127.0.0.1:1"},
			stdin: "id:y", message: refusal("--url", scheme) + useHTTP}),
		row(secretRow{flag: "url", name: "config update-server, --url holds an ftp URL", argv: []string{"config", "update-server", "x", "--url", "ftp://127.0.0.1:1"},
			before: [][]string{strings.Fields("config add-server x --url {url} --cacert {pem}")}, message: refusal("--url", scheme) + useHTTP}),
		row(secretRow{flag: "server", name: "auth get-token, a cache miss, --server holds an ftp URL", argv: []string{"auth", "get-token", "--server", "ftp://127.0.0.1:1", "--cluster", "c-1"},
			message: refusal("--server", scheme) + useHTTP + kubectl}),
		row(secretRow{flag: "server", name: "token, a cache miss, --server holds an ftp URL", argv: []string{"token", "--server", "ftp://127.0.0.1:1", "--user", "kube", "--cluster", "c-1"},
			message: refusal("--server", scheme) + useHTTP + kubectl}),
		row(secretRow{env: env, name: "auth get-token, a cache miss, the variable holds an ftp URL", argv: []string{"auth", "get-token", "--cluster", "c-1"},
			setEnv: map[string]string{env: "ftp://127.0.0.1:1"}, message: refusal(env, scheme) + useHTTP}),
		row(secretRow{flag: "server", name: "auth get-token, --name, a stored entry holds an ftp URL", argv: []string{"auth", "get-token", "--name", "x", "--cluster", "c-1"},
			setEnv: map[string]string{env: ""}, stored: &secretStored{url: "ftp://127.0.0.1:1"}, message: refusal(entry, scheme) + "; change the entry's URL with rancher",
			contains: []string{"config update-server 'x' --url <url>"}}),
		row(secretRow{flag: "url", name: "config update-server --cacert, a stored entry holds an ftp URL", argv: []string{"config", "update-server", "x", "--cacert", "{pem}"},
			stored: &secretStored{url: "ftp://127.0.0.1:1"}, message: refusal(entry, scheme) + update}),
	)

	// A store name the variable holds is shown by a command that takes no
	// secret, and left out by every command that takes one.
	const storeRefusal = "RANCHER_CLI_CREDENTIAL_STORE: unknown credential store (valid: keychain, file, auto, exec:<name>)"
	add(
		genericRow(row(secretRow{env: "RANCHER_CLI_CREDENTIAL_STORE", name: "token, the variable holds the canary", argv: []string{"token", "--server", "{url}", "--user", "kube", "--cluster", "c-1"},
			setEnv: map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": urlCanary}, message: storeRefusal})),
		genericRow(row(secretRow{env: "RANCHER_CLI_CREDENTIAL_STORE", name: "config add-server, the variable holds the canary", argv: []string{"config", "add-server", "x", "--url", "{url}"},
			setEnv: map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": urlCanary}, message: storeRefusal})),
		genericRow(row(secretRow{env: "RANCHER_CLI_CREDENTIAL_STORE", name: "config update-server, the variable holds the canary", argv: []string{"config", "update-server", "x", "--url", "{url}"},
			setEnv: map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": urlCanary}, message: storeRefusal})),
		genericRow(secretRow{env: "RANCHER_CLI_CREDENTIAL_STORE", name: "config status shows the value of a command that takes no secret", argv: []string{"config", "status"},
			setEnv: map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": "bogus"}, message: `RANCHER_CLI_CREDENTIAL_STORE: unknown credential store "bogus" (valid: keychain, file, auto, exec:<name>)`}),
	)

	// Which URL wins: only the URL a call uses is checked.
	add(
		secretRow{env: env, name: "get cluster, a bad stored entry under a good variable", argv: []string{"get", "cluster"},
			stored: &secretStored{url: canaryURL}, canary: urlCanary, noRefusal: true, reaches: true},
		secretRow{env: env, name: "get cluster, a good stored entry under a bad variable", argv: []string{"get", "cluster"},
			setEnv: map[string]string{env: canaryURL}, stored: &secretStored{url: "{url}"}, canary: urlCanary, code: validation, message: refusal(env, userinfo) + plain},
		secretRow{flag: "server", name: "auth get-token, a bad variable under a good --server", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1"},
			setEnv: map[string]string{env: canaryURL}, canary: urlCanary, noRefusal: true, forbid: []string{env}, instead: "interactive login required for auth config local"},
	)

	// Credential errors name the source and never the value, nor the next
	// word a flag left without its value takes.
	add(
		row(secretRow{flag: "cacert", name: "auth login --cacert holds the canary", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}", "--cacert", urlCanary},
			stdin: "id:y", message: "reading --cacert: the file does not exist"}),
		row(secretRow{flag: "cacert", name: "config add-server --cacert holds the canary", argv: []string{"config", "add-server", "x", "--url", "{url}", "--cacert", urlCanary},
			message: "reading --cacert: the file does not exist"}),
		row(secretRow{flag: "cacert", name: "config update-server --cacert holds the canary", argv: []string{"config", "update-server", "x", "--cacert", urlCanary},
			before: [][]string{strings.Fields("config add-server x --url {url} --cacert {pem}")}, message: "reading --cacert: the file does not exist"}),
		row(secretRow{flag: "cacert", name: "auth get-token --cacert holds the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", "--cacert", urlCanary},
			message: "reading --cacert: the file does not exist"}),
		row(secretRow{flag: "cacerts", name: "token --cacerts holds the canary", argv: []string{"token", "--server", "{url}", "--user", "kube", "--cluster", "c-1", "--cacerts", urlCanary},
			message: "reading --cacerts: the file does not exist"}),
		row(secretRow{flag: "cacert", name: "auth login --cacert takes the next word, a URL with a password", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}", "--cacert", canaryURL},
			stdin: "id:y", message: "reading --cacert: the file does not exist"}),
		row(secretRow{flag: "cacert", name: "auth get-token --cacert takes the next word, a URL with a password", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", "--cacert", canaryURL},
			message: "reading --cacert: the file does not exist"}),
		row(secretRow{env: "RANCHER_CLI_CACERT", name: "auth get-token, the variable holds the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1"},
			setEnv: map[string]string{config.EnvCACert: urlCanary}, message: "reading RANCHER_CLI_CACERT: the file does not exist"}),
		row(secretRow{env: "RANCHER_CLI_CACERT", name: "auth login, the variable holds the canary", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}"},
			stdin: "id:y", setEnv: map[string]string{config.EnvCACert: urlCanary}, message: "reading RANCHER_CLI_CACERT: the file does not exist"}),
		row(secretRow{flag: "credential-store", name: "auth login --credential-store holds the canary", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}", "--credential-store", urlCanary},
			stdin: "id:y", message: "--credential-store: unknown credential store (valid: keychain, file, auto, exec:<name>)"}),
		row(secretRow{flag: "credential-store", name: "auth login --credential-store takes the next word, a URL with a password", argv: []string{"auth", "login", "x", "--token-stdin", "--credential-store", canaryURL},
			stdin: "id:y", message: "--credential-store: unknown credential store"}),
		row(secretRow{flag: "credential-store", name: "auth login --credential-store holds an invalid helper name", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}", "--credential-store", "exec:" + urlCanary},
			stdin: "id:y", message: "--credential-store: invalid credential helper name"}),
		genericRow(row(secretRow{env: "RANCHER_CLI_CREDENTIAL_STORE", name: "auth login, the variable holds the canary", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}"},
			stdin: "id:y", setEnv: map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": urlCanary}, message: "RANCHER_CLI_CREDENTIAL_STORE: unknown credential store"})),
		genericRow(row(secretRow{env: "RANCHER_CLI_CREDENTIAL_STORE", name: "auth get-token, the variable holds the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1"},
			setEnv: map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": urlCanary}, message: "RANCHER_CLI_CREDENTIAL_STORE: unknown credential store"})),
		row(secretRow{env: "RANCHER_CLI_CREDENTIAL_STORE", name: "auth login, the variable holds the canary and the directory declares a store", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}"},
			stdin: "id:y", setEnv: map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": urlCanary}, stored: &secretStored{url: "{url}", credentialStore: "file"},
			message: "RANCHER_CLI_CREDENTIAL_STORE asks for an unrecognized value"}),
		genericRow(row(secretRow{flag: "credential-store", name: "auth login, config.yaml credentialStore holds the canary", argv: []string{"auth", "login", "x", "--token-stdin", "--url", "{url}"},
			stdin: "id:y", stored: &secretStored{url: "{url}", credentialStore: urlCanary}, message: "credentialStore: unknown credential store"})),
		row(secretRow{flag: "cacert", name: "auth get-token, a flag left without its value takes --cacert=<canary>", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "--cacert=" + urlCanary},
			message: "flag --cluster got flag --cacert as its value"}),
		row(secretRow{flag: "cacerts", name: "token, a flag left without its value takes --cacerts=<canary>", argv: []string{"token", "--server", "{url}", "--user", "kube", "--cluster", "--cacerts=" + urlCanary},
			message: "flag --cluster got flag --cacerts as its value"}),
		row(secretRow{flag: "user-id", name: "auth get-token, a flag left without its value takes --user-id=<canary>", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "--user-id=" + urlCanary},
			message: "flag --cluster got flag --user-id as its value"}),
		row(secretRow{flag: "server", name: "auth get-token, an extra argument holds the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", urlCanary},
			message: "auth get-token takes no arguments; unexpected argument at position 1"}),
		row(secretRow{flag: "server", name: "token, an extra argument holds the canary", argv: []string{"token", "--server", "{url}", "--user", "kube", "--cluster", "c-1", urlCanary},
			message: "unexpected argument"}),
		row(secretRow{flag: "server", name: "auth get-token, a bool flag is given the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", "--no-input=" + urlCanary},
			message: "invalid value for flag -no-input: the value must be true or false"}),
		row(secretRow{flag: "server", name: "auth get-token, --auth-flow holds the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", "--auth-flow", urlCanary},
			message: "--auth-flow value is not one of devicecode|authcode"}),
		row(secretRow{flag: "user-id", name: "auth get-token --user-id holds the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", "--user-id", urlCanary},
			message: "--user-id is not a Rancher user id"}),
		row(secretRow{flag: "user-id", name: "auth get-token --user-id takes the next word, a URL with a password", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", "--user-id", canaryURL},
			message: "--user-id is not a Rancher user id"}),
		row(secretRow{flag: "auth-provider", name: "auth get-token --auth-provider holds the canary", argv: []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-1", "--user-id", "u-abc12", "--auth-provider", urlCanary},
			message: "--auth-provider is not an auth config name"}),
	)
	return rows
}

// secretOKRow is a command that keeps working on a stored entry whose URL is
// unusable: it exits 0, and its output holds no part of the URL.
type secretOKRow struct {
	name   string
	argv   []string
	stdin  string
	stored secretStored
	// seed runs after the config is written, with the config directory.
	seed  func(tb testing.TB, dir string)
	check func(t *testing.T, stdout, stderr string, seen []string)
}

// dataOf decodes the envelope of stdout and returns its data.
func dataOf(t *testing.T, stdout string) any {
	t.Helper()
	var env struct {
		Data any `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env), "stdout: %s", stdout)
	return env.Data
}

// noAuthLoginCommand asserts text holds no recovery login: a command that
// names the server URL the entry holds.
func noAuthLoginCommand(t *testing.T, text string) {
	t.Helper()
	assert.NotContains(t, text, "rancher auth login")
	assert.NotContains(t, text, "--url '")
}

func secretOKRows() []secretOKRow {
	var rows []secretOKRow
	const (
		userinfo      = "it holds a user name or password"
		badPortReason = "it is not a valid URL"
		entryName     = "server entry 'x'"
		update        = "; change the entry's URL with rancher config update-server 'x' --url <url>"
	)
	for _, form := range []struct {
		name, url, reason string
		badPort           bool
	}{{"userinfo", canaryURL, userinfo, false}, {"a bad port", canaryURLBadPort, badPortReason, true}} {
		unusableEntry := "the server URL in " + entryName + " cannot be used: " + form.reason + update
		st := secretStored{url: form.url}
		withToken := secretStored{url: form.url, token: true}
		rows = append(rows,
			secretOKRow{name: "config list-servers, table, " + form.name, argv: []string{"config", "list-servers"}, stored: st,
				check: func(t *testing.T, stdout, _ string, _ []string) {
					t.Helper()
					assert.Contains(t, stdout, "x")
				}},
			secretOKRow{name: "config list-servers, json, " + form.name, argv: []string{"-o", "json", "config", "list-servers"}, stored: st,
				check: func(t *testing.T, stdout, _ string, _ []string) {
					t.Helper()
					list, ok := dataOf(t, stdout).([]any)
					require.True(t, ok)
					require.Len(t, list, 1)
					url, _ := list[0].(map[string]any)["url"].(string)
					assert.NotContains(t, url, "@")
					assert.NotContains(t, url, urlCanary)
				}},
			secretOKRow{name: "config status, json, a recorded token, " + form.name, argv: []string{"-o", "json", "config", "status"}, stored: withToken,
				check: func(t *testing.T, stdout, _ string, _ []string) {
					t.Helper()
					servers := dataOf(t, stdout).(map[string]any)["servers"].(map[string]any)
					current := servers["x"].(map[string]any)["currentToken"].(map[string]any)
					recovery, ok := current["recovery"].([]any)
					require.True(t, ok, "recovery is an empty array, not null: %v", current)
					assert.Empty(t, recovery)
					noAuthLoginCommand(t, stdout)
				}},
			secretOKRow{name: "config status, text, a recorded token, " + form.name, argv: []string{"config", "status"}, stored: withToken,
				check: func(t *testing.T, stdout, _ string, _ []string) {
					t.Helper()
					noAuthLoginCommand(t, stdout)
					assert.Contains(t, stdout, unusableEntry)
					assert.NotContains(t, stdout, "no revoke commands are shown")
				}},
			secretOKRow{name: "config use-server, " + form.name, argv: []string{"config", "use-server", "x"}, stored: st},
			secretOKRow{name: "config set-token-reuse on, a recorded token, " + form.name, argv: []string{"-o", "json", "config", "set-token-reuse", "on"}, stored: withToken},
			secretOKRow{name: "auth list-tokens, " + form.name, argv: []string{"auth", "list-tokens"}, stored: withToken},
			secretOKRow{name: "auth logout x, " + form.name, argv: []string{"auth", "logout", "x"}, stored: st},
			secretOKRow{name: "auth logout x, a recorded token, " + form.name, argv: []string{"-o", "json", "auth", "logout", "x"}, stored: secretStored{url: form.url, token: true, stubCA: true},
				check: func(t *testing.T, stdout, stderr string, seen []string) {
					t.Helper()
					assert.Empty(t, seen, "no request reaches the server of an unusable URL")
					data := dataOf(t, stdout).(map[string]any)
					notRevoked, ok := data["notRevoked"].([]any)
					require.True(t, ok, "stdout: %s", stdout)
					require.NotEmpty(t, notRevoked)
					for _, n := range notRevoked {
						assert.Equal(t, unusableEntry+"; the token was not sent", n.(map[string]any)["error"])
					}
					assert.NotContains(t, stdout, "--url '")
				}},
			secretOKRow{name: "config delete-server x, " + form.name, argv: []string{"-o", "json", "config", "delete-server", "x"}, stored: st},
			secretOKRow{name: "config delete-server x, a recorded token, " + form.name, argv: []string{"-o", "json", "config", "delete-server", "x"}, stored: secretStored{url: form.url, token: true, stubCA: true},
				check: func(t *testing.T, stdout, stderr string, seen []string) {
					t.Helper()
					assert.Empty(t, seen, "no request reaches the server of an unusable URL")
					data := dataOf(t, stdout).(map[string]any)
					notRevoked, ok := data["notRevoked"].([]any)
					require.True(t, ok, "stdout: %s", stdout)
					require.NotEmpty(t, notRevoked)
					for _, n := range notRevoked {
						entry := n.(map[string]any)
						assert.Equal(t, "the server URL in "+entryName+" cannot be used: "+form.reason+"; this command removes the entry; the token was not sent", entry["error"])
						if form.badPort {
							_, hasRecovery := entry["recovery"]
							assert.False(t, hasRecovery, "no recovery sequence is built from an unusable server")
						}
					}
					if form.badPort {
						noAuthLoginCommand(t, stdout)
					}
				}},
			secretOKRow{name: "config delete-server x, text, a recorded token, " + form.name, argv: []string{"config", "delete-server", "x"}, stored: secretStored{url: form.url, token: true, stubCA: true},
				check: func(t *testing.T, stdout, stderr string, seen []string) {
					t.Helper()
					assert.Empty(t, seen)
					assert.NotContains(t, stdout+stderr, "config update-server", "the entry is gone: no repair command can run")
					if form.badPort {
						noAuthLoginCommand(t, stdout+stderr)
						assert.Contains(t, stderr, "the server recorded for token 'token-abc12' cannot be used: "+form.reason+
							"; the token stays on the server that issued it; delete it there with rancher delete token 'token-abc12' --apply --yes while signed in to that server")
					}
				}},
			secretOKRow{name: "config update-server x --url, the repair, " + form.name, argv: []string{"config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}"}, stored: st},
			secretOKRow{name: "auth delete-token x, " + form.name, argv: []string{"auth", "delete-token", "x"}, stored: withToken},
		)
	}
	return append(rows, secretRecordRows()...)
}

// oldRecord is a superseded token whose recorded server is the text the
// earlier identity fallback wrote for a URL with userinfo and a bad port: it
// does not canonicalize, and it names a server other than the entry's.
var oldRecord = config.SupersededToken{
	Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old99"}, UserID: "u-abc12",
	SupersededAt: "2026-10-01T00:00:00Z", Server: "https://u:canary-9x@127.0.0.1:abc",
}

// recordStays is the text for oldRecord, whatever the entry holds.
const recordStays = "the server recorded for token 'token-old99' cannot be used: it is not a valid URL; " +
	"the token stays on the server that issued it; delete it there with rancher delete token 'token-old99' --apply --yes while signed in to that server"

// assertRecordStays asserts text holds the text for oldRecord, no repair of
// the entry, and no command built from the record's server.
func assertRecordStays(t *testing.T, text string) {
	t.Helper()
	assert.Contains(t, text, recordStays)
	assert.NotContains(t, text, "config update-server")
	assert.NotContains(t, text, "no revoke commands are shown")
	assert.NotContains(t, text, "127.0.0.1:abc'", "no command names the record's server")
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "token-old99") {
			assert.NotContains(t, line, "waits for a login")
			assert.NotContains(t, line, "rancher auth login")
		}
	}
}

// seedLegacyHostRecord writes a cache record whose server is a bare host, as
// a release before records carried a scheme wrote it.
func seedLegacyHostRecord(tb testing.TB, dir string) {
	tb.Helper()
	require.NoError(tb, config.NewFileStore(dir).SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok", time.Time{},
		"kubeuser", "acme.ex", "c-1", config.CacheBindingForTest(dir, "", "")))
}

// secretRecordRows are the rows of a token record whose server cannot be
// used, on an entry whose own URL can, cannot, or is the exec plugin's.
func secretRecordRows() []secretOKRow {
	usable := secretStored{url: "{url}", stubCA: true, token: true, records: []config.SupersededToken{oldRecord}}
	text := func(t *testing.T, stdout, stderr string, _ []string) {
		t.Helper()
		assertRecordStays(t, stdout+stderr)
	}
	rows := []secretOKRow{
		{name: "config status, a usable entry and a record with an unusable server", argv: []string{"config", "status"}, stored: usable, check: text},
		{name: "config set-token-reuse off, a usable plugin entry and a record with an unusable server", argv: []string{"config", "set-token-reuse", "off"},
			stored: secretStored{name: "exec-login-abc", url: "{url}", stubCA: true, token: true, provenance: clierr.ProvenanceExecLogin, records: []config.SupersededToken{oldRecord}},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertRecordStays(t, stdout+stderr)
				assert.NotContains(t, stdout+stderr, "auth logout 'exec-login-abc' if it holds a login", "the entry is usable: only the record is the problem")
			}},
		{name: "config set-token-reuse off, a plugin entry with an unusable URL and a record that names no server", argv: []string{"config", "set-token-reuse", "off"},
			stored: secretStored{name: "exec-login-abc", url: canaryURLBadPort, provenance: clierr.ProvenanceExecLogin,
				records: []config.SupersededToken{{Ref: oldRecord.Ref, UserID: oldRecord.UserID, SupersededAt: oldRecord.SupersededAt}}},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assert.Contains(t, stdout+stderr, "the server URL in server entry 'exec-login-abc' cannot be used: it is not a valid URL; run rancher auth logout 'exec-login-abc' if it holds a login, then remove the entry with rancher config delete-server 'exec-login-abc'")
				assert.NotContains(t, stdout+stderr, "config update-server")
			}},
		{name: "auth logout x, a usable entry and a record with an unusable server", argv: []string{"auth", "logout", "x"}, stored: usable, check: text},
		{name: "config delete-server x, a usable entry and a record with an unusable server", argv: []string{"config", "delete-server", "x"}, stored: usable, check: text},
		{name: "auth login x, a pasted token, a usable entry and a record with an unusable server", argv: []string{"auth", "login", "x", "--token-stdin", "--no-verify"}, stdin: "id:y",
			stored: usable, check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertRecordStays(t, stdout+stderr)
				assert.Contains(t, stdout+stderr, "the superseded token ext/token-old99 was not retired: "+recordStays)
			}},
		{name: "config status, a usable plugin entry and a record with an unusable server", argv: []string{"config", "status"},
			stored: secretStored{name: "exec-login-abc", url: "{url}", stubCA: true, token: true, records: []config.SupersededToken{oldRecord}},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertRecordStays(t, stdout+stderr)
				assert.NotContains(t, stdout+stderr, "config delete-server")
			}},
		{name: "config update-server x --url, the repair of an entry with a recorded token", argv: []string{"config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}"},
			stored: secretStored{url: "https://u:" + urlCanary + "@{host}:abc", stubCA: true, token: true},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assert.NotContains(t, stdout+stderr, "config update-server", "the entry is repaired by this run")
				assert.Contains(t, stdout+stderr, "the server recorded for token 'token-abc12' cannot be used: ")
			}},
		{name: "config update-server x --url, only the user name and password are removed, json", argv: []string{"-o", "json", "config", "update-server", "x", "--url", "{url}"},
			stored: secretStored{url: "https://u:" + urlCanary + "@{host}", stubCA: true, token: true},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				data := dataOf(t, stdout).(map[string]any)
				assert.Equal(t, false, data["unchanged"])
				assert.Equal(t, false, data["credentialsCleared"])
				assert.Equal(t, []any{"url"}, data["changed"], "the stored URL changed, though the server did not")
				assert.Empty(t, data["notRevoked"])
				assert.NotContains(t, stdout+stderr, "otherServer")
				dir := os.Getenv(config.EnvConfigDir)
				raw, err := os.ReadFile(config.FilePath(dir))
				require.NoError(t, err)
				assert.NotContains(t, string(raw), "@", "config.yaml holds no user information")
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				assert.NotEmpty(t, cfg.Servers["x"].CA, "the saved CA is kept")
				require.NotNil(t, cfg.Servers["x"].TokenRef, "the login is kept")
				tok, err := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "x"})
				require.NoError(t, err)
				assert.Equal(t, "ext/token-abc12:secret", tok, "the stored login is kept")
			}},
		{name: "config update-server x --url, only the user name and password are removed, text", argv: []string{"config", "update-server", "x", "--url", "{url}"},
			stored: secretStored{url: "https://u:" + urlCanary + "@{host}", stubCA: true, token: true},
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assert.Contains(t, stdout, "url: https://127.0.0.1:")
				assert.Contains(t, stdout, " (user name and password removed)")
				assert.NotContains(t, stdout, "(changed)")
			}},
		{name: "config update-server x --url, an uppercase host with no user information is a plain change", argv: []string{"config", "update-server", "x", "--url", "{url}"},
			stored: secretStored{url: "HTTPS://{host}", stubCA: true},
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assert.NotContains(t, stdout, "user name and password removed")
			}},
	}
	rows = append(rows, secretCanonicalRecordRows()...)
	rows = append(rows, secretRemovedEntryRows()...)
	for _, cmd := range [][]string{{"config", "delete-server", "x"}, {"auth", "logout", "x"}} {
		rows = append(rows, secretOKRow{name: strings.Join(cmd, " ") + ", a legacy record names a bare host", argv: append([]string{"-o", "json"}, cmd...),
			stored: secretStored{url: "https://acme.ex"}, seed: seedLegacyHostRecord,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				data := dataOf(t, stdout).(map[string]any)
				notRevoked, ok := data["notRevoked"].([]any)
				require.True(t, ok, "stdout: %s", stdout)
				require.NotEmpty(t, notRevoked)
				for _, n := range notRevoked {
					assert.Equal(t, "acme.ex", n.(map[string]any)["server"], "the host shows as the record names it")
				}
			}})
	}
	rows = append(rows, secretOKRow{name: "config delete-server x, text, a legacy record names a bare host", argv: []string{"config", "delete-server", "x"},
		stored: secretStored{url: "https://acme.ex"}, seed: seedLegacyHostRecord,
		check: func(t *testing.T, stdout, _ string, _ []string) {
			t.Helper()
			assert.Contains(t, stdout, "tokens on acme.ex:")
			assert.NotContains(t, stdout, "unusable:")
		}})
	return rows
}

// TestSecretURLCommandsThatKeepWorking asserts that the commands that only
// list, show or remove stored entries keep working on an entry whose URL is
// unusable (a hand-edited config.yaml is the only way to make one), and that
// their output holds no part of the URL: the commands that connect are in the
// table of refusals. A revoke that such a command starts never contacts the
// entry's server and builds no command from its URL. Not parallel: it sets
// the process environment.
func TestSecretURLCommandsThatKeepWorking(t *testing.T) {
	restoreRawArgv(t)
	for _, row := range secretOKRows() {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, code := runSecretOKRow(t, row)
			require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			assertNoCanary(t, urlCanary, map[string]string{"stdout": stdout, "stderr": stderr})
			if row.check != nil {
				row.check(t, withoutRunDirectory(stdout), withoutRunDirectory(stderr), secretSeen.all())
			}
		})
	}
}

// runSecretOKRow runs the command line of row against a stored entry in a
// fresh stub server and config directory. Not parallel: it sets the process
// environment.
func runSecretOKRow(t *testing.T, row secretOKRow) (stdout, stderr string, code int) {
	t.Helper()
	srv := secretServer(t, nil, secretBearer)
	fill := secretFiller(t, srv)
	t.Setenv(config.EnvURL, "")
	writeSecretStored(t, os.Getenv(config.EnvConfigDir), row.stored, fill)
	if row.seed != nil {
		row.seed(t, os.Getenv(config.EnvConfigDir))
	}
	dir := os.Getenv(config.EnvConfigDir)
	before := directoryFiles(t, dir)
	stdout, stderr, code = runWithStdin(t, fillAll(fill, row.argv), row.stdin)
	for name, content := range directoryFiles(t, dir) {
		if name != "config.yaml" && before[name] != content {
			assertNoCanary(t, urlCanary, map[string]string{"written file " + name: content})
		}
	}
	return stdout, stderr, code
}

// directoryFiles is the content of every file under dir, by relative path.
func directoryFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, path)
		files[rel] = string(b)
		return nil
	}))
	return files
}

// The two forms every refusal of a server URL that cannot be used takes: the
// URL's own, naming its source, and the server a token record names.
var (
	urlRefusalForm    = regexp.MustCompile(`the server URL in (--server|--url|RANCHER_CLI_URL|server entry '[^']+') cannot be used: [^;]+; \S`)
	recordRefusalForm = regexp.MustCompile(`the server recorded for token '([^']+)' cannot be used: [^;]+; \S`)
)

// TestEveryRefusalOfAServerURLHasOneForm asserts that each place that refuses
// a server URL it cannot use, or says what to do about one, words it in the
// one form: "the server URL in <source> cannot be used: <reason>; <repair>"
// (or, for the server a token record names, "the server recorded for token
// '<ref>' cannot be used: <reason>; <repair>"), and names the source the URL
// came from. Each site is driven through its command line with the row that
// asserts its repair text. Not parallel: it sets the process environment.
func TestEveryRefusalOfAServerURLHasOneForm(t *testing.T) {
	restoreRawArgv(t)
	refusals := map[string]secretRow{}
	for _, r := range secretURLRows() {
		refusals[r.name] = r
	}
	kept := map[string]secretOKRow{}
	for _, r := range secretOKRows() {
		kept[r.name] = r
	}
	const execEntry = "server entry 'exec-login-abc'"
	for _, site := range []struct {
		where, row, source string
	}{
		{"--server of auth get-token", "auth get-token, a cache miss, --server holds userinfo", "--server"},
		{"--server of token", "token, a cache miss, --server holds userinfo", "--server"},
		{"--server of auth get-token, a scheme", "auth get-token, a cache miss, --server holds an ftp URL", "--server"},
		{"RANCHER_CLI_URL through connection", "get cluster, the variable holds userinfo", "RANCHER_CLI_URL"},
		{"RANCHER_CLI_URL of the plugin, a scheme", "auth get-token, a cache miss, the variable holds an ftp URL", "RANCHER_CLI_URL"},
		{"a stored entry through connection", "get cluster, a stored entry holds userinfo", "server entry 'x'"},
		{"a stored entry of the plugin, a scheme", "auth get-token, --name, a stored entry holds an ftp URL", "server entry 'x'"},
		{"entryLoginTarget", "auth login x, a stored entry holds userinfo", "server entry 'x'"},
		{"--url", "config add-server, --url holds userinfo", "--url"},
		{"--url, a scheme", "config add-server, --url holds an ftp URL", "--url"},
		{"the stored URL of update-server --cacert", "config update-server --cacert, a stored entry holds userinfo", "server entry 'x'"},
		{"the stored URL of update-server --cacert, a scheme", "config update-server --cacert, a stored entry holds an ftp URL", "server entry 'x'"},
		{"ErrNotLoggedInFor", "get cluster, the only entry is the plugin's own and holds a bad port", execEntry},
		{"the plugin's own entry of update-server", "config update-server --cacert, the plugin's own entry holds a bad port", execEntry},
		{"the CONFLICT of add-server", "config add-server on a stored entry with userinfo, with another URL", "server entry 'x'"},
		{"the name collision of auth login", "auth login x on a stored entry with userinfo, with another URL", "server entry 'x'"},
		{"the derived name collision of auth login", "auth login with a derived name on a stored entry with userinfo", "server entry '127.0.0.1'"},
	} {
		t.Run(site.where, func(t *testing.T) {
			row, ok := refusals[site.row]
			require.True(t, ok, "no row %q", site.row)
			stdout, stderr, code := runSecretRow(t, row)
			require.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			message := renderedError(t, "json", stderr).Message
			m := urlRefusalForm.FindStringSubmatch(message)
			require.NotNil(t, m, "message: %s", message)
			assert.Equal(t, site.source, m[1])
		})
	}
	for _, site := range []struct {
		where, row string
		form       *regexp.Regexp
		source     string
	}{
		{"the revoke client of auth logout", "auth logout x, a recorded token, userinfo", urlRefusalForm, "server entry 'x'"},
		{"the revoke client of config delete-server", "config delete-server x, a recorded token, userinfo", urlRefusalForm, "server entry 'x'"},
		{"the record lines of config status, the entry's URL", "config status, text, a recorded token, userinfo", urlRefusalForm, "server entry 'x'"},
		{"the record lines of config status, a record's server", "config status, a usable entry and a record with an unusable server", recordRefusalForm, "token-old99"},
		{"the dormant plugin logins of config set-token-reuse off", "config set-token-reuse off, a usable plugin entry and a record with an unusable server", recordRefusalForm, "token-old99"},
		{"the warning of auth logout, a record's server", "auth logout x, a usable entry and a record with an unusable server", recordRefusalForm, "token-old99"},
		{"the warning of auth login with a pasted token", "auth login x, a pasted token, a usable entry and a record with an unusable server", recordRefusalForm, "token-old99"},
	} {
		t.Run(site.where, func(t *testing.T) {
			row, ok := kept[site.row]
			require.True(t, ok, "no row %q", site.row)
			stdout, stderr, code := runSecretOKRow(t, row)
			require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			m := site.form.FindStringSubmatch(stdout + stderr)
			require.NotNil(t, m, "output: %s%s", stdout, stderr)
			assert.Equal(t, site.source, m[1])
		})
	}
}

// A record whose server is the entry's own URL plus user information, and one
// at another port with user information. Both canonicalize, so the recovery
// sequence names the URL without it, whatever the entry holds; the owner
// differs from the entry's, so no login of the entry retires either.
var (
	sameServerRecord = config.SupersededToken{
		Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-same77"}, UserID: "u-other77",
		SupersededAt: "2026-10-01T00:00:00Z", Server: "https://u:" + urlCanary + "@{host}",
	}
	otherPortRecord = config.SupersededToken{
		Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-port55"}, UserID: "u-other77",
		SupersededAt: "2026-10-01T00:00:00Z", Server: "https://u:" + urlCanary + "@127.0.0.1:9443",
	}
)

// assertSequence asserts text holds every command of the recovery sequence of
// rec at server, in order, with no repair of the entry and no text for a
// record whose server cannot be used beside it.
func assertSequence(t *testing.T, text, server string, rec config.SupersededToken) {
	t.Helper()
	pos := 0
	for _, command := range clierr.RecoverySequence(server, rec.UserID, rec.Ref) {
		i := strings.Index(text[pos:], command)
		require.GreaterOrEqual(t, i, 0, "command %q in order in: %s", command, text)
		pos += i + len(command)
	}
	assert.NotContains(t, text, "config update-server")
	assert.NotContains(t, text, "the server recorded for token")
}

// recordJSON is the state of the superseded token named name in the JSON of
// config status, and its recovery.
func recordJSON(t *testing.T, stdout, entry, name string) (state string, recovery []any) {
	t.Helper()
	servers := dataOf(t, stdout).(map[string]any)["servers"].(map[string]any)
	for _, r := range servers[entry].(map[string]any)["supersededTokens"].([]any) {
		rec := r.(map[string]any)
		if rec["ref"].(map[string]any)["name"] == name {
			recovery, _ = rec["recovery"].([]any)
			return rec["state"].(string), recovery
		}
	}
	require.Fail(t, "no record "+name, stdout)
	return "", nil
}

// secretCanonicalRecordRows are the rows of a record whose server holds user
// information and canonicalizes: every command gives the recovery sequence
// built from the canonical URL, and an entry that works is never told to
// change its URL.
func secretCanonicalRecordRows() []secretOKRow {
	same := secretStored{url: "{url}", stubCA: true, token: true, records: []config.SupersededToken{sameServerRecord}}
	port := secretStored{url: "{url}", stubCA: true, token: true, records: []config.SupersededToken{otherPortRecord}}
	return []secretOKRow{
		{name: "config set-token-reuse off, a usable plugin entry and a record at its URL with user information", argv: []string{"config", "set-token-reuse", "off"},
			stored: secretStored{name: "exec-login-abc", url: "{url}", stubCA: true, token: true, provenance: clierr.ProvenanceExecLogin, records: []config.SupersededToken{sameServerRecord}},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertSequence(t, stdout+stderr, secretServerURL, sameServerRecord)
				assert.NotContains(t, stdout+stderr, "auth logout 'exec-login-abc' if it holds a login")
			}},
		{name: "config status, text, a usable entry and a record at its URL with user information", argv: []string{"config", "status"}, stored: same,
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertSequence(t, stdout+stderr, secretServerURL, sameServerRecord)
				assert.NotContains(t, stdout, "otherServer")
			}},
		{name: "config status, json, a usable entry and a record at its URL with user information", argv: []string{"-o", "json", "config", "status"}, stored: same,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				state, recovery := recordJSON(t, stdout, "x", "token-same77")
				assert.NotEqual(t, "otherServer", state)
				want := clierr.RecoverySequence(secretServerURL, sameServerRecord.UserID, sameServerRecord.Ref)
				require.Len(t, recovery, len(want))
				for i, c := range want {
					assert.Equal(t, c, recovery[i])
				}
			}},
		{name: "auth logout x, a usable entry and a record at its URL with user information", argv: []string{"auth", "logout", "x"}, stored: same,
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertSequence(t, stdout+stderr, secretServerURL, sameServerRecord)
			}},
		{name: "config status, text, a record at another port with user information", argv: []string{"config", "status"}, stored: port,
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertSequence(t, stdout+stderr, "https://127.0.0.1:9443", otherPortRecord)
				assert.Contains(t, stdout, "state: otherServer")
			}},
		{name: "auth login x, a pasted token, a record at another port with user information", argv: []string{"auth", "login", "x", "--token-stdin", "--no-verify"}, stdin: "id:y",
			stored: port, check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertSequence(t, stdout+stderr, "https://127.0.0.1:9443", otherPortRecord)
			}},
		{name: "config delete-server x, a recorded token, a schemeless URL: one reason for one value", argv: []string{"config", "delete-server", "x"},
			stored: secretStored{url: canaryURLNoHost, token: true},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				reasons := map[string]bool{}
				for _, m := range regexp.MustCompile(`cannot be used: ([^;]+);`).FindAllStringSubmatch(stdout+stderr, -1) {
					reasons[m[1]] = true
				}
				assert.Len(t, reasons, 1, "one value, one reason: %v", reasons)
				assert.Contains(t, stdout+stderr, "it must be absolute (scheme://host)")
			}},
	}
}

// loginToken is the entry's own current token in the rows that store a login
// (secretStored.token), with its owner.
var loginToken = config.SupersededToken{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}, UserID: "u-abc12"}

// notRevokedOf decodes data.notRevoked of a JSON result.
func notRevokedOf(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	list, ok := dataOf(t, stdout).(map[string]any)["notRevoked"].([]any)
	require.True(t, ok, "stdout: %s", stdout)
	out := make([]map[string]any, 0, len(list))
	for _, n := range list {
		out = append(out, n.(map[string]any))
	}
	return out
}

// envelopeWarnings decodes the warnings of a JSON result.
func envelopeWarnings(t *testing.T, stdout string) []string {
	t.Helper()
	var env struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env), "stdout: %s", stdout)
	return env.Warnings
}

// assertRecoveryIs asserts the recovery of the not-revoked token named name is
// the sequence of rec at server, or, with no server, empty.
func assertRecoveryIs(t *testing.T, stdout, name, server string, rec config.SupersededToken) {
	t.Helper()
	for _, n := range notRevokedOf(t, stdout) {
		if n["ref"] == nil || n["ref"].(map[string]any)["name"] != name {
			continue
		}
		recovery, _ := n["recovery"].([]any)
		if server == "" {
			assert.Empty(t, recovery, "no sequence is built for %s", name)
			return
		}
		want := clierr.RecoverySequence(server, rec.UserID, rec.Ref)
		require.Len(t, recovery, len(want))
		for i, c := range want {
			assert.Equal(t, c, recovery[i])
		}
		return
	}
	require.Fail(t, "no not-revoked token "+name, stdout)
}

// secretRemovedEntryRows are the rows of an entry that a command removes, of a
// record that is the entry's own URL, and of servers with another scheme.
func secretRemovedEntryRows() []secretOKRow {
	var rows []secretOKRow
	for _, form := range []struct{ name, url string }{
		{"user name and password", "https://u:" + urlCanary + "@{host}"},
		{"a user name alone", "https://u@{host}"},
	} {
		stored := secretStored{url: form.url, token: true, stubCA: true}
		rows = append(rows,
			secretOKRow{name: "config delete-server x, text, the entry holds " + form.name + " and a login", argv: []string{"config", "delete-server", "x"}, stored: stored,
				check: func(t *testing.T, stdout, stderr string, _ []string) {
					t.Helper()
					assertSequence(t, stdout+stderr, secretServerURL, loginToken)
				}},
			secretOKRow{name: "config delete-server x, json, the entry holds " + form.name + " and a login", argv: []string{"-o", "json", "config", "delete-server", "x"}, stored: stored,
				check: func(t *testing.T, stdout, stderr string, _ []string) {
					t.Helper()
					assertRecoveryIs(t, stdout, "token-abc12", secretServerURL, loginToken)
					assertSequence(t, strings.Join(envelopeWarnings(t, stdout), "\n"), secretServerURL, loginToken)
				}},
		)
	}
	rawRecord := config.SupersededToken{
		Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-raw11"}, UserID: "u-other77",
		SupersededAt: "2026-10-01T00:00:00Z", Server: "https://u:" + urlCanary + "@{host}",
	}
	atRaw := secretStored{url: "https://u:" + urlCanary + "@{host}", token: true, stubCA: true, records: []config.SupersededToken{rawRecord}}
	rows = append(rows,
		secretOKRow{name: "auth logout x, json, a record at the entry's own URL with user information", argv: []string{"-o", "json", "auth", "logout", "x"}, stored: atRaw,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assertRecoveryIs(t, stdout, "token-raw11", "", rawRecord)
				assert.Contains(t, strings.Join(envelopeWarnings(t, stdout), "\n"),
					"the server URL in server entry 'x' cannot be used: it holds a user name or password; change the entry's URL with rancher config update-server 'x' --url <url>")
			}},
		secretOKRow{name: "config delete-server x, json, a record at the entry's own URL with user information", argv: []string{"-o", "json", "config", "delete-server", "x"}, stored: atRaw,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assertRecoveryIs(t, stdout, "token-raw11", secretServerURL, rawRecord)
			}},
	)

	rows = append(rows, secretOKRow{name: "auth logout --all, json, a cached token that names no server", argv: []string{"-o", "json", "auth", "logout", "--all"},
		stored: secretStored{url: "{url}", stubCA: true}, seed: seedCachedTokenWithNoServer,
		check: func(t *testing.T, stdout, _ string, _ []string) {
			t.Helper()
			assertRecoveryIs(t, stdout, "token-nosrv1", "", config.SupersededToken{})
			assert.Contains(t, strings.Join(envelopeWarnings(t, stdout), "\n"),
				"no server is recorded for token 'token-nosrv1'; delete it with rancher delete token 'token-nosrv1' --apply --yes "+
					"while signed in to the server that issued it")
			assert.NotContains(t, strings.Join(envelopeWarnings(t, stdout), "\n"), "--url ''")
		}},
		secretOKRow{name: "auth logout --all, json, a cached token at an ftp server with user information", argv: []string{"-o", "json", "auth", "logout", "--all"},
			stored: secretStored{url: "{url}", stubCA: true}, seed: seedCachedTokenAtFTPUserinfo,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assertRecoveryIs(t, stdout, "token-ftpc1", "", config.SupersededToken{})
				joined := strings.Join(envelopeWarnings(t, stdout), "\n")
				assert.Contains(t, joined, "the server recorded for token 'token-ftpc1' cannot be used: its scheme is not http or https; ")
				assert.NotContains(t, joined, "--url 'ftp://")
			}})

	ftpHost := "ftp://{host}"
	ftpUserinfo := config.SupersededToken{
		Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-ftp33"}, UserID: "u-other77",
		SupersededAt: "2026-10-01T00:00:00Z", Server: "ftp://u:" + urlCanary + "@{host}",
	}
	ftpPlain := config.SupersededToken{
		Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-ftp44"}, UserID: "u-other77",
		SupersededAt: "2026-10-01T00:00:00Z", Server: ftpHost,
	}
	const schemeStays = "the server recorded for token 'token-ftp33' cannot be used: its scheme is not http or https; the token stays on the server that issued it; " +
		"delete it there with rancher delete token 'token-ftp33' --apply --yes while signed in to that server"
	withFTPUserinfo := secretStored{url: "{url}", token: true, stubCA: true, records: []config.SupersededToken{ftpUserinfo}}
	noFTPCommand := func(t *testing.T, text string) {
		t.Helper()
		assert.Contains(t, text, schemeStays)
		assert.NotContains(t, text, "--url 'ftp://")
	}
	rows = append(rows,
		secretOKRow{name: "config status, text, a record with user information on an ftp server", argv: []string{"config", "status"}, stored: withFTPUserinfo,
			check: func(t *testing.T, stdout, stderr string, _ []string) { t.Helper(); noFTPCommand(t, stdout+stderr) }},
		secretOKRow{name: "config status, json, a record with user information on an ftp server", argv: []string{"-o", "json", "config", "status"}, stored: withFTPUserinfo,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				_, recovery := recordJSON(t, stdout, "x", "token-ftp33")
				assert.Empty(t, recovery)
				assert.NotContains(t, stdout, "--url 'ftp://")
			}},
		secretOKRow{name: "auth logout x, json, a record with user information on an ftp server", argv: []string{"-o", "json", "auth", "logout", "x"}, stored: withFTPUserinfo,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assertRecoveryIs(t, stdout, "token-ftp33", "", ftpUserinfo)
				noFTPCommand(t, strings.Join(envelopeWarnings(t, stdout), "\n"))
			}},
		secretOKRow{name: "config delete-server x, text, a record with user information on an ftp server", argv: []string{"config", "delete-server", "x"}, stored: withFTPUserinfo,
			check: func(t *testing.T, stdout, stderr string, _ []string) { t.Helper(); noFTPCommand(t, stdout+stderr) }},
		secretOKRow{name: "config delete-server x, json, a record with user information on an ftp server", argv: []string{"-o", "json", "config", "delete-server", "x"}, stored: withFTPUserinfo,
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assertRecoveryIs(t, stdout, "token-ftp33", "", ftpUserinfo)
				noFTPCommand(t, strings.Join(envelopeWarnings(t, stdout), "\n"))
			}},
		secretOKRow{name: "config delete-server x, json, the entry holds user information on an ftp server and a login", argv: []string{"-o", "json", "config", "delete-server", "x"},
			stored: secretStored{url: "ftp://u:" + urlCanary + "@{host}", token: true},
			check: func(t *testing.T, stdout, _ string, _ []string) {
				t.Helper()
				assertRecoveryIs(t, stdout, "token-abc12", "", loginToken)
				joined := strings.Join(envelopeWarnings(t, stdout), "\n")
				assert.Contains(t, joined, "the server recorded for token 'token-abc12' cannot be used: it holds a user name or password; ")
				assert.NotContains(t, joined, "its scheme is not http or https")
				assert.NotContains(t, joined, "--url 'ftp://")
				for _, n := range notRevokedOf(t, stdout) {
					assert.Contains(t, n["error"], "it holds a user name or password")
				}
			}},
		secretOKRow{name: "config status, text, a plain record on an ftp server keeps its sequence", argv: []string{"config", "status"},
			stored: secretStored{url: "{url}", token: true, stubCA: true, records: []config.SupersededToken{ftpPlain}},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertSequence(t, stdout+stderr, "ftp://"+strings.TrimPrefix(secretServerURL, "https://"), ftpPlain)
			}},
		secretOKRow{name: "config status, text, an entry whose own URL is a plain ftp URL keeps its sequence", argv: []string{"config", "status"},
			stored: secretStored{url: ftpHost, token: true},
			check: func(t *testing.T, stdout, stderr string, _ []string) {
				t.Helper()
				assertSequence(t, stdout+stderr, "ftp://"+strings.TrimPrefix(secretServerURL, "https://"), loginToken)
			}},
	)
	return rows
}

// seedCachedTokenAtFTPUserinfo seeds a cached token whose record names an ftp
// server with a user name and password, as only a hand edit writes one.
func seedCachedTokenAtFTPUserinfo(tb testing.TB, dir string) {
	tb.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(tb, err)
	server := "ftp://u:" + urlCanary + "@127.0.0.1:9"
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-ftpc1"}
	binding := config.CacheBindingFor(config.CanonicalDir(dir), cfg, "x").WithCreatedToken(ref, "u-abc12")
	require.NoError(tb, config.NewFileStore(dir).SetCachedCred(config.CacheKey("x", "ftp://127.0.0.1:9", "c-1"), "ext/token-ftpc1:secret",
		time.Now().Add(time.Hour), "x", server, "c-1", binding))
}

// seedCachedTokenWithNoServer writes an exec credential cache record that
// names a CLI-created token and no server.
func seedCachedTokenWithNoServer(tb testing.TB, dir string) {
	tb.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(tb, err)
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-nosrv1"}
	binding := config.CacheBindingFor(config.CanonicalDir(dir), cfg, "x").WithCreatedToken(ref, "u-abc12")
	require.NoError(tb, config.NewFileStore(dir).SetCachedCred(config.CacheKey("x", "", "c-1"), "ext/token-nosrv1:secret",
		time.Now().Add(time.Hour), "x", "", "c-1", binding))
}
