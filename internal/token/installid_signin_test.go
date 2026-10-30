package token

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// signInCase is a Rancher that answers the install id setting and logs every
// request, and the sign-in request function registered for the call.
type signInCase struct {
	rec   *installIDServer
	log   *requestLog
	probe *signInProbe
}

func newSignInCase(t *testing.T) *signInCase {
	t.Helper()
	c := &signInCase{rec: &installIDServer{}, log: &requestLog{}}
	c.probe = registerSignInProbe(t, c.log.sequence)
	return c
}

// opts are the facade options that route the install id setting to the
// case and log every request.
func (c *signInCase) opts(o testrancher.Options) testrancher.Options {
	o.Fallthrough = c.rec
	o.RequestHook = c.log.hook(o.RequestHook)
	return o
}

// assertSignInRequestAtItsPlace asserts the sign-in called the function once,
// when the cluster token was created and the login token was not yet
// deleted, with the origin of server, that its install id request carried
// the login token (a live token of the Rancher, never the token handed to
// kubectl), and that the call registered no fetcher.
func (c *signInCase) assertSignInRequestAtItsPlace(t *testing.T, server, handedToKubectl string) {
	t.Helper()
	calls := c.probe.got()
	require.Len(t, calls, 1, "the sign-in sends its request once")
	call := calls[0]
	assert.Equal(t, kube.BaseURL(server), call.origin)
	require.NoError(t, call.err)
	assert.Equal(t, http.StatusOK, call.status)
	assert.Equal(t, testInstallID, call.value)
	assert.Contains(t, call.sequence, "POST "+ExtTokensPath+" v3", "the cluster token was created")
	assert.NotContains(t, call.sequence, "POST "+LogoutPath+" v3", "the login token was not deleted yet")

	seen := c.rec.seen()
	require.Len(t, seen, 1)
	assert.Equal(t, "v3", seen[0].kind, "the bearer is a login token the Rancher held at the time")
	assert.NotEqual(t, handedToKubectl, seen[0].bearer, "never the token handed to kubectl")
	create := c.log.find(http.MethodPost, ExtTokensPath)
	require.Len(t, create, 1)
	assert.Equal(t, create[0].bearer, seen[0].bearer, "the login token that created the cluster token")

	seq := c.log.sequence()
	install := slices.Index(seq, "GET "+installIDRoute+" v3")
	require.GreaterOrEqual(t, install, 0)
	assert.Greater(t, install, slices.Index(seq, "POST "+ExtTokensPath+" v3"))
	assert.Less(t, install, slices.Index(seq, "POST "+LogoutPath+" v3"))
	_, registered := httpx.RegisteredInstallIDFetcher()
	assert.False(t, registered, "a sign-in registers no fetcher")
}

// TestEverySignInCauseSendsItsInstallIDRequestBetweenTheCreateAndTheLogout
// asserts, for each way a miss reaches the sign-in, that the sign-in calls
// the request function once, after the cluster token create and before the
// login token's logout, and that its install id request carries the login
// token.
func TestEverySignInCauseSendsItsInstallIDRequestBetweenTheCreateAndTheLogout(t *testing.T) {
	t.Run("no stored login", func(t *testing.T) {
		c := newSignInCase(t)
		x := newPluginFixture(t, false, c.opts(testrancher.Options{}))
		c.rec.f = x.f
		res, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		c.assertSignInRequestAtItsPlace(t, x.url, res.Data.(*execCredential).Status.Token)
	})
	t.Run("a config that cannot be read", func(t *testing.T) {
		c := newSignInCase(t)
		x := newPluginFixture(t, false, c.opts(testrancher.Options{}))
		c.rec.f = x.f
		require.NoError(t, os.WriteFile(config.FilePath(x.dir), []byte("\t: not yaml ["), 0o600))
		in := x.input("c-test", "n")
		in.s["cacert"] = selfSignedCAPEM(t)
		res, err := x.command(x.store).Execute(t.Context(), in, localPrompter())
		require.NoError(t, err)
		c.assertSignInRequestAtItsPlace(t, x.url, res.Data.(*execCredential).Status.Token)
	})
	t.Run("a stored login the server rejects", func(t *testing.T) {
		c := newSignInCase(t)
		x := newPluginFixture(t, false, c.opts(testrancher.Options{}))
		c.rec.f = x.f
		seedStoredLogin(t, x, "stored")
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: "stored", URL: x.url}, "ext/gone:wrong"))
		c.log.reset()
		res, err := x.command(x.store).Execute(t.Context(), x.input("testcluster", "kc"), localPrompter())
		require.NoError(t, err)
		c.assertSignInRequestAtItsPlace(t, x.url, res.Data.(*execCredential).Status.Token)
	})
	t.Run("a plugin login the server rejects while token reuse is off", func(t *testing.T) {
		c := newSignInCase(t)
		x := newPluginFixture(t, true, c.opts(testrancher.Options{}))
		c.rec.f = x.f
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		entry, _ := x.entry(t)
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: x.entryName(), URL: x.url}, "ext/"+entry.TokenRef.Name+":wrong"))
		cfg, err := config.Load(config.FilePath(x.dir))
		require.NoError(t, err)
		cfg.TokenReuse = false
		require.NoError(t, cfg.Write())
		c.log.reset()
		c.probe = registerSignInProbe(t, c.log.sequence)
		res, err := x.getToken(t, "testcluster", "n2", localPrompter())
		require.NoError(t, err)
		c.assertSignInRequestAtItsPlace(t, x.url, res.Data.(*execCredential).Status.Token)
	})
	// This row may take the plain reuse-off path instead: it holds the plugin
	// login lock for 200 ms and turns reuse off, and a call that has not
	// reached the lock by then reads reuse off and never waits. The lock is
	// taken inside internal/config, which has no hook that tells a test the
	// call is waiting on it: the test cannot make the wait certain. Both
	// paths end in the one signInForClusterToken call, and the row asserts the
	// request in its place on either.
	t.Run("token reuse turned off while the call waited for the plugin login", func(t *testing.T) {
		c := newSignInCase(t)
		x := newPluginFixture(t, true, c.opts(testrancher.Options{}))
		c.rec.f = x.f
		serverID := credhelperCanonical(t, x.url)
		var res runtime.Result
		var callErr error
		var wg sync.WaitGroup
		require.NoError(t, config.WithPluginLoginLock(t.Context(), x.dir, serverID, func() error {
			wg.Go(func() { res, callErr = x.getToken(t, "c-test", "n", localPrompter()) })
			time.Sleep(200 * time.Millisecond)
			cfg, err := config.Load(config.FilePath(x.dir))
			require.NoError(t, err)
			cfg.TokenReuse = false
			return cfg.Write()
		}))
		wg.Wait()
		require.NoError(t, callErr)
		c.assertSignInRequestAtItsPlace(t, x.url, res.Data.(*execCredential).Status.Token)
	})
	t.Run("an entry another call logged in for another user", func(t *testing.T) {
		c := newSignInCase(t)
		x := newAuthConfigFixture(t, true, c.opts(testrancher.Options{}))
		c.rec.f = x.f
		x.hook = c.log.hook(nil)
		name := config.ExecLoginEntryNameFor(x.serverID(), "user-alice", "corp-ldap")
		x.entryThrough(name, "", "", "user-alice", "openldap", true)
		c.log.reset()
		token, _, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
		require.NoError(t, err)
		c.assertSignInRequestAtItsPlace(t, x.srv.URL, token)
	})
	t.Run("--user-id with no cluster", func(t *testing.T) {
		c := newSignInCase(t)
		x := newOwnerFixture(t, false, c.opts(testrancher.Options{}))
		c.rec.f = x.f
		x.hook = c.log.hook(nil)
		token, _, err := x.callWith(authCall{userID: "user-alice", signIn: []string{"alice", "pw-alice"}})
		require.NoError(t, err)
		c.assertSignInRequestAtItsPlace(t, x.srv.URL, token)
	})
	t.Run("a sign-in over SAML", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := newSignInCase(t)
			f := testrancher.New(c.opts(testrancher.Options{EnableSAMLAuthTokens: true, ExtraProviders: []string{"oktaProvider"}}))
			c.rec.f = f
			srv := httptest.NewTestServer(t, f)
			rt := hostTransport{hostOf(rancherOrigin): srv.Client().Transport}
			p := &urlRecorder{handoffPrompter: newHandoffPrompter(func(id, key string) { f.DepositAuthToken(id, key) })}
			input := &loginInput{server: rancherOrigin, authProvider: "oktaProvider"}
			si, err := signInForClusterToken(t.Context(), rt.client(), input, "local", p)
			require.NoError(t, err)
			c.assertSignInRequestAtItsPlace(t, rancherOrigin, si.Cluster.Bearer)
		})
	})
}

// TestASignInThatFailsBeforeItsClusterTokenExistsSendsNoRequest asserts the
// sign-in calls the function only after the ext create succeeded: a cluster
// lookup the server refuses and a create it refuses call nothing.
func TestASignInThatFailsBeforeItsClusterTokenExistsSendsNoRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster string
		opts    testrancher.Options
	}{
		{"a refused cluster lookup", "testcluster", testrancher.Options{ClusterVisibility: facadeClusters(func(string) bool { return false })}},
		{"a refused create", "c-test", testrancher.Options{ScopedCreateStatus: http.StatusForbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSignInCase(t)
			x := newPluginFixture(t, false, c.opts(tc.opts))
			c.rec.f = x.f
			_, err := x.getToken(t, tc.cluster, "n", localPrompter())
			require.Error(t, err)
			assert.Empty(t, c.probe.got(), "the function was not called")
			assert.Empty(t, c.rec.seen(), "no install id request went out")
		})
	}
}

// TestASignInForTheLegacyCommandWithSkipVerifySendsItsRequestThroughItsOwnClient
// asserts the request the sign-in hands the function goes through the
// client the call built: against a TLS server whose certificate the call
// was not given, with --skip-verify, it gets the install id.
func TestASignInForTheLegacyCommandWithSkipVerifySendsItsRequestThroughItsOwnClient(t *testing.T) {
	c := newSignInCase(t)
	opts := c.opts(testrancher.Options{})
	f := newFacadeWithClusters(&opts)
	c.rec.f = f
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	x := &pluginFixture{f: f, srv: srv, url: srv.URL}
	x.init(t, false)
	cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), tin{
		s: map[string]string{"server": srv.URL, "user": "alice", "cluster": "c-test", "auth-provider": "localProvider"},
		b: map[string]bool{"skip-verify": true},
	}, localPrompter())
	require.NoError(t, err)
	c.assertSignInRequestAtItsPlace(t, srv.URL, res.Data.(*execCredential).Status.Token)
}

// signInUnder runs a sign-in for c-test on a Rancher whose install id
// setting answers as answers says, with the function register
// registers, and returns its result, the Rancher and how long it took.
func signInUnder(t *testing.T, answers http.Handler, register func(cancel context.CancelFunc)) (clusterSignIn, *testrancher.Facade, time.Duration) {
	t.Helper()
	clearRecords(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if register != nil {
		register(cancel)
	}
	start := time.Now()
	f, _, si, err := signInWith(t, ctx, testrancher.Options{Fallthrough: answers}, "c-test", localPrompter())
	elapsed := time.Since(start)
	require.NoError(t, err)
	return si, f, elapsed
}

// TestTheSignInRequestFunctionNeverChangesTheSignIn asserts a function that
// calls a server answering 500, one that sleeps 300 ms, one that waits for
// its context while the call is cancelled, and one that panics leave the
// sign-in's result as it is with no function: the cluster token, the login
// token deleted before the call returns, and no error.
func TestTheSignInRequestFunctionNeverChangesTheSignIn(t *testing.T) {
	check := func(t *testing.T, si clusterSignIn, f *testrancher.Facade) {
		t.Helper()
		require.NotNil(t, si.Cluster.Ref)
		assert.Equal(t, TokenKindExt, si.Cluster.Ref.Kind)
		require.NotNil(t, si.Login)
		assert.Equal(t, ResultRevoked, si.Login.Cleanup.Result)
		assert.Empty(t, f.BearersOfKind("v3"), "the login token was deleted")
		assert.Equal(t, []string{si.Cluster.Bearer}, f.BearersOfKind("ext"))
	}
	answering := &installIDServer{}
	si, f, plain := signInUnder(t, answering, nil)
	check(t, si, f)
	assert.Empty(t, answering.seen(), "with no function registered the sign-in sends no install id request")

	t.Run("a server that answers 500", func(t *testing.T) {
		failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		var status int
		si, f, _ := signInUnder(t, failing, func(context.CancelFunc) {
			httpx.RegisterSignInRequest(func(ctx context.Context, origin string, fetch httpx.InstallIDFunc) {
				_, status, _ = fetch(ctx)
			})
		})
		check(t, si, f)
		assert.Equal(t, http.StatusInternalServerError, status, "the function got the failure")
	})
	t.Run("a function that sleeps 300 ms", func(t *testing.T) {
		si, f, elapsed := signInUnder(t, answering, func(context.CancelFunc) {
			httpx.RegisterSignInRequest(func(context.Context, string, httpx.InstallIDFunc) { time.Sleep(300 * time.Millisecond) })
		})
		check(t, si, f)
		assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond, "the sign-in waited for the function")
		t.Logf("the sign-in took %v with the 300 ms function and %v with none", elapsed, plain)
	})
	t.Run("a function that waits for its context while the call is cancelled", func(t *testing.T) {
		si, f, _ := signInUnder(t, answering, func(cancel context.CancelFunc) {
			httpx.RegisterSignInRequest(func(ctx context.Context, origin string, fetch httpx.InstallIDFunc) {
				cancel()
				<-ctx.Done()
			})
		})
		check(t, si, f)
	})
	t.Run("a function that panics", func(t *testing.T) {
		si, f, _ := signInUnder(t, answering, func(context.CancelFunc) {
			httpx.RegisterSignInRequest(func(context.Context, string, httpx.InstallIDFunc) { panic("boom") })
		})
		check(t, si, f)
	})
}
