package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// pluginFixture is one facade, one config dir with the setting on, and a
// file store; reuse is a flag so the invariant test can turn it off.
type pluginFixture struct {
	f     *testrancher.Facade
	srv   *httptest.Server
	dir   string
	store config.CredStore
}

func newPluginFixture(t *testing.T, reuse bool, opts testrancher.Options) *pluginFixture {
	t.Helper()
	f, srv := newTokenFacade(t, opts)
	dir := t.TempDir()
	// The policy lives in the config, not the environment, so a test can
	// change it mid-flight and the revalidation sees the change.
	t.Setenv(config.EnvCredentialStore, "")
	cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: reuse, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return &pluginFixture{f: f, srv: srv, dir: dir, store: config.NewFileStore(dir)}
}

func (x *pluginFixture) getToken(t *testing.T, cluster, name string, p runtime.Prompter) (runtime.Result, error) {
	t.Helper()
	cmd := NewTokenCommand(x.dir, x.store).(runtime.ReadCommand)
	return cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "cluster": cluster, "name": name, "auth-provider": "localProvider"}}, p)
}

func (x *pluginFixture) entryName() string { return config.ExecLoginEntryName(x.srv.URL) }

func (x *pluginFixture) entry(t *testing.T) (config.Server, bool) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	s, ok := cfg.Servers[x.entryName()]
	return s, ok
}

func localPrompter() runtime.Prompter {
	return ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
}

// TestPluginLoginInvariantSettingOffStoresNothing asserts the default
// posture: with the setting off, the plugin-only lane runs the scoped
// interactive login (responseType kubeconfig_<cluster>), creates no
// server entry, writes no credential, and caches only name@server/cluster.
func TestPluginLoginInvariantSettingOffStoresNothing(t *testing.T) {
	var mu sync.Mutex
	var responseTypes []string
	x := newPluginFixture(t, false, testrancher.Options{RequestHook: func(r *http.Request) {
		if r.URL.Path == "/v1-public/login" {
			var body struct {
				ResponseType string `json:"responseType"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			mu.Lock()
			responseTypes = append(responseTypes, body.ResponseType)
			mu.Unlock()
		}
	}})
	res, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	assert.Equal(t, "ExecCredential", res.Kind)
	assert.Equal(t, []string{"kubeconfig_c-test"}, responseTypes)
	_, present := x.entry(t)
	assert.False(t, present)
	creds, _ := x.store.ListCachedCreds()
	require.Len(t, creds, 1)
	assert.Equal(t, "c-test", creds[0].Cluster)
	_, serr := os.Stat(config.CredentialsFilePath(x.dir))
	assert.True(t, os.IsNotExist(serr), "no login credential is written")
}

// TestPluginLoginFirstUseCreatesEntryAndClusterToken asserts the whole
// transaction: one provider login with responseType json, the derived
// token D stored under exec-login-<host> with provenance execLogin, its
// tokenRef, owner, and expiry, a marked cluster token created with D as
// bearer whose bearer is the ExecCredential, and the cache entry.
func TestPluginLoginFirstUseCreatesEntryAndClusterToken(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	res, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	assert.Equal(t, 1, x.f.LoginCount())
	cred := res.Data.(*execCredential)

	s, ok := x.entry(t)
	require.True(t, ok)
	assert.Equal(t, clierr.ProvenanceExecLogin, s.Provenance)
	require.NotNil(t, s.TokenRef)
	assert.Equal(t, clierr.TokenKindExt, s.TokenRef.Kind)
	assert.Equal(t, "user-e2e", s.TokenUserID)
	assert.NotEmpty(t, s.CredentialExpiresAt)
	assert.Empty(t, s.SupersededTokens)
	stored, err := x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.srv.URL})
	require.NoError(t, err)
	assert.Contains(t, stored, "ext/"+s.TokenRef.Name+":")
	assert.NotEqual(t, stored, cred.Status.Token, "the ExecCredential is the cluster token, never the login credential")

	creates := x.f.CreateRequests()
	require.Len(t, creates, 3, "login, derived create, cluster create")
	assert.Equal(t, "derived", creates[1].Role)
	assert.Equal(t, "cluster", creates[2].Role)
	assert.Equal(t, stored, creates[2].Bearer, "the cluster token is created with the plugin-created login as bearer")
	assert.Equal(t, "c-test", creates[2].ClusterName)
	marker := creates[1].Description[strings.Index(creates[1].Description, "[op:"):]
	assert.Contains(t, creates[2].Description, marker, "the cluster token carries the acquisition's marker")
	assert.Len(t, x.f.DeleteRequests(), 1, "S was revoked; nothing else")

	_, expiry, cerr := x.store.GetCachedCred(config.CacheKey("n", config.CanonicalServerIdentity(x.srv.URL), "c-test"))
	require.NoError(t, cerr)
	assert.False(t, expiry.IsZero())
}

// TestPluginLoginSecondClusterIsSilent asserts the reuse: a second
// cluster on the same server creates its token from the stored plugin
// login with no prompt and an unmarked description.
func TestPluginLoginSecondClusterIsSilent(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	_, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	var errw bytes.Buffer
	res, err := x.getToken(t, "local", "n", ui.New(&errw, newLineReader(), true, false))
	require.NoError(t, err)
	assert.Equal(t, 1, x.f.LoginCount(), "no second login")
	last := x.f.CreateRequests()[len(x.f.CreateRequests())-1]
	assert.Equal(t, "cluster", last.Role)
	assert.Equal(t, "local", last.ClusterName)
	assert.NotContains(t, last.Description, "[op:", "a routine per-cluster create stays unmarked")
	assert.NotEmpty(t, res.Data.(*execCredential).Status.Token)
}

// TestPluginLoginPostAcquisitionFailuresRevokeInOrderAndPersistNothing
// asserts every boundary after acquisition: the cluster token's fate is
// revoked with the plugin-created login as bearer first, then the login
// credential revokes itself; no entry, no credential, no cache entry.
func TestPluginLoginPostAcquisitionFailuresRevokeInOrderAndPersistNothing(t *testing.T) {
	other := "c-other"
	cases := []struct {
		name      string
		opts      testrancher.Options
		storeErr  error
		wantStage clierr.LoginStage
		wantCT    bool // a cluster-token fate with a ref is expected
	}{
		{"scope mismatch", testrancher.Options{ScopedCreateClusterName: &other}, nil, clierr.StageClusterToken, true},
		{"cluster create 5xx", testrancher.Options{ScopedCreateStatus: http.StatusBadGateway}, nil, clierr.StageClusterToken, false},
		{"cluster create 403", testrancher.Options{ScopedCreateStatus: http.StatusForbidden}, nil, clierr.StageClusterToken, false},
		{"credential write fails", testrancher.Options{}, errors.New("keyring refused"), clierr.StageCommit, true},
		{"read-only store", testrancher.Options{}, errors.Join(config.ErrTokenStoreUnsupported, errors.New("helper: read-only")), clierr.StageCommit, true},
		{"legacy v3 credential write fails", testrancher.Options{Legacy: true, V3TokenCreate: true, LegacyLoginBodyUserID: true}, errors.New("keyring refused"), clierr.StageCommit, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newPluginFixture(t, true, tc.opts)
			cmd := &tokenCmd{dir: x.dir, store: x.store}
			if tc.storeErr != nil {
				cmd.openStore = func(string, string) (config.CredStore, string, error) {
					return failingStore{CredStore: x.store, err: tc.storeErr}, "", nil
				}
			}
			_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "cluster": "c-test", "name": "n", "auth-provider": "localProvider"}}, localPrompter())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			d, ok := ce.Details.(clierr.LoginFailureDetails)
			require.True(t, ok)
			assert.Equal(t, tc.wantStage, d.Stage)
			if tc.name == "read-only store" {
				assert.Contains(t, ce.Message, "set-token-reuse off")
				assert.Contains(t, ce.Message, "file")
			}
			// Order: the cluster token (D as bearer), then D itself.
			deletes := x.f.DeleteRequests()
			stored := deletesOfRole(x.f, deletes)
			if tc.wantCT {
				require.NotNil(t, d.ClusterToken)
				assert.Equal(t, clierr.ResultRevoked, d.ClusterToken.Cleanup.Result)
				// A legacy server has no derived token: the session token is
				// the credential itself, so there are two deletes, not three.
				least := 3
				if tc.opts.Legacy {
					least = 2
				}
				require.GreaterOrEqual(t, len(deletes), least, "the cluster token and the credential")
				assert.Equal(t, "cluster", stored[len(deletes)-2], "the cluster token is revoked before the credential")
				assert.Equal(t, deletes[len(deletes)-2].Bearer, deletes[len(deletes)-1].Bearer, "both deletes bear the plugin-created login")
			}
			credFate := d.DerivedToken
			if tc.opts.Legacy {
				credFate = d.SessionToken
			}
			require.NotNil(t, credFate)
			assert.Equal(t, clierr.ResultRevoked, credFate.Cleanup.Result)
			if tc.name == "cluster create 5xx" {
				// The facade answered after creating the record: the CLI
				// never learned the name, so the token survives as an
				// unknown fate with the marker search in the recovery text.
				assert.Equal(t, clierr.CreateUnknown, d.ClusterToken.Outcome)
				assert.Len(t, x.f.TokensWithDescriptionContaining("ext", "[op:"+d.OperationMarker+"]"), 1)
				assert.Contains(t, d.Recovery, "[op:"+d.OperationMarker+"]")
			} else {
				assert.Equal(t, 0, x.f.TokenCount(), "no token this operation created survives")
			}
			if tc.opts.Legacy {
				for _, del := range deletes[len(deletes)-2:] {
					assert.Equal(t, "v3", del.Kind, "a legacy server's tokens are revoked on the v3 path")
				}
			}
			_, present := x.entry(t)
			assert.False(t, present)
			_, gerr := x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.srv.URL})
			require.ErrorIs(t, gerr, config.ErrNoToken)
			creds, _ := x.store.ListCachedCreds()
			assert.Empty(t, creds)
		})
	}
}

// failingStore fails the commit with err.
type failingStore struct {
	config.CredStore
	err error
}

func (s failingStore) SetTokenWithOutcome(config.ServerRef, string) (config.CommitOutcome, error) {
	return config.CommitOutcome{Backend: clierr.BackendFile, State: config.CommitNotWritten}, s.err
}
func (s failingStore) SetToken(ref config.ServerRef, tok string) error {
	_, err := s.SetTokenWithOutcome(ref, tok)
	return err
}

// deletesOfRole maps each recorded delete to the role of the token it
// named, using the create log.
func deletesOfRole(f *testrancher.Facade, deletes []testrancher.DeleteRecord) []string {
	roleByName := map[string]string{}
	for _, c := range f.CreateRequests() {
		roleByName[c.Name] = c.Role
	}
	out := make([]string, len(deletes))
	for i, d := range deletes {
		out[i] = roleByName[d.Name]
	}
	return out
}

// TestPluginLoginAcquisitionFailureIsReportedAsAcquisition asserts a
// failure inside acquisition is reported as acquisition reports it, with no
// cluster token attempted and no entry or credential written.
func TestPluginLoginAcquisitionFailureIsReportedAsAcquisition(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{DerivedCreateStatus: http.StatusInternalServerError})
	_, err := x.getToken(t, "c-test", "n", localPrompter())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	d := ce.Details.(clierr.LoginFailureDetails)
	assert.Equal(t, clierr.StageAuthentication, d.Stage)
	assert.Nil(t, d.ClusterToken)
	assert.Equal(t, clierr.CreateUnknown, d.DerivedToken.Outcome)
	for _, c := range x.f.CreateRequests() {
		assert.NotEqual(t, "cluster", c.Role)
	}
	_, present := x.entry(t)
	assert.False(t, present)
}

// flippingPrompter answers the password prompt and then mutates the config
// so the commit-time revalidation sees a change made during the interaction.
type flippingPrompter struct {
	runtime.Prompter
	flip func()
}

func (p flippingPrompter) PromptSecret(ctx context.Context, msg string) (string, error) {
	s, err := p.Prompter.PromptSecret(ctx, msg)
	p.flip()
	return s, err
}

// TestPluginLoginRevalidationAbortsAndRevokesBoth asserts the revalidation
// rule end to end: the setting turned off, or the store policy switched to
// the plaintext file without disclosure, during the interactive login ⇒
// nothing is written, and both created tokens are revoked.
func TestPluginLoginRevalidationAbortsAndRevokesBoth(t *testing.T) {
	cases := []struct {
		name string
		flip func(dir string)
	}{
		{"setting off", func(dir string) {
			cfg, _ := config.Load(config.FilePath(dir))
			cfg.TokenReuse = false
			_ = cfg.Write()
		}},
		{"policy to file undisclosed", func(dir string) {
			cfg, _ := config.Load(config.FilePath(dir))
			cfg.CredentialStore = "file"
			_ = cfg.Write()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newPluginFixture(t, true, testrancher.Options{})
			if tc.name == "policy to file undisclosed" {
				// Start from the keychain so no disclosure is given.
				t.Setenv(config.EnvCredentialStore, "")
				cfg, _ := config.Load(config.FilePath(x.dir))
				cfg.CredentialStore = "keychain"
				require.NoError(t, cfg.Write())
				keyring.MockInit()
			}
			p := flippingPrompter{Prompter: localPrompter(), flip: func() { tc.flip(x.dir) }}
			_, err := x.getToken(t, "c-test", "n", p)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			d := ce.Details.(clierr.LoginFailureDetails)
			assert.Equal(t, clierr.StageCommit, d.Stage)
			assert.Equal(t, clierr.ConfigNotWritten, d.ConfigState)
			assert.Equal(t, clierr.ResultRevoked, d.ClusterToken.Cleanup.Result)
			assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result)
			assert.Equal(t, 0, x.f.TokenCount())
			_, present := x.entry(t)
			assert.False(t, present)
		})
	}
}

// TestPluginLoginPlaintextDisclosurePrecedesTheLogin asserts the file
// backend's warning names credentials.yaml and is emitted before the
// login POST.
func TestPluginLoginPlaintextDisclosurePrecedesTheLogin(t *testing.T) {
	var order []string
	var mu sync.Mutex
	x := newPluginFixture(t, true, testrancher.Options{RequestHook: func(r *http.Request) {
		if r.URL.Path == "/v1-public/login" {
			mu.Lock()
			order = append(order, "login")
			mu.Unlock()
		}
	}})
	var errw bytes.Buffer
	p := noticeOrder{Prompter: ui.New(&errw, newLineReader("e2e-user", "e2e-pass"), false, true), record: func(msg string) {
		if strings.Contains(msg, "credentials.yaml") {
			mu.Lock()
			order = append(order, "disclosure")
			mu.Unlock()
		}
	}}
	_, err := x.getToken(t, "c-test", "n", p)
	require.NoError(t, err)
	assert.Equal(t, []string{"disclosure", "login"}, order)
}

// TestPluginLoginFirstUseReportsTheStoreItPins asserts the store-selection
// disclosure survives the clean first use. Under an auto policy with no pin
// and no matching entry, nothing resolves a backend before the plugin login
// takes the flock, so the open inside it is the probe that pins: its notice
// must reach the result's warnings rather than being discarded.
func TestPluginLoginFirstUseReportsTheStoreItPins(t *testing.T) {
	keyring.MockInit()
	config.ResetKeyringUsableCacheForTest(t)
	x := newPluginFixture(t, true, testrancher.Options{})
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	cfg.CredentialStore = "auto"
	require.NoError(t, cfg.Write())
	require.Empty(t, config.ReadPin(x.dir))

	res, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, config.NoticeFirstPinKeychain)
	assert.Equal(t, "keychain", config.ReadPin(x.dir))
}

type noticeOrder struct {
	runtime.Prompter
	record func(string)
}

func (n noticeOrder) Notice(msg string) { n.record(msg); n.Prompter.Notice(msg) }

// TestPluginLogin401RefreshesExactlyOnceAsAnUpdate asserts an expired
// plugin-created login is replaced by one re-login under the same entry:
// the new tokenRef, owner, and expiry are written, the prior token is
// retired with the new credential, and a second 401 on the fresh
// credential is a hard error with no second prompt.
func TestPluginLogin401RefreshesExactlyOnceAsAnUpdate(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	_, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	before, _ := x.entry(t)
	// Expire the stored credential server-side: delete D on the facade.
	require.NoError(t, x.store.SetToken(config.ServerRef{Name: x.entryName(), URL: x.srv.URL}, "ext/"+before.TokenRef.Name+":wrong"))

	// "testcluster" resolves through the cluster list, so the 401 that
	// triggers the refresh comes from the list request, not the create.
	res, err := x.getToken(t, "testcluster", "n2", localPrompter())
	require.NoError(t, err)
	assert.Equal(t, 2, x.f.LoginCount(), "exactly one re-login")
	after, _ := x.entry(t)
	assert.NotEqual(t, before.TokenRef.Name, after.TokenRef.Name)
	assert.Equal(t, clierr.ProvenanceExecLogin, after.Provenance)
	assert.Empty(t, after.SupersededTokens, "the prior token was retired")
	fresh, err := x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.srv.URL})
	require.NoError(t, err)
	var retired bool
	for _, d := range x.f.DeleteRequests() {
		if d.Name == before.TokenRef.Name {
			retired = true
			assert.Equal(t, fresh, d.Bearer, "the prior token is revoked with the new credential")
		}
	}
	assert.True(t, retired, "the prior token was deleted on the server")
	assert.NotEmpty(t, res.Data.(*execCredential).Status.Token)

	t.Run("a fresh credential rejected on the cluster create is a hard error", func(t *testing.T) {
		// One prompt, then the failure: no second login.
		y := newPluginFixture(t, true, testrancher.Options{ScopedCreateStatus: http.StatusUnauthorized})
		_, err := y.getToken(t, "c-test", "n", localPrompter())
		require.Error(t, err)
		assert.Equal(t, 1, y.f.LoginCount())
		ce, _ := errors.AsType[*clierr.Error](err)
		assert.Equal(t, clierr.StageClusterToken, ce.Details.(clierr.LoginFailureDetails).Stage)
	})

	t.Run("a fresh credential rejected on the cluster list is a hard error", func(t *testing.T) {
		// A fallthrough that rejects every bearer makes the fresh
		// credential's cluster resolution answer 401.
		z := newPluginFixture(t, true, testrancher.Options{Fallthrough: clustersFallthrough(func(string) bool { return false })})
		_, err := z.getToken(t, "testcluster", "n", localPrompter())
		require.Error(t, err)
		assert.Equal(t, 1, z.f.LoginCount())
		ce, _ := errors.AsType[*clierr.Error](err)
		assert.Equal(t, clierr.StageClusterToken, ce.Details.(clierr.LoginFailureDetails).Stage)
		assert.Equal(t, 0, z.f.TokenCount(), "both tokens revoked")
	})
}

// TestPluginLoginExecHelperSuccessHasNoFailureEnvelope asserts a
// helperNonAtomic commit (every exec-helper write) is a success at the
// plugin boundary: the entry is written and no failure shape is emitted.
func TestPluginLoginExecHelperSuccessHasNoFailureEnvelope(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	cmd := &tokenCmd{dir: x.dir, store: x.store}
	cmd.openStore = func(string, string) (config.CredStore, string, error) {
		return outcomeStore{CredStore: x.store, out: config.CommitOutcome{Backend: clierr.BackendExec, State: config.CommitHelperNonAtomic, PriorSource: clierr.PriorUnknown, HelperRecordRetained: true}}, "", nil
	}
	res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "cluster": "c-test", "name": "n", "auth-provider": "localProvider"}}, localPrompter())
	require.NoError(t, err)
	assert.Equal(t, "ExecCredential", res.Kind)
	s, ok := x.entry(t)
	require.True(t, ok)
	assert.Equal(t, clierr.ProvenanceExecLogin, s.Provenance)
}

// outcomeStore reports a fixed successful outcome and stores through the
// wrapped store.
type outcomeStore struct {
	config.CredStore
	out config.CommitOutcome
}

func (s outcomeStore) SetTokenWithOutcome(ref config.ServerRef, tok string) (config.CommitOutcome, error) {
	if err := s.SetToken(ref, tok); err != nil {
		return config.CommitOutcome{Backend: s.out.Backend, State: config.CommitNotWritten}, err
	}
	return s.out, nil
}

// TestPluginLogin403NeverPrompts asserts a plugin-created login that
// authenticates but cannot create tokens surfaces RBAC with no login.
func TestPluginLogin403NeverPrompts(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{ScopedCreateStatus: http.StatusForbidden})
	cred, err := AcquireLoginCredential(t.Context(), x.srv.Client(), LoginRequest{Server: x.srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0, localPrompter())
	require.NoError(t, err)
	base := x.f.LoginCount()
	seedEntry(t, x.dir, x.store, x.entryName(), config.Server{URL: x.srv.URL, Provenance: clierr.ProvenanceExecLogin, TokenRef: cred.Ref, TokenUserID: cred.UserID}, cred.Token, true)
	_, err = x.getToken(t, "c-test", "n", localPrompter())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, base, x.f.LoginCount(), "a 403 never prompts")
}

// TestPluginLoginSettingOffSkipsTheDormantEntry asserts turning the
// setting off after a plugin-created login exists restores the scoped
// interactive lane without deleting the credential, and a same-URL user
// login added later is used regardless of the setting.
func TestPluginLoginSettingOffSkipsTheDormantEntry(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	_, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	cfg, _ := config.Load(config.FilePath(x.dir))
	cfg.TokenReuse = false
	require.NoError(t, cfg.Write())

	_, err = x.getToken(t, "local", "n", localPrompter())
	require.NoError(t, err)
	assert.Equal(t, 2, x.f.LoginCount(), "per-cluster interactive resumes")
	stored, err := x.store.GetToken(config.ServerRef{Name: x.entryName(), URL: x.srv.URL})
	require.NoError(t, err)
	assert.NotEmpty(t, stored, "turning the setting off never deletes the credential")

	// A user entry at the same URL is used with the setting off.
	userCred, err := AcquireLoginCredential(t.Context(), x.srv.Client(), LoginRequest{Server: x.srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0, localPrompter())
	require.NoError(t, err)
	seedEntry(t, x.dir, x.store, "prod", config.Server{URL: x.srv.URL, Provenance: clierr.ProvenanceDerived, TokenRef: userCred.Ref}, userCred.Token, false)
	logins := x.f.LoginCount()
	_, err = x.getToken(t, "c-test", "n3", ui.New(&bytes.Buffer{}, newLineReader(), true, false))
	require.NoError(t, err)
	assert.Equal(t, logins, x.f.LoginCount(), "the user credential creates silently")
}

// TestPluginLoginConcurrentFirstUseAndExpiry asserts one provider login,
// one stored credential, and two ExecCredentials for two concurrent
// invocations, both on first use and on a shared expired credential; the
// waiter never prompts.
func TestPluginLoginConcurrentFirstUseAndExpiry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "first use"
		if expired {
			name = "expiry"
		}
		t.Run(name, func(t *testing.T) {
			x := newPluginFixture(t, true, testrancher.Options{})
			if expired {
				_, err := x.getToken(t, "c-test", "seed", localPrompter())
				require.NoError(t, err)
				s, _ := x.entry(t)
				require.NoError(t, x.store.SetToken(config.ServerRef{Name: x.entryName(), URL: x.srv.URL}, "ext/"+s.TokenRef.Name+":wrong"))
			}
			base := x.f.LoginCount()
			var prompts atomic.Int32
			mk := func() runtime.Prompter {
				return countingPrompter{Prompter: localPrompter(), n: &prompts}
			}
			var wg sync.WaitGroup
			results := make([]error, 2)
			clusters := []string{"c-test", "local"}
			for i := range 2 {
				wg.Go(func() {
					_, results[i] = x.getToken(t, clusters[i], "n", mk())
				})
			}
			wg.Wait()
			require.NoError(t, results[0])
			require.NoError(t, results[1])
			assert.Equal(t, base+1, x.f.LoginCount(), "one provider login")
			assert.Equal(t, int32(2), prompts.Load(), "one username and one password prompt in total")
			cfg, _ := config.Load(config.FilePath(x.dir))
			n := 0
			for name := range cfg.Servers {
				if config.IsExecLoginEntryName(name) {
					n++
				}
			}
			assert.Equal(t, 1, n, "one plugin login entry")
			creds, _ := x.store.ListCachedCreds()
			assert.GreaterOrEqual(t, len(creds), 2)
		})
	}
}

type countingPrompter struct {
	runtime.Prompter
	n *atomic.Int32
}

func (c countingPrompter) Prompt(ctx context.Context, msg string) (string, error) {
	c.n.Add(1)
	return c.Prompter.Prompt(ctx, msg)
}
func (c countingPrompter) PromptSecret(ctx context.Context, msg string) (string, error) {
	c.n.Add(1)
	return c.Prompter.PromptSecret(ctx, msg)
}

// TestPluginLoginWaiterTimesOutBehindAnAbandonedLogin asserts a waiter
// behind a holder stuck in its prompt fails LOCK_TIMEOUT, retryable,
// without the no-lock hint, and never starts a second login.
func TestPluginLoginWaiterTimesOutBehindAnAbandonedLogin(t *testing.T) {
	restore := config.SetLockWaitTimeoutForTest(300 * time.Millisecond)
	t.Cleanup(restore)
	x := newPluginFixture(t, true, testrancher.Options{})
	inPrompt, release := make(chan struct{}, 1), make(chan struct{})
	holder := blockingPrompter{Prompter: localPrompter(), entered: inPrompt, release: release}
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = x.getToken(t, "c-test", "a", holder) })
	<-inPrompt
	_, err := x.getToken(t, "local", "b", localPrompter())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Empty(t, ce.Hint)
	assert.Equal(t, 0, x.f.LoginCount(), "the waiter never logged in; the holder has not finished")
	close(release)
	wg.Wait()
	assert.Equal(t, 1, x.f.LoginCount())
}

type blockingPrompter struct {
	runtime.Prompter
	entered, release chan struct{}
}

func (b blockingPrompter) Prompt(ctx context.Context, msg string) (string, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return b.Prompter.Prompt(ctx, msg)
}

// TestPluginLoginCancelledWinnerRevokesWhatItCreated asserts a caller
// cancelled during the cluster-token create revokes the tokens it created
// on the cleanup context and persists nothing.
func TestPluginLoginCancelledWinnerRevokesWhatItCreated(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	// The facade has already consumed the body by the time the hook runs,
	// so the cluster-token create is identified by its position: the
	// credential's create is first, the cluster token's second.
	var creates atomic.Int32
	x := newPluginFixture(t, true, testrancher.Options{DerivedCreateHook: func(r *http.Request) {
		if creates.Add(1) == 2 {
			cancel()
			<-r.Context().Done()
		}
	}})
	cmd := NewTokenCommand(x.dir, x.store).(runtime.ReadCommand)
	_, err := cmd.Execute(ctx, tin{s: map[string]string{"server": x.srv.URL, "cluster": "c-test", "name": "n", "auth-provider": "localProvider"}}, localPrompter())
	require.ErrorIs(t, err, context.Canceled)
	ce, _ := errors.AsType[*clierr.Error](err)
	d := ce.Details.(clierr.LoginFailureDetails)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result, "the credential is revoked on the cleanup context despite the cancellation")
	// The facade created the cluster token after the client gave up: the
	// CLI never learned its name, so its fate is unknown with the marker
	// and the recovery text carries the marker search.
	require.NotNil(t, d.ClusterToken)
	assert.Equal(t, clierr.CreateUnknown, d.ClusterToken.Outcome)
	assert.Contains(t, d.Recovery, "[op:"+d.OperationMarker+"]")
	// The facade creates the record after the client gave up, on its own
	// goroutine: wait for it rather than race it.
	require.Eventually(t, func() bool {
		return len(x.f.TokensWithDescriptionContaining("ext", "[op:"+d.OperationMarker+"]")) == 1
	}, 2*time.Second, 10*time.Millisecond, "only the cluster token the CLI could not name remains")
	_, present := x.entry(t)
	assert.False(t, present)
}

// TestPluginLoginNoRequestWhileTheConfigLockIsHeld asserts across the whole
// flow that every request the facade sees arrives while the config lock is
// free (the plugin-login flock is held by design; the config lock is
// not).
func TestPluginLoginNoRequestWhileTheConfigLockIsHeld(t *testing.T) {
	var violations atomic.Int32
	var dir string
	x := newPluginFixture(t, true, testrancher.Options{RequestHook: func(*http.Request) {
		fl := flock.New(config.LockPath(dir))
		ok, err := fl.TryLock()
		if err != nil || !ok {
			violations.Add(1)
			return
		}
		_ = fl.Unlock()
	}})
	dir = x.dir
	_, err := x.getToken(t, "c-test", "n", localPrompter())
	require.NoError(t, err)
	_, err = x.getToken(t, "local", "n", localPrompter())
	require.NoError(t, err)
	assert.Equal(t, int32(0), violations.Load())
}
