package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// noCAEntry is a derived-login entry with no saved CA.
func noCAEntry(marked bool) *Server {
	e := derivedEntry()
	e.CA, e.CAFingerprint, e.CAFromEnvironment = "", "", marked
	return e
}

// TestPersistLoginRecordsWhetherTheLoginConnectedWithTheVariable asserts the
// known-entry login write: an entry that keeps no CA records that the login
// connected with RANCHER_CLI_CACERT, and a login that connected without it
// clears the record. An entry that has a CA never carries the record, and
// the entry's CA is never changed.
func TestPersistLoginRecordsWhetherTheLoginConnectedWithTheVariable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entry      *Server
		viaEnv     bool
		wantMarked bool
		wantCA     string
	}{
		{"no CA, connected with the variable", noCAEntry(false), true, true, ""},
		{"no CA, connected without it", noCAEntry(true), false, false, ""},
		{"no CA, already marked, connected with it again", noCAEntry(true), true, true, ""},
		{"a saved CA, connected with the variable", derivedEntry(), true, false, persistRestoreBase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := persistFixture(t, tc.entry)
			store := NewFileStore(dir)
			require.NoError(t, store.SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "ext/token-old:s"))
			in := loginInput(dir, store)
			in.CAFromEnvironment = tc.viaEnv
			_, err := PersistLoginCredential(t.Context(), dir, in)
			require.NoError(t, err)
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantMarked, cfg.Servers["prod"].CAFromEnvironment)
			assert.Equal(t, tc.wantCA, cfg.Servers["prod"].CA)
		})
	}
}

// TestAcceptedTrustClearsTheVariableRecord asserts an accepted trust prompt,
// which gives the entry a CA, clears the record that the entry has none
// because the login used the variable; so does restoring a snapshot that
// takes a pending trust, and a restore without one keeps the record.
func TestAcceptedTrustClearsTheVariableRecord(t *testing.T) {
	dir, path := persistFixture(t, noCAEntry(true))
	store := NewFileStore(dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "ext/token-old:s"))
	in := loginInput(dir, store)
	in.PendingTrust = &PendingTrust{CA: "-----BEGIN CERTIFICATE-----\nNEW\n-----END CERTIFICATE-----\n", Fingerprint: "f1"}
	// The primitive clears the record itself, whatever the login says about
	// the variable.
	in.CAFromEnvironment = true
	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	cfg, err := Load(path)
	require.NoError(t, err)
	got := cfg.Servers["prod"]
	assert.Equal(t, in.PendingTrust.CA, got.CA)
	assert.False(t, got.CAFromEnvironment, "the entry has a CA now")

	snap := noCAEntry(true)
	assert.False(t, restoredEntry(snap, &PendingTrust{CA: "new-ca", Fingerprint: "f1"}, false).CAFromEnvironment)
	assert.True(t, restoredEntry(snap, nil, false).CAFromEnvironment, "a restore without a trust is the entry as it was")
	assert.True(t, snap.CAFromEnvironment, "the snapshot itself is untouched")
}

// TestServerRecordsTheVariableInConfigYAML asserts the field is written as
// caFromEnvironment, only when set, and read back.
func TestServerRecordsTheVariableInConfigYAML(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{CurrentServer: "a", Servers: map[string]Server{
		"a": {URL: "https://a.ex", CAFromEnvironment: true},
		"b": {URL: "https://b.ex"},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())

	raw, err := os.ReadFile(FilePath(dir))
	require.NoError(t, err)
	var doc struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	assert.Equal(t, true, doc.Servers["a"]["caFromEnvironment"])
	assert.NotContains(t, doc.Servers["b"], "caFromEnvironment")

	back, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.True(t, back.Servers["a"].CAFromEnvironment)
	assert.False(t, back.Servers["b"].CAFromEnvironment)
}

// TestAFailedRollbackOfAMarkedEntryReportsTheRecord asserts the recovery
// component of a login whose rollback write failed carries, for an entry
// that has no CA because a login connected with RANCHER_CLI_CACERT, the
// record of that fact beside the CA, in the JSON of the failure and in its
// text, and omits it for an entry without the record.
func TestAFailedRollbackOfAMarkedEntryReportsTheRecord(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked", false: "not marked"}[marked], func(t *testing.T) {
			dir, _ := persistFixture(t, noCAEntry(marked))
			restore := SetPersistWriteHookForTest(func(step string) error {
				if step == "rollback" {
					return errors.New("read-only filesystem")
				}
				return nil
			})
			st := failStore{CredStore: NewFileStore(dir), out: CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}, err: errors.New("refused")}
			out, err := PersistLoginCredential(t.Context(), dir, loginInput(dir, st))
			restore()
			require.ErrorContains(t, err, "refused")
			require.NotNil(t, out.ConfigRecovery)
			require.NotNil(t, out.ConfigRecovery.Prior)
			assert.Equal(t, marked, out.ConfigRecovery.Prior.CAFromEnvironment)

			details := clierr.LoginFailureDetails{
				Primary: clierr.PrimaryFailure{Code: clierr.CodeGeneric, Message: "refused"}, Stage: clierr.StageCommit,
				ConfigState: out.ConfigState, ConfigRecovery: out.ConfigRecovery,
			}
			raw, err := json.Marshal(details)
			require.NoError(t, err)
			var env struct {
				ConfigRecovery struct {
					Prior map[string]any `json:"prior"`
				} `json:"configRecovery"`
			}
			require.NoError(t, json.Unmarshal(raw, &env))
			if marked {
				assert.Equal(t, true, env.ConfigRecovery.Prior["caFromEnvironment"])
				assert.Contains(t, strings.Join(details.TextDetails(), "\n"), "last login connected with RANCHER_CLI_CACERT")
				return
			}
			assert.NotContains(t, env.ConfigRecovery.Prior, "caFromEnvironment")
			assert.NotContains(t, strings.Join(details.TextDetails(), "\n"), "RANCHER_CLI_CACERT")
		})
	}
}
