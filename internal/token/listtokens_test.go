package token

import (
	"context"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListTokens(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred("prod", "t1", time.Now().Add(time.Hour)))
	require.NoError(t, store.SetCachedCred("dev", "t2", time.Now().Add(time.Hour)))

	res, err := NewListTokensCommand(dir, store).(runtime.ReadCommand).
		Execute(context.Background(), tin{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "TokenList", res.Kind)
	list := res.Data.([]config.CachedCredInfo)
	assert.Len(t, list, 2)
}
