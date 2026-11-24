package config

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// execLoginEntry is a plugin login entry for the fixture's server, with the
// CA ca and the record marked.
func execLoginEntry(ca string, marked bool) *Server {
	return &Server{URL: "https://r.ex", CA: ca, Provenance: clierr.ProvenanceExecLogin,
		TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}, TokenUserID: "user-a",
		SignedInWithSkipVerify: marked}
}

// TestServerRecordsASkipVerifySignInInConfigYAML asserts the field is
// written as signedInWithSkipVerify, only when set, and read back.
func TestServerRecordsASkipVerifySignInInConfigYAML(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{CurrentServer: "a", Servers: map[string]Server{
		"a": {URL: "https://a.ex", Provenance: clierr.ProvenanceExecLogin, SignedInWithSkipVerify: true},
		"b": {URL: "https://b.ex", Provenance: clierr.ProvenanceExecLogin},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())

	raw, err := os.ReadFile(FilePath(dir))
	require.NoError(t, err)
	var doc struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	assert.Equal(t, true, doc.Servers["a"]["signedInWithSkipVerify"])
	assert.NotContains(t, doc.Servers["b"], "signedInWithSkipVerify")

	back, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.True(t, back.Servers["a"].SignedInWithSkipVerify)
	assert.False(t, back.Servers["b"].SignedInWithSkipVerify)
}

// TestEveryLoginWriteSetsOrClearsTheSkipVerifyRecord asserts the login
// write: a create or an update records a sign-in that skipped verification
// when the written entry saves no CA, an update that does not pass it
// clears the record, and an entry that saves a CA never carries it.
func TestEveryLoginWriteSetsOrClearsTheSkipVerifyRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entry      *Server
		ca         string
		skipVerify bool
		wantMarked bool
		wantCA     string
	}{
		{"a create with no CA, skipped verification", nil, "", true, true, ""},
		{"a create with no CA, verified", nil, "", false, false, ""},
		{"a create with a CA, skipped verification", nil, persistRestoreBase, true, false, persistRestoreBase},
		{"an update of an entry with no CA, skipped verification", execLoginEntry("", false), "", true, true, ""},
		{"an update of a marked entry, verified", execLoginEntry("", true), "", false, false, ""},
		{"an update of a marked entry, skipped verification again", execLoginEntry("", true), "", true, true, ""},
		{"an update of an entry with a CA, skipped verification", execLoginEntry(persistRestoreBase, false), "", true, false, persistRestoreBase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := persistFixture(t, tc.entry)
			store := NewFileStore(dir)
			if tc.entry != nil {
				require.NoError(t, store.SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "ext/token-old:s"))
			}
			in := pluginInput(dir, "prod", store)
			in.CA = tc.ca
			in.SignedInWithSkipVerify = tc.skipVerify
			_, err := PersistLoginCredential(t.Context(), dir, in)
			require.NoError(t, err)
			cfg, err := Load(path)
			require.NoError(t, err)
			got := cfg.Servers["prod"]
			assert.Equal(t, tc.wantMarked, got.SignedInWithSkipVerify)
			assert.Equal(t, tc.wantCA, got.CA)
		})
	}
}

// TestARolledBackLoginKeepsTheEntrysSkipVerifyRecord asserts a login whose
// credential commit fails puts the entry back with the record it had, set
// or not, whatever the failed login said about its sign-in.
func TestARolledBackLoginKeepsTheEntrysSkipVerifyRecord(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked", false: "not marked"}[marked], func(t *testing.T) {
			dir, path := persistFixture(t, execLoginEntry("", marked))
			st := failStore{CredStore: NewFileStore(dir), out: CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}, err: errors.New("refused")}
			in := pluginInput(dir, "prod", st)
			in.CA = ""
			in.SignedInWithSkipVerify = !marked
			_, err := PersistLoginCredential(t.Context(), dir, in)
			require.ErrorContains(t, err, "refused")
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, marked, cfg.Servers["prod"].SignedInWithSkipVerify)
			assert.Equal(t, marked, restoredEntry(execLoginEntry("", marked), nil, true).SignedInWithSkipVerify)
		})
	}
}
