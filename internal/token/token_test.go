package token

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tin struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (t tin) String(k string) string { return t.s[k] }
func (t tin) Bool(k string) bool     { return t.b[k] }
func (t tin) Args() []string         { return t.a }

func TestExecCredentialJSONShape(t *testing.T) {
	cred := newExecCredential("kubeconfig-u-abc:xyz", time.Time{})
	b, err := json.Marshal(cred)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "ExecCredential", got["kind"])
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", got["apiVersion"])
	status := got["status"].(map[string]any)
	assert.Equal(t, "kubeconfig-u-abc:xyz", status["token"])
}

func TestDeleteTokenCommand(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cmd := NewDeleteTokenCommand(dir, store)

	t.Run("no name and no --all is validation error", func(t *testing.T) {
		_, err := cmd.Execute(context.Background(), tin{}, safety.Effect{})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})

	t.Run("--all flushes the cache", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour)))
		_, err := cmd.Execute(context.Background(), tin{b: map[string]bool{"all": true}}, safety.Effect{})
		require.NoError(t, err)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("named delete uses the full compound key", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour)))
		_, err := cmd.Execute(context.Background(), tin{a: []string{"acme/u/c"}}, safety.Effect{})
		require.NoError(t, err)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})
}

func TestBasicAuthRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := &loginInput{
		server:       "https://test.example.com",
		authProvider: "localProvider",
	}

	_, err := basicAuth(ctx, nil, input, true)
	assert.Error(t, err, "basicAuth should fail when given a cancelled context")
}

func TestExecCredentialOmitsEmptyStatusToken(t *testing.T) {
	// Embedding (not a json:",inline" tag) is what promotes kind/apiVersion.
	cred := newExecCredential("", time.Time{})
	b, err := json.Marshal(cred)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "ExecCredential", got["kind"])
	status := got["status"].(map[string]any)
	_, hasTok := status["token"]
	assert.False(t, hasTok, "empty token must be omitted, not emitted as empty string")
}
