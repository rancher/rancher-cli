package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestALoginResolvesOneBackendSetAndOpensIt asserts the one answer every
// condition in the login order is derived from: what a reader resolves to
// now, what the login commits to, what a reader resolves to afterwards,
// and the two questions that follow - does this entry's credential change
// store, and does the change end with a pin. The rows are the states a
// directory can be in when a login reaches it.
func TestALoginResolvesOneBackendSetAndOpensIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// declared is config.yaml's credentialStore; pin is credstore-pin;
		// policy is what the login resolved for itself; declaration is
		// what it will write afterwards; env is
		// RANCHER_CLI_CREDENTIAL_STORE.
		declared, pin, policy, declaration, env string
		// fileTokens seeds a token for "other" in credentials.yaml;
		// others makes "other" an entry of the directory as well;
		// ownToken seeds the logging-in entry's own token; otherRecord
		// leaves "other" an identity record in credstore-id/.
		fileTokens, others, ownToken, otherRecord bool
		// keychain is what the probe answers.
		keychain                        bool
		readsNow, commitsTo, readsAfter string
		changes, writesPin, notice      bool
		// pinsFirst is the pin written in the pre-commit step because the one
		// on disk names a third store.
		pinsFirst bool
		wantProbe bool
	}{
		{
			name:   "a fresh directory decides, and the change is the pin alone",
			policy: "auto", declaration: "auto", keychain: true, wantProbe: true,
			readsNow: "", commitsTo: "keychain", readsAfter: "",
			changes: false, writesPin: true,
		},
		{
			name:   "a fresh directory whose keychain cannot be used says so",
			policy: "auto", declaration: "auto", keychain: false, wantProbe: true,
			readsNow: "", commitsTo: "file", readsAfter: "",
			changes: false, writesPin: true, notice: true,
		},
		{
			name:   "an unpinned directory holding its own file token upgrades to the keychain",
			policy: "auto", declaration: "auto", ownToken: true, keychain: true, wantProbe: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "",
			changes: true, writesPin: true,
		},
		{
			name:     "the auto repin: pinned file, keychain now usable",
			declared: "auto", pin: "file", policy: "auto", keychain: true, wantProbe: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "file",
			changes: true, writesPin: true,
		},
		{
			name:     "a keychain pin is never re-probed, so a broken keychain never moves a directory to plaintext",
			declared: "auto", pin: "keychain", policy: "auto", keychain: false,
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			changes: false, writesPin: false,
		},
		{
			name:     "a pinned file directory whose keychain stays unusable changes nothing",
			declared: "auto", pin: "file", policy: "auto", keychain: false, wantProbe: true,
			readsNow: "file", commitsTo: "file", readsAfter: "file",
			changes: false, writesPin: false,
		},
		{
			name:     "an explicit policy never asks the pin",
			declared: "keychain", pin: "file", policy: "keychain",
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			changes: false, writesPin: false,
		},
		{
			name:     "the flag changes a keychain directory to the file store",
			declared: "keychain", policy: "file", declaration: "file",
			readsNow: "keychain", commitsTo: "file", readsAfter: "file",
			changes: true, writesPin: false,
		},
		{
			name:     "the flag changes a file directory to the keychain",
			declared: "file", policy: "keychain", declaration: "keychain", fileTokens: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "keychain",
			changes: true, writesPin: false,
		},
		{
			name:     "a flag equal to the declaration changes nothing",
			declared: "file", policy: "file", declaration: "file",
			readsNow: "file", commitsTo: "file", readsAfter: "file",
			changes: false, writesPin: false,
		},
		{
			// With no pin at all, readers after the declaration write
			// would each resolve by their own probe - on a machine whose
			// keychain they cannot use, the plaintext file - so the pin
			// is written first.
			name:     "a declared keychain directory asked for auto keeps reading the keychain, and records it",
			declared: "keychain", policy: "auto", declaration: "auto", keychain: true, wantProbe: true,
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			changes: false, writesPin: false, pinsFirst: true,
		},
		{
			// The `file` pre-pin keeps readers on the file store until
			// the pin step, so nothing needs writing early.
			name:     "a declared file directory with no pin asked for auto keeps its pin last",
			declared: "file", policy: "auto", declaration: "auto", keychain: true, wantProbe: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "",
			changes: true, writesPin: true,
		},
		{
			name:     "a helper is a backend like any other",
			declared: "exec:vault", policy: "exec:vault",
			readsNow: "exec:vault", commitsTo: "exec:vault", readsAfter: "exec:vault",
			changes: false, writesPin: false,
		},
		{
			// The remedy for a directory the variable and the declaration
			// disagree about: the flag names the store, and what a reader
			// resolves to meanwhile is the directory's own record.
			name:     "a named store settles a directory the variable disagrees with",
			declared: "keychain", env: "file", policy: "file", declaration: "file",
			readsNow: "keychain", commitsTo: "file", readsAfter: "file",
			changes: true, writesPin: false,
		},
		{
			// The flag is allowed to name what the directory already
			// says: that is an ordinary login, not a change.
			name:     "a flag naming the store the directory already declares",
			declared: "keychain", policy: "keychain", declaration: "keychain",
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			changes: false, writesPin: false,
		},
		{
			// A keychain pin left behind under an explicit declaration
			// would send every reader to the file store between the
			// declaration write and a pin step at the end, so the pin is
			// written first, while no reader asks it.
			name:     "a pin that names a third store is written first",
			declared: "keychain", pin: "file", policy: "auto", declaration: "auto",
			keychain: true, wantProbe: true,
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			changes: false, writesPin: false, pinsFirst: true,
		},
		{
			name: "a named store on an undeclared directory reads through the pin",
			env:  "file", policy: "file", declaration: "file", pin: "keychain",
			readsNow: "keychain", commitsTo: "file", readsAfter: "file",
			changes: true, writesPin: false,
		},
		{
			// Where the credentials are is the directory's own record: the
			// variable chooses where this login writes, and a store other
			// than the one they are in is a change asked for by name.
			name: "the variable on an undeclared directory is not where the credentials are",
			env:  "keychain", policy: "keychain", declaration: "keychain", pin: "file", fileTokens: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "keychain",
			changes: true, writesPin: false,
		},
		{
			// Another entry's credential is in the file store, so the
			// upgrade stays there and says so rather than leave the
			// directory reading two stores.
			name:     "the auto upgrade stays on the file store while another entry is there",
			declared: "auto", pin: "file", policy: "auto", keychain: true, wantProbe: true,
			fileTokens: true, others: true,
			readsNow: "file", commitsTo: "file", readsAfter: "file",
			changes: false, writesPin: false, notice: true,
		},
		{
			// Another entry left a keychain credential behind, so moving
			// the readers there would serve it again: the login stays.
			name:     "the auto upgrade stays on the file store while another entry has a credential left in the keychain",
			declared: "auto", pin: "file", policy: "auto", keychain: true, wantProbe: true,
			others: true, otherRecord: true,
			readsNow: "file", commitsTo: "file", readsAfter: "file",
			changes: false, writesPin: false, notice: true,
		},
		{
			name:     "an unpinned directory stays on the file store while another entry is there, and pins it",
			declared: "auto", policy: "auto", declaration: "auto", keychain: true, wantProbe: true,
			fileTokens: true, others: true,
			readsNow: "file", commitsTo: "file", readsAfter: "",
			changes: false, writesPin: true, notice: true,
		},
		{
			// Declared `auto` and pinned `file`: the pin is written first,
			// while the declaration is still explicit.
			name:     "a declared file directory asked for auto stays on the file store while another entry is there",
			declared: "file", policy: "auto", declaration: "auto", keychain: true, wantProbe: true,
			fileTokens: true, others: true,
			readsNow: "file", commitsTo: "file", readsAfter: "file",
			changes: false, writesPin: false, pinsFirst: true, notice: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentialStore, tc.env)
			dir := t.TempDir()
			if tc.pin != "" {
				require.NoError(t, WritePin(dir, tc.pin))
			}
			if tc.fileTokens {
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "other", URL: "https://r.ex"}, "other-tok"))
			}
			if tc.otherRecord {
				require.NoError(t, os.MkdirAll(identityDirPath(dir), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(identityDirPath(dir), identityFileName("other")), []byte("x"), 0o600))
			}
			if tc.ownToken {
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-tok"))
			}
			probes := 0
			orig := keyringUsable
			keyringUsable = func() error { probes++; return probeAnswer(tc.keychain) }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

			cfg := &Config{CredentialStore: tc.declared, Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
			if tc.others {
				cfg.Servers["other"] = Server{URL: "https://r.ex"}
			}
			b, err := loginBackendsOf(dir, cfg, "prod", tc.policy, tc.declaration)
			require.NoError(t, err)

			assert.Equal(t, tc.readsNow, b.ReadsNow, "what a reader resolves to now")
			assert.Equal(t, tc.commitsTo, b.CommitsTo, "what the login commits to")
			assert.Equal(t, tc.readsAfter, b.ReadsAfter, "what a reader resolves to afterwards")
			assert.Equal(t, tc.changes, b.ChangesBackend(), "whether this entry's credential changes store")
			assert.Equal(t, tc.writesPin, b.WritesPin(), "whether the change ends with a pin")
			assert.Equal(t, tc.pinsFirst, b.PinsFirst, "whether the pin is written in the pre-commit step")
			assert.Equal(t, tc.changes || tc.writesPin || tc.pinsFirst, b.ChangesStore())
			if tc.notice {
				assert.Contains(t, b.Notice, "plaintext")
			} else {
				assert.Empty(t, b.Notice)
			}
			if tc.wantProbe {
				assert.Equal(t, 1, probes, "the probe runs once, and its result is the process's")
			} else {
				assert.Zero(t, probes, "nothing here needs the keychain probed")
			}

			assert.Equal(t, tc.pin, ReadPin(dir), "deciding writes no pin and changes none")
		})
	}
}

// loginRefusal is LoginStoreChange's refusal alone, for a test that asks
// only whether the login may go on.
func loginRefusal(dir string, cfg *Config, entry, policy, declaration string) error {
	_, err := LoginStoreChange(dir, cfg, entry, policy, declaration)
	return err
}

// TestLoginBackendsCarriesAStoreConflict asserts a directory whose
// declaration the variable disagrees with never gets as far as a backend:
// a login that reached one would already be wrong about where it is
// writing.
func TestLoginBackendsCarriesAStoreConflict(t *testing.T) {
	t.Setenv(EnvCredentialStore, "file")
	dir := t.TempDir()

	_, err := loginBackendsOf(dir, &Config{CredentialStore: "keychain"}, "prod", "keychain", "")
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, EnvCredentialStore)
	assert.NoFileExists(t, PinPath(dir), "no pin is written on the way out")
}

// TestLoginPolicyPrefersTheFlagAndRefusesADisagreeingVariable asserts the
// precedence a login uses. The flag names the store outright - changing
// the directory's declaration is what it is for - so it is weighed only
// against the variable; without a flag the ordinary rules apply.
func TestLoginPolicyPrefersTheFlagAndRefusesADisagreeingVariable(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		env, requested, declared string
		wantPolicy, wantSource   string
		wantErr, wantErrContains string
	}{
		{name: "the flag alone", requested: "file", declared: "keychain", wantPolicy: "file", wantSource: "flag"},
		{name: "the flag and an agreeing variable", env: "file", requested: "file", declared: "keychain",
			wantPolicy: "file", wantSource: "flag"},
		{name: "the flag and a disagreeing variable", env: "keychain", requested: "file", declared: "",
			wantErr: "this login asks for", wantErrContains: EnvCredentialStore},
		{name: "an undeclared directory and a disagreeing variable", env: "keychain", requested: "file",
			wantErr: "this login asks for", wantErrContains: "--credential-store"},
		{name: "no flag: the declaration", declared: "file", wantPolicy: "file", wantSource: "config"},
		{name: "no flag: the variable on an undeclared directory", env: "file", wantPolicy: "file", wantSource: "env"},
		{name: "no flag: the config conflict still fires", env: "file", declared: "keychain",
			wantErr: "this config directory stores credentials in", wantErrContains: EnvCredentialStore},
		{name: "a store this release does not know", requested: "vault", wantErr: "unknown credential store"},
		{name: "a helper name that is not one", requested: "exec:NOPE", wantErr: "invalid credential helper name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentialStore, tc.env)
			policy, source, err := LoginPolicy(tc.requested, tc.declared)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				if tc.wantErrContains != "" {
					require.ErrorContains(t, err, tc.wantErrContains)
				}
				ce, ok := errorsAsClierr(err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				if tc.requested != "" {
					assert.Empty(t, policy, "a refused flag resolves to nothing at all")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPolicy, policy)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}

// TestAnAutoLoginNeverMovesADirectoryIntoPlaintext asserts the refusal
// that keeps `auto` from being a silent downgrade. A login that asks for
// `auto` on a directory whose credentials are in the keychain or a helper,
// on a machine whose keychain cannot be used right now, would otherwise
// put every one of them in the plaintext file on the strength of one
// probe. It is refused before the sign-in instead, as
// CREDENTIAL_STORE_UNAVAILABLE with the probe's cause and the two ways out
// in the message. A fresh directory, which has nothing to move, still
// falls back to the file store and says so.
func TestAnAutoLoginNeverMovesADirectoryIntoPlaintext(t *testing.T) {
	for _, tc := range []struct {
		name, declared, pin, from string
	}{
		{name: "a keychain directory with no pin", declared: "keychain", from: "keychain"},
		{name: "a keychain directory with a stale file pin", declared: "keychain", pin: "file", from: "keychain"},
		{name: "a helper directory", declared: "exec:vault", from: "exec:vault"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentialStore, "")
			dir := t.TempDir()
			if tc.pin != "" {
				require.NoError(t, WritePin(dir, tc.pin))
			}
			orig := keyringUsable
			keyringUsable = func() error { return errNoKeychainSession }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

			cfg := &Config{CredentialStore: tc.declared, Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
			for _, err := range []error{
				func() error {
					_, err := loginBackendsOf(dir, cfg, "prod", "auto", "auto")
					return err
				}(),
				loginRefusal(dir, cfg, "prod", "auto", "auto"),
			} {
				require.Error(t, err)
				assertStoreUnavailable(t, err)
				require.ErrorContains(t, err, "the keychain cannot be used right now (no keychain session)")
				require.ErrorContains(t, err, "never moves this directory's credentials out of "+tc.from+" into the plaintext file")
				assert.Contains(t, clierr.ExpandRemedies(err.Error(), ""), keychainRemedy()+", then run this command again, or run rancher auth login 'prod' --credential-store file")
			}
			assert.Equal(t, tc.pin, ReadPin(dir), "the refusal writes nothing")
		})
	}

	t.Run("a fresh directory still falls back, and says so", func(t *testing.T) {
		t.Setenv(EnvCredentialStore, "")
		dir := t.TempDir()
		orig := keyringUsable
		keyringUsable = func() error { return errNoKeychainSession }
		resetKeyringUsableCache()
		t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

		b, err := loginBackendsOf(dir, &Config{}, "prod", "auto", "auto")
		require.NoError(t, err)
		assert.Equal(t, backendFile, b.CommitsTo)
		assert.Contains(t, b.Notice, "plaintext")
	})
}

// TestAnUpgradeOnAOneEntryDirectoryLooksUpNoOtherEntry asserts what the
// `auto` upgrade costs a directory whose only entry is the one logging
// in - the kubectl plugin's re-login after a 401, say: nothing about other
// entries is read, so neither credstore-id/ nor credentials.yaml is looked
// at for them.
func TestAnUpgradeOnAOneEntryDirectoryLooksUpNoOtherEntry(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir()
	require.NoError(t, WritePin(dir, backendFile))
	orig := keyringUsable
	keyringUsable = func() error { return nil }
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })
	ops := map[string]int{}
	t.Cleanup(SetIdentityObserverForTest(func(op string) { ops[op]++ }))

	b, err := loginBackendsOf(dir, &Config{CredentialStore: "auto", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}, "prod", "auto", "")
	require.NoError(t, err)
	assert.Equal(t, backendKeychain, b.CommitsTo, "the upgrade goes through")
	assert.Zero(t, ops["dirstat"], "no look at credstore-id/")
	assert.Zero(t, ops["list"])
}
