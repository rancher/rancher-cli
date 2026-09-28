package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// matrixSeed is where an entry's credential is before the login, and what
// it is. An empty store seeds nothing.
type matrixSeed struct {
	store, token string
}

// readerMatrixRow is one directory state a login starts from, and the
// login that runs on it.
type readerMatrixRow struct {
	name string
	// prodNew leaves prod out of config.yaml: the login creates the entry,
	// as the kubectl plugin's does, and prodStale is then a credential
	// stored under a name no entry has.
	prodNew bool
	// declared is config.yaml's credentialStore, pin is credstore-pin and
	// env is RANCHER_CLI_CREDENTIAL_STORE; empty for none.
	declared, pin, env string
	// flag is the login's --credential-store; plugin makes the login pass
	// its policy the way the kubectl plugin does, with no flag.
	flag   string
	plugin bool
	// probe is what the login's keychain probe answers.
	probe bool
	// prod is the logging-in entry; staging is another entry, left out of
	// the directory when neither of its seeds names a store; prodStale is
	// a copy of prod's credential in a store the directory does not read;
	// stagingLeft is a credential staging was logged out of that stayed
	// in a store the directory does not read.
	prod, staging, prodStale, stagingLeft matrixSeed
	// stagingBare makes staging an entry with nothing stored at all.
	stagingBare bool
	// lands is the store prod's new credential ends up in.
	lands string
	// refused, when set, is text the refusal before the sign-in carries;
	// the login then writes nothing.
	refused string
}

const matrixNewToken = "id:new"

// errMatrixNotConfigured is the matrix reader's answer for an entry
// config.yaml does not have yet.
var errMatrixNotConfigured = errors.New("no such entry in config.yaml")

// readerMatrixRows are the states item by item: each row is one a reader
// that takes no lock can meet during a login.
func readerMatrixRows() []readerMatrixRow {
	return []readerMatrixRow{
		{
			name:     "a login creating an entry whose name has a credential left in the file store",
			declared: "auto", plugin: true, probe: true, prodNew: true,
			staging: matrixSeed{backendKeychain, "staging-tok"}, prodStale: matrixSeed{backendFile, "prod-orphan"},
			lands: backendKeychain,
		},
		{
			name:     "a login creating an entry whose name has a credential left in the keychain",
			declared: "auto", plugin: true, probe: true, prodNew: true,
			staging: matrixSeed{backendFile, "staging-tok"}, prodStale: matrixSeed{backendKeychain, "prod-orphan"},
			lands: backendFile,
		},
		{
			name:     "the auto upgrade on a file-pinned directory whose only file credential is its own",
			declared: "auto", pin: backendFile, probe: true,
			prod:  matrixSeed{backendFile, "prod-old"},
			lands: backendKeychain,
		},
		{
			name:     "an ordinary login on a keychain-pinned directory with another entry",
			declared: "auto", pin: backendKeychain, probe: true,
			prod: matrixSeed{backendKeychain, "prod-old"}, staging: matrixSeed{backendKeychain, "staging-tok"},
			lands: backendKeychain,
		},
		{
			name:     "an ordinary login on a declared file directory with another entry",
			declared: backendFile,
			prod:     matrixSeed{backendFile, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			lands: backendFile,
		},
		{
			name:     "a keychain directory with one entry changes to the file store by name",
			declared: backendKeychain, flag: backendFile,
			prod:  matrixSeed{backendKeychain, "prod-old"},
			lands: backendFile,
		},
		{
			name:     "a declared file directory with no pin changes to the keychain by name",
			declared: backendFile, flag: backendKeychain,
			prod:  matrixSeed{backendFile, "prod-old"},
			lands: backendKeychain,
		},
		{
			name:     "a declared file directory with no pin logs in with auto from the flag",
			declared: backendFile, flag: "auto", probe: true,
			prod:  matrixSeed{backendFile, "prod-old"},
			lands: backendKeychain,
		},
		{
			name:     "a keychain directory with a stale file pin logs in with auto from the flag",
			declared: backendKeychain, pin: backendFile, flag: "auto", probe: true,
			prod: matrixSeed{backendKeychain, "prod-old"}, prodStale: matrixSeed{backendFile, "prod-file-stale"},
			lands: backendKeychain,
		},
		{
			name:     "a helper directory changes to the keychain by name",
			declared: "exec:fake", flag: backendKeychain,
			prod:  matrixSeed{"exec:fake", "prod-old"},
			lands: backendKeychain,
		},
		{
			name: "the variable asks for auto on an undeclared file-pinned directory",
			env:  "auto", pin: backendFile, probe: true,
			prod:  matrixSeed{backendFile, "prod-old"},
			lands: backendKeychain,
		},
		{
			name: "the plugin logs in with the variable asking for auto on a file-pinned directory",
			env:  "auto", pin: backendFile, plugin: true, probe: true,
			prod:  matrixSeed{backendFile, "prod-old"},
			lands: backendKeychain,
		},
		{
			name:     "a declared file directory with another entry changes to the keychain by name",
			declared: backendFile, flag: backendKeychain,
			prod: matrixSeed{backendFile, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			refused: "the credential of staging is still in the plaintext file",
		},
		{
			name:     "a keychain directory with another entry changes to the file store by name",
			declared: backendKeychain, flag: backendFile,
			prod: matrixSeed{backendKeychain, "prod-old"}, staging: matrixSeed{backendKeychain, "staging-tok"},
			refused: "the credential of staging is still in the keychain",
		},
		{
			name:     "a helper directory with another entry changes to the keychain by name",
			declared: "exec:fake", flag: backendKeychain,
			prod: matrixSeed{"exec:fake", "prod-old"}, staging: matrixSeed{"exec:fake", "staging-tok"},
			refused: "the credential helper fake may answer for staging",
		},
		{
			name:     "the auto upgrade stays on a file-pinned directory whose other entry is in the file store",
			declared: "auto", pin: backendFile, probe: true,
			prod: matrixSeed{backendFile, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			lands: backendFile,
		},
		{
			name: "the auto upgrade from the variable stays on the file store while another entry is there",
			env:  "auto", pin: backendFile, probe: true,
			prod: matrixSeed{backendFile, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			lands: backendFile,
		},
		{
			name: "the plugin's auto upgrade with the variable set stays on the file store while another entry is there",
			env:  "auto", pin: backendFile, plugin: true, probe: true,
			prod: matrixSeed{backendFile, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			lands: backendFile,
		},
		{
			name:     "a declared file directory logs in with auto while another entry is in the file store",
			declared: backendFile, flag: "auto", probe: true,
			prod: matrixSeed{backendFile, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			lands: backendFile,
		},
		{
			name:  "a fresh directory's first login",
			probe: true,
			lands: backendKeychain,
		},
		{
			name:  "a fresh directory's first login on a keychain it cannot use",
			lands: backendFile,
		},
		{
			name:     "an unpinned directory whose only file credential is its own upgrades to the keychain",
			declared: "auto", probe: true,
			prod:  matrixSeed{backendFile, "prod-old"},
			lands: backendKeychain,
		},
		{
			name:     "an unpinned directory with another entry in the file store stays there",
			declared: "auto", probe: true,
			prod: matrixSeed{backendFile, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			lands: backendFile,
		},
		{
			name:     "an unpinned directory the keychain holds logs in with auto",
			declared: "auto", probe: true,
			prod: matrixSeed{backendKeychain, "prod-old"}, staging: matrixSeed{backendKeychain, "staging-tok"},
			lands: backendKeychain,
		},
		{
			name:     "an unpinned directory the keychain holds changes to the file store by name",
			declared: "auto", flag: backendFile,
			prod:  matrixSeed{backendKeychain, "prod-old"},
			lands: backendFile,
		},
		{
			name:     "the auto upgrade stays on the file store while another entry has a credential left in the keychain",
			declared: "auto", pin: backendFile, probe: true,
			prod: matrixSeed{backendFile, "prod-old"}, stagingLeft: matrixSeed{backendKeychain, "staging-logged-out"},
			lands: backendFile,
		},
		{
			name:     "a declared file directory changes to the keychain by name while another entry has a credential left there",
			declared: backendFile, flag: backendKeychain,
			prod: matrixSeed{backendFile, "prod-old"}, stagingLeft: matrixSeed{backendKeychain, "staging-logged-out"},
			refused: "the keychain still holds a credential of staging from an earlier login",
		},
		{
			name:     "a keychain directory changes to the file store by name while another entry has a credential left there",
			declared: backendKeychain, flag: backendFile,
			prod: matrixSeed{backendKeychain, "prod-old"}, stagingLeft: matrixSeed{backendFile, "staging-logged-out"},
			refused: "still holds a credential of staging from an earlier login",
		},
		{
			name: "an unpinned directory that has stored nothing changes to a helper by name",
			flag: "exec:fake", stagingBare: true,
			refused: "the credential helper fake may answer for staging",
		},
		{
			name:     "an unpinned directory with credentials in both stores",
			declared: "auto", probe: true,
			prod: matrixSeed{backendKeychain, "prod-old"}, staging: matrixSeed{backendFile, "staging-tok"},
			refused: "credentials in the keychain (prod) and in the plaintext file",
		},
	}
}

// TestAReaderDuringALoginResolvesTheOldOrTheNewCredential asserts what a
// process that takes no lock and runs no probe gets for each entry after
// every write a login makes that such a process can observe: config.yaml,
// the store-change record, credstore-pin, an identity record, a keychain
// set or delete, credentials.yaml, a helper store or erase. Each entry
// resolves to a store holding its own old credential or, for the entry
// logging in, its new one, or the read fails - but never with the
// both-stores error, which would stop every entry in the middle of a
// login. Never a third store, never the previous credential while the
// binding id has advanced unless the entry's cache writes are paused, and
// never a reader that writes or probes.
// Afterwards the new credential is where the row says and every other
// entry's credential is where it was.
func TestAReaderDuringALoginResolvesTheOldOrTheNewCredential(t *testing.T) {
	for _, row := range readerMatrixRows() {
		t.Run(row.name, func(t *testing.T) { runReaderMatrixRow(t, row) })
	}
}

func runReaderMatrixRow(t *testing.T, row readerMatrixRow) {
	t.Helper()
	for _, s := range []matrixSeed{row.prod, row.staging, row.prodStale, row.stagingLeft} {
		if isExecBackend(s.store) {
			useFakeHelper(t, "ok")
			break
		}
	}
	t.Setenv(EnvCredentialStore, row.env)
	keyring.MockInit()
	dir := t.TempDir()

	reading := false
	var violations []string
	orig := keyringUsable
	keyringUsable = func() bool {
		if reading {
			violations = append(violations, "a reader probed the keychain")
		}
		return row.probe
	}
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

	urls := map[string]string{"prod": "https://r.ex", "staging": "https://staging.ex"}
	cfg := &Config{CredentialStore: row.declared, CurrentServer: "prod", Servers: map[string]Server{
		"prod": {URL: urls["prod"], CacheBindingID: "b0"},
	}}
	hasStaging := row.staging.store != "" || row.stagingLeft.store != "" || row.stagingBare
	if hasStaging {
		cfg.Servers["staging"] = Server{URL: urls["staging"]}
	}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	if row.pin != "" {
		require.NoError(t, WritePin(dir, row.pin))
	}
	seed := func(entry string, s matrixSeed) {
		if s.store == "" {
			return
		}
		ref := ServerRef{Name: entry, URL: urls[entry]}
		switch s.store {
		case backendFile:
			require.NoError(t, NewFileStore(dir).SetToken(ref, s.token))
		case backendKeychain:
			seedKeychainCredential(t, dir, entry, s.token)
		default:
			name, _ := execHelperName(s.store)
			require.NoError(t, newExecStore(name, dir).SetToken(ref, s.token))
		}
	}
	seed("prod", row.prod)
	seed("staging", row.staging)
	seed("prod", row.prodStale)
	seed("staging", row.stagingLeft)

	entries := []string{"prod"}
	if hasStaging {
		entries = append(entries, "staging")
	}
	read := func(entry string) (string, error) {
		reading = true
		defer func() { reading = false }()
		resetKeyringUsableCache()
		c, store, err := LoadAndStore(dir)
		if err != nil {
			return "", err
		}
		if _, ok := c.Servers[entry]; !ok {
			// No command reads a credential for an entry config.yaml
			// does not have.
			return "", errMatrixNotConfigured
		}
		return store.GetToken(ServerRef{Name: entry, URL: c.Servers[entry].URL})
	}
	if row.prodNew {
		c, lerr := Load(FilePath(dir))
		require.NoError(t, lerr)
		delete(c.Servers, "prod")
		c.CurrentServer = ""
		require.NoError(t, c.Write())
	}
	baseline := map[string]string{}
	for _, e := range entries {
		tok, err := read(e)
		if err == nil {
			baseline[e] = tok
		}
	}
	if row.refused == "" {
		require.Equal(t, row.prod.token, baseline["prod"], "before the login a reader gets prod's credential")
		require.Equal(t, row.staging.token, baseline["staging"], "and staging's")
	}

	writes := 0
	t.Cleanup(SetAfterWriteHookForTest(func(what string) {
		if reading {
			violations = append(violations, "a reader wrote "+what)
			return
		}
		writes++
		now, lerr := Load(FilePath(dir))
		// A reader that still gets prod's previous credential once its id
		// has advanced must find prod's cache writes paused, so no record
		// made from it is written under the new id.
		advanced := lerr == nil && now.Servers["prod"].CacheBindingID != "b0"
		paused := lerr == nil && now.Servers["prod"].Paused()
		for _, e := range entries {
			tok, err := read(e)
			if IsCredentialsInBothStores(err) {
				violations = append(violations, fmt.Sprintf(
					"after write %d (%s) a reader of %s gets the both-stores error in the middle of the login", writes, what, e))
				continue
			}
			if errors.Is(err, ErrNoToken) {
				// "Not logged in" is loud, but it is also what a reader
				// resolving to an empty third store gets. Another entry's
				// credential never moves, so for it that answer is always
				// the wrong store; for prod it is while its old credential
				// is still in its old store.
				switch {
				case e != "prod" && baseline[e] != "":
					violations = append(violations, fmt.Sprintf(
						"after write %d (%s) a reader of %s gets \"not logged in\", though its credential never moved", writes, what, e))
				case e == "prod" && row.prod.store != "" &&
					matrixStored(t, dir, "prod", urls["prod"], row.prod.store) == row.prod.token:
					violations = append(violations, fmt.Sprintf(
						"after write %d (%s) a reader of prod gets \"not logged in\" while its old credential is still in %s", writes, what, row.prod.store))
				}
				continue
			}
			if err != nil {
				continue
			}
			old := tok != "" && tok == baseline[e]
			switch {
			case e == "prod" && tok == matrixNewToken:
			case old && e != "prod", old && !advanced, old && paused:
			case old:
				violations = append(violations, fmt.Sprintf(
					"after write %d (%s) the binding id has advanced, cache writes are not paused, and a reader still gets prod's previous credential", writes, what))
			default:
				violations = append(violations, fmt.Sprintf(
					"after write %d (%s) a reader of %s gets %q, which is neither its old nor its new credential", writes, what, e, tok))
			}
		}
	}))

	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	var policy string
	if row.plugin {
		policy, err = EffectivePolicy(loaded.CredentialStore)
	} else {
		policy, _, err = LoginPolicy(row.flag, loaded.CredentialStore)
	}
	require.NoError(t, err)
	declaration := DeclarationFor(loaded.CredentialStore, row.flag, policy)
	serr := loginRefusal(dir, loaded, "prod", policy, declaration)
	if row.refused != "" {
		require.ErrorContains(t, serr, row.refused)
		assert.Zero(t, writes, "a refusal before the sign-in writes nothing")
		assert.Empty(t, violations)
		return
	}
	require.NoError(t, serr)

	_, err = PersistLoginCredential(t.Context(), dir, PersistLoginInput{
		Name: "prod", CanonicalURL: urls["prod"], Bearer: matrixNewToken,
		Provenance:          clierr.ProvenanceDerived,
		Policy:              policy,
		ObservedDeclaration: loaded.CredentialStore,
		Declaration:         declaration,
		OpenStore:           OpenResolvedStore,
		OwnsEntry:           row.prodNew,
	})
	require.NoError(t, err)
	assert.Positive(t, writes, "the login wrote something a reader could observe")
	assert.Empty(t, violations)
	if row.prodNew {
		assert.NotEqual(t, row.prodStale.token, matrixStored(t, dir, "prod", urls["prod"], row.prodStale.store),
			"the credential no entry named is gone from its store")
	}

	got, err := read("prod")
	require.NoError(t, err)
	assert.Equal(t, matrixNewToken, got, "after the login a reader gets the new credential")
	assert.Equal(t, matrixNewToken, matrixStored(t, dir, "prod", urls["prod"], row.lands), "in the store the row names")
	if row.staging.store != "" {
		got, err := read("staging")
		require.NoError(t, err)
		assert.Equal(t, row.staging.token, got, "another entry reads its own credential")
		assert.Equal(t, row.staging.token, matrixStored(t, dir, "staging", urls["staging"], row.staging.store),
			"from the store it was in: nothing moved it")
	} else if row.stagingLeft.store != "" {
		_, err := read("staging")
		require.ErrorIs(t, err, ErrNoToken, "an entry that was logged out still reads as not logged in")
	}
}

// matrixStored is what one store holds for an entry, read directly rather
// than through the directory's resolution.
func matrixStored(t *testing.T, dir, entry, url, store string) string {
	t.Helper()
	ref := ServerRef{Name: entry, URL: url}
	var (
		tok string
		err error
	)
	switch store {
	case backendFile:
		tok, err = NewFileStore(dir).GetToken(ref)
	case backendKeychain:
		return keychainTokenFor(t, dir, entry)
	default:
		name, _ := execHelperName(store)
		tok, err = newExecStore(name, dir).GetToken(ref)
	}
	if errors.Is(err, ErrNoToken) {
		return ""
	}
	require.NoError(t, err)
	return strings.TrimSpace(tok)
}
