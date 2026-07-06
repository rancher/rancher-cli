package token

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

const (
	oauthCodeFlowTimeout          = 5 * time.Minute
	oauthCodeExchangeTimeout      = 30 * time.Second
	callbackServerShutdownTimeout = time.Second
)

type openBrowserFunc func(url string) error

type callbackResult struct {
	code  string
	state string
	err   error
}

func oauthAuth(ctx context.Context, client *http.Client, input *loginInput, provider typedProvider, useV1Public bool, p *ui.Prompter) (*Token, error) {
	if input.authFlow == "" {
		input.authFlow = os.Getenv("CATTLE_OAUTH_AUTH_FLOW")
	}
	input.authFlow = strings.ToLower(input.authFlow)

	switch input.authFlow {
	case authCodeFlow:
		return oauthAuthCodeAuth(ctx, client, input, provider, oauthCodeFlowTimeout, useV1Public, openBrowser, p)
	case deviceAuthFlow, "":
		return oauthDeviceCodeAuth(ctx, client, input, provider, useV1Public, p)
	default:
		return nil, fmt.Errorf("invalid auth-flow value: %s", input.authFlow)
	}
}

func getCallbackPort(p *ui.Prompter) (int, error) {
	env := os.Getenv("CATTLE_OAUTH_CALLBACK_PORT")
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
	useV1Public bool,
	openBrowser openBrowserFunc,
	p *ui.Prompter,
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
	srv := startCallbackServer(listener, state, resultCh)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), callbackServerShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
		}
	}()

	p.Notice("\nOpening browser for authentication...\n")
	if err := openBrowser(authURL); err != nil {
		logrus.Debugf("Failed to open browser: %v", err)
		p.Notice(fmt.Sprintf("Failed to open browser automatically. Please open the following URL manually:\n%s\n", authURL))
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	timeout := time.NewTimer(timeoutAfter)
	defer timeout.Stop()

	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}

		ctx, cancel := context.WithTimeout(context.Background(), oauthCodeExchangeTimeout)
		defer cancel()
		ctx = context.WithValue(ctx, oauth2.HTTPClient, client)

		oauthToken, err := oauthConfig.Exchange(ctx, result.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("failed to exchange authorization code for token: %w", err)
		}

		return rancherLogin(ctx, client, input, oauthToken, useV1Public)
	case <-timeout.C:
		return nil, clierr.New(clierr.CodeAuth, "timed out waiting for browser authentication").WithRetryable(true)
	case <-interrupt:
		return nil, clierr.New(clierr.CodeGeneric, "authentication interrupted by user")
	}
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

	var (
		cmd  string
		args []string
	)
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{openURL}
	case "linux":
		cmd = "xdg-open"
		args = []string{openURL}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", "", openURL}
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return exec.Command(cmd, args...).Start()
}

func startCallbackServer(listener net.Listener, expectedState string, resultCh chan<- callbackResult) *http.Server {
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

		once.Do(func() {
			query := r.URL.Query()

			if errCode := query.Get("error"); errCode != "" {
				errDesc := query.Get("error_description")
				_, _ = fmt.Fprintf(w, "<html><body><h1>Authentication Failed</h1><p>%s: %s</p><p>You can close this window.</p></body></html>", errCode, errDesc)
				resultCh <- callbackResult{err: fmt.Errorf("authentication error: %s: %s", errCode, errDesc)}
				return
			}

			state := query.Get("state")
			if state != expectedState {
				http.Error(w, "Invalid state parameter", http.StatusBadRequest)
				resultCh <- callbackResult{err: errors.New("invalid state parameter in callback (possible CSRF attack)")}
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

func oauthDeviceCodeAuth(ctx context.Context, client *http.Client, input *loginInput, provider typedProvider, useV1Public bool, p *ui.Prompter) (*Token, error) {
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

	token, err := rancherLogin(ctx, client, input, oauthToken, useV1Public)
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

func rancherLogin(ctx context.Context, client *http.Client, input *loginInput, oauthToken *oauth2.Token, useV1Public bool) (*Token, error) {
	reqURL := fmt.Sprintf(loginURL, input.server)
	if !useV1Public {
		providerName := strings.ToLower(strings.TrimSuffix(input.authProvider, "Provider"))
		reqURL = fmt.Sprintf(loginURLv3, input.server, input.authProvider, providerName)
	}

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

	resp, respBody, err := doRequest(client, req)
	if err == nil && resp.StatusCode != http.StatusCreated {
		err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err != nil {
		return nil, err
	}

	token := &Token{}
	err = json.Unmarshal(respBody, token)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling login response: %w", err)
	}

	return token, nil
}
