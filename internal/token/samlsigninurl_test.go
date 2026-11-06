package token

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher"
)

// TestEverySAMLSignInPathPrintsAURLTheDashboardSignInPageAccepts asserts that
// every sign-in path that starts a browser sign-in prints a sign-in URL the
// dashboard sign-in page accepts, whatever responseType the path sends to
// Rancher's login endpoint. The code that builds the sign-in URL ignores that
// value, so no sign-in path can change it. Not parallel: the rows shrink the
// package's SAML timing.
func TestEverySAMLSignInPathPrintsAURLTheDashboardSignInPageAccepts(t *testing.T) {
	restore := SetSAMLTimingForTest(time.Minute, time.Millisecond)
	t.Cleanup(restore)
	samlOpts := testrancher.Options{EnableSAMLAuthTokens: true, ExtraProviders: []string{"oktaProvider"}}
	for _, tc := range []struct {
		name  string
		reuse bool
		run   func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error
	}{
		{"loginAndGenerateCred with responseType json", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			_, err := loginAndGenerateCred(ctx, x.client(), samlInput(x.url), p)
			return err
		}},
		{"loginAndGenerateCred with no responseType", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			input := samlInput(x.url)
			input.responseType = ""
			_, err := loginAndGenerateCred(ctx, x.client(), input, p)
			return err
		}},
		{"AcquireLoginCredential with responseType json", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			_, err := AcquireLoginCredential(ctx, x.client(), LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "oktaProvider", ResponseType: "json"}, 0, p)
			return err
		}},
		{"AcquireLoginCredential with no responseType", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			_, err := AcquireLoginCredential(ctx, x.client(), LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "oktaProvider"}, 0, p)
			return err
		}},
		{"signInForClusterToken", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			input := &loginInput{caFlag: "--cacert", server: x.url, authProvider: "oktaProvider"}
			_, err := signInForClusterToken(ctx, x.client(), input, "local", p)
			return err
		}},
		{"get-token with a cluster, token reuse off", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			in := x.input("local", "kc")
			in.s["auth-provider"] = "oktaProvider"
			_, err := x.command(x.store).Execute(ctx, in, p)
			return err
		}},
		{"get-token with a cluster, token reuse on (the plugin login)", true, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			in := x.input("local", "kc")
			in.s["auth-provider"] = "oktaProvider"
			_, err := x.command(x.store).Execute(ctx, in, p)
			return err
		}},
		{"legacy get-token with a cluster", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			_, err := x.command(x.store).runWithParams(ctx, runParams{Server: x.url, Cluster: "local", AuthProvider: "oktaProvider", legacyShim: true}, p)
			return err
		}},
		{"legacy get-token with no cluster", false, func(ctx context.Context, x *pluginFixture, p *handoffPrompter) error {
			_, err := x.command(x.store).runWithParams(ctx, runParams{Server: x.url, AuthProvider: "oktaProvider", legacyShim: true}, p)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newInMemoryPluginFixture(t, tc.reuse, samlOpts)
				p := newHandoffPrompter(t, func(id, key string) { x.f.DepositAuthToken(id, key) })
				require.NoError(t, tc.run(t.Context(), x, p))
				// The prompter fails the test on a sign-in URL the dashboard
				// sign-in page would refuse, so a printed sign-in URL is an
				// accepted one.
				require.NotEmpty(t, p.signInURL, "no sign-in URL was printed")
			})
		})
	}
}
