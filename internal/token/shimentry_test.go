package token

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
)

// TestShimNeverIdentifiesAnEntryByItsSlotName asserts the legacy token
// command's cache-slot label identifies no entry, even where an entry has the
// label for its name: the credential lookup for a call that recorded no entry
// finds the same none and never refuses with a change of config.yaml.
func TestShimNeverIdentifiesAnEntryByItsSlotName(t *testing.T) {
	keyring.MockInit()
	slot := config.LegacyCachePrefix + "alice"
	cfg := &config.Config{Servers: map[string]config.Server{slot: {URL: "https://r.ex"}}}
	got := lookupStoredLogin(cfg, config.NewFileStore(t.TempDir()), "https://r.ex", slot, "", true,
		entryChoice{known: true, name: ""}, userCheck{})
	assert.Empty(t, got.Identified)
	assert.False(t, got.identifiedErr)
	if got.Err != nil {
		assert.NotContains(t, got.Err.Error(), "config.yaml changed")
	}
}

// TestShimWithAnEntryNamedLikeItsSlotRuns asserts a legacy token call whose
// slot label is the name of an entry at the server runs and is served from
// that entry's login, with no refusal about a change of config.yaml.
func TestShimWithAnEntryNamedLikeItsSlotRuns(t *testing.T) {
	r := newRecordFixture(t, false)
	r.login(config.LegacyCachePrefix+"alice", "", "alice", false)
	owner, err := r.shimCall("alice", false)
	require.NoError(t, err)
	assert.Equal(t, "alice", owner)
}
