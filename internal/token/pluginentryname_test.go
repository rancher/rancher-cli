package token

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// TestPluginLoginEntryIsNamedForTheServerAndTheUser asserts the plugin
// login entry the plugin-created login makes is named for the server and,
// for a call with --user-id, the user, so one server holds one such entry
// for each user; a call without --user-id keeps the name of the server
// alone.
func TestPluginLoginEntryIsNamedForTheServerAndTheUser(t *testing.T) {
	x := newOwnerFixture(t, true, testrancher.Options{})
	x.entry("prod", "", "", false)
	_, err := x.call("prod-c-test", "user-carol", "carol", nil)
	require.NoError(t, err)
	_, err = x.call("prod-c-test", "user-alice", "alice", nil)
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	for user, entry := range map[string]string{"user-carol": x.pluginEntryOf("user-carol"), "user-alice": x.pluginEntryOf("user-alice")} {
		assert.True(t, strings.HasPrefix(entry, config.ExecLoginPrefix), "%s is in the plugin's namespace", entry)
		assert.Equal(t, user, cfg.Servers[entry].TokenUserID, entry)
		assert.True(t, strings.HasSuffix(entry, "-"+user), "%s is named for the user", entry)
		assert.Equal(t, clierr.ProvenanceExecLogin, cfg.Servers[entry].Provenance)
	}
	assert.NotContains(t, cfg.Servers, x.pluginEntry(), "no entry has the name of the server alone")

	y := newOwnerFixture(t, true, testrancher.Options{})
	y.entry("prod", "", "", false)
	_, err = y.call("prod-c-test", "", "carol", nil)
	require.NoError(t, err)
	_, host := y.stored(y.pluginEntry())
	assert.Equal(t, "user-carol", host.TokenUserID, "a call without --user-id makes the entry named for the server alone")
}

// TestUserIDPluginCreatedLoginWritesInPlaceOnlyOverItsOwnUser asserts the
// plugin login entry named for the server and the user is written in
// place when it records that user or none, and is never written over when
// it records another user: the call signs in interactively, with no
// disclosure for a login that does not happen, and the entry stays as it
// was.
func TestUserIDPluginCreatedLoginWritesInPlaceOnlyOverItsOwnUser(t *testing.T) {
	for _, recorded := range []string{"user-carol", ""} {
		t.Run("recording "+recorded, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			name := x.pluginEntryOf("user-carol")
			x.entry(name, "", recorded, true)
			got, err := x.call("kube-c-test", "user-carol", "carol", nil)
			require.NoError(t, err)
			assert.Equal(t, "user-carol", got)
			login, s := x.stored(name)
			assert.NotEmpty(t, login, "the entry is logged in in place")
			assert.Equal(t, "user-carol", s.TokenUserID)
			cfg, err := config.Load(config.FilePath(x.dir))
			require.NoError(t, err)
			assert.Len(t, cfg.Servers, 1, "no second entry is made")
		})
	}
	for _, holds := range []bool{false, true} {
		t.Run(map[bool]string{false: "recording another user, no login", true: "recording another user, holding a login"}[holds], func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			name := x.pluginEntryOf("user-carol")
			user := ""
			if holds {
				user = "bob"
			}
			x.entry(name, user, "user-bob", true)
			beforeLogin, before := x.stored(name)
			var notices bytes.Buffer
			got, err := x.call("kube-c-test", "user-carol", "carol", &notices)
			require.NoError(t, err)
			assert.Equal(t, "user-carol", got, "the call signs in interactively")
			afterLogin, after := x.stored(name)
			assert.Equal(t, beforeLogin, afterLogin, "the other user's login is not written over")
			assert.Equal(t, before, after)
			assert.NotContains(t, notices.String(), "plaintext file")
			assert.True(t, x.cached("kube-c-test", "user-carol"), "the sign-in's token is cached")
		})
	}
}

// TestPluginLoginEntryNameLengthIsBounded asserts a plugin-created login
// whose entry name is longer than the credential store accepts is refused
// before the sign-in, on every operating system, with the login of an
// entry of a shorter name as the remedy; one name at the limit is written.
func TestPluginLoginEntryNameLengthIsBounded(t *testing.T) {
	require.Equal(t, 467, config.MaxEntryNameLength, "500 less the 32-character account id and the colon")
	for _, tc := range []struct {
		length  int
		refused bool
	}{
		{config.MaxEntryNameLength, false},
		{config.MaxEntryNameLength + 1, true},
	} {
		t.Run(map[bool]string{false: "at the limit", true: "one over"}[tc.refused], func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			name := config.ExecLoginPrefix + strings.Repeat("x", tc.length-len(config.ExecLoginPrefix))
			require.Len(t, name, tc.length)
			x.entry(name, "", "user-carol", true)
			var notices bytes.Buffer
			got, err := x.call(name+"-c-test", "user-carol", "carol", &notices)
			if !tc.refused {
				require.NoError(t, err)
				assert.Equal(t, "user-carol", got)
				login, _ := x.stored(name)
				assert.NotEmpty(t, login)
				return
			}
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, "the exec plugin's login entry for this server would have a name of 468 characters, and the credential store accepts at most 467; "+
				"<name> stands for a name of your choice; to log in under a shorter name, pipe an API token of 'user-carol' to: rancher auth login <name> --url '"+x.serverID()+"' --token-stdin", msg)
			assert.NotContains(t, notices.String(), "Enter username", "nothing is signed in")
			login, _ := x.stored(name)
			assert.Empty(t, login, "nothing is stored")
			assert.Empty(t, x.f.CreateRequests(), "and nothing is created")
		})
	}
}

// requestCounter is a transport that fails every request and counts them.
type requestCounter struct{ calls atomic.Int32 }

func (f *requestCounter) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls.Add(1)
	return nil, errors.New("no request is expected before the entry name is checked")
}

// refusedHostLogin runs the plugin-created login for a server whose URL has
// the host, over an empty directory with a transport that fails the test when
// it is used, and returns the directory, the transport and the error.
func refusedHostLogin(t *testing.T, host string) (string, *requestCounter, error) {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: true, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	server := "https://" + host
	serverID := config.CanonicalServerIdentity(server)
	tr := &requestCounter{}
	cmd := tokenCmd{dir: dir, store: config.NewFileStore(dir), anchor: anchorFor(dir),
		user: userCheck{clusterID: "c-test", serverID: serverID}}
	_, _, err := cmd.pluginLogin(t.Context(), pluginLoginInput{
		server: server, serverID: serverID, clusterID: "c-test", client: &http.Client{Transport: tr},
	}, noTerminal())
	return dir, tr, err
}

// TestPluginLoginRefusesAHostThatPrintsAsAnotherNameBeforeAnySignIn asserts a
// plugin-created login for a server whose URL host holds a character a
// terminal removes is refused before any request, with the login under a name
// of the operator's own as the remedy, the refused host nowhere in the text,
// and nothing written: no entry, no credential.
func TestPluginLoginRefusesAHostThatPrintsAsAnotherNameBeforeAnySignIn(t *testing.T) {
	zeroWidthSpace := string(rune(0x200b))
	host := "a" + zeroWidthSpace + "b.invalid"
	require.Contains(t, config.CanonicalServerIdentity("https://"+host), zeroWidthSpace, "the canonical identity keeps the character")

	dir, tr, err := refusedHostLogin(t, host)
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "the server URL's host holds U+200B, which is removed when a name is printed; the kubectl plugin cannot create its login entry for it; "+
		"<url> stands for the server's address; <name> stands for a name of your choice; to log in under a name of your own, pipe an API token to: rancher auth login <name> --url '<url>' --token-stdin", msg)
	assert.Equal(t, msg, output.Printable(msg), "the text holds nothing a printer removes")
	assert.NotContains(t, msg, host)
	assert.NotContains(t, msg, "ab.invalid")
	assert.Zero(t, tr.calls.Load(), "no request is made")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, cfg.Servers, "no entry is written")
	_, serr := os.Stat(config.CredentialsFilePath(dir))
	assert.ErrorIs(t, serr, os.ErrNotExist, "no credential is stored")
}

// TestPluginLoginRefusesAHostThatIsNotValidUTF8 asserts a host with a byte
// that is not valid UTF-8 gets its own text, with the same remedy and no host
// in it.
func TestPluginLoginRefusesAHostThatIsNotValidUTF8(t *testing.T) {
	host := "a\xffb.invalid"
	require.Contains(t, config.CanonicalServerIdentity("https://"+host), "\xff", "the canonical identity keeps the invalid byte")
	_, tr, err := refusedHostLogin(t, host)
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "the server URL's host is not valid UTF-8; the kubectl plugin cannot create its login entry for it; "+
		"<url> stands for the server's address; <name> stands for a name of your choice; to log in under a name of your own, pipe an API token to: rancher auth login <name> --url '<url>' --token-stdin", msg)
	assert.NotContains(t, msg, "\xff")
	assert.Zero(t, tr.calls.Load(), "no request is made")
}

// TestPluginLoginKeepsRefreshingAnEntryStoredUnderSuchAName asserts a plugin
// login entry already stored with an invisible character in its name still
// refreshes its login: the refusal applies to an entry the plugin would
// create.
func TestPluginLoginKeepsRefreshingAnEntryStoredUnderSuchAName(t *testing.T) {
	x := newOwnerFixture(t, true, testrancher.Options{})
	name := config.ExecLoginPrefix + "a" + string(rune(0x200b)) + "b"
	x.entry(name, "", "user-carol", true)
	var notices bytes.Buffer
	got, err := x.call(name+"-c-test", "user-carol", "carol", &notices)
	require.NoError(t, err)
	assert.Equal(t, "user-carol", got)
	login, _ := x.stored(name)
	assert.NotEmpty(t, login, "the stored entry holds the new login")
}

// TestAUserIDOrAuthConfigWithAnInvisibleCharacterNeverReachesThePluginLogin
// asserts the host is the only part of the plugin's entry name that can hold
// such a character: a --user-id or --auth-provider that holds one is refused
// as not a name before any request, so nothing is read, signed in or stored.
func TestAUserIDOrAuthConfigWithAnInvisibleCharacterNeverReachesThePluginLogin(t *testing.T) {
	separator := string(rune(0x2063))
	t.Run("--user-id", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		_, err := x.call("prod-c-test", "user-"+separator+"carol", "carol", nil)
		require.ErrorContains(t, err, "--user-id is not a Rancher user id")
		assert.Empty(t, x.seen(), "no request, no cache read")
		cfg, lerr := config.Load(config.FilePath(x.dir))
		require.NoError(t, lerr)
		assert.Empty(t, cfg.Servers)
	})
	t.Run("--auth-provider", func(t *testing.T) {
		x := newAuthConfigFixture(t, true, testrancher.Options{})
		_, _, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp" + separator + "ldap", signIn: signInLines("corp-ldap", "alice")})
		require.ErrorContains(t, err, "--auth-provider is not an auth config name")
		assert.Empty(t, x.seen(), "no request, no cache read")
		cfg, lerr := config.Load(config.FilePath(x.dir))
		require.NoError(t, lerr)
		assert.Empty(t, cfg.Servers)
	})
}

// TestPluginLoginRefusesTheSuffixedNameWhenAnotherServerHoldsTheDerivedOne
// asserts the early check sees the name the login will write: an entry stored
// under the name derived from the host, at another URL, makes the login write
// the next free suffixed name, and a host with an invisible character is
// refused with the plugin's text before any request, not after a sign-in.
func TestPluginLoginRefusesTheSuffixedNameWhenAnotherServerHoldsTheDerivedOne(t *testing.T) {
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	dir := t.TempDir()
	host := "a" + string(rune(0x200b)) + "b.invalid"
	server := "https://" + host
	serverID := config.CanonicalServerIdentity(server)
	other := config.CanonicalServerIdentity("http://" + host)
	name := config.ExecLoginEntryNameFor(serverID, "", "")
	require.NotEqual(t, serverID, other)
	require.Equal(t, name, config.ExecLoginEntryNameFor(other, "", ""), "both URLs derive one name")
	cfg := &config.Config{Servers: map[string]config.Server{name: {URL: other}}, TokenReuse: true, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.Equal(t, name+"-2", config.CollisionFreeName(cfg, name, serverID), "the name the login writes is the suffixed one")

	tr := &requestCounter{}
	cmd := tokenCmd{dir: dir, store: config.NewFileStore(dir), anchor: anchorFor(dir),
		user: userCheck{clusterID: "c-test", serverID: serverID}}
	_, _, err := cmd.pluginLogin(t.Context(), pluginLoginInput{
		server: server, serverID: serverID, clusterID: "c-test", client: &http.Client{Transport: tr},
		newIdPClient: func() *http.Client { return &http.Client{Transport: tr} },
	}, noTerminal())
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, msg, "the server URL's host holds U+200B")
	assert.Zero(t, tr.calls.Load(), "no request is made")
	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Len(t, after.Servers, 1, "no entry is written")
}

// TestPluginLoginHostRefusalPutsTheInlineCAClauseAfterTheUrlClause asserts a
// call that carries its CA inline gets the clause that says where the CA is
// carried after the clause that says what <url> stands for, for each place
// the CA comes from, and that the text holds nothing a printer removes.
func TestPluginLoginHostRefusalPutsTheInlineCAClauseAfterTheUrlClause(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"
	const head = "the server URL's host holds U+200B, which is removed when a name is printed; the kubectl plugin cannot create its login entry for it; " +
		"<url> stands for the server's address; <name> stands for a name of your choice; "
	const tail = " and --cacert needs a file; to log in under a name of your own, pipe an API token to: rancher auth login <name> --url '<url>' --cacert <file> --token-stdin"
	for name, tc := range map[string]struct {
		from config.CASource
		want string
	}{
		"the exec entry":       {config.CASource{}, head + "this exec entry carries the CA inline" + tail},
		"RANCHER_CLI_CACERT":   {config.CASource{Kind: config.CASourceVariable}, head + "RANCHER_CLI_CACERT carries the CA inline" + tail},
		"another server entry": {config.CASource{Kind: config.CASourceEntry, Entry: "prod"}, head + "server entry 'prod' carries the CA inline in config.yaml" + tail},
	} {
		t.Run(name, func(t *testing.T) {
			ce := userCheck{clusterID: "c-test", serverID: "https://r.example.com", caSource: pem, caFrom: tc.from}.
				entryNameCharacters(&config.Config{}, config.NameProblem{Rune: rune(0x200b)})
			msg := clierr.ExpandRemedies(ce.Message, "")
			assert.Equal(t, tc.want, msg)
			assert.Equal(t, msg, output.Printable(msg))
		})
	}
}
