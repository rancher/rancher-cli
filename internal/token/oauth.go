package token

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	iruntime "github.com/rancher/rancher-cli/internal/runtime"
	"golang.org/x/oauth2"
)

const (
	oauthCodeFlowTimeout          = 5 * time.Minute
	oauthCodeExchangeTimeout      = 30 * time.Second
	callbackServerShutdownTimeout = time.Second
	// maxCallbackParamLen bounds an untrusted callback query param before it
	// reaches the HTML response or the CLI's own error message.
	maxCallbackParamLen = 200
)

type openBrowserFunc func(url string) error

type callbackResult struct {
	code  string
	state string
	err   error
}

// firstNonEmptyEnv returns the value of the primary env var when non-empty,
// otherwise the value of the legacy env var. Used for the "modern spelling,
// then legacy CATTLE_* fallback" pattern that both resolveAuthFlow and
// getCallbackPort share.
func firstNonEmptyEnv(primary, legacy string) string {
	if v := os.Getenv(primary); v != "" {
		return v
	}
	return os.Getenv(legacy)
}

// resolveAuthFlow returns the effective OAuth auth-flow: the flag value wins
// when non-empty; otherwise RANCHER_CLI_OAUTH_AUTH_FLOW; otherwise
// CATTLE_OAUTH_AUTH_FLOW (legacy spelling). This is the single source of truth
// for both the native get-token path and the legacy token shim — both funnel
// through oauthAuth, so the env resolution lives here once.
func resolveAuthFlow(flag string) string {
	if flag != "" {
		return flag
	}
	return firstNonEmptyEnv(config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow)
}

func oauthAuth(ctx context.Context, client *http.Client, input *loginInput, provider typedProvider, p iruntime.Prompter) (*Token, error) {
	input.authFlow = strings.ToLower(resolveAuthFlow(input.authFlow))

	if p.NoInput() {
		return nil, clierr.New(clierr.CodeAuth, "interactive login required for "+input.authProvider).
			WithHint("run without --no-input, or use a cached --name")
	}

	switch input.authFlow {
	case authCodeFlow:
		return oauthAuthCodeAuth(ctx, client, input, provider, oauthCodeFlowTimeout, openBrowser, p)
	case deviceAuthFlow, "":
		return oauthDeviceCodeAuth(ctx, client, input, provider, p)
	default:
		return nil, fmt.Errorf("invalid auth-flow value: %s", input.authFlow)
	}
}

func getCallbackPort(p iruntime.Prompter) (int, error) {
	env := firstNonEmptyEnv(config.EnvOAuthCallbackPort, config.EnvLegacyOAuthCallbackPort)
	if env == "" {
		return 0, nil
	}

	port, err := strconv.Atoi(env)
	if err != nil {
		return 0, fmt.Errorf("invalid callback port value: %w", err)
	}
	if port < 0 || port > 65535 {
		return 0, errors.New("callback port value must be between 0 and 65535")
	}
	if port > 0 && port < 1024 {
		p.Notice(fmt.Sprintf("Using privileged port %d may require elevated permissions", port))
	}

	return port, nil
}

func oauthAuthCodeAuth(
	ctx context.Context,
	client *http.Client,
	input *loginInput,
	provider typedProvider,
	timeoutAfter time.Duration,
	openBrowser openBrowserFunc,
	p iruntime.Prompter,
) (*Token, error) {
	oauthConfig, err := newOauthConfig(provider)
	if err != nil {
		return nil, fmt.Errorf("failed to create oauth config: %w", err)
	}

	callbackPort, err := getCallbackPort(p)
	if err != nil {
		return nil, err
	}

	verifier := oauth2.GenerateVerifier()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", callbackPort))
	if err != nil {
		return nil, fmt.Errorf("failed to start local callback server on port %d: %w", callbackPort, err)
	}
	defer func() { _ = listener.Close() }()

	if callbackPort == 0 {
		callbackPort = listener.Addr().(*net.TCPAddr).Port
	}

	oauthConfig.RedirectURL = fmt.Sprintf("http://localhost:%d", callbackPort)

	state, err := generateState()
	if err != nil {
		return nil, err
	}

	authURL := oauthConfig.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))

	resultCh := make(chan callbackResult, 1)
	srv := startCallbackServer(listener, state, resultCh, p)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), callbackServerShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
		}
	}()

	p.Notice("\nOpening browser for authentication...\n")
	if err := openBrowser(authURL); err != nil {
		p.Notice(fmt.Sprintf("Failed to open browser automatically (%v). Please open the following URL manually:\n%s\n", err, authURL))
	}

	timeout := time.NewTimer(timeoutAfter)
	defer timeout.Stop()

	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}

		exchangeCtx, cancel := context.WithTimeout(ctx, oauthCodeExchangeTimeout)
		defer cancel()
		exchangeCtx = context.WithValue(exchangeCtx, oauth2.HTTPClient, client)

		oauthToken, err := oauthConfig.Exchange(exchangeCtx, result.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("failed to exchange authorization code for token: %w", err)
		}

		return rancherLogin(ctx, client, input, oauthToken)
	case <-timeout.C:
		return nil, clierr.Retryable(clierr.CodeAuth, "timed out waiting for browser authentication")
	case <-ctx.Done():
		// Abort promptly on caller cancellation (Ctrl-C via main's
		// signal-cancellable context, SIGTERM, test timeout) instead of
		// waiting for the browser callback and only then failing later at
		// the exchange with a confusing post-callback context.Canceled.
		return nil, ctx.Err()
	}
}

// truncateParam bounds an untrusted OAuth callback query param.
func truncateParam(s string) string {
	if len(s) > maxCallbackParamLen {
		return s[:maxCallbackParamLen]
	}
	return s
}

func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func openBrowser(openURL string) error {
	URL, err := url.Parse(openURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if URL.Scheme != "http" && URL.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %s", URL.Scheme)
	}

	// The launch is delegated to an OS-specific opener (browser_windows.go /
	// browser_other.go). The URL is never handed to a shell: non-Windows passes
	// it as a single argv element, Windows uses the ShellExecute API — so a
	// server-supplied authorize URL (which legitimately contains `&` between
	// query parameters) cannot inject shell metacharacters.
	return osOpenURL(openURL)
}

func startCallbackServer(listener net.Listener, expectedState string, resultCh chan<- callbackResult, p iruntime.Prompter) *http.Server {
	mux := http.NewServeMux()
	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	var once sync.Once

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				resultCh <- callbackResult{err: fmt.Errorf("panic in callback handler: %v", r)}
			}
		}()

		// The state check gates entry into the one-shot handler itself, not
		// just the error branch: an unsolicited/forged request (wrong or
		// missing state) must get its own error response WITHOUT consuming
		// once — otherwise the real IdP redirect that arrives afterward
		// finds once already fired and gets no response at all.
		query := r.URL.Query()
		state := query.Get("state")
		if state != expectedState {
			http.Error(w, "Invalid state parameter", http.StatusBadRequest)
			// No resultCh signal (the one-shot handler must stay armed for the
			// real redirect) — but a forged/stray request as the ONLY request
			// this server ever sees would otherwise hang the flow silently for
			// the full timeout. Tell the user why the wait continues.
			p.Notice("ignored a callback with an invalid state; still waiting for the identity provider redirect")
			return
		}

		once.Do(func() {
			if errCode := query.Get("error"); errCode != "" {
				errCode = truncateParam(errCode)
				errDesc := truncateParam(query.Get("error_description"))
				_, _ = fmt.Fprintf(w, "<html><body><h1>Authentication Failed</h1><p>%s: %s</p><p>You can close this window.</p></body></html>",
					html.EscapeString(errCode), html.EscapeString(errDesc))
				resultCh <- callbackResult{err: fmt.Errorf("authentication error: %s: %s", errCode, errDesc)}
				return
			}

			code := strings.TrimSpace(query.Get("code"))
			if code == "" {
				http.Error(w, "Missing authorization code", http.StatusBadRequest)
				resultCh <- callbackResult{err: errors.New("missing authorization code in callback")}
				return
			}

			_, _ = fmt.Fprint(w, "<html><body><h1>Authentication Successful</h1><p>You can close this window and return to the terminal.</p></body></html>")
			resultCh <- callbackResult{code: code, state: state}
		})
	})

	go func() { _ = srv.Serve(listener) }()

	return srv
}

func oauthDeviceCodeAuth(ctx context.Context, client *http.Client, input *loginInput, provider typedProvider, p iruntime.Prompter) (*Token, error) {
	oauthConfig, err := newOauthConfig(provider)
	if err != nil {
		return nil, fmt.Errorf("failed to create oauth config: %w", err)
	}

	ctxWithClient := context.WithValue(ctx, oauth2.HTTPClient, client)

	deviceAuthResp, err := oauthConfig.DeviceAuth(ctxWithClient)
	if err != nil {
		return nil, fmt.Errorf("failed to initiate device authorization: %w", err)
	}

	p.Notice(fmt.Sprintf(
		"\nTo sign in, use a web browser to open the page %s and enter the code %s to authenticate.\n",
		deviceAuthResp.VerificationURI,
		deviceAuthResp.UserCode,
	))

	oauthToken, err := oauthConfig.DeviceAccessToken(ctxWithClient, deviceAuthResp)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve access token: %w", err)
	}

	token, err := rancherLogin(ctx, client, input, oauthToken)
	if err != nil {
		return nil, fmt.Errorf("error during auth login: %w", err)
	}

	return token, nil
}

func newOauthConfig(provider typedProvider) (*oauth2.Config, error) {
	var oauthProvider OAuthProvider

	switch p := provider.(type) {
	case *AzureADProvider:
		oauthProvider = p.OAuthProvider
	default:
		return nil, fmt.Errorf("provider %s is not a supported OAuth provider", provider.GetType())
	}

	return &oauth2.Config{
		ClientID: oauthProvider.ClientID,
		Scopes:   oauthProvider.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:       oauthProvider.AuthURL,
			DeviceAuthURL: oauthProvider.DeviceAuthURL,
			TokenURL:      oauthProvider.TokenURL,
		},
	}, nil
}

func rancherLogin(ctx context.Context, client *http.Client, input *loginInput, oauthToken *oauth2.Token) (*Token, error) {
	reqURL := fmt.Sprintf(loginURL, input.server)

	responseType := "kubeconfig"
	if input.clusterID != "" {
		responseType = fmt.Sprintf("%s_%s", responseType, input.clusterID)
	}

	reqBody, err := json.Marshal(map[string]any{
		"type":         input.authProvider,
		"responseType": responseType,
		"id_token":     oauthToken.Extra("id_token"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, respBody, err := httpx.Do(client, req)
	if err == nil && resp.StatusCode != http.StatusCreated {
		err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err != nil {
		return nil, err
	}

	token, err := parseLoginResponse(respBody)
	if err != nil {
		return nil, err
	}
	return &token, nil
}
