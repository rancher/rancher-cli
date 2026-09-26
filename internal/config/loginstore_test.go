package config

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestALoginResolvesOneBackendSetAndOpensIt asserts the one answer every
// condition in the login order is derived from: what a reader resolves to
// now, what the login commits to, what a reader resolves to afterwards,
// and the two questions that follow - does anything move, and does the
// change end with a pin. The rows are the states a directory can be in
// when a login reaches it.
func TestALoginResolvesOneBackendSetAndOpensIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// declared is config.yaml's credentialStore; pin is credstore-pin;
		// policy is what the login resolved for itself and source is
		// where it came from; declaration is what it will write
		// afterwards; env is RANCHER_CLI_CREDENTIAL_STORE.
		declared, pin, policy, source, declaration, env string
		// fileTokens seeds credentials.yaml.
		fileTokens bool
		// keychain is what the probe answers.
		keychain                        bool
		readsNow, commitsTo, readsAfter string
		moves, writesPin, notice        bool
		// pinsFirst is the pin written in step 0 because the one on disk
		// names a third store.
		pinsFirst bool
		wantProbe bool
	}{
		{
			name:   "a fresh directory decides, and the change is the pin alone",
			policy: "auto", declaration: "auto", keychain: true, wantProbe: true,
			readsNow: "", commitsTo: "keychain", readsAfter: "",
			moves: false, writesPin: true,
		},
		{
			name:   "a fresh directory whose keychain cannot be used says so",
			policy: "auto", declaration: "auto", keychain: false, wantProbe: true,
			readsNow: "", commitsTo: "file", readsAfter: "",
			moves: false, writesPin: true, notice: true,
		},
		{
			name:   "an unpinned directory holding file tokens moves them",
			policy: "auto", declaration: "auto", fileTokens: true, keychain: true, wantProbe: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "",
			moves: true, writesPin: true,
		},
		{
			name:     "the auto repin: pinned file, keychain now usable",
			declared: "auto", pin: "file", policy: "auto", keychain: true, wantProbe: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "file",
			moves: true, writesPin: true,
		},
		{
			name:     "a keychain pin is never re-probed, so a broken keychain never moves a directory to plaintext",
			declared: "auto", pin: "keychain", policy: "auto", keychain: false,
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			moves: false, writesPin: false,
		},
		{
			name:     "a pinned file directory whose keychain stays unusable changes nothing",
			declared: "auto", pin: "file", policy: "auto", keychain: false, wantProbe: true,
			readsNow: "file", commitsTo: "file", readsAfter: "file",
			moves: false, writesPin: false,
		},
		{
			name:     "an explicit policy never asks the pin",
			declared: "keychain", pin: "file", policy: "keychain",
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			moves: false, writesPin: false,
		},
		{
			name:     "the flag moves a keychain directory to the file store",
			declared: "keychain", policy: "file", declaration: "file",
			readsNow: "keychain", commitsTo: "file", readsAfter: "file",
			moves: true, writesPin: false,
		},
		{
			name:     "the flag moves a file directory to the keychain",
			declared: "file", policy: "keychain", declaration: "keychain", fileTokens: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "keychain",
			moves: true, writesPin: false,
		},
		{
			name:     "a flag equal to the declaration changes nothing",
			declared: "file", policy: "file", declaration: "file",
			readsNow: "file", commitsTo: "file", readsAfter: "file",
			moves: false, writesPin: false,
		},
		{
			// With no pin at all, readers after the declaration write
			// would each resolve by their own probe - on a machine whose
			// keychain they cannot use, the plaintext file - so the pin
			// is written first.
			name:     "a declared keychain directory asked for auto keeps reading the keychain, and records it",
			declared: "keychain", policy: "auto", declaration: "auto", keychain: true, wantProbe: true,
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			moves: false, writesPin: false, pinsFirst: true,
		},
		{
			// The `file` pre-pin keeps readers on the file store until
			// each entry has moved, so nothing needs writing early.
			name:     "a declared file directory with no pin asked for auto keeps its pin last",
			declared: "file", policy: "auto", source: PolicySourceFlag, declaration: "auto", keychain: true, wantProbe: true,
			readsNow: "file", commitsTo: "keychain", readsAfter: "",
			moves: true, writesPin: true,
		},
		{
			name:     "a helper is a backend like any other",
			declared: "exec:vault", policy: "exec:vault",
			readsNow: "exec:vault", commitsTo: "exec:vault", readsAfter: "exec:vault",
			moves: false, writesPin: false,
		},
		{
			// The remedy for a directory the variable and the declaration
			// disagree about: the flag names the store, and what a reader
			// resolves to meanwhile is the directory's own record.
			name:     "a named store settles a directory the variable disagrees with",
			declared: "keychain", env: "file", policy: "file", source: PolicySourceFlag, declaration: "file",
			readsNow: "keychain", commitsTo: "file", readsAfter: "file",
			moves: true, writesPin: false,
		},
		{
			// The flag is allowed to name what the directory already
			// says: that is an ordinary login, not a change.
			name:     "a flag naming the store the directory already declares",
			declared: "keychain", policy: "keychain", source: PolicySourceFlag, declaration: "keychain",
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			moves: false, writesPin: false,
		},
		{
			// A keychain pin left behind under an explicit declaration
			// would send every reader to the file store between the
			// declaration write and a pin step at the end, so the pin is
			// written first, while no reader asks it.
			name:     "a pin that names a third store is written first",
			declared: "keychain", pin: "file", policy: "auto", source: PolicySourceFlag, declaration: "auto",
			keychain: true, wantProbe: true,
			readsNow: "keychain", commitsTo: "keychain", readsAfter: "keychain",
			moves: false, writesPin: false, pinsFirst: true,
		},
		{
			name: "a named store on an undeclared directory reads through the pin",
			env:  "file", policy: "file", source: PolicySourceFlag, declaration: "file", pin: "keychain",
			readsNow: "keychain", commitsTo: "file", readsAfter: "file",
			moves: true, writesPin: false,
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
			probes := 0
			orig := keyringUsable
			keyringUsable = func() bool { probes++; return tc.keychain }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

			cfg := &Config{CredentialStore: tc.declared}
			b, err := loginBackendsOf(dir, cfg, tc.policy, tc.source, tc.declaration)
			require.NoError(t, err)

			assert.Equal(t, tc.readsNow, b.ReadsNow, "what a reader resolves to now")
			assert.Equal(t, tc.commitsTo, b.CommitsTo, "what the login commits to")
			assert.Equal(t, tc.readsAfter, b.ReadsAfter, "what a reader resolves to afterwards")
			assert.Equal(t, tc.moves, b.Moves(), "whether the other entries move")
			assert.Equal(t, tc.writesPin, b.WritesPin(), "whether the change ends with a pin")
			assert.Equal(t, tc.pinsFirst, b.PinsFirst, "whether the pin is written before the commit")
			assert.Equal(t, tc.moves || tc.writesPin || tc.pinsFirst, b.ChangesStore())
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

// TestLoginBackendsCarriesAStoreConflict asserts a directory whose
// declaration the variable disagrees with never gets as far as a backend:
// a login that reached one would already be wrong about where it is
// writing.
func TestLoginBackendsCarriesAStoreConflict(t *testing.T) {
	t.Setenv(EnvCredentialStore, "file")
	dir := t.TempDir()

	_, err := loginBackendsOf(dir, &Config{CredentialStore: "keychain"}, "keychain", "", "")
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
