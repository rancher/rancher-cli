package token

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestACallThatLeavesAnEntryWithAnAtSignAfterTheHostOutStartsNoSignIn asserts
// that when the server a call names is only the host read from the URL of an
// entry that holds an @ after the host, the call is refused with that entry's
// refusal and repair, with a terminal and without one: no request reaches the
// server and no login step names the host. It runs the four lookups: the entry
// --name identifies, the entries that record --user-id, the ranked entries of
// the URL, and the plugin login entry that follows an identified entry with
// no login (token reuse on). When a usable entry is registered at that server
// the call goes on as for any entry with no stored login: it signs in at that
// entry's URL, and the raw-at entry's stored login is neither read nor sent.
// Not parallel: it sets the process environment.
func TestACallThatLeavesAnEntryWithAnAtSignAfterTheHostOutStartsNoSignIn(t *testing.T) {
	isolateSignIn(t)
	const storedLogin = "ext/token-abc12:secret"
	for _, tc := range []struct {
		name string
		// input is the call: --name, --user-id.
		input map[string]string
		// plugin makes the raw-at entry a plugin login entry (token reuse on).
		plugin bool
		// usable registers a usable entry y, with no stored login, at the server.
		usable bool
		// signsIn is set when the call goes on to the sign-in of y.
		signsIn bool
	}{
		{"the identified entry", map[string]string{"name": "x"}, false, false, false},
		{"the entries that record the user", map[string]string{"user-id": "u-abc12"}, false, false, false},
		{"the ranked entries of the URL", map[string]string{}, false, false, false},
		{"the plugin login entry after an identified entry with no login", map[string]string{"name": "y"}, true, true, true},
		{"the same with a user", map[string]string{"name": "y", "user-id": "u-abc12"}, true, true, true},
		{"an identified entry with no login, no token reuse", map[string]string{"name": "y"}, false, true, true},
		{"the ranked entries when a usable entry has no login", map[string]string{}, false, true, true},
	} {
		for _, terminal := range []bool{false, true} {
			name := tc.name + ", no terminal"
			var p runtime.Prompter = ui.Discard()
			if terminal {
				name = tc.name + ", a terminal"
				p = terminalLines("alice", "pw")
			}
			t.Run(name, func(t *testing.T) {
				var mu sync.Mutex
				var requests []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
					mu.Unlock()
					w.WriteHeader(http.StatusNotFound)
				}))
				t.Cleanup(srv.Close)
				dir := t.TempDir()
				cfg := &config.Config{CredentialStore: "file", TokenReuse: tc.plugin, Servers: map[string]config.Server{}}
				raw := srv.URL + "/pq@x"
				rawName := "x"
				prov := clierr.ProvenanceDerived
				if tc.plugin {
					rawName, prov = "exec-login-x", clierr.ProvenanceExecLogin
				}
				if tc.usable {
					cfg.Servers["y"] = config.Server{URL: srv.URL, Provenance: clierr.ProvenanceDerived, TokenUserID: "u-abc12"}
				}
				ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
				cfg.Servers[rawName] = config.Server{URL: raw, Provenance: prov, TokenUserID: "u-abc12", TokenRef: &ref}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
				store := config.NewFileStore(dir)
				require.NoError(t, store.SetToken(config.ServerRef{Name: rawName, URL: raw}, storedLogin))

				in := map[string]string{"server": srv.URL, "cluster": "c-2"}
				maps.Copy(in, tc.input)
				cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
				_, err := cmd.Execute(t.Context(), tin{s: in}, p)
				require.Error(t, err)
				mu.Lock()
				defer mu.Unlock()
				for _, r := range requests {
					assert.NotContains(t, r, storedLogin, "the stored login of the entry is not sent")
				}
				ce, ok := err.(*clierr.Error)
				if tc.signsIn {
					assert.NotNil(t, requests, "the call goes on to the sign-in of the usable entry")
					if ok {
						assert.NotEqual(t, clierr.CodeConflict, ce.Code, "%v", err)
					}
					return
				}
				assert.Empty(t, requests, "no request reaches the server")
				require.True(t, ok, "%v", err)
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				assert.Contains(t, ce.Message, "the server URL in server entry '"+rawName+"' cannot be used: it holds an @ after the host")
				assert.NotContains(t, ce.Message, "--url '"+srv.URL)
			})
		}
	}
}
