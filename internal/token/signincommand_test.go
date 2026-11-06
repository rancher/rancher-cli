package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// forcedSingleLogout is the options of a provider whose logout fails and
// whose standard users cannot delete a v3 token any other way.
func forcedSingleLogout(opts testrancher.Options) testrancher.Options {
	opts.ForcedSingleLogout, opts.V3DeleteForbiddenForNonAdmin = true, true
	return opts
}

// TestExecPluginSignInWithTokenReuseOffReturnsAnExtClusterToken asserts the
// whole call: the sign-in's cluster token is an ext token the exec
// credential carries and the cache records, the login token is gone, and
// the requests are the login (json), the cluster read, the ext create and
// the logout.
func TestExecPluginSignInWithTokenReuseOffReturnsAnExtClusterToken(t *testing.T) {
	log := &requestLog{}
	x := newPluginFixture(t, false, testrancher.Options{RequestHook: log.hook(nil)})
	res, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, []string{cred.Status.Token}, x.f.BearersOfKind("ext"), "the exec credential is the ext cluster token")
	assert.Empty(t, x.f.BearersOfKind("v3"), "the login token was deleted")
	assert.Equal(t, 1, x.f.TokenCount())
	rec := cachedInfo(t, x.dir, "n")
	require.NotNil(t, rec.TokenRef)
	assert.Equal(t, TokenKindExt, rec.TokenRef.Kind)
	assert.Equal(t, "user-e2e", rec.TokenUserID)
	assert.Equal(t, []string{
		"GET /v1-public/authproviders none",
		"POST /v1-public/login none",
		"GET " + testrancher.ClustersCollectionPath + " v3",
		"POST " + ExtTokensPath + " v3",
		"POST /v1/logout v3",
	}, log.sequence())
	_, present := x.entry(t)
	assert.False(t, present, "no login is kept")
}

// TestExecPluginSignInOverSAMLWithAClusterWorks asserts a SAML sign-in with
// a cluster ends in an ext cluster token: the login token is the SAML
// kubeconfig token, deleted by its own logout. The login URL asks for json.
func TestExecPluginSignInOverSAMLWithAClusterWorks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newInMemoryPluginFixture(t, false, testrancher.Options{EnableSAMLAuthTokens: true, ExtraProviders: []string{"oktaProvider"}})
		p := &urlRecorder{handoffPrompter: newHandoffPrompter(func(id, key string) { x.f.DepositAuthToken(id, key) })}
		in := x.input("local", "kc")
		in.s["auth-provider"] = "oktaProvider"
		res, err := x.command(x.store).Execute(t.Context(), in, p)
		require.NoError(t, err)
		assert.Contains(t, p.loginURL(), "responseType=json")
		assert.Equal(t, []string{res.Data.(*execCredential).Status.Token}, x.f.BearersOfKind("ext"))
		assert.Empty(t, x.f.BearersOfKind("v3"), "the kubeconfig token was deleted")
		creates := x.f.CreateRequests()
		require.Len(t, creates, 1)
		assert.Equal(t, "user-e2e", creates[0].OwnerUserID, "the SAML kubeconfig token belongs to the signed-in user, so the cluster token names its owner")
	})
}

// TestExecPluginWarnsOfALoginTokenItCouldNotDelete asserts the warning on
// each of the three sign-ins that create a login token, with forced single
// logout in the fake: the plugin's sign-in with token reuse off, its
// plugin-created login with token reuse on, and a call that names a
// kubeconfig token (SAML), whose noun is Rancher's own.
func TestExecPluginWarnsOfALoginTokenItCouldNotDelete(t *testing.T) {
	t.Run("token reuse off", func(t *testing.T) {
		x := newPluginFixture(t, false, forcedSingleLogout(testrancher.Options{}))
		res, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		require.Len(t, res.Warnings, 1)
		login := x.loginTokenName(t)
		assert.Regexp(t, "^"+regexp.QuoteMeta("the transient session token v3/"+login+" created by this login could not be revoked (logout 500; delete 403 Forbidden); "+
			"v3/"+login+" expires at ")+`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`+regexp.QuoteMeta("; an administrator can delete it sooner with: rancher delete management-token '"+login+"' --apply --yes --allow-deprecated")+"$",
			clierr.ExpandRemedies(res.Warnings[0], ""))
	})
	t.Run("token reuse on", func(t *testing.T) {
		x := newPluginFixture(t, true, forcedSingleLogout(testrancher.Options{}))
		res, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		require.Len(t, res.Warnings, 1)
		login := x.loginTokenName(t)
		assert.Regexp(t, "^"+regexp.QuoteMeta("the transient session token v3/"+login+" created by this login could not be revoked (logout 500; delete 403 Forbidden); "+
			"v3/"+login+" expires at ")+`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`+regexp.QuoteMeta("; an administrator can delete it sooner with: rancher delete management-token '"+login+"' --apply --yes --allow-deprecated")+"$",
			clierr.ExpandRemedies(res.Warnings[0], ""))
		_, present := x.entry(t)
		assert.True(t, present, "the login is kept")
	})
	t.Run("an ext login token names its owner", func(t *testing.T) {
		for _, reuse := range []bool{false, true} {
			x := newPluginFixture(t, reuse, testrancher.Options{ExtSessionLogin: true, SessionDeleteMode: "fail"})
			res, err := x.getToken(t, "c-test", "n", localPrompter())
			require.NoError(t, err)
			require.Len(t, res.Warnings, 1, "reuse %v", reuse)
			w := clierr.ExpandRemedies(res.Warnings[0], "")
			assert.Contains(t, w, "the transient session token ext/")
			assert.Contains(t, w, " could not be revoked (500 Internal Server Error); user 'user-e2e' can delete it from a directory signed in as user 'user-e2e'; run: rancher delete token ")
			assert.Contains(t, w, " --apply --yes; or it expires at ", "reuse %v", reuse)
		}
	})
	t.Run("SAML names a kubeconfig token on both", func(t *testing.T) {
		for _, reuse := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				x := newInMemoryPluginFixture(t, reuse, forcedSingleLogout(testrancher.Options{EnableSAMLAuthTokens: true, ExtraProviders: []string{"oktaProvider"}}))
				p := &urlRecorder{handoffPrompter: newHandoffPrompter(func(id, key string) { x.f.DepositAuthToken(id, key) })}
				in := x.input("local", "kc")
				in.s["auth-provider"] = "oktaProvider"
				res, err := x.command(x.store).Execute(t.Context(), in, p)
				require.NoError(t, err, "reuse %v", reuse)
				require.Len(t, res.Warnings, 1, "reuse %v", reuse)
				assert.True(t, strings.HasPrefix(res.Warnings[0], "the transient kubeconfig token v3/"), res.Warnings[0])
			})
		}
	})
}

// loginTokenName is the name of the one v3 token the server holds: the
// login token a forced single logout left.
func (x *pluginFixture) loginTokenName(t *testing.T) string {
	t.Helper()
	var name string
	for _, c := range x.f.CreateRequests() {
		if c.Role == "session" {
			require.Empty(t, name, "one login")
			name = c.Name
		}
	}
	require.NotEmpty(t, name)
	return name
}

// TestSignInErrorsAfterTheLoginTokenCarryItsFate asserts every error the
// call can raise after the login token's delete reports that token's fate
// beside the cluster token's, as one login failure: an owner mismatch, a
// cluster token whose expiry cannot be read, and the warnings the call had
// collected still ride the error.
func TestSignInErrorsAfterTheLoginTokenCarryItsFate(t *testing.T) {
	t.Run("an owner mismatch", func(t *testing.T) {
		x := newOwnerFixture(t, false, forcedSingleLogout(testrancher.Options{}))
		t.Setenv(execInfoEnv, "{not json")
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		ce, d := failureOf(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, clierr.StageClusterToken, d.Stage)
		require.NotNil(t, d.SessionToken)
		assert.Equal(t, ResultFailed, sessionCleanup(t, d), "the login token could not be deleted")
		require.NotNil(t, d.ClusterToken)
		assert.Equal(t, ResultRevoked, clusterCleanup(t, d))
		assert.Nil(t, d.Primary.Details, "the cluster token's fate is not repeated under the primary")
		recovery := clierr.ExpandRemedies(d.Recovery, "")
		assert.Contains(t, recovery, "the session token v3/"+d.SessionToken.Ref.Name+": v3/"+d.SessionToken.Ref.Name+" expires at ")
		assert.Contains(t, recovery, "; an administrator can delete it sooner with: rancher delete management-token ")
		assert.True(t, strings.HasSuffix(recovery, "; run the kubectl command again"), recovery)
		require.NotEmpty(t, ce.Warnings(), "the warnings the call had collected are kept")
		assert.Contains(t, ce.Warnings()[0], execInfoEnv+" could not be read")
	})
	t.Run("an owner mismatch whose cluster token cannot be deleted names the owner", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{ClusterDeleteMode: "fail"})
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		_, d := failureOf(t, err)
		recovery := clierr.ExpandRemedies(d.Recovery, "")
		require.NotNil(t, d.ClusterToken.Ref)
		assert.Contains(t, recovery, "the cluster token "+d.ClusterToken.Ref.String()+": user 'user-carol' can delete it from a directory signed in as user 'user-carol'; run: rancher delete token '"+d.ClusterToken.Ref.Name+
			"' --apply --yes")
	})
	t.Run("an owner request that fails on a Rancher without ext tokens", func(t *testing.T) {
		x := newOwnerFixture(t, false, forcedSingleLogout(testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateOmitOwner: true, SelfSubjectReviewStatus: http.StatusInternalServerError}))
		_, err := x.call("prod-c-test", "user-carol", "carol", nil)
		ce, d := failureOf(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Equal(t, ResultFailed, sessionCleanup(t, d))
		assert.Equal(t, ResultFailed, clusterCleanup(t, d))
		recovery := clierr.ExpandRemedies(d.Recovery, "")
		assert.Contains(t, recovery, "the session token v3/")
		assert.Contains(t, recovery, "the cluster token v3/")
	})
	t.Run("an expiry that cannot be read", func(t *testing.T) {
		// A warning collected before the sign-in: the direct endpoint's
		// description cannot be read.
		t.Setenv(execInfoEnv, "{not json")
		x := newPluginFixture(t, false, forcedSingleLogout(testrancher.Options{ScopedCreateExpiresAt: new("not a time")}))
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		ce, d := failureOf(t, err)
		require.NotEmpty(t, ce.Warnings(), "the warnings the call had collected are kept")
		assert.Contains(t, ce.Warnings()[0], execInfoEnv+" could not be read")
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Contains(t, ce.Message, "parsing token expiry: ")
		assert.Contains(t, ce.Message, "; the token the sign-in created was revoked")
		assert.Equal(t, ResultFailed, sessionCleanup(t, d))
		require.NotNil(t, d.ClusterToken)
		assert.Equal(t, ResultRevoked, clusterCleanup(t, d))
		var clusterDeletes []testrancher.DeleteRecord
		for _, del := range x.f.DeleteRequests() {
			if del.Name == d.ClusterToken.Ref.Name {
				clusterDeletes = append(clusterDeletes, del)
			}
		}
		require.Len(t, clusterDeletes, 1)
		assert.Contains(t, clusterDeletes[0].Bearer, clusterDeletes[0].Name, "deleted with its own bearer")
		assert.Empty(t, x.f.BearersOfKind("ext"), "no cluster token is left")
		_, _, cerr := x.store.GetCachedCred(config.CacheKey("n", credhelperCanonical(t, x.url), "c-test"), anchorFor(x.dir))
		assert.ErrorIs(t, cerr, config.ErrNoToken, "nothing cached")
	})
}

// TestSignInCancelledDuringTheDirectWait asserts Ctrl-C while the call waits
// for a direct endpoint: the cluster token is deleted with its own bearer,
// and the error carries the login token's fate, which forced single logout
// leaves, and the cluster token's. When the cluster token cannot be deleted
// the message names the user who can.
func TestSignInCancelledDuringTheDirectWait(t *testing.T) {
	run := func(t *testing.T, opts testrancher.Options, cancelOnLogout bool) (*pluginFixture, error) {
		t.Helper()
		d := newInMemoryDirectEndpoint(t, 1000, "review")
		t.Setenv(execInfoEnv, d.execInfo(t))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if cancelOnLogout {
			opts.RequestHook = func(r *http.Request) {
				if r.URL.Path == LogoutPath {
					cancel()
				}
			}
		}
		x := newInMemoryPluginFixture(t, false, opts)
		x.route(d)
		if !cancelOnLogout {
			go func() {
				for d.probes.Load() < 2 {
					time.Sleep(time.Millisecond)
				}
				cancel()
			}()
		}
		_, err := x.command(x.store).Execute(ctx, x.input("local", "kc"), localPrompter())
		return x, err
	}
	t.Run("the cluster token is revoked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			x, err := run(t, forcedSingleLogout(testrancher.Options{}), false)
			require.ErrorIs(t, err, context.Canceled)
			ce, d := failureOf(t, err)
			assert.Contains(t, ce.Message, "cancelled while waiting for the direct endpoint")
			assert.Contains(t, ce.Message, "the token it created was revoked")
			assert.Equal(t, ResultFailed, sessionCleanup(t, d))
			require.NotNil(t, d.ClusterToken)
			assert.Equal(t, ResultRevoked, clusterCleanup(t, d))
			assert.Empty(t, x.f.BearersOfKind("ext"))
		})
	})
	t.Run("the cluster token cannot be revoked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, err := run(t, testrancher.Options{ClusterDeleteMode: "fail"}, false)
			ce, d := failureOf(t, err)
			assert.Equal(t, ResultFailed, clusterCleanup(t, d))
			msg := clierr.ExpandRemedies(ce.Message, "")
			assert.Contains(t, msg, "the token it created could not be revoked; user 'user-e2e' can delete it from a directory signed in as user 'user-e2e'; run: rancher delete token '"+d.ClusterToken.Ref.Name+"' --apply --yes; or it expires at ")
		})
	})
	t.Run("Ctrl-C as the login token is being deleted", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			x, err := run(t, testrancher.Options{}, true)
			require.ErrorIs(t, err, context.Canceled)
			_, d := failureOf(t, err)
			assert.Equal(t, ResultRevoked, sessionCleanup(t, d), "the login token's delete is not cancelled")
			assert.Equal(t, ResultRevoked, clusterCleanup(t, d), "the wait sees the cancellation and revokes the cluster token")
			assert.Empty(t, x.f.BearersOfKind("ext"))
			assert.Empty(t, x.f.BearersOfKind("v3"))
		})
	})
}

// TestOtherProducersKeepTheirCancelledWaitDetails asserts the callers that
// are not the token reuse off sign-in - a stored login, the plugin-created
// login and the legacy shim's login with no cluster - keep the cancelled
// wait's details as they were: the cluster token's fate under clusterToken.
// The failed-delete text names the token, who can delete it and the command
// that person runs: the owner of an ext token from a directory signed in as
// that owner, an administrator for a v3 token. The details line says "cluster
// token cleanup:" when the create named a cluster and "token cleanup:" when it
// named none (the shim's token is the login's own, with no cluster).
func TestOtherProducersKeepTheirCancelledWaitDetails(t *testing.T) {
	const lead = "cancelled while waiting for the direct endpoint to accept the new credential; the token it created could not be revoked; "
	for _, tc := range []struct {
		name   string
		stored bool
		reuse  bool
		shim   bool
	}{{"a stored login", true, false, false}, {"the plugin-created login", false, true, false}, {"the legacy shim with no cluster", false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newInMemoryDirectEndpoint(t, 1000, "review")
				t.Setenv(execInfoEnv, "")
				opts := testrancher.Options{ClusterDeleteMode: "fail"}
				if tc.shim {
					opts = testrancher.Options{SessionDeleteMode: "fail"}
				}
				x := newInMemoryPluginFixture(t, tc.reuse, opts)
				x.route(d)
				if tc.stored {
					parent, err := AcquireLoginCredential(t.Context(), x.client(), LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0,
						ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
					require.NoError(t, err)
					seedEntry(t, x.dir, x.store, "stored", config.Server{URL: x.url, TokenRef: parent.Ref, TokenUserID: parent.UserID}, parent.Token, false)
				}
				t.Setenv(execInfoEnv, d.execInfo(t))
				ctx, cancel := context.WithCancel(t.Context())
				go func() {
					for d.probes.Load() < 2 {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}()
				var err error
				if tc.shim {
					_, err = x.command(x.store).runWithParams(ctx, runParams{Server: x.url, AuthProvider: "localProvider", legacyShim: true}, localPrompter())
				} else {
					_, err = x.command(x.store).Execute(ctx, x.input("local", "kc"), localPrompter())
				}
				require.ErrorIs(t, err, context.Canceled)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				details, ok := ce.Details.(directWaitDetails)
				require.True(t, ok, "the details are the cancelled wait's, got %T", ce.Details)
				require.NotNil(t, details.Fate)
				assert.Equal(t, ResultFailed, details.Fate.Cleanup.Result)
				require.NotNil(t, details.Fate.Ref)
				require.Equal(t, tc.shim, details.unscoped, "the create names a cluster for every producer but the shim")
				label := "cluster token cleanup: "
				if tc.shim {
					label = "token cleanup: "
				}
				lines := details.TextDetails()
				require.Len(t, lines, 1)
				assert.True(t, strings.HasPrefix(lines[0], label+"failed ("+details.Fate.Ref.String()+")"), "got %q", lines[0])
				msg := clierr.ExpandRemedies(ce.Message, "")
				require.True(t, strings.HasPrefix(msg, lead), "got %q", msg)
				_, remove := clierr.RecoveryCommands(*details.Fate.Ref)
				remove = clierr.ExpandRemedies(remove, "")
				switch details.Fate.Ref.Kind {
				case TokenKindExt:
					assert.Contains(t, msg, "user 'user-e2e' can delete it from a directory signed in as user 'user-e2e'; run: "+remove)
				default:
					assert.Contains(t, msg, details.Fate.Ref.String()+" expires")
					assert.Contains(t, msg, "an administrator can delete it sooner with: "+remove)
				}
			})
		})
	}
}

// TestShimWithUserAndClusterSignsInWithTheUsernameDefault asserts the legacy
// shim with --cluster signs in through the same function, so the exec
// entry's --user is the prompt's default, and the cluster token belongs to
// the user who signed in; without --cluster the shim keeps asking for the
// unscoped kubeconfig token.
func TestShimWithUserAndClusterSignsInWithTheUsernameDefault(t *testing.T) {
	t.Run("with --cluster", func(t *testing.T) {
		var mu sync.Mutex
		var responseTypes []string
		x := newPluginFixture(t, false, testrancher.Options{Users: entryTableUsers(), RequestHook: func(r *http.Request) {
			if r.URL.Path == "/v1-public/login" {
				mu.Lock()
				defer mu.Unlock()
				responseTypes = append(responseTypes, loginBodyResponseType(r))
			}
		}})
		var notices bytes.Buffer
		cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
		res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "user": "alice", "cluster": "c-test", "auth-provider": "localProvider"}},
			ui.New(&notices, newLineReader("", "pw-alice"), false, true))
		require.NoError(t, err)
		assert.Contains(t, notices.String(), "Enter username [alice]: ")
		owner, ok := x.f.BearerOwner(res.Data.(*execCredential).Status.Token)
		require.True(t, ok)
		assert.Equal(t, "user-alice", owner, "the default reached the login")
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{"json"}, responseTypes)
	})
	t.Run("without --cluster", func(t *testing.T) {
		var mu sync.Mutex
		var responseTypes []string
		x := newPluginFixture(t, false, testrancher.Options{Users: entryTableUsers(), RequestHook: func(r *http.Request) {
			if r.URL.Path == "/v1-public/login" {
				mu.Lock()
				defer mu.Unlock()
				responseTypes = append(responseTypes, loginBodyResponseType(r))
			}
		}})
		cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "user": "alice", "auth-provider": "localProvider"}},
			ui.New(&bytes.Buffer{}, newLineReader("", "pw-alice"), false, true))
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{"kubeconfig"}, responseTypes, "the unscoped kubeconfig token the shim has always asked for")
		creates := x.f.CreateRequests()
		require.Len(t, creates, 1, "the kubeconfig token alone: no cluster token is created")
		assert.Equal(t, "session", creates[0].Role)
	})
}

// loginBodyResponseType is the responseType of a login request, read and
// put back.
func loginBodyResponseType(r *http.Request) string {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body struct {
		ResponseType string `json:"responseType"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.ResponseType
}

// TestExecPluginSignInWithoutATerminalRefusesBeforeAnyToken asserts the
// no-terminal refusal and its remedies are the ones the call has always
// had: the sign-in stops before any token exists.
func TestExecPluginSignInWithoutATerminalRefusesBeforeAnyToken(t *testing.T) {
	x := newPluginFixture(t, false, testrancher.Options{})
	_, err := x.getToken(t, "c-test", "n", ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.True(t, strings.HasPrefix(ce.Message, "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token to: "), ce.Message)
	assert.Contains(t, ce.Message, "or run kubectl once in a terminal and sign in, which lasts until the cached token expires")
	assert.Nil(t, ce.Details)
	assert.Zero(t, x.f.LoginCount())
	assert.Zero(t, x.f.TokenCount())
}

// TestParallelSignInsEachDeleteTheirOwnLoginToken asserts that concurrent
// sign-ins with no lock between them each create and delete their own login
// token: after N calls the server holds N cluster tokens and no login token.
func TestParallelSignInsEachDeleteTheirOwnLoginToken(t *testing.T) {
	x := newPluginFixture(t, false, testrancher.Options{})
	const n = 3
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = x.command(x.store).Execute(t.Context(), x.input("c-test", ""), localPrompter())
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Len(t, x.f.BearersOfKind("ext"), n)
	assert.Empty(t, x.f.BearersOfKind("v3"))
}

// TestSignInWithNoExpiryCachesNothingAndSaysSo asserts a cluster token the
// server says never expires - an empty status.expiresAt on the ext create, an
// empty expiresAt on the Norman create - is returned with no expiration
// timestamp, is not cached and the notice says why.
func TestSignInWithNoExpiryCachesNothingAndSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts testrancher.Options
	}{
		{"ext", testrancher.Options{AuthTokenMaxTTLZero: true}},
		{"Norman", testrancher.Options{Legacy: true, V3TokenCreate: true, AuthTokenMaxTTLZero: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newPluginFixture(t, false, tc.opts)
			var notices bytes.Buffer
			res, err := x.command(x.store).Execute(t.Context(), x.input("c-test", "n"), ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true))
			require.NoError(t, err)
			cred := res.Data.(*execCredential)
			assert.NotEmpty(t, cred.Status.Token)
			assert.Nil(t, cred.Status.ExpirationTimestamp)
			assert.Contains(t, notices.String(), "not caching: the new token has no expiry")
			creds, err := x.store.ListCachedCreds()
			require.NoError(t, err)
			assert.Empty(t, creds, "an immortal token is never cached")
		})
	}
}
