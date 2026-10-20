package token

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// eventPrompter records, in order, every notice and prompt a sign-in gives
// and the point its BeforeSignIn hook ran, and answers like the prompter it
// wraps.
type eventPrompter struct {
	runtime.Prompter
	mu     sync.Mutex
	events []string
}

func newEventPrompter(inner runtime.Prompter) *eventPrompter { return &eventPrompter{Prompter: inner} }

func (e *eventPrompter) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *eventPrompter) Notice(msg string) {
	e.add("notice: " + msg)
	e.Prompter.Notice(msg)
}

func (e *eventPrompter) Prompt(ctx context.Context, label string) (string, error) {
	e.add("prompt: " + label)
	return e.Prompter.Prompt(ctx, label)
}

func (e *eventPrompter) PromptSecret(ctx context.Context, label string) (string, error) {
	e.add("prompt: " + label)
	return e.Prompter.PromptSecret(ctx, label)
}

// hook is the BeforeSignIn a test hands the sign-in.
func (e *eventPrompter) hook() { e.add("hook") }

// at is the position of the first event that contains text, -1 for none.
func (e *eventPrompter) at(text string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, ev := range e.events {
		if strings.Contains(ev, text) {
			return i
		}
	}
	return -1
}

// count is how many events contain text.
func (e *eventPrompter) count(text string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if strings.Contains(ev, text) {
			n++
		}
	}
	return n
}

// requireHookBefore asserts the hook ran exactly once and before the first
// event that contains text.
func (e *eventPrompter) requireHookBefore(t *testing.T, text string) {
	t.Helper()
	require.Equal(t, 1, e.count("hook"), "events %q", e.events)
	require.GreaterOrEqual(t, e.at(text), 0, "no event contains %q: %q", text, e.events)
	assert.Less(t, e.at("hook"), e.at(text), "events %q", e.events)
}

// terminalLines is the prompter of a call with a terminal that answers
// each prompt with the next line.
func terminalLines(lines ...string) *eventPrompter {
	return newEventPrompter(ui.New(io.Discard, newLineReader(lines...), false, true))
}

// TestSignInStartsRunsTheHookOnce asserts the hook the flows call is
// optional and runs at most once, however many flows reach it.
func TestSignInStartsRunsTheHookOnce(t *testing.T) {
	t.Parallel()
	(&loginInput{}).signInStarts(ui.Discard())

	var n int
	in := &loginInput{beforeSignIn: func() { n++ }}
	in.signInStarts(ui.Discard())
	in.signInStarts(ui.Discard())
	assert.Equal(t, 1, n)
}

// TestTheBasicSignInGivesTheHookBeforeItsFirstPrompt asserts a username and
// password sign-in with a terminal runs the hook right before the username
// prompt - after the provider menu when there is one - and a sign-in with no
// terminal refuses before the hook and before any prompt.
func TestTheBasicSignInGivesTheHookBeforeItsFirstPrompt(t *testing.T) {
	t.Parallel()
	t.Run("one provider", func(t *testing.T) {
		t.Parallel()
		srv := providersServer(t, "localProvider")
		p := terminalLines("alice", "secret")
		_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, BeforeSignIn: p.hook}, 0, p)
		require.Error(t, err, "the server has no login endpoint")
		p.requireHookBefore(t, "prompt: Enter username")
	})
	t.Run("several providers", func(t *testing.T) {
		t.Parallel()
		srv := providersServer(t, "localProvider", "openLdapProvider")
		p := terminalLines("0", "alice", "secret")
		_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, BeforeSignIn: p.hook}, 0, p)
		require.Error(t, err, "the server has no login endpoint")
		p.requireHookBefore(t, "prompt: Enter username")
		assert.Less(t, p.at("notice: Auth configs:"), p.at("hook"), "the menu comes first: the choice is made before the hook")
		assert.Less(t, p.at("prompt: Select auth config"), p.at("hook"))
	})
	t.Run("a prompt refused after the username", func(t *testing.T) {
		t.Parallel()
		p := newEventPrompter(usernameOnly{})
		_, err := basicAuth(t.Context(), failingTransportClient(t), &loginInput{server: "https://r.invalid", authProvider: "localProvider", beforeSignIn: func() { p.hook() }}, p)
		_, ok := errors.AsType[*needsTerminal](err)
		require.True(t, ok, "got %v", err)
		p.requireHookBefore(t, "prompt: Enter username")
	})
	t.Run("no terminal", func(t *testing.T) {
		t.Parallel()
		srv := providersServer(t, "localProvider")
		p := newEventPrompter(noTerminal())
		_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, BeforeSignIn: p.hook}, 0, p)
		_, ok := errors.AsType[*needsTerminal](err)
		require.True(t, ok, "got %v", err)
		assert.Empty(t, p.events, "no hook and no prompt")
	})
	t.Run("a prompter that cannot prompt refuses before any prompt", func(t *testing.T) {
		t.Parallel()
		p := newEventPrompter(cannotPrompt{})
		_, err := basicAuth(t.Context(), failingTransportClient(t), &loginInput{server: "https://r.invalid", authProvider: "localProvider", beforeSignIn: func() { p.hook() }}, p)
		nt, ok := errors.AsType[*needsTerminal](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, "local", nt.authConfig)
		require.ErrorContains(t, err, "run in a terminal, or use a cached --name")
		assert.Empty(t, p.events, "no hook and no prompt")
	})
}

// cannotPrompt reports that it cannot prompt and would still answer a
// prompt it is given, so a sign-in that prompts anyway shows in the events.
type cannotPrompt struct{}

func (cannotPrompt) Prompt(context.Context, string) (string, error)       { return "admin", nil }
func (cannotPrompt) PromptSecret(context.Context, string) (string, error) { return "secret", nil }
func (cannotPrompt) ReadLine(context.Context) (string, error)             { return "", ui.ErrNoInput }
func (cannotPrompt) Notice(string)                                        {}
func (cannotPrompt) CanPrompt() bool                                      { return false }
func (cannotPrompt) NoInput() bool                                        { return false }

// TestTheSAMLSignInGivesTheHookBeforeItsRequestID asserts the hook runs
// right before the notice that asks someone to open the login page, with
// no terminal as much as with one, and not at all under --no-input.
func TestTheSAMLSignInGivesTheHookBeforeItsRequestID(t *testing.T) {
	restore := SetSAMLTimingForTest(50*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(restore)
	srv := providersServer(t, "oktaProvider")

	p := newEventPrompter(noTerminal())
	_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, BeforeSignIn: p.hook}, 0, p)
	require.ErrorContains(t, err, "timed out")
	p.requireHookBefore(t, "Login Request Id")

	p = newEventPrompter(ui.Discard())
	_, err = AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, BeforeSignIn: p.hook}, 0, p)
	_, ok := errors.AsType[*needsTerminal](err)
	require.True(t, ok, "got %v", err)
	assert.Empty(t, p.events)
}

// TestTheOAuthDeviceCodeSignInGivesTheHookBeforeItsCode asserts the hook
// runs after the device-authorization request answered and right before
// the notice that carries the code, and does not run when that request
// fails, under --no-input, or without an identity provider client.
func TestTheOAuthDeviceCodeSignInGivesTheHookBeforeItsCode(t *testing.T) {
	t.Parallel()
	t.Run("the code is shown", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			so := splitOriginFacadeInMemory(t)
			p := newEventPrompter(interactivePrompter())
			_, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
				Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow, IdPClient: so.idpClient, BeforeSignIn: p.hook,
			}, 0, p)
			require.NoError(t, err)
			p.requireHookBefore(t, "To sign in, use a web browser")
		})
	})
	t.Run("the device-authorization request fails", func(t *testing.T) {
		t.Parallel()
		so := splitOriginFacade(t)
		p := newEventPrompter(interactivePrompter())
		_, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
			Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow, IdPClient: so.rancherClient, BeforeSignIn: p.hook,
		}, 0, p)
		require.ErrorContains(t, err, "certificate not trusted")
		assert.Empty(t, p.events, "no hook and no notice")
	})
	t.Run("no-input", func(t *testing.T) {
		t.Parallel()
		so := splitOriginFacade(t)
		p := newEventPrompter(ui.Discard())
		_, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
			Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow, IdPClient: so.idpClient, BeforeSignIn: p.hook,
		}, 0, p)
		_, ok := errors.AsType[*needsTerminal](err)
		require.True(t, ok, "got %v", err)
		assert.Empty(t, p.events)
	})
	t.Run("no identity provider client", func(t *testing.T) {
		t.Parallel()
		so := splitOriginFacade(t)
		p := newEventPrompter(interactivePrompter())
		_, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
			Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow, BeforeSignIn: p.hook,
		}, 0, p)
		require.ErrorContains(t, err, "identity provider client is not configured")
		assert.Empty(t, p.events)
	})
}

// TestTheOAuthAuthCodeSignInGivesTheHookBeforeTheBrowser asserts the hook
// runs right before the notice that the browser is opening, and the URL
// notice for a browser that did not open follows it; and that a callback
// port that is invalid or cannot be listened on fails before the hook.
func TestTheOAuthAuthCodeSignInGivesTheHookBeforeTheBrowser(t *testing.T) {
	so := splitOriginFacade(t)
	providers, _, _, err := getAuthProviders(t.Context(), so.rancherClient, so.rancherURL)
	require.NoError(t, err)
	provider, err := selectAuthProvider(t.Context(), providers, nil, "azureADProvider", false, ui.Discard())
	require.NoError(t, err)
	newInput := func(p *eventPrompter) *loginInput {
		return &loginInput{server: so.rancherURL, authProvider: "azureADProvider", idpClient: so.idpClient, beforeSignIn: p.hook}
	}

	t.Run("the browser opens", func(t *testing.T) {
		p := newEventPrompter(ui.Discard())
		browser := func(authURL string) error {
			u, err := url.Parse(authURL)
			if err != nil {
				return err
			}
			q := u.Query()
			resp, err := ownTransportClient(t).Get(q.Get("redirect_uri") + "?code=c-1&state=" + url.QueryEscape(q.Get("state")))
			if err != nil {
				return err
			}
			return resp.Body.Close()
		}
		_, err := oauthAuthCodeAuth(t.Context(), so.rancherClient, newInput(p), provider, 5*time.Second, browser, p)
		require.NoError(t, err)
		p.requireHookBefore(t, "Opening browser for authentication")
	})
	t.Run("the browser does not open", func(t *testing.T) {
		p := newEventPrompter(ui.Discard())
		browser := func(string) error { return errors.New("no display") }
		_, err := oauthAuthCodeAuth(t.Context(), so.rancherClient, newInput(p), provider, 50*time.Millisecond, browser, p)
		require.Error(t, err)
		p.requireHookBefore(t, "Opening browser for authentication")
		require.GreaterOrEqual(t, p.at("Failed to open browser automatically"), 0)
		assert.Less(t, p.at("hook"), p.at("Failed to open browser automatically"), "the URL notice follows the hook")
	})
	t.Run("an invalid callback port", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_OAUTH_CALLBACK_PORT", "70000")
		p := newEventPrompter(ui.Discard())
		_, err := oauthAuthCodeAuth(t.Context(), so.rancherClient, newInput(p), provider, time.Second, func(string) error { return nil }, p)
		require.ErrorContains(t, err, "callback port value must be between 0 and 65535")
		assert.Empty(t, p.events)
	})
	t.Run("a callback port in use", func(t *testing.T) {
		busy, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = busy.Close() })
		t.Setenv("RANCHER_CLI_OAUTH_CALLBACK_PORT", fmt.Sprint(busy.Addr().(*net.TCPAddr).Port))
		p := newEventPrompter(ui.Discard())
		_, err = oauthAuthCodeAuth(t.Context(), so.rancherClient, newInput(p), provider, time.Second, func(string) error { return nil }, p)
		require.ErrorContains(t, err, "failed to start local callback server")
		assert.Empty(t, p.events)
	})
}
