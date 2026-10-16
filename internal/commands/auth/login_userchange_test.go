package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

const userChangeWarning = "server entry prod is now logged in as user user-bob; it was logged in as user user-alice"

// userChangeFixture is a Rancher with two local users and a directory with
// the server entry prod.
type userChangeFixture struct {
	t     *testing.T
	dir   string
	store config.CredStore
	srv   *httptest.Server
	cmd   *loginCmd
}

func newUserChangeFixture(t *testing.T) *userChangeFixture {
	t.Helper()
	f := testrancher.New(testrancher.Options{Users: []testrancher.User{
		{Name: "alice", Password: "pw", UserID: "user-alice"},
		{Name: "bob", Password: "pw", UserID: "user-bob"},
	}})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", true)
	return &userChangeFixture{t: t, dir: dir, store: store, srv: srv, cmd: newPlainLoginCmd(dir, store)}
}

// userChange is the warnings that say who the entry is logged in as now: the
// result carries others, such as the notice of a superseded token the login
// could not retire.
func userChange(warnings []string) []string {
	var out []string
	for _, w := range warnings {
		if strings.HasPrefix(w, "server entry prod is now logged in as") {
			out = append(out, w)
		}
	}
	return out
}

// signIn logs in to prod through the local auth config as user and returns
// the result's warnings.
func (x *userChangeFixture) signIn(user string) []string {
	x.t.Helper()
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "local"}, Pos: []string{"prod"}}
	res, err := x.cmd.Execute(x.t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader(user, "pw"), false, true))
	require.NoError(x.t, err)
	return res.Warnings
}

// pasteToken logs in to prod with a token that is stored without asking the
// server who owns it, so the entry records no user.
func (x *userChangeFixture) pasteToken() []string {
	x.t.Helper()
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true, "no-verify": true}, Pos: []string{"prod"}}
	res, err := x.cmd.Execute(x.t.Context(), in, cmdtest.StdinPrompter("id:secret"))
	require.NoError(x.t, err)
	return res.Warnings
}

// TestALoginAsAnotherUserWarnsAndSucceeds asserts a login that replaces the
// stored login of another user succeeds and says, in the result's warnings,
// who the server entry is logged in as now and who it was.
func TestALoginAsAnotherUserWarnsAndSucceeds(t *testing.T) {
	t.Parallel()
	x := newUserChangeFixture(t)
	assert.Empty(t, userChange(x.signIn("alice")), "a first login says nothing")
	assert.Equal(t, []string{userChangeWarning}, userChange(x.signIn("bob")))
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	assert.Equal(t, "user-bob", cfg.Servers["prod"].TokenUserID, "the login still succeeds and records the new user")
}

// TestALoginAsTheSameUserSaysNothing asserts a re-login as the user the entry
// recorded gives no warning.
func TestALoginAsTheSameUserSaysNothing(t *testing.T) {
	t.Parallel()
	x := newUserChangeFixture(t)
	x.signIn("alice")
	assert.Empty(t, userChange(x.signIn("alice")))
}

// TestALoginAfterALogoutSaysNothing asserts a login after auth logout gives
// no warning, whoever the user: no stored login is left to replace.
func TestALoginAfterALogoutSaysNothing(t *testing.T) {
	t.Parallel()
	x := newUserChangeFixture(t)
	x.signIn("alice")
	_, err := (&logoutCmd{dir: x.dir}).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	assert.Empty(t, userChange(x.signIn("bob")))
}

// TestALoginWithAnUnknownUserIDSaysNothing asserts no warning when the entry
// recorded no user (a token stored without checking it) or when the new
// login has none.
func TestALoginWithAnUnknownUserIDSaysNothing(t *testing.T) {
	t.Parallel()
	t.Run("the old login recorded none", func(t *testing.T) {
		t.Parallel()
		x := newUserChangeFixture(t)
		assert.Empty(t, userChange(x.pasteToken()))
		assert.Empty(t, userChange(x.signIn("alice")))
	})
	t.Run("the new login records none", func(t *testing.T) {
		t.Parallel()
		x := newUserChangeFixture(t)
		x.signIn("alice")
		assert.Empty(t, userChange(x.pasteToken()))
	})
}

// afterPassword is a prompter that runs fn when the password is asked for:
// another process's login landing while this one waits for its sign-in.
type afterPassword struct {
	runtime.Prompter
	fn func()
}

func (p afterPassword) PromptSecret(ctx context.Context, label string) (string, error) {
	p.fn()
	return p.Prompter.PromptSecret(ctx, label)
}

// TestTheReplacedUserIsTheOneUnderTheConfigLock asserts the user the warning
// names as the old one is the one the entry records when the login commits,
// not the one it recorded before the sign-in: a login of another process that
// lands in between is the one replaced.
func TestTheReplacedUserIsTheOneUnderTheConfigLock(t *testing.T) {
	t.Parallel()
	x := newUserChangeFixture(t)
	x.signIn("alice")
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "local"}, Pos: []string{"prod"}}
	p := afterPassword{Prompter: ui.New(&bytes.Buffer{}, newLineReader("bob", "pw"), false, true), fn: func() {
		cfg, err := config.Load(config.FilePath(x.dir))
		require.NoError(t, err)
		s := cfg.Servers["prod"]
		s.TokenUserID = "user-carol"
		cfg.Servers["prod"] = s
		require.NoError(t, cfg.Write())
	}}
	res, err := x.cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Equal(t, []string{"server entry prod is now logged in as user user-bob; it was logged in as user user-carol"}, userChange(res.Warnings))
}

// TestTheUserChangeWarningRidesTheTextResult asserts the warning is part of
// the result, which the text form writes to stderr and the envelope carries
// in its warnings.
func TestTheUserChangeWarningRidesTheTextResult(t *testing.T) {
	t.Parallel()
	x := newUserChangeFixture(t)
	x.signIn("alice")
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "local"}, Pos: []string{"prod"}}
	res, err := x.cmd.Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("bob", "pw"), false, true))
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, userChangeWarning)
	assert.Equal(t, "LoginResult", res.Kind)
}

// TestAFailedReLoginSaysNothingAboutTheUser asserts a re-login as another
// user whose credential cannot be saved fails without saying the server
// entry is now logged in as that user: the failure carries the warnings
// gathered before it, and the entry still holds the old login. Mutation:
// add the warning whether or not the login was saved.
func TestAFailedReLoginSaysNothingAboutTheUser(t *testing.T) {
	t.Parallel()
	x := newUserChangeFixture(t)
	assert.Empty(t, userChange(x.signIn("alice")), "a first login says nothing")
	failing := &loginCmd{dir: x.dir, newStore: func(string, string) (config.CredStore, string, error) {
		return cmdtest.SetErrStore{CredStore: x.store, SetErr: errors.New("keychain refused")}, "", nil
	}}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "local"}, Pos: []string{"prod"}}
	_, err := failing.Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("bob", "pw"), false, true))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, userChange(ce.Warnings()))
}
