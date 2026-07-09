package token

import (
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
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/tidwall/gjson"
)

const (
	loginURL        = "%s/v1-public/login"
	authProviderURL = "%s/v1-public/authproviders"
	authTokenURL    = "%s/v1-public/authtokens/%s"
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
	clusterID    string
	authProvider string
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
			{Name: "cluster", Type: "string"},
			{Name: "name", Type: "string"},
			{Name: "auth-provider", Type: "string"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}},
			{Name: "cacert", Type: "string"},
			{Name: "skip-verify", Type: "bool"},
		},
		Toolset: "core",
		Network: true,
		Auth:    true,
	}
}

func (t tokenCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	return t.run(ctx, in, p)
}

func (t tokenCmd) run(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	name := in.String("name")

	// Fast path: when --server is given, build the cache key from flags alone and
	// serve a warm hit WITHOUT reading config, so a broken config.yaml never
	// blocks a cached credential.
	flagServer := in.String("server")
	flagKey := ""
	if name != "" && flagServer != "" {
		flagKey = config.CacheKey(name, cacheServerHost(flagServer), in.String("cluster"))
		if r, hit := t.cachedResult(flagKey, p); hit {
			return r, nil
		}
	}

	// Full resolution (config + CA disk I/O): required to authenticate on a miss
	// and to build the definitive cache key when --server was not provided.
	server, caPEM, clusterID, err := t.resolveConn(in)
	if err != nil {
		return runtime.Result{}, err
	}
	serverHost := cacheServerHost(server)
	cacheKey := config.CacheKey(name, serverHost, clusterID)

	switch {
	case name == "":
		p.Notice("caching disabled: no --name; each call re-authenticates")
	case cacheKey != flagKey:
		// The flag fast path either did not run or used a different key (e.g.
		// cluster sourced from config) — check the resolved key now.
		if r, hit := t.cachedResult(cacheKey, p); hit {
			return r, nil
		}
	}

	input := &loginInput{
		server:       server,
		clusterID:    clusterID,
		authProvider: in.String("auth-provider"),
		skipVerify:   in.Bool("skip-verify"),
		authFlow:     in.String("auth-flow"),
	}

	client, err := httpx.New(httpx.Options{CACert: caPEM, Insecure: input.skipVerify})
	if err != nil {
		return runtime.Result{}, err
	}

	rancherToken, err := loginAndGenerateCred(ctx, client, input, p)
	if err != nil {
		return runtime.Result{}, err
	}
	if strings.TrimSpace(rancherToken.Token) == "" {
		return runtime.Result{}, clierr.New(clierr.CodeAuth, "authentication returned an empty token").
			WithHint("check the auth provider and credentials")
	}

	var tokenExpiry time.Time
	if rancherToken.ExpiresAt != "" {
		tokenExpiry, err = time.Parse(time.RFC3339, rancherToken.ExpiresAt)
		if err != nil {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric, "parsing token expiry: "+err.Error())
		}
	}

	if name != "" {
		if cacheErr := t.store.SetCachedCred(cacheKey, rancherToken.Token, tokenExpiry, name, serverHost, clusterID); cacheErr != nil {
			p.Notice("cache write failed: " + cacheErr.Error())
		}
	}

	return runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(rancherToken.Token, tokenExpiry)}, nil
}

// cachedResult returns an ExecCredential result for a warm cache entry and
// whether it hit. A non-ErrNoToken read failure is noticed but treated as a miss.
func (t tokenCmd) cachedResult(key string, p runtime.Prompter) (runtime.Result, bool) {
	tok, expiry, err := t.store.GetCachedCred(key)
	if err == nil {
		return runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(tok, expiry)}, true
	}
	if !errors.Is(err, config.ErrNoToken) {
		p.Notice("cache read failed: " + err.Error())
	}
	return runtime.Result{}, false
}

type deleteTokenCmd struct{ store config.CredStore }

// NewDeleteTokenCommand builds `auth delete-token` (local): clear cached
// exec-credential token(s). A positional name is the cache entry name; --all
// clears every cached credential.
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
			"rancher auth delete-token prod",
		},
		Toolset: "core",
	}
}

func (d deleteTokenCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	all := in.Bool("all")
	keys := in.Args()
	if all && len(keys) > 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"specify either credential name(s) or --all, not both").
			WithHint("rancher auth delete-token prod  OR  rancher auth delete-token --all")
	}
	if all {
		if err := d.store.DeleteAllCachedCreds(); err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: "TokenDelete", Data: map[string]any{"deleted": "all"}}, nil
	}
	if len(keys) == 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"provide a cached-credential name or --all").
			WithHint("name is the cached entry name; or: rancher auth delete-token --all")
	}
	// Resolve each argument against the cached record set: a bare display name
	// matches EVERY entry with that name (a legacy bare entry and a scoped entry
	// sharing the name are the same logical credential — deleting cached tokens
	// is always safe, they re-auth), and the full composite key matches one.
	// Deletion is idempotent: an unknown or already-deleted name is a success
	// with nothing removed, so teardown scripts can call it unconditionally.
	infos, err := d.store.ListCachedCreds()
	if err != nil {
		return runtime.Result{}, err
	}
	deleted := make([]string, 0, len(keys))
	for _, arg := range keys {
		for _, info := range infos {
			if info.Name != arg && info.Key != arg {
				continue
			}
			if derr := d.store.DeleteCachedCred(info.Key); derr != nil {
				return runtime.Result{}, clierr.New(clierr.CodeGeneric,
					fmt.Sprintf("failed to delete %q: %v", arg, derr)).
					WithHint(fmt.Sprintf("already deleted: %v", deleted))
			}
			deleted = append(deleted, info.Key)
		}
	}
	return runtime.Result{Kind: "TokenDelete", Data: map[string]any{"deleted": deleted}}, nil
}

func loginAndGenerateCred(ctx context.Context, client *http.Client, input *loginInput, p runtime.Prompter) (Token, error) {
	authProviders, err := getAuthProviders(ctx, client, input.server)
	if err != nil {
		return Token{}, err
	}
	selectedProvider, err := selectAuthProvider(ctx, authProviders, input.authProvider, p)
	if err != nil {
		return Token{}, err
	}
	input.authProvider = selectedProvider.GetType()

	var token Token
	if samlProviders[input.authProvider] {
		if p.NoInput() {
			return Token{}, clierr.New(clierr.CodeAuth, "interactive login required for "+input.authProvider).
				WithHint("run without --no-input, or use a cached --name")
		}
		token, err = samlAuth(ctx, client, input, p)
	} else if oauthProviders[input.authProvider] {
		var tokenPtr *Token
		tokenPtr, err = oauthAuth(ctx, client, input, selectedProvider, p)
		if err == nil {
			token = *tokenPtr
		}
	} else {
		token, err = basicAuth(ctx, client, input, p)
	}
	if err != nil {
		return Token{}, err
	}
	return token, nil
}

func basicAuth(ctx context.Context, client *http.Client, input *loginInput, p runtime.Prompter) (Token, error) {
	username, err := p.Prompt(ctx, "Enter username: ")
	if err != nil {
		if errors.Is(err, ui.ErrNoInput) {
			return Token{}, clierr.New(clierr.CodeAuth, "interactive login required for "+input.authProvider).
				WithHint("run in a terminal, or use a cached --name")
		}
		return Token{}, err
	}
	password, err := p.PromptSecret(ctx, "Enter password: ")
	if err != nil {
		if errors.Is(err, ui.ErrNoInput) {
			return Token{}, clierr.New(clierr.CodeAuth, "interactive login required for "+input.authProvider).
				WithHint("run in a terminal, or use a cached --name")
		}
		return Token{}, err
	}

	responseType := "kubeconfig"
	if input.clusterID != "" {
		responseType = "kubeconfig_" + input.clusterID
	}
	reqBody, err := json.Marshal(map[string]any{
		"type": input.authProvider, "responseType": responseType,
		"username": username, "password": password,
	})
	if err != nil {
		return Token{}, fmt.Errorf("failed to marshal request body: %w", err)
	}

	reqURL := fmt.Sprintf(loginURL, input.server)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return Token{}, fmt.Errorf("error creating request: %w", err)
	}
	resp, respBody, err := doRequest(client, req)
	if err != nil {
		return Token{}, fmt.Errorf("error logging user in: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Token{}, clierr.New(clierr.CodeAuth, fmt.Sprintf("login rejected: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	if resp.StatusCode != http.StatusCreated {
		return Token{}, clierr.New(clierr.CodeGeneric, fmt.Sprintf("login failed: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	return parseLoginResponse(respBody)
}

func samlAuth(ctx context.Context, client *http.Client, input *loginInput, p runtime.Prompter) (Token, error) {
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

	p.Notice(fmt.Sprintf("\nLogin Request Id: %s\n", id))
	p.Notice(fmt.Sprintf("\nLogin to Rancher Server at %s \n", loginURL))

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	timeout := time.NewTimer(15 * time.Minute)
	defer timeout.Stop()
	poll := time.NewTicker(10 * time.Second)
	defer poll.Stop()

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
				p.Notice("error deleting auth token: " + err.Error())
			}

			return token, nil

		case <-timeout.C:
			return token, clierr.New(clierr.CodeAuth, "timed out waiting for the auth token").WithRetryable(true)
		case <-interrupt:
			return token, clierr.New(clierr.CodeGeneric, "interrupted waiting for the auth token")
		}
	}
}

func getAuthProviders(ctx context.Context, client *http.Client, server string) ([]typedProvider, error) {
	authProvidersURL := fmt.Sprintf(authProviderURL, server)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authProvidersURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	resp, respBody, err := doRequest(client, req)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err != nil {
		return nil, fmt.Errorf("error listing auth providers: %w", err)
	}

	if !gjson.ValidBytes(respBody) {
		return nil, errors.New("invalid JSON response")
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
				return nil, fmt.Errorf("error decoding the auth provider %s: %w", providerType, err)
			}

			if tp.GetType() == "localProvider" {
				supportedProviders = append([]typedProvider{tp}, supportedProviders...)
			} else {
				supportedProviders = append(supportedProviders, tp)
			}
		}
	}

	return supportedProviders, err
}

func selectAuthProvider(ctx context.Context, providers []typedProvider, want string, p runtime.Prompter) (typedProvider, error) {
	if len(providers) == 0 {
		return nil, clierr.New(clierr.CodeGeneric, "no auth provider configured")
	}

	cands := func() []clierr.Candidate {
		cs := make([]clierr.Candidate, 0, len(providers))
		for _, p := range providers {
			cs = append(cs, clierr.Candidate{ID: p.GetType(), Name: p.GetType()})
		}
		return cs
	}

	if want != "" {
		for _, prov := range providers {
			if prov.GetType() == want {
				return prov, nil
			}
		}
		return nil, clierr.New(clierr.CodeNotFound, "auth provider not found: "+want).
			WithCandidates(cands()).WithHint("pass --auth-provider <id>")
	}

	if len(providers) == 1 {
		return providers[0], nil
	}

	if p.CanPrompt() {
		var b strings.Builder
		for i, prov := range providers {
			fmt.Fprintf(&b, "%d - %s\n", i, prov.GetType())
		}
		p.Notice("Auth providers:\n" + b.String())
		sel, err := p.Prompt(ctx, "Select auth provider: ")
		if err != nil {
			return nil, err
		}
		if idx, aerr := strconv.Atoi(strings.TrimSpace(sel)); aerr == nil && idx >= 0 && idx < len(providers) {
			return providers[idx], nil
		}
		// invalid (non-numeric / out-of-range) pick → fall through to AMBIGUOUS_SELECTION
	}

	return nil, clierr.New(clierr.CodeAmbiguousSelection, "multiple auth providers; select one").
		WithCandidates(cands()).WithHint("pass --auth-provider <id>")
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

// cacheServerHost reduces a server URL to its host for the cache key, falling
// back to the raw string when it does not parse to a host.
func cacheServerHost(server string) string {
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		return u.Host
	}
	return server
}

// resolveConn fills server/CA/cluster from flags, falling back to the CLI config
// (which already merges env over the file). Flags win over config. An explicit
// --server with no --cacert never inherits the CURRENT context's CA — that CA
// belongs to whatever server the context happens to point at, which may differ
// from the server just named; instead it borrows the CA of the configured
// server entry whose URL actually matches (see caForServer).
func (t tokenCmd) resolveConn(in runtime.Input) (server, caPEM, cluster string, err error) {
	server = in.String("server")
	cluster = in.String("cluster")
	caIn := in.String("cacert")
	explicitServer := server != ""
	// Normalize the flag value's scheme here, once: it must happen before
	// caForServer below, and a config-sourced server (from eff.URL) is always
	// already scheme-qualified (login rejects a schemeless --server), so
	// there is no second site left needing this. Guarded on explicitServer —
	// an empty server must stay "" for the fallback-to-config check below,
	// not become the bogus non-empty "https://".
	if explicitServer {
		if u, perr := url.Parse(server); perr == nil && u.Scheme == "" {
			server = "https://" + server
		}
	}

	if server == "" || cluster == "" || caIn == "" {
		cfg, cerr := config.Load(config.FilePath(t.dir))
		if cerr != nil {
			return "", "", "", cerr
		}
		eff, cerr := cfg.Effective(t.store)
		if cerr != nil {
			return "", "", "", cerr
		}
		if server == "" {
			server = eff.URL
		}
		if cluster == "" {
			cluster = eff.Cluster
		}
		if caIn == "" {
			if explicitServer {
				caIn = caForServer(cfg, server)
			} else {
				caIn = eff.CA
			}
		}
	}

	if server == "" {
		return "", "", "", clierr.New(clierr.CodeValidation, "--server is required").
			WithHint("pass --server or set a current context")
	}
	caPEM, err = config.ResolveCA(caIn)
	if err != nil {
		return "", "", "", clierr.New(clierr.CodeValidation, "reading --cacert: "+err.Error())
	}
	return server, caPEM, cluster, nil
}

// caForServer returns the CA of the configured server entry whose URL
// matches server (compared by scheme+host via kube.BaseURL), or "" if none
// matches — so an explicit --server with no --cacert never silently borrows
// a different server's trust root.
func caForServer(cfg *config.Config, server string) string {
	target := kube.BaseURL(server)
	for _, s := range cfg.Servers {
		if kube.BaseURL(s.URL) == target {
			return s.CA
		}
	}
	return ""
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
