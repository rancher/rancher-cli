package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

const persistRestoreBase = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

func persistFixture(t *testing.T, existing *Server) (dir string, cfgPath string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv(EnvCredentialStore, "")
	// The file policy is explicit: under auto the login would probe the
	// keychain and commit to it rather than to the store the test injected.
	cfg := &Config{Servers: map[string]Server{"other": {URL: "https://other.ex"}}, CurrentServer: "other", TokenReuse: true, CredentialStore: "file"}
	if existing != nil {
		cfg.Servers["prod"] = *existing
	}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, FilePath(dir)
}

func fileOpen(store CredStore) func(string, string) (CredStore, string, error) {
	return func(string, string) (CredStore, string, error) { return store, "", nil }
}

// pluginInput is the plugin caller's input against the fixture's file
// policy: the decision names the file backend AND the disclosure the flow
// gave, so revalidation passes; the revalidation tests override Decision
// to make it fail on purpose.
func pluginInput(dir, name string, store CredStore) PersistLoginInput {
	return PersistLoginInput{Name: name, CollisionSuffix: true, CanonicalURL: "https://r.ex", CA: persistRestoreBase,
		Provenance: clierr.ProvenanceExecLogin, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-new"},
		TokenUserID: "user-a", Bearer: "ext/token-new:s", ExpiresAt: "2026-10-01T00:00:00Z",
		OwnsEntry: true, ReadOnlyStoreIsFailure: true, OpenStore: fileOpen(store),
		Decision: &StoreDecision{RequireSetting: true, Policy: "file", DisclosedPlaintextPath: CredentialsFilePath(dir)},
		Now:      func() time.Time { return time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC) }}
}

func loginInput(dir string, store CredStore) PersistLoginInput {
	in := pluginInput(dir, "prod", store)
	in.CollisionSuffix, in.OwnsEntry, in.ReadOnlyStoreIsFailure = false, false, false
	in.Provenance = clierr.ProvenanceDerived
	in.Decision = nil
	return in
}

func derivedEntry() *Server {
	return &Server{URL: "https://r.ex", CA: persistRestoreBase, CAFingerprint: "f0",
		Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"},
		TokenUserID: "user-a", CredentialExpiresAt: "2026-09-20T08:00:00Z"}
}

// testCertificateAndFingerprint returns a self-signed certificate as PEM
// and the fingerprint tlstrust.Fingerprint would compute for it.
func testCertificateAndFingerprint(t *testing.T) (pemText, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	sum := sha256.Sum256(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), hex.EncodeToString(sum[:])
}

// TestPersistSuccessWritesEntryOnceAndRecordsThePriorToken asserts
// the success path for both callers: exactly one config write before the
// result, the entry naming the committed token, a superseded record for a
// CLI-created prior token, CurrentServer untouched, and no failure
// components.
func TestPersistSuccessWritesEntryOnceAndRecordsThePriorToken(t *testing.T) {
	for _, caller := range []string{"login", "plugin"} {
		t.Run(caller, func(t *testing.T) {
			dir, path := persistFixture(t, derivedEntry())
			writes := 0
			restore := SetPersistWriteHookForTest(func(string) error { writes++; return nil })
			t.Cleanup(restore)
			store := NewFileStore(dir)
			in := loginInput(dir, store)
			if caller == "plugin" {
				in = pluginInput(dir, "prod", store)
			}
			out, err := PersistLoginCredential(t.Context(), dir, in)
			require.NoError(t, err)
			// Two: the first, before the commit, which advances the
			// entry's cache binding id and pauses its cache writes, and
			// the last, after it, which resumes them. They cannot be one
			// write - no record may be written between the two.
			assert.Equal(t, 2, writes, "the first write and the last")
			assert.Equal(t, clierr.ConfigWritten, out.ConfigState)
			assert.Equal(t, clierr.CredentialCommitted, out.CredentialState)
			assert.Nil(t, out.ConfigRecovery)
			assert.Empty(t, out.Aborted)

			cfg, err := Load(path)
			require.NoError(t, err)
			got := cfg.Servers["prod"]
			assert.Equal(t, in.Provenance, got.Provenance)
			assert.Equal(t, "token-new", got.TokenRef.Name)
			assert.Equal(t, "user-a", got.TokenUserID)
			assert.Equal(t, "2026-10-01T00:00:00Z", got.CredentialExpiresAt)
			assert.Equal(t, persistRestoreBase, got.CA, "an update never changes trust")
			assert.Equal(t, "f0", got.CAFingerprint)
			assert.Len(t, got.CacheBindingID, randomIDLen, "the login leaves the entry a cache binding id")
			require.Len(t, got.SupersededTokens, 1)
			assert.Equal(t, clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}, got.SupersededTokens[0].Ref)
			assert.Equal(t, "user-a", got.SupersededTokens[0].UserID)
			assert.Equal(t, "2026-09-20T08:00:00Z", got.SupersededTokens[0].ExpiresAt)
			assert.Equal(t, "2026-09-04T18:00:00Z", got.SupersededTokens[0].SupersededAt)
			assert.Equal(t, got.SupersededTokens, out.SupersededTokens)
			assert.Equal(t, "other", cfg.CurrentServer, "CurrentServer never changes")
			assert.Equal(t, "other", out.CurrentServer)
			assert.True(t, out.CurrentServerValid)
			tok, err := store.GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
			require.NoError(t, err)
			assert.Equal(t, "ext/token-new:s", tok)
		})
	}
}

// TestPersistNeverRecordsAPriorTokenTheCLIDidNotCreate asserts a prior
// token with a user-supplied or absent provenance leaves no record.
func TestPersistNeverRecordsAPriorTokenTheCLIDidNotCreate(t *testing.T) {
	for _, prior := range []Server{
		{URL: "https://r.ex", Provenance: clierr.ProvenanceUserSupplied},
		{URL: "https://r.ex"},
	} {
		name := string(prior.Provenance)
		if name == "" {
			name = "no provenance"
		}
		t.Run(name, func(t *testing.T) {
			dir, path := persistFixture(t, &prior)
			_, err := PersistLoginCredential(t.Context(), dir, loginInput(dir, NewFileStore(dir)))
			require.NoError(t, err)
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Empty(t, cfg.Servers["prod"].SupersededTokens)
		})
	}
}

// TestPersistCreatesAnAbsentEntryForThePluginCallerOnly asserts the plugin
// caller creates an absent entry with the connection's CA and no records,
// and suffixes the name when it collides with a different-URL entry; the
// login caller gets NOT_FOUND for an absent entry.
func TestPersistCreatesAnAbsentEntryForThePluginCallerOnly(t *testing.T) {
	dir, path := persistFixture(t, nil)
	store := NewFileStore(dir)
	out, err := PersistLoginCredential(t.Context(), dir, pluginInput(dir, "exec-login-r.ex", store))
	require.NoError(t, err)
	assert.Equal(t, "exec-login-r.ex", out.Name)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Contains(t, cfg.Servers, "exec-login-r.ex", "the absent entry was created")
	assert.Equal(t, persistRestoreBase, cfg.Servers["exec-login-r.ex"].CA)
	assert.Empty(t, cfg.Servers["exec-login-r.ex"].SupersededTokens)

	// A same-URL plugin entry is reused, never duplicated.
	out, err = PersistLoginCredential(t.Context(), dir, pluginInput(dir, "exec-login-r.ex", store))
	require.NoError(t, err)
	assert.Equal(t, "exec-login-r.ex", out.Name)
	cfg, err = Load(path)
	require.NoError(t, err)
	assert.Len(t, cfg.Servers, 2, "the same-URL entry is reused, never duplicated")

	// A different-URL collision takes the next free suffix.
	cfg, _ = Load(path)
	cfg.Servers["exec-login-x"] = Server{URL: "https://elsewhere.ex"}
	cfg.Servers["exec-login-x-2"] = Server{URL: "https://elsewhere2.ex"}
	require.NoError(t, cfg.Write())
	out, err = PersistLoginCredential(t.Context(), dir, pluginInput(dir, "exec-login-x", store))
	require.NoError(t, err)
	assert.Equal(t, "exec-login-x-3", out.Name)

	_, err = PersistLoginCredential(t.Context(), dir, loginInput(dir, store))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

// TestPersistEntryWriteFailureTouchesNothing asserts a failed entry
// write reports writeFailed, leaves the store untouched, and leaves the
// file byte-identical.
func TestPersistEntryWriteFailureTouchesNothing(t *testing.T) {
	for _, caller := range []string{"login", "plugin"} {
		t.Run(caller, func(t *testing.T) {
			dir, path := persistFixture(t, derivedEntry())
			before, _ := os.ReadFile(path)
			restore := SetPersistWriteHookForTest(func(step string) error {
				if step == "entry" {
					return errors.New("disk full")
				}
				return nil
			})
			t.Cleanup(restore)
			store := NewFileStore(dir)
			in := loginInput(dir, store)
			if caller == "plugin" {
				in = pluginInput(dir, "prod", store)
			}
			out, err := PersistLoginCredential(t.Context(), dir, in)
			require.ErrorContains(t, err, "disk full")
			assert.Equal(t, clierr.ConfigWriteFailed, out.ConfigState)
			assert.Nil(t, out.Commit)
			after, _ := os.ReadFile(path)
			assert.Equal(t, string(before), string(after))
			_, gerr := store.GetToken(ServerRef{Name: "prod"})
			require.ErrorIs(t, gerr, ErrNoToken)
		})
	}
}

// failStore fails SetTokenWithOutcome with a fixed outcome and error.
type failStore struct {
	CredStore
	out CommitOutcome
	err error
}

func (f failStore) SetTokenWithOutcome(ServerRef, string, PriorCredential) (CommitOutcome, error) {
	return f.out, f.err
}
func (f failStore) SetToken(ref ServerRef, tok string) error {
	_, err := f.SetTokenWithOutcome(ref, tok, PriorCredential{})
	return err
}

// TestPersistRollsBackOnEveryErrorCarryingCommit asserts the rollback
// cases: notWritten for create and update, possiblyCommitted for an
// update, and the read-only store as a failure for the plugin caller. In
// every case the entry is removed (create) or restored to its snapshot
// (update), ConfigState is notWritten, and CurrentServer is unchanged. A
// possiblyCommitted create keeps the entry and has its own test.
func TestPersistRollsBackOnEveryErrorCarryingCommit(t *testing.T) {
	boom := errors.New("backend refused")
	cases := []struct {
		name      string
		existing  *Server
		out       CommitOutcome
		err       error
		wantState clierr.CredentialState
	}{
		{"create notWritten", nil, CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}, boom, clierr.CredentialNotWritten},
		{"update notWritten", derivedEntry(), CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}, boom, clierr.CredentialNotWritten},
		{"update possiblyCommitted", derivedEntry(), CommitOutcome{Backend: clierr.BackendExec, State: CommitPossiblyCommitted, PriorSource: clierr.PriorUnknown, HelperRecordRetained: true}, boom, clierr.CredentialPossiblyCommitted},
		{"read-only store is a failure", derivedEntry(), CommitOutcome{Backend: clierr.BackendExec, State: CommitNotWritten, PriorSource: clierr.PriorUnknown}, errors.Join(ErrTokenStoreUnsupported, errors.New("helper: read-only")), clierr.CredentialNotWritten},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := persistFixture(t, tc.existing)
			name := "prod"
			if tc.existing == nil {
				name = "exec-login-r.ex"
			}
			var before Server
			if tc.existing != nil {
				before = *tc.existing
			}
			st := failStore{CredStore: NewFileStore(dir), out: tc.out, err: tc.err}
			out, err := PersistLoginCredential(t.Context(), dir, pluginInput(dir, name, st))
			require.ErrorIs(t, err, tc.err, "the commit's own error is the primary")
			assert.Equal(t, clierr.ConfigNotWritten, out.ConfigState, "rolled back")
			assert.Equal(t, tc.wantState, out.CredentialState)
			require.NotNil(t, out.Commit)
			assert.Nil(t, out.ConfigRecovery)
			cfg, lerr := Load(path)
			require.NoError(t, lerr)
			if tc.existing == nil {
				_, present := cfg.Servers[name]
				assert.False(t, present, "a created entry is removed")
			} else {
				got := cfg.Servers[name]
				assert.NotEqual(t, before.CacheBindingID, got.CacheBindingID, "all but the id, which never moves back")
				assert.False(t, got.Paused(), "and the rollback, the change's last write, resumes cache writes")
				before.CacheBindingID = got.CacheBindingID
				assert.Equal(t, before, got, "an updated entry is restored to its snapshot")
			}
			assert.Equal(t, "other", cfg.CurrentServer)
		})
	}
}

// TestPersistKeepsACreatedEntryWhenTheCallerCannotClearTheStore is the one
// exception to removal on create: a possiblyCommitted create keeps the
// entry so `auth logout <entry>` has something to address, and the caller
// reports credentialRecovery. The caller revokes the credential on every
// failure, so the kept entry must not name a token: it is written back
// with URL, trust, and provenance only, as a logout handle.
func TestPersistKeepsACreatedEntryWhenTheCallerCannotClearTheStore(t *testing.T) {
	dir, path := persistFixture(t, nil)
	st := failStore{CredStore: NewFileStore(dir),
		out: CommitOutcome{Backend: clierr.BackendExec, State: CommitPossiblyCommitted, PriorSource: clierr.PriorUnknown, HelperRecordRetained: true},
		err: errors.New("helper hung")}
	out, err := PersistLoginCredential(t.Context(), dir, pluginInput(dir, "exec-login-r.ex", st))
	require.ErrorContains(t, err, "helper hung")
	assert.Equal(t, clierr.ConfigWritten, out.ConfigState, "the entry stays as the logout handle")
	// The credential-recovery component is composed by token.FailLogin from
	// these facts; the primitive must carry them intact.
	assert.Equal(t, clierr.CredentialPossiblyCommitted, out.CredentialState)
	require.NotNil(t, out.Commit)
	assert.True(t, out.Commit.HelperRecordRetained)
	assert.Equal(t, clierr.BackendExec, out.Commit.Backend)
	cfg, _ := Load(path)
	got, present := cfg.Servers["exec-login-r.ex"]
	require.True(t, present)
	assert.Equal(t, "https://r.ex", got.URL)
	assert.Equal(t, clierr.ProvenanceExecLogin, got.Provenance)
	assert.Nil(t, got.TokenRef, "the entry never names a token the caller will revoke")
	assert.Empty(t, got.TokenUserID)
	assert.Empty(t, got.CredentialExpiresAt)
	assert.Len(t, got.CacheBindingID, randomIDLen, "the handle carries the id the change's first write gave it")
	assert.False(t, got.Paused(), "and its write, the change's last, resumes cache writes")
}

// TestPersistReadOnlyHelperForLoginRestoresAndReturnsTheSentinel
// asserts auth login's warn-and-succeed input: the entry goes back to its
// snapshot, nothing is stored, the sentinel is returned for the caller,
// and a pending guided-trust CA is still applied to the restored entry.
func TestPersistReadOnlyHelperForLoginRestoresAndReturnsTheSentinel(t *testing.T) {
	dir, path := persistFixture(t, derivedEntry())
	st := failStore{CredStore: NewFileStore(dir),
		out: CommitOutcome{Backend: clierr.BackendExec, State: CommitNotWritten, PriorSource: clierr.PriorUnknown},
		err: errors.Join(ErrTokenStoreUnsupported, errors.New("helper: read-only"))}
	in := loginInput(dir, st)
	in.PendingTrust = &PendingTrust{CA: "-----BEGIN CERTIFICATE-----\nNEW\n-----END CERTIFICATE-----\n", Fingerprint: "f1"}
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorIs(t, err, ErrTokenStoreUnsupported)
	assert.Equal(t, clierr.ConfigWritten, out.ConfigState)
	cfg, _ := Load(path)
	got := cfg.Servers["prod"]
	assert.Equal(t, "token-old", got.TokenRef.Name, "metadata restored from the snapshot")
	assert.Empty(t, got.SupersededTokens)
	assert.Equal(t, in.PendingTrust.CA, got.CA, "trust the user accepted is persisted as today")
	assert.Equal(t, "f1", got.CAFingerprint)
}

// TestPersistRollbackFailureReportsConfigRecovery asserts a failed
// rollback write leaves the entry as written, reports rollbackFailed, and
// carries the entry, the exact prior snapshot, and the delete-server
// command; a create carries no prior.
func TestPersistRollbackFailureReportsConfigRecovery(t *testing.T) {
	for _, existing := range []*Server{derivedEntry(), nil} {
		label, name := "update", "prod"
		if existing == nil {
			label, name = "create", "exec-login-r.ex"
		}
		t.Run(label, func(t *testing.T) {
			dir, path := persistFixture(t, existing)
			restore := SetPersistWriteHookForTest(func(step string) error {
				if step == "rollback" {
					return errors.New("read-only filesystem")
				}
				return nil
			})
			st := failStore{CredStore: NewFileStore(dir), out: CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}, err: errors.New("refused")}
			out, err := PersistLoginCredential(t.Context(), dir, pluginInput(dir, name, st))
			restore()
			require.ErrorContains(t, err, "refused")
			assert.Equal(t, clierr.ConfigRollbackFailed, out.ConfigState)
			require.NotNil(t, out.ConfigRecovery)
			assert.Equal(t, name, out.ConfigRecovery.Entry)
			assert.Equal(t, "rancher config delete-server "+clierr.ShellQuote(name), out.ConfigRecovery.Command)
			cfg, _ := Load(path)
			assert.Equal(t, "token-new", cfg.Servers[name].TokenRef.Name, "the entry stays as written after the failed rollback")
			if existing == nil {
				assert.Nil(t, out.ConfigRecovery.Prior)
				return
			}
			prior := out.ConfigRecovery.Prior
			require.NotNil(t, prior)
			assert.Equal(t, existing.URL, prior.URL)
			assert.Equal(t, existing.CA, prior.CA, "the exact CA value, byte for byte")
			assert.Equal(t, "f0", prior.CAFingerprint)
			assert.Equal(t, existing.Provenance, prior.Provenance)
			assert.Equal(t, existing.TokenRef, prior.TokenRef)
			assert.Equal(t, existing.TokenUserID, prior.TokenUserID)
			assert.Equal(t, existing.CredentialExpiresAt, prior.CredentialExpiresAt)
		})
	}
}

// TestPersistPriorFingerprintIsComputedWhenNotStored asserts a snapshot
// with an inline CA and no stored fingerprint still reports one, in the
// same encoding tlstrust.Fingerprint uses (lowercase hex of SHA-256 over
// the certificate's DER).
func TestPersistPriorFingerprintIsComputedWhenNotStored(t *testing.T) {
	certPEM, wantFingerprint := testCertificateAndFingerprint(t)
	existing := derivedEntry()
	existing.CA, existing.CAFingerprint = certPEM, ""
	dir, _ := persistFixture(t, existing)
	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "rollback" {
			return errors.New("ro")
		}
		return nil
	})
	t.Cleanup(restore)
	st := failStore{CredStore: NewFileStore(dir), out: CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}, err: errors.New("refused")}
	out, _ := PersistLoginCredential(t.Context(), dir, loginInput(dir, st))
	require.NotNil(t, out.ConfigRecovery)
	assert.Equal(t, wantFingerprint, out.ConfigRecovery.Prior.CAFingerprint)
}

// TestPersistRecoveryCommandQuotesAHostileEntryName asserts the emitted
// recovery command survives an operator-chosen entry name with whitespace,
// quotes, and shell metacharacters: entry names are only prefix-checked,
// and the command is meant to be pasted. The value round-trips through
// /bin/sh as one argument.
func TestPersistRecoveryCommandQuotesAHostileEntryName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell")
	}
	hostile := "prod it's; rm -rf /"
	existing := derivedEntry()
	dir, path := persistFixture(t, nil)
	cfg, _ := Load(path)
	cfg.Servers[hostile] = *existing
	require.NoError(t, cfg.Write())
	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "rollback" {
			return errors.New("ro")
		}
		return nil
	})
	t.Cleanup(restore)
	st := failStore{CredStore: NewFileStore(dir), out: CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}, err: errors.New("refused")}
	in := loginInput(dir, st)
	in.Name = hostile
	out, _ := PersistLoginCredential(t.Context(), dir, in)
	require.NotNil(t, out.ConfigRecovery)
	assert.Equal(t, "rancher config delete-server 'prod it'\\''s; rm -rf /'", out.ConfigRecovery.Command)
	words, err := exec.Command("/bin/sh", "-c", "printf '%s\\n' "+strings.TrimPrefix(out.ConfigRecovery.Command, "rancher ")).Output()
	require.NoError(t, err)
	assert.Equal(t, []string{"config", "delete-server", hostile}, strings.Split(strings.TrimRight(string(words), "\n"), "\n"))
}

// TestPersistRevalidationAbortsBeforeAnyWrite asserts the plugin
// caller's pre-interaction decision is re-checked under the lock: the
// setting turned off, or the backend now the plaintext file without the
// disclosure, aborts with no write and no store access.
func TestPersistRevalidationAbortsBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(dir string, cfg *Config)
		decide  StoreDecision
		wantMsg string
	}{
		{"setting turned off", func(_ string, cfg *Config) { cfg.TokenReuse = false },
			StoreDecision{RequireSetting: true, Policy: "file", DisclosedPlaintextPath: ""}, "token reuse was turned off"},
		{"policy became file undisclosed", func(_ string, cfg *Config) { cfg.CredentialStore = "file" },
			StoreDecision{RequireSetting: true, Policy: "keychain"}, "plaintext"},
		{"auto pin became file undisclosed", func(dir string, cfg *Config) { cfg.CredentialStore = ""; require.NoError(t, WritePin(dir, "file")) },
			StoreDecision{RequireSetting: true, Policy: "auto"}, "plaintext"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := persistFixture(t, nil)
			// The login's own answer is what is checked, and under a file
			// pin it would probe; an unusable keychain keeps the pin.
			old := keyringUsable
			keyringUsable = func() bool { return false }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = old; resetKeyringUsableCache() })
			cfg, _ := Load(path)
			tc.mutate(dir, cfg)
			require.NoError(t, cfg.Write())
			before, _ := os.ReadFile(path)
			opened := false
			in := pluginInput(dir, "exec-login-r.ex", NewFileStore(dir))
			in.Decision = &tc.decide
			in.OpenStore = func(p, d string) (CredStore, string, error) { opened = true; return NewFileStore(d), "", nil }
			out, err := PersistLoginCredential(t.Context(), dir, in)
			require.ErrorIs(t, err, ErrPersistAborted)
			require.ErrorContains(t, err, tc.wantMsg)
			assert.NotEmpty(t, out.Aborted)
			assert.Equal(t, clierr.ConfigNotWritten, out.ConfigState)
			assert.False(t, opened, "no store access after an abort")
			after, _ := os.ReadFile(path)
			assert.Equal(t, string(before), string(after))
		})
	}

	t.Run("disclosed file backend proceeds", func(t *testing.T) {
		dir, path := persistFixture(t, nil)
		cfg, _ := Load(path)
		cfg.CredentialStore = "" // auto, pinned to file below
		require.NoError(t, cfg.Write())
		require.NoError(t, WritePin(dir, "file"))
		// The login probes the keychain under an auto file pin; the
		// probe is stubbed unusable so the file pin stands.
		old := keyringUsable
		keyringUsable = func() bool { return false }
		resetKeyringUsableCache()
		t.Cleanup(func() { keyringUsable = old; resetKeyringUsableCache() })
		in := pluginInput(dir, "exec-login-r.ex", NewFileStore(dir))
		in.Decision = &StoreDecision{RequireSetting: true, Policy: "auto", DisclosedPlaintextPath: CredentialsFilePath(dir)}
		_, err := PersistLoginCredential(t.Context(), dir, in)
		require.NoError(t, err)
	})
}

// TestPersistRevalidatesAgainstTheLoginsOwnPolicy asserts the re-check
// resolves the backend from the policy this login carries, not from the
// declaration it read. A login that carries its own store disclosed the
// backend THAT policy names before the interactive sign-in, so that is
// the promise the re-check has to hold the directory to; asking about the
// declaration instead compares the wrong two things and lets a plaintext
// commit through with no disclosure.
func TestPersistRevalidatesAgainstTheLoginsOwnPolicy(t *testing.T) {
	dir, path := persistFixture(t, nil)
	cfg, err := Load(path)
	require.NoError(t, err)
	// The directory declares the keychain; this login carries "file" and
	// disclosed nothing.
	cfg.CredentialStore = "keychain"
	require.NoError(t, cfg.Write())

	in := pluginInput(dir, "exec-login-r.ex", NewFileStore(dir))
	in.Policy = "file"
	in.ObservedDeclaration = "keychain"
	in.Decision = &StoreDecision{RequireSetting: true, Policy: "file", DisclosedPlaintextPath: ""}

	_, perr := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorIs(t, perr, ErrPersistAborted)
	require.ErrorContains(t, perr, "plaintext",
		"resolved from the login's own policy, the backend is the file and the disclosure is missing")
}

// TestPersistPreCheckRunsUnderTheLockBeforeAnyWrite asserts the caller's
// pre-check sees the freshly loaded config and a failure writes nothing.
func TestPersistPreCheckRunsUnderTheLockBeforeAnyWrite(t *testing.T) {
	dir, path := persistFixture(t, derivedEntry())
	before, _ := os.ReadFile(path)
	in := loginInput(dir, NewFileStore(dir))
	in.PreCheck = func(cfg *Config) error {
		if _, ok := cfg.Servers["prod"]; !ok {
			return errors.New("missing")
		}
		return clierr.New(clierr.CodeConflict, "server entry \"prod\" changed during login")
	}
	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorContains(t, err, "changed during login")
	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after))
}

// TestPersistPendingTrustWriteFailureCompensatesAndRestores asserts auth
// login's second write: a failed CA write after a committed credential
// compensates the store and restores the snapshot; a failed restore
// reports rollbackFailed with ConfigRecovery.
func TestPersistPendingTrustWriteFailureCompensatesAndRestores(t *testing.T) {
	dir, path := persistFixture(t, derivedEntry())
	store := NewFileStore(dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "ext/token-old:s"))
	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "trust" {
			return errors.New("trust write failed")
		}
		return nil
	})
	t.Cleanup(restore)
	in := loginInput(dir, store)
	in.PendingTrust = &PendingTrust{CA: "new-ca", Fingerprint: "f1"}
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorContains(t, err, "trust write failed")
	assert.Equal(t, clierr.ConfigNotWritten, out.ConfigState, "restored")
	assert.Equal(t, clierr.CredentialCompensated, out.CredentialState)
	tok, _ := store.GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	assert.Equal(t, "ext/token-old:s", tok, "the prior credential is back")
	cfg, _ := Load(path)
	assert.Equal(t, "token-old", cfg.Servers["prod"].TokenRef.Name)
	assert.Equal(t, persistRestoreBase, cfg.Servers["prod"].CA)
}

// TestPersistSuccessStatesAreSuccessForEveryBackend asserts the nil-error
// commit states are success for both callers: the exec fake helper
// (helperNonAtomic) and a keychain login (committed). None produces a
// rollback or a recovery component.
func TestPersistSuccessStatesAreSuccessForEveryBackend(t *testing.T) {
	t.Run("exec helperNonAtomic", func(t *testing.T) {
		warmFakeHelper(t)
		useFakeHelper(t, "ok")
		dir, _ := persistFixture(t, derivedEntry())
		in := pluginInput(dir, "prod", nil)
		in.OpenStore = func(string, string) (CredStore, string, error) { return OpenResolvedStore("exec:fake", dir) }
		out, err := PersistLoginCredential(t.Context(), dir, in)
		require.NoError(t, err)
		assert.Equal(t, clierr.CredentialHelperNonAtomic, out.CredentialState)
		assert.Equal(t, clierr.ConfigWritten, out.ConfigState)
		assert.Nil(t, out.ConfigRecovery)
	})
	t.Run("keyring committed", func(t *testing.T) {
		keyring.MockInit()
		dir, path := persistFixture(t, derivedEntry())
		cfg, _ := Load(path)
		cfg.CredentialStore = "keychain" // explicit: no auto probe, no repin
		require.NoError(t, cfg.Write())
		withFakeKeyring(t, &fakeKeyring{items: map[string]string{}})
		in := loginInput(dir, nil)
		in.OpenStore = func(string, string) (CredStore, string, error) { return newPinnedKeyringStore(dir, false), "", nil }
		out, err := PersistLoginCredential(t.Context(), dir, in)
		require.NoError(t, err)
		assert.Equal(t, clierr.CredentialCommitted, out.CredentialState)
		assert.Nil(t, out.ConfigRecovery)
	})
}

// TestPersistRotatedLoginTrustFailureRestoresTheOldIdentity asserts the
// compensation for a rotated entry, driven through
// the login rather than by calling the two halves by hand: the trust write
// fails after the commit, the account the rotation created is deleted, the
// record goes back to the identity and fingerprint the entry had, and the
// account the other config directory now owns is left exactly as it was.
func TestPersistRotatedLoginTrustFailureRestoresTheOldIdentity(t *testing.T) {
	keyring.MockInit()
	dir, path := persistFixture(t, derivedEntry())
	cfg, _ := Load(path)
	cfg.CredentialStore = "keychain"
	require.NoError(t, cfg.Write())

	// This directory's record, and a slot another copy has written since:
	// the login diverges and has to rotate.
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	before := identityRecord{
		Version:     identityVersion,
		Entry:       "prod",
		State:       identityActive,
		ID:          id,
		Fingerprint: credentialFingerprint(id, "ours"),
	}
	require.NoError(t, writeIdentityRecord(dir, before))
	shared := keychainAccount(id, "prod")
	fk := &fakeKeyring{items: map[string]string{shared: "the-other-copys-token"}}
	withFakeKeyring(t, fk)

	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "trust" {
			return errors.New("disk full")
		}
		return nil
	})
	t.Cleanup(restore)

	in := loginInput(dir, nil)
	in.PendingTrust = &PendingTrust{CA: persistRestoreBase, Fingerprint: "f1"}
	in.OpenStore = func(string, string) (CredStore, string, error) { return newPinnedKeyringStore(dir, false), "", nil }
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	assert.Equal(t, clierr.CredentialCompensated, out.CredentialState)
	assert.Empty(t, out.CompensationError)

	assert.Equal(t, map[string]string{shared: "the-other-copys-token"}, fk.items,
		"the rotated account is gone and the other copy's is untouched")
	assert.Equal(t, before, recordFor(t, dir, "prod"), "the entry is back on its old identity")
}

// TestPersistRotatedLoginCompensationFailureNamesBothAccounts asserts
// what a compensation that cannot finish has to say: the state is a
// failed compensation, and the message names the account the rotation
// created and the one the entry had, because once the record is gone
// neither is reachable from the other.
func TestPersistRotatedLoginCompensationFailureNamesBothAccounts(t *testing.T) {
	keyring.MockInit()
	dir, path := persistFixture(t, derivedEntry())
	cfg, _ := Load(path)
	cfg.CredentialStore = "keychain"
	require.NoError(t, cfg.Write())

	id, err := newKeychainAccountID()
	require.NoError(t, err)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version:     identityVersion,
		Entry:       "prod",
		State:       identityActive,
		ID:          id,
		Fingerprint: credentialFingerprint(id, "ours"),
	}))
	shared := keychainAccount(id, "prod")
	fk := &fakeKeyring{
		items:     map[string]string{shared: "the-other-copys-token"},
		deleteErr: errors.New("keychain refused the delete"),
	}
	withFakeKeyring(t, fk)

	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "trust" {
			return errors.New("disk full")
		}
		return nil
	})
	t.Cleanup(restore)

	in := loginInput(dir, nil)
	in.PendingTrust = &PendingTrust{CA: persistRestoreBase, Fingerprint: "f1"}
	in.OpenStore = func(string, string) (CredStore, string, error) { return newPinnedKeyringStore(dir, false), "", nil }
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	assert.Equal(t, clierr.CredentialCompensationFailed, out.CredentialState)

	rotated := recordFor(t, dir, "prod")
	require.NotEqual(t, id, rotated.ID, "the login rotated before the trust write failed")
	assert.Contains(t, out.CompensationError, rotated.Account(), "the account the rotation created")
	assert.Contains(t, out.CompensationError, shared, "and the one the entry had")
}

// TestPersistThroughALazyStoreWritesTheIdentityRecord asserts that a
// login handed a store that resolves its backend on first use still
// writes the entry's identity: the login resolves it to decide, which is
// the resolution the first credential call would have done a moment
// later, rather than refusing for want of a concrete store.
func TestPersistThroughALazyStoreWritesTheIdentityRecord(t *testing.T) {
	keyring.MockInit()
	dir, path := persistFixture(t, derivedEntry())
	cfg, _ := Load(path)
	cfg.CredentialStore = "keychain"
	require.NoError(t, cfg.Write())
	withFakeKeyring(t, &fakeKeyring{items: map[string]string{}})

	in := loginInput(dir, nil)
	in.OpenStore = func(policy, d string) (CredStore, string, error) {
		lazy, lerr := NewLazyStore(&Config{CredentialStore: policy}, d)
		return lazy, "", lerr
	}
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.Equal(t, clierr.CredentialCommitted, out.CredentialState)

	rec := recordFor(t, dir, "prod")
	assert.Equal(t, identityActive, rec.State)
	tok, gerr := newPinnedKeyringStore(dir, false).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.NoError(t, gerr)
	assert.Equal(t, in.Bearer, tok)
}

// TestPersistIdentityRevertFailureRollsTheEntryBack asserts the entry
// contract holds when the identity record cannot be put back either: the
// credential did not land, so the server entry returns to the snapshot it
// had rather than keeping the URL and certificate this failed login wrote.
func TestPersistIdentityRevertFailureRollsTheEntryBack(t *testing.T) {
	keyring.MockInit()
	snapshot := derivedEntry()
	dir, path := persistFixture(t, snapshot)
	cfg, _ := Load(path)
	cfg.CredentialStore = "keychain"
	require.NoError(t, cfg.Write())

	// An entry this directory already has a credential for, so the failed
	// login has a record to revert to.
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version:     identityVersion,
		Entry:       "prod",
		State:       identityActive,
		ID:          id,
		Fingerprint: credentialFingerprint(id, "old"),
	}))
	account := keychainAccount(id, "prod")
	withFakeKeyring(t, &fakeKeyring{items: map[string]string{account: "old"}, setErr: errors.New("dbus: reply lost")})

	// The pending record is written; putting it back afterwards is not.
	writes := 0
	restore := SetIdentityWriteHookForTest(func() error {
		writes++
		if writes >= 2 {
			return errors.New("read-only file system")
		}
		return nil
	})
	t.Cleanup(restore)

	in := loginInput(dir, nil)
	in.OpenStore = func(string, string) (CredStore, string, error) { return newPinnedKeyringStore(dir, false), "", nil }
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorContains(t, err, "recording the credential identity")
	assert.Equal(t, CommitNotWritten, out.CredentialState)

	after, lerr := Load(path)
	require.NoError(t, lerr)
	got := after.Servers["prod"]
	assert.NotEqual(t, snapshot.CacheBindingID, got.CacheBindingID, "all but the id, which never moves back")
	assert.False(t, got.Paused())
	want := *snapshot
	want.CacheBindingID = got.CacheBindingID
	assert.Equal(t, want, got, "a login that stored nothing leaves the entry as it found it")
	assert.Equal(t, "token-old", after.Servers["prod"].TokenRef.Name)
}

// TestPersistKeychainUnreadablePriorRefusesTheLogin asserts that a
// keychain the login cannot read is a refusal before anything is written,
// not a write whose prior is unknown: the account a login stores under and
// the state it would compensate back to are decided together, so one that
// cannot be read has to stop the login.
func TestPersistKeychainUnreadablePriorRefusesTheLogin(t *testing.T) {
	keyring.MockInit()
	dir, path := persistFixture(t, derivedEntry())
	cfg, _ := Load(path)
	cfg.CredentialStore = "keychain"
	require.NoError(t, cfg.Write())

	// An active record, so the login has an account to read, and a keychain
	// that answers neither a value nor a clean miss for it.
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version:     identityVersion,
		Entry:       "prod",
		State:       identityActive,
		ID:          id,
		Fingerprint: credentialFingerprint(id, "old"),
	}))
	fk := &fakeKeyring{items: map[string]string{}, reads: []keyringRead{{err: errors.New("locked")}}}
	withFakeKeyring(t, fk)

	in := loginInput(dir, nil)
	in.OpenStore = func(string, string) (CredStore, string, error) { return newPinnedKeyringStore(dir, false), "", nil }
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorContains(t, err, "locked")
	assert.Equal(t, clierr.CredentialNotWritten, out.CredentialState)
	assert.Empty(t, fk.items, "nothing is written to a keychain whose state could not be read")

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	require.NotNil(t, rec)
	assert.Equal(t, identityActive, rec.State, "the refused login leaves the record as it was")
	assert.Equal(t, id, rec.ID)
}

// TestPersistNoNetworkAndLockHeld asserts the primitive runs entirely
// under the config lock (the lock hook fires once,
// before any write) and takes no context-carrying network dependency.
func TestPersistNoNetworkAndLockHeld(t *testing.T) {
	dir, _ := persistFixture(t, derivedEntry())
	held := 0
	restore := SetLockHookForTest(func() { held++ })
	t.Cleanup(restore)
	_, err := PersistLoginCredential(t.Context(), dir, loginInput(dir, NewFileStore(dir)))
	require.NoError(t, err)
	assert.Equal(t, 1, held)
}

// TestUpdateServerEntryWritesOnlyWhenChanged asserts the record-write
// helper: it reloads under the lock, writes only when fn reports a change,
// and fails NOT_FOUND for an unknown entry.
func TestUpdateServerEntryWritesOnlyWhenChanged(t *testing.T) {
	dir, path := persistFixture(t, derivedEntry())
	writes := 0
	restore := SetPersistWriteHookForTest(func(string) error { writes++; return nil })
	t.Cleanup(restore)
	require.NoError(t, UpdateServerEntry(t.Context(), dir, "prod", func(*Server) bool { return false }))
	assert.Equal(t, 0, writes)
	require.NoError(t, UpdateServerEntry(t.Context(), dir, "prod", func(s *Server) bool { s.LoggedOutAt = "now"; return true }))
	assert.Equal(t, 1, writes)
	cfg, _ := Load(path)
	assert.Equal(t, "now", cfg.Servers["prod"].LoggedOutAt)
	err := UpdateServerEntry(t.Context(), dir, "nope", func(*Server) bool { return true })
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

// declaredFixture is a directory whose config.yaml declares declared, or
// nothing when it is empty - the state a first login records a store for.
func declaredFixture(t *testing.T, declared string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvCredentialStore, "")
	cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}}, CurrentServer: "prod", CredentialStore: declared}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

// declarationInput is a login that resolved the file policy for itself.
func declarationInput(dir, declared string, store CredStore) PersistLoginInput {
	return PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:secret",
		Provenance:          clierr.ProvenanceDerived,
		Policy:              "file",
		ObservedDeclaration: declared,
		Declaration:         DeclarationFor(declared, "", "file"),
		OpenStore:           fileOpen(store),
	}
}

// TestPersistDeclaresTheStoreOnlyAfterTheCommit asserts that a directory
// which has declared no credential store records the one its first login
// used, and that the record lands only after the credential is committed.
// The entry write that precedes the commit must leave the declaration
// alone: readers that take no lock read config.yaml in that window, and a
// declaration written there would send them to a store that does not hold
// the new credential yet.
func TestPersistDeclaresTheStoreOnlyAfterTheCommit(t *testing.T) {
	dir := declaredFixture(t, "")
	in := declarationInput(dir, "", NewFileStore(dir))

	var atDeclaration *Config
	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "declaration" && atDeclaration == nil {
			onDisk, err := Load(FilePath(dir))
			require.NoError(t, err)
			atDeclaration = onDisk
		}
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.Equal(t, clierr.CredentialCommitted, out.CredentialState)

	require.NotNil(t, atDeclaration, "with no trust write the declaration is a write of its own")
	assert.Empty(t, atDeclaration.CredentialStore, "the pre-commit entry write leaves the declaration alone")
	assert.Equal(t, "id:secret", storedFileToken(t, dir), "the credential is committed before the declaration is written")

	after, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "file", after.CredentialStore)
}

// TestPersistLeavesAnExistingDeclarationAlone asserts a login that
// changes no store writes no declaration: the directory already says
// where its credentials live.
func TestPersistLeavesAnExistingDeclarationAlone(t *testing.T) {
	dir := declaredFixture(t, "file")
	in := declarationInput(dir, "file", NewFileStore(dir))
	require.Empty(t, in.Declaration)

	var wrote bool
	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "declaration" {
			wrote = true
		}
		return nil
	})
	defer restore()

	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.False(t, wrote, "no declaration write when the declaration does not change")

	after, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "file", after.CredentialStore)
}

// TestPersistOpensTheStoreTheLoginResolved asserts the policy reads under
// the lock take the login's own value, not the loaded declaration: a
// login that resolved one store before asking anyone to authenticate must
// commit into that store.
func TestPersistOpensTheStoreTheLoginResolved(t *testing.T) {
	dir := declaredFixture(t, "")
	var opened string
	in := declarationInput(dir, "", nil)
	in.OpenStore = func(policy, d string) (CredStore, string, error) {
		opened = policy
		return NewFileStore(d), "", nil
	}

	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.Equal(t, "file", opened, "the login's own policy, not the empty declaration")
}

// TestPersistDeclarationWriteFailureIsNamed asserts the outcome when the
// credential committed and the record of where it lives did not: the
// state says so, nothing is rolled back, and the message names the entry
// and the store. Rolling back here would delete a credential that is
// already the directory's own.
func TestPersistDeclarationWriteFailureIsNamed(t *testing.T) {
	dir := declaredFixture(t, "")
	in := declarationInput(dir, "", NewFileStore(dir))

	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "declaration" {
			return errors.New("disk full")
		}
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	require.ErrorContains(t, err, "disk full")
	require.ErrorContains(t, err, `"file"`)
	assert.Equal(t, clierr.ConfigDeclarationNotWritten, out.ConfigState)
	assert.Equal(t, clierr.CredentialCommitted, out.CredentialState)
	assert.Equal(t, "id:secret", storedFileToken(t, dir), "the credential stays: it is the directory's own")

	after, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, after.CredentialStore, "the directory still declares what it declared")
}

// TestPersistRollbackRestoresTheDeclaration asserts a rollback undoes the
// attempt entirely: a login that got as far as setting a new declaration
// in memory and then failed leaves the directory naming the store it
// named before.
func TestPersistRollbackRestoresTheDeclaration(t *testing.T) {
	pemText, fingerprint := testCertificateAndFingerprint(t)
	dir := declaredFixture(t, "")
	in := declarationInput(dir, "", NewFileStore(dir))
	in.PendingTrust = &PendingTrust{CA: pemText, Fingerprint: fingerprint}

	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "trust" {
			return errors.New("disk full")
		}
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	assert.Equal(t, clierr.ConfigNotWritten, out.ConfigState)

	after, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, after.CredentialStore, "the rollback write carries the old declaration")
}

// TestDeclarationFor asserts what a login records: the store it was asked
// for when it carries one, nothing when the directory already declares a
// store and the login changes none, and otherwise the policy it used.
func TestDeclarationFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                        string
		declared, requested, policy string
		want                        string
	}{
		{name: "first login on a fresh directory", policy: "auto", want: "auto"},
		{name: "first login under a variable", policy: "file", want: "file"},
		{name: "ordinary re-login", declared: "file", policy: "file"},
		{name: "a login that asks for a store", declared: "file", requested: "keychain", policy: "keychain", want: "keychain"},
		{name: "a login that asks for the one already declared", declared: "file", requested: "file", policy: "file", want: "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, DeclarationFor(tc.declared, tc.requested, tc.policy))
		})
	}
}

// storedFileToken reads the file store directly, so a test can tell a
// committed credential from a reported one.
func storedFileToken(t *testing.T, dir string) string {
	t.Helper()
	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	if errors.Is(err, ErrNoToken) {
		return ""
	}
	require.NoError(t, err)
	return tok
}

// TestAnUndisclosedPlaintextCommitIsAbortedOnABareDirectory asserts the
// plugin's plaintext promise holds without a read-path pin. A fresh `auto`
// directory whose keychain cannot be used commits to the file store; a
// decision that disclosed nothing before the sign-in is aborted, with
// nothing written, and one that disclosed the file goes through.
func TestAnUndisclosedPlaintextCommitIsAbortedOnABareDirectory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		disclosed bool
	}{
		{name: "nothing disclosed"},
		{name: "the file disclosed", disclosed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Setenv(EnvCredentialStore, "")
			dir := t.TempDir()
			old := keyringUsable
			keyringUsable = func() bool { return false }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = old; resetKeyringUsableCache() })
			cfg := &Config{Servers: map[string]Server{}, TokenReuse: true}
			cfg.SetPath(FilePath(dir))
			require.NoError(t, cfg.Write())

			in := pluginInput(dir, "exec-login-r.ex", nil)
			in.OpenStore = OpenResolvedStore
			in.Policy, in.ObservedDeclaration = "auto", ""
			in.Declaration = DeclarationFor("", "", "auto")
			in.Decision = &StoreDecision{RequireSetting: true, Policy: "auto"}
			if tc.disclosed {
				in.Decision.DisclosedPlaintextPath = CredentialsFilePath(dir)
			}
			out, err := PersistLoginCredential(t.Context(), dir, in)
			if !tc.disclosed {
				require.ErrorIs(t, err, ErrPersistAborted)
				require.ErrorContains(t, err, "did not disclose before the interactive sign-in")
				assert.Equal(t, clierr.ConfigNotWritten, out.ConfigState)
				assert.NoFileExists(t, CredentialsFilePath(dir), "nothing reached the plaintext file")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, backendFile, ReadPin(dir), "the login's own pin step wrote it")
		})
	}
}

// TestALoginPassesATypedStoreErrorThroughRatherThanAborting asserts what
// becomes of a refusal that turns up under the lock, after the sign-in: it
// keeps its own code and remedy. Only the undisclosed-plaintext comparison
// is an abort; credentials found in both stores are the both-stores error.
func TestALoginPassesATypedStoreErrorThroughRatherThanAborting(t *testing.T) {
	keyring.MockInit()
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir()
	cfg := &Config{Servers: map[string]Server{"staging": {URL: "https://s.ex"}, "qa": {URL: "https://q.ex"}}, TokenReuse: true}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))
	seedKeychainCredential(t, dir, "qa", "qa-tok")

	in := pluginInput(dir, "exec-login-r.ex", nil)
	in.OpenStore = OpenResolvedStore
	in.Policy, in.ObservedDeclaration = "auto", ""
	in.Declaration = DeclarationFor("", "", "auto")
	in.Decision = &StoreDecision{RequireSetting: true, Policy: "auto"}
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPersistAborted, "not folded into the abort")
	assert.Empty(t, out.Aborted)
	assert.True(t, IsCredentialsInBothStores(err))
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}
