package token

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	"github.com/rancher/rancher-cli/internal/ui"
)

// pluginWithInstallID is a plugin fixture whose Rancher answers the install
// id setting and records the bearer of each request.
func pluginWithInstallID(t *testing.T, reuse bool, opts testrancher.Options) (*pluginFixture, *installIDServer) {
	t.Helper()
	rec := &installIDServer{}
	opts.Fallthrough = rec
	x := newPluginFixture(t, reuse, opts)
	rec.f = x.f
	return x, rec
}

// assertRegistered asserts the call registered a fetcher for server's origin
// with the given skip, and that the request it sends carries wantBearer.
func assertRegistered(t *testing.T, rec *installIDServer, server string, skip bool, wantBearer, handedToKubectl string) {
	t.Helper()
	f := registeredFetcher(t)
	assert.Equal(t, kube.BaseURL(server), f.Origin)
	assert.Equal(t, skip, f.SkipFailureWait)
	fetchRegistered(t, f)
	assert.Equal(t, []string{wantBearer}, rec.bearers())
	assert.NotEmpty(t, wantBearer)
	if handedToKubectl != "" {
		assert.NotEqual(t, handedToKubectl, wantBearer, "the request never carries the token handed to kubectl")
	}
}

// TestACacheMissOnAStoredLoginRegistersThatLogin asserts a miss that creates
// the cluster token from a stored login registers the install id request
// with the stored login, without the skip, and the ExecCredential holds the
// token created from it.
func TestACacheMissOnAStoredLoginRegistersThatLogin(t *testing.T) {
	x, rec := pluginWithInstallID(t, false, testrancher.Options{})
	login := seedStoredLogin(t, x, "stored")
	clearRecords(t)
	res, err := x.command(x.store).Execute(t.Context(), x.input("local", "kc"), ui.Discard())
	require.NoError(t, err)
	assertRegistered(t, rec, x.url, false, login, res.Data.(*execCredential).Status.Token)
}

// TestACacheMissThatCreatedThePluginLoginRegistersItWithTheSkip asserts a
// miss that creates the plugin's login with token reuse on registers the
// install id request with that login and the skip, and a miss that replaces
// a rejected plugin login registers the new one with the skip.
func TestACacheMissThatCreatedThePluginLoginRegistersItWithTheSkip(t *testing.T) {
	t.Run("a clean miss", func(t *testing.T) {
		x, rec := pluginWithInstallID(t, true, testrancher.Options{})
		clearRecords(t)
		res, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		login, err := x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.url})
		require.NoError(t, err)
		assertRegistered(t, rec, x.url, true, login, res.Data.(*execCredential).Status.Token)
	})
	t.Run("after a rejection of the plugin login", func(t *testing.T) {
		x, rec := pluginWithInstallID(t, true, testrancher.Options{})
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		before, _ := x.entry(t)
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: x.entryName(), URL: x.url}, "ext/"+before.TokenRef.Name+":wrong"))
		clearRecords(t)
		res, err := x.getToken(t, "testcluster", "n2", localPrompter())
		require.NoError(t, err)
		require.Equal(t, 2, x.f.LoginCount(), "one re-login")
		login, err := x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.url})
		require.NoError(t, err)
		assertRegistered(t, rec, x.url, true, login, res.Data.(*execCredential).Status.Token)
	})
}

// TestACacheMissThatFoundTheLoginAnotherCallStoredRegistersItWithoutTheSkip
// asserts the plugin login of a call that waited for another call's login
// carries the stored login and is not new, and the second call that uses
// the login registers it without the skip.
func TestACacheMissThatFoundTheLoginAnotherCallStoredRegistersItWithoutTheSkip(t *testing.T) {
	x, rec := pluginWithInstallID(t, true, testrancher.Options{})
	_, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	login, err := x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.url})
	require.NoError(t, err)

	tok, _, err := x.command(x.store).pluginLogin(t.Context(), x.command(x.store).pluginInput(x.url, x.url, "", "c-test", x.client(),
		runParams{Name: "n", AuthProvider: "localProvider"}), ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, login, tok.loginToken, "the waiter's token carries the login it found")
	assert.False(t, tok.loginIsNew, "and says it did not create it")
	assert.NotEqual(t, login, tok.Token)

	clearRecords(t)
	res, err := x.getToken(t, "local", "n2", ui.Discard())
	require.NoError(t, err)
	assertRegistered(t, rec, x.url, false, login, res.Data.(*execCredential).Status.Token)
}

// TestACacheMissOnARancherWithoutExtTokensRegistersTheStoredLoginItServes
// asserts the miss that serves the stored login itself registers it, without
// the skip, and the request carries the token kubectl gets.
func TestACacheMissOnARancherWithoutExtTokensRegistersTheStoredLoginItServes(t *testing.T) {
	rec := &installIDServer{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == installIDRoute {
			rec.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	clearRecords(t)

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local", "cacert": serverCAPEM(srv)}}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "stored-login-token", res.Data.(*execCredential).Status.Token)
	assertRegistered(t, rec, srv.URL, false, "stored-login-token", "")
}

// TestTheLegacyCommandWithoutAClusterRegistersItsKubeconfigTokenWithTheSkip
// asserts the legacy command without a cluster registers the token it
// signed in with, which is the token kubectl gets, with the skip.
func TestTheLegacyCommandWithoutAClusterRegistersItsKubeconfigTokenWithTheSkip(t *testing.T) {
	x, rec := pluginWithInstallID(t, true, testrancher.Options{})
	clearRecords(t)
	cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.url, "user": "alice", "auth-provider": "localProvider"}}, localPrompter())
	require.NoError(t, err)
	assertRegistered(t, rec, x.url, true, res.Data.(*execCredential).Status.Token, "")
}

// TestAShimCallWithSkipVerifyRegistersARequestThatSkipsVerifyToo asserts the
// request a legacy command registers goes through the client the call built:
// against a TLS server whose certificate the call was not given, with
// --skip-verify, it gets the install id.
func TestAShimCallWithSkipVerifyRegistersARequestThatSkipsVerifyToo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster string
		reuse   bool
	}{
		{"a stored login", "c-test", true},
		{"no cluster", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &installIDServer{}
			f := newFacadeWithClusters(&testrancher.Options{Fallthrough: rec})
			rec.f = f
			srv := httptest.NewTLSServer(f)
			t.Cleanup(srv.Close)
			x := &pluginFixture{f: f, srv: srv, url: srv.URL}
			x.init(t, tc.reuse)
			clearRecords(t)
			cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
			in := map[string]string{"server": srv.URL, "user": "alice", "auth-provider": "localProvider"}
			if tc.cluster != "" {
				in["cluster"] = tc.cluster
			}
			_, err := cmd.Execute(t.Context(), tin{s: in, b: map[string]bool{"skip-verify": true}}, localPrompter())
			require.NoError(t, err)
			fetchRegistered(t, registeredFetcher(t))
		})
	}
}

// TestACacheHitRegistersNothingAndSendsNoSignInRequest asserts every path
// that serves a cached credential leaves the install id fetcher unset and
// never calls the sign-in request function: the flag fast path, the
// resolved path without and with a direct endpoint, and the legacy
// command's pre-probe.
func TestACacheHitRegistersNothingAndSendsNoSignInRequest(t *testing.T) {
	var called int
	hit := func(t *testing.T, call func() error) {
		t.Helper()
		clearRecords(t)
		httpx.RegisterSignInRequest(func(context.Context, string, httpx.InstallIDFunc) { called++ })
		require.NoError(t, call())
		_, registered := httpx.RegisteredInstallIDFetcher()
		assert.False(t, registered, "a cache hit registers no fetcher")
		assert.Zero(t, called, "a cache hit calls no sign-in request function")
	}
	t.Run("the flag fast path", func(t *testing.T) {
		x, _ := pluginWithInstallID(t, false, testrancher.Options{})
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		hit(t, func() error { _, err := x.getToken(t, "c-test", "n", ui.Discard()); return err })
	})
	t.Run("the resolved path", func(t *testing.T) {
		x, _ := pluginWithInstallID(t, false, testrancher.Options{})
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		t.Setenv(execInfoEnv, execInfoFor(t, x.url+"/k8s/clusters/c-test", nil))
		hit(t, func() error { _, err := x.getToken(t, "c-test", "n", ui.Discard()); return err })
	})
	t.Run("the legacy command's pre-probe", func(t *testing.T) {
		x, _ := pluginWithInstallID(t, false, testrancher.Options{})
		cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
		in := tin{s: map[string]string{"server": x.url, "user": "alice", "cluster": "c-test", "auth-provider": "localProvider"}}
		_, err := cmd.Execute(t.Context(), in, localPrompter())
		require.NoError(t, err)
		hit(t, func() error { _, err := cmd.Execute(t.Context(), in, ui.Discard()); return err })
	})
	t.Run("the resolved path with a direct endpoint", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newInMemoryDirectEndpoint(t, 0, "review")
			t.Setenv(execInfoEnv, d.execInfo(t))
			x := newInMemoryPluginFixture(t, false, testrancher.Options{})
			x.route(d)
			_, err := x.getToken(t, "local", "kc", localPrompter())
			require.NoError(t, err)
			hit(t, func() error { _, err := x.getToken(t, "local", "kc", ui.Discard()); return err })
		})
	})
}

// TestACallThatFailsAfterItsRowRegistersNothing asserts a miss that fails
// after the token exists registers no fetcher: an expiry that is not a time,
// a create that returns no token, and a cancelled wait for the direct
// endpoint.
func TestACallThatFailsAfterItsRowRegistersNothing(t *testing.T) {
	notRegistered := func(t *testing.T) {
		t.Helper()
		_, registered := httpx.RegisteredInstallIDFetcher()
		assert.False(t, registered)
	}
	t.Run("an expiry that is not a time", func(t *testing.T) {
		x, _ := pluginWithInstallID(t, false, testrancher.Options{ScopedCreateExpiresAt: new("not a time")})
		seedStoredLogin(t, x, "stored")
		clearRecords(t)
		_, err := x.command(x.store).Execute(t.Context(), x.input("local", "kc"), ui.Discard())
		require.ErrorContains(t, err, "parsing token expiry")
		notRegistered(t)
	})
	t.Run("an expiry that is not a time for the legacy command", func(t *testing.T) {
		srv := loginAnswering(t, http.StatusCreated, `{"id":"token-abc","token":"token-abc:secret","expiresAt":"not a time","type":"token"}`)
		dir := t.TempDir()
		clearRecords(t)
		cmd := NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "user": "alice"}}, terminalLines("alice", "pw"))
		require.ErrorContains(t, err, "parsing token expiry")
		notRegistered(t)
	})
	t.Run("a create that returns no token from a stored login", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == ExtTokensPath && r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"metadata":{"name":"created"},"status":{"bearerToken":" "}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		dir := t.TempDir()
		store := config.NewFileStore(dir)
		require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		clearRecords(t)
		cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local", "cacert": serverCAPEM(srv)}}, ui.Discard())
		require.ErrorContains(t, err, "no bearer token")
		notRegistered(t)
	})
	t.Run("an empty token from the legacy command", func(t *testing.T) {
		srv := loginAnswering(t, http.StatusCreated, `{}`)
		dir := t.TempDir()
		clearRecords(t)
		cmd := NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "user": "alice"}}, terminalLines("alice", "pw"))
		require.ErrorContains(t, err, "empty token")
		notRegistered(t)
	})
	t.Run("a cancelled wait for the direct endpoint", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newInMemoryDirectEndpoint(t, 1000, "review")
			t.Setenv(execInfoEnv, "")
			x := newInMemoryPluginFixture(t, false, testrancher.Options{})
			x.route(d)
			parent, err := AcquireLoginCredential(t.Context(), x.client(), LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0, localPrompter())
			require.NoError(t, err)
			seedEntry(t, x.dir, x.store, "stored", config.Server{URL: x.url, TokenRef: parent.Ref, TokenUserID: parent.UserID}, parent.Token, false)
			t.Setenv(execInfoEnv, d.execInfo(t))
			clearRecords(t)
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				for d.probes.Load() < 2 {
					time.Sleep(time.Millisecond)
				}
				cancel()
			}()
			_, err = x.command(x.store).Execute(ctx, x.input("local", "kc"), localPrompter())
			require.ErrorIs(t, err, context.Canceled)
			notRegistered(t)
		})
	})
}
