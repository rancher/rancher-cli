package token

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/tidwall/gjson"
	"golang.org/x/term"
)

const (
	loginURL          = "%s/v1-public/login"
	authProviderURL   = "%s/v1-public/authproviders"
	authTokenURL      = "%s/v1-public/authtokens/%s"
	loginURLv3        = "%s/v3-public/%ss/%s?action=login"
	authProviderURLv3 = "%s/v3-public/authProviders"
	authTokenURLv3    = "%s/v3-public/authTokens/%s"
)

const (
	deviceAuthFlow = "devicecode"
	authCodeFlow   = "authcode"
)

var samlProviders = map[string]bool{
	"pingProvider":       true,
	"adfsProvider":       true,
	"keyCloakProvider":   true,
	"oktaProvider":       true,
	"shibbolethProvider": true,
}

var oauthProviders = map[string]bool{
	"azureADProvider": true,
}

var supportedAuthProviders = map[string]bool{
	"localProvider":           true,
	"freeIpaProvider":         true,
	"openLdapProvider":        true,
	"activeDirectoryProvider": true,
	"pingProvider":            true,
	"adfsProvider":            true,
	"keyCloakProvider":        true,
	"oktaProvider":            true,
	"shibbolethProvider":      true,
	"azureADProvider":         true,
}

type loginInput struct {
	server       string
	userID       string
	clusterID    string
	authProvider string
	caCerts      string
	skipVerify   bool
	authFlow     string
}

type typedProvider interface {
	GetType() string
}

type tokenCmd struct {
	dir   string
	store config.CredStore
}

func NewTokenCommand(dir string, store config.CredStore) runtime.Command {
	return &tokenCmd{dir: dir, store: store}
}

func (tokenCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth get-token",
		Usage:       "Authenticate and emit a kubeconfig ExecCredential",
		CommandType: runtime.Local,
		Flags: []runtime.ArgSpec{
			{Name: "server", Type: "string"},
			{Name: "user", Type: "string"},
			{Name: "cluster", Type: "string"},
			{Name: "auth-provider", Type: "string"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}},
			{Name: "cacerts", Type: "string"},
			{Name: "skip-verify", Type: "bool"},
		},
	}
}

func (tokenCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}

func (t tokenCmd) Execute(ctx context.Context, in runtime.Input, _ safety.Effect) (runtime.Result, error) {
	return t.run(ctx, in)
}

func (t tokenCmd) run(ctx context.Context, in runtime.Input) (runtime.Result, error) {
	server := in.String("server")
	if server == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--server is required")
	}

	serverURL, err := url.Parse(server)
	if err != nil {
		return runtime.Result{}, err
	}
	if serverURL.Scheme == "" {
		server = fmt.Sprintf("https://%s", server)
	}

	userID := in.String("user")
	if userID == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--user is required")
	}
	clusterID := in.String("cluster")

	cachedCredName := fmt.Sprintf("%s/%s/%s", server, userID, clusterID)
	token, expiry, err := t.store.GetCachedCred(cachedCredName)
	if err == nil {
		return runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(token, expiry)}, nil
	}
	if !errors.Is(err, config.ErrNoToken) {
		customPrint(fmt.Errorf("LoadToken: %v", err))
	}

	input := &loginInput{
		server:       server,
		userID:       userID,
		clusterID:    clusterID,
		authProvider: in.String("auth-provider"),
		caCerts:      in.String("cacerts"),
		skipVerify:   in.Bool("skip-verify"),
		authFlow:     in.String("auth-flow"),
	}

	var certPEM string
	if input.caCerts != "" {
		certPEM, err = loadAndVerifyCert(input.caCerts)
		if err != nil {
			return runtime.Result{}, err
		}
	}

	client, err := httpx.New(httpx.Options{
		CACert:   certPEM,
		Insecure: input.skipVerify,
		Timeout:  0,
	})
	if err != nil {
		return runtime.Result{}, err
	}

	rancherToken, err := loginAndGenerateCred(ctx, client, input)
	if err != nil {
		return runtime.Result{}, err
	}

	var tokenExpiry time.Time
	if rancherToken.ExpiresAt != "" {
		tokenExpiry, err = time.Parse(time.RFC3339, rancherToken.ExpiresAt)
		if err != nil {
			customPrint(fmt.Sprintf("\nerror parsing time %s %v", rancherToken.ExpiresAt, err))
			return runtime.Result{}, err
		}
	}

	if cacheErr := t.store.SetCachedCred(cachedCredName, rancherToken.Token, tokenExpiry); cacheErr != nil {
		customPrint(fmt.Errorf("CacheToken: %v", cacheErr))
	}

	return runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(rancherToken.Token, tokenExpiry)}, nil
}

type deleteTokenCmd struct{ store config.CredStore }

// NewDeleteTokenCommand builds `auth delete-token` (local): clear cached
// exec-credential token(s). A positional name is the full compound cache key
// "<server>/<user>/<cluster>"; --all clears every cached credential.
func NewDeleteTokenCommand(_ string, store config.CredStore) runtime.Command {
	return &deleteTokenCmd{store: store}
}

func (deleteTokenCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth delete-token",
		Usage:       "Delete cached exec-credential token(s)",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:       []runtime.ArgSpec{{Name: "all", Type: "bool"}},
		Examples: []string{
			"rancher auth delete-token --all",
			"rancher auth delete-token https://r.ex/u-abc/c-1",
		},
	}
}

func (deleteTokenCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}

func (d deleteTokenCmd) Execute(_ context.Context, in runtime.Input, _ safety.Effect) (runtime.Result, error) {
	if in.Bool("all") {
		if err := d.store.DeleteAllCachedCreds(); err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: "TokenDelete", Data: map[string]any{"deleted": "all"}}, nil
	}
	keys := in.Args()
	if len(keys) == 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"provide a cached-credential name or --all").
			WithHint("name is <server>/<user>/<cluster>; or: rancher auth delete-token --all")
	}
	for _, key := range keys {
		if err := d.store.DeleteCachedCred(key); err != nil {
			return runtime.Result{}, err
		}
	}
	return runtime.Result{Kind: "TokenDelete", Data: map[string]any{"deleted": keys}}, nil
}

func loginAndGenerateCred(ctx context.Context, client *http.Client, input *loginInput) (Token, error) {
	authProviders, useV1Public, err := getAuthProviders(ctx, client, input.server, true)
	if err != nil {
		return Token{}, err
	}

	selectedProvider, err := selectAuthProvider(authProviders, input.authProvider)
	if err != nil {
		return Token{}, err
	}
	input.authProvider = selectedProvider.GetType()

	var token Token
	if samlProviders[input.authProvider] {
		token, err = samlAuth(ctx, client, input, useV1Public)
		if err != nil {
			return Token{}, err
		}
	} else if oauthProviders[input.authProvider] {
		tokenPtr, err := oauthAuth(ctx, client, input, selectedProvider, useV1Public)
		if err != nil {
			return Token{}, err
		}
		token = *tokenPtr
	} else {
		customPrint(fmt.Sprintf("Enter credentials for %s \n", input.authProvider))
		token, err = basicAuth(ctx, client, input, useV1Public)
		if err != nil {
			return Token{}, err
		}
	}

	return token, nil
}

func basicAuth(ctx context.Context, client *http.Client, input *loginInput, useV1Public bool) (Token, error) {
	prompt := "Enter username"
	if input.userID != "" {
		prompt += " [" + input.userID + "]"
	}
	username, err := customPrompt(prompt+": ", true)
	if err != nil {
		return Token{}, err
	}

	if username == "" && input.userID != "" {
		username = input.userID
	}

	password, err := customPrompt("Enter password: ", false)
	if err != nil {
		return Token{}, err
	}

	responseType := "kubeconfig"
	if input.clusterID != "" {
		responseType = fmt.Sprintf("%s_%s", responseType, input.clusterID)
	}

	reqBody, err := json.Marshal(map[string]any{
		"type":         input.authProvider,
		"responseType": responseType,
		"username":     username,
		"password":     password,
	})
	if err != nil {
		return Token{}, fmt.Errorf("failed to marshal request body: %w", err)
	}

	reqURL := fmt.Sprintf(loginURL, input.server)
	if !useV1Public {
		providerName := strings.ToLower(strings.TrimSuffix(input.authProvider, "Provider"))
		reqURL = fmt.Sprintf(loginURLv3, input.server, input.authProvider, providerName)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return Token{}, fmt.Errorf("error creating request: %w", err)
	}

	resp, respBody, err := doRequest(client, req)
	if err == nil && resp.StatusCode != http.StatusCreated {
		err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err != nil {
		return Token{}, fmt.Errorf("error logging user in: %w", err)
	}

	var token Token
	err = json.Unmarshal(respBody, &token)
	if err != nil {
		return Token{}, fmt.Errorf("error unmarshaling login response: %w", err)
	}

	return token, nil
}

func samlAuth(ctx context.Context, client *http.Client, input *loginInput, useV1Public bool) (Token, error) {
	token := Token{}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return token, fmt.Errorf("error generating key: %w", err)
	}

	publicKey := privateKey.PublicKey
	marshalKey, err := json.Marshal(publicKey)
	if err != nil {
		return token, fmt.Errorf("error marshaling public key: %w", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(marshalKey)

	id, err := generateKey()
	if err != nil {
		return token, fmt.Errorf("error generating request id: %w", err)
	}

	responseType := "kubeconfig"
	if input.clusterID != "" {
		responseType = fmt.Sprintf("%s_%s", responseType, input.clusterID)
	}

	tokenURL := fmt.Sprintf(authTokenURL, input.server, id)
	if !useV1Public {
		tokenURL = fmt.Sprintf(authTokenURLv3, input.server, id)
	}

	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, bytes.NewBuffer(nil))
	if err != nil {
		return token, fmt.Errorf("error creating get token request: %w", err)
	}
	getReq.Header.Set("content-type", "application/json")
	getReq.Header.Set("accept", "application/json")

	deleteReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, tokenURL, bytes.NewBuffer(nil))
	if err != nil {
		return token, fmt.Errorf("error creating delete token request: %w", err)
	}
	deleteReq.Header.Set("content-type", "application/json")
	deleteReq.Header.Set("accept", "application/json")

	loginURL, err := url.Parse(input.server + "/dashboard/auth/login")
	if err != nil {
		return token, fmt.Errorf("error parsing login url: %w", err)
	}

	q := url.Values{}
	q.Set("cli", "true")
	q.Set("requestId", id)
	q.Set("publicKey", encodedKey)
	q.Set("responseType", responseType)
	loginURL.RawQuery = q.Encode()

	customPrint(fmt.Sprintf("\nLogin Request Id: %s\n", id))
	customPrint(fmt.Sprintf("\nLogin to Rancher Server at %s \n", loginURL))

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	timeout := time.NewTimer(15 * time.Minute)
	defer timeout.Stop()
	poll := time.NewTicker(10 * time.Second)
	defer poll.Stop()

loop:
	for {
		select {
		case <-poll.C:
			resp, respBody, err := doRequest(client, getReq)
			if err != nil {
				return token, fmt.Errorf("error fetching auth token: %w", err)
			}
			switch resp.StatusCode {
			case http.StatusOK:
			case http.StatusNotFound:
				continue
			default:
				return token, fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
			}

			err = json.Unmarshal(respBody, &token)
			if err != nil {
				return token, fmt.Errorf("error unmarshaling auth token response: %w", err)
			}
			if token.Token == "" {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(token.Token)
			if err != nil {
				return token, fmt.Errorf("error decoding auth token: %w", err)
			}
			decryptedBytes, err := privateKey.Decrypt(nil, decoded, &rsa.OAEPOptions{Hash: crypto.SHA256})
			if err != nil {
				return token, fmt.Errorf("error decrypting auth token: %w", err)
			}
			token.Token = string(decryptedBytes)

			resp, _, err = doRequest(client, deleteReq)
			if err == nil {
				switch resp.StatusCode {
				case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
				default:
					err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
				}
			}
			if err != nil {
				customPrint(fmt.Errorf("error deleting auth token: %s", err))
			}

			return token, nil

		case <-timeout.C:
			customPrint("timed out waiting for the auth token")
			break loop

		case <-interrupt:
			customPrint("interrupted waiting for the auth token")
			break loop
		}
	}

	return token, nil
}

func getAuthProviders(ctx context.Context, client *http.Client, server string, useV1Public bool) ([]typedProvider, bool, error) {
	authProvidersURL := fmt.Sprintf(authProviderURL, server)
	if !useV1Public {
		authProvidersURL = fmt.Sprintf(authProviderURLv3, server)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authProvidersURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("error creating request: %w", err)
	}

	resp, respBody, err := doRequest(client, req)
	if err == nil {
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusNotFound:
			if useV1Public {
				return getAuthProviders(ctx, client, server, !useV1Public)
			}
			fallthrough
		default:
			err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
	}
	if err != nil {
		return nil, false, fmt.Errorf("error listing auth providers: %w", err)
	}

	if !gjson.ValidBytes(respBody) {
		return nil, false, errors.New("invalid JSON response")
	}
	data := gjson.GetBytes(respBody, "data").Array()

	var supportedProviders []typedProvider
	for _, provider := range data {
		providerType := provider.Get("type").String()

		if providerType != "" && supportedAuthProviders[providerType] {
			var tp typedProvider

			switch providerType {
			case "azureADProvider":
				tp = &AzureADProvider{}
			case "localProvider":
				tp = &LocalProvider{}
			default:
				tp = &AuthProvider{}
			}

			err = json.Unmarshal([]byte(provider.Raw), tp)
			if err != nil {
				return nil, false, fmt.Errorf("error decoding the auth provider %s: %w", providerType, err)
			}

			if tp.GetType() == "localProvider" {
				supportedProviders = append([]typedProvider{tp}, supportedProviders...)
			} else {
				supportedProviders = append(supportedProviders, tp)
			}
		}
	}

	return supportedProviders, useV1Public, err
}

func selectAuthProvider(authProviders []typedProvider, providerType string) (typedProvider, error) {
	if len(authProviders) == 0 {
		return nil, errors.New("no auth provider configured")
	}

	if providerType != "" {
		for _, p := range authProviders {
			if p.GetType() == providerType {
				return p, nil
			}
		}
		return nil, fmt.Errorf("provider %s not found", providerType)
	}

	if len(authProviders) == 1 {
		return authProviders[0], nil
	}

	var providers []string
	for i, val := range authProviders {
		providers = append(providers, fmt.Sprintf("%d - %s", i, val.GetType()))
	}

	for range 3 {
		customPrint(fmt.Sprintf("Auth providers:\n%v", strings.Join(providers, "\n")))
		providerIndexStr, err := customPrompt("Select auth provider: ", true)
		if err != nil {
			continue
		}

		providerIndex, err := strconv.Atoi(providerIndexStr)
		if err != nil || (providerIndex < 0 || providerIndex > len(providers)-1) {
			customPrint("Pick a valid auth provider")
			continue
		}

		return authProviders[providerIndex], nil
	}

	return nil, errors.New("invalid auth provider")
}

func generateKey() (string, error) {
	characters := "abcdfghjklmnpqrstvwxz12456789"
	tokenLength := 32
	token := make([]byte, tokenLength)
	for i := range token {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(len(characters))))
		if err != nil {
			return "", err
		}
		token[i] = characters[r.Int64()]
	}

	return string(token), nil
}

func loadAndVerifyCert(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func doRequest(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return resp, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}

	return resp, body, nil
}

func customPrint(data any) {
	fmt.Fprintf(os.Stderr, "%v \n", data)
}

func customPrompt(msg string, show bool) (result string, err error) {
	fmt.Fprint(os.Stderr, msg)
	if show {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			result = scanner.Text()
		}
		err = scanner.Err()
	} else {
		var data []byte
		data, err = term.ReadPassword(int(os.Stdin.Fd()))
		result = string(data)
		fmt.Fprintf(os.Stderr, "\n")
	}
	return result, err
}
