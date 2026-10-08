package token

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListTokens(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, seedCache(t, dir, store, config.CacheKey("prod", "r.ex", "c-1"), "t1", time.Now().Add(time.Hour), "prod", "r.ex", "c-1"))
	require.NoError(t, seedCache(t, dir, store, config.CacheKey("dev", "r.ex", "c-2"), "t2", time.Now().Add(time.Hour), "dev", "r.ex", "c-2"))

	res, err := NewListTokensCommand(dir, store).(runtime.ReadCommand).
		Execute(t.Context(), tin{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "TokenList", res.Kind)
	list := res.Data.(credList)
	assert.Len(t, list, 2)
}

// TestListTokensShowsTheTokenEachRecordNames asserts the TOKEN column: the
// record's ref as kind/name, or "-" for a record that names none; the JSON
// payload carries the ref and the owner only when present.
func TestListTokensShowsTheTokenEachRecordNames(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc"}
	b := testBinding(t, dir, "prod", "https://r.ex", "c-1").WithCreatedToken(ref, "user-alice")
	require.NoError(t, store.SetCachedCred("prod@https://r.ex/c-1", "ext/token-abc:s", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1", b))
	require.NoError(t, seedCache(t, dir, store, "dev@https://r.ex/c-2", "t2", time.Now().Add(time.Hour), "dev", "https://r.ex", "c-2"))

	res, err := NewListTokensCommand(dir, store).(runtime.ReadCommand).Execute(t.Context(), tin{}, ui.Discard())
	require.NoError(t, err)
	list := res.Data.(credList)
	assert.Equal(t, []string{"NAME", "SERVER", "CLUSTER", "TOKEN", "EXPIRY"}, list.Columns())
	rows := list.Rows()
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"dev", "https://r.ex", "c-2", "-"}, rows[0][:4])
	assert.Equal(t, []string{"prod", "https://r.ex", "c-1", "ext/token-abc"}, rows[1][:4])

	raw, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded, 2)
	assert.NotContains(t, decoded[0], "tokenRef")
	assert.NotContains(t, decoded[0], "tokenUserId")
	assert.Equal(t, map[string]any{"kind": "ext", "name": "token-abc"}, decoded[1]["tokenRef"])
	assert.Equal(t, "user-alice", decoded[1]["tokenUserId"])
}
