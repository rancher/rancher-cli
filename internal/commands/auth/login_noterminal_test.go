package auth

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestLoginProviderFlowWithoutATerminalRefusesInItsOwnWords asserts auth
// login names no remedy of the exec plugin's when its provider flow cannot
// ask: with the provider named it is the sign-in prompt's refusal as it is,
// an AUTH error with no candidates and no login-failure details, whether
// the cause is no terminal (a username and password provider) or
// --no-input (a SAML or OAuth provider).
func TestLoginProviderFlowWithoutATerminalRefusesInItsOwnWords(t *testing.T) {
	t.Parallel()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"},{"type":"azureADProvider"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fake.Close)

	for _, tc := range []struct {
		name     string
		provider string
		noInput  bool
		want     string
	}{
		{"a username and password provider, no terminal", "localProvider", false,
			"interactive login required for localProvider; run in a terminal, or use a cached --name"},
		{"an OAuth provider under --no-input", "azureADProvider", true,
			"interactive login required for azureADProvider; run without --no-input, or use a cached --name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", fake.URL, "", false)
			in := cmdtest.Input{Str: map[string]string{"auth-provider": tc.provider}, Pos: []string{"prod"}}
			p := ui.New(&bytes.Buffer{}, strings.NewReader(""), tc.noInput, false)
			_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.Empty(t, ce.Candidates)
			assert.Nil(t, ce.Details, "nothing was signed in: no login-failure details")
			assert.Empty(t, ce.Hint)
		})
	}
}
