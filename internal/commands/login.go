package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/tidwall/gjson"
)

type loginCmd struct {
	dir   string
	store config.CredStore
	// rancherVersion reads the server-version Setting for the PS-8 floor
	// check, injectable for tests; defaults to probeServerVersion.
	rancherVersion func(ctx context.Context, server, token, cacertFlag string) (string, error)
}

// NewLogin builds the `auth login` command (local).
func NewLogin(dir string, store config.CredStore) runtime.Command {
	return &loginCmd{dir: dir, store: store, rancherVersion: probeServerVersion}
}

func (loginCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth login",
		Usage:       "Login to a Rancher server",
		CommandType: runtime.Local,
		Flags: []runtime.ArgSpec{
			{Name: "server", Type: "string", Required: true},
			{Name: "token-stdin", Type: "bool"},
			{Name: "name", Type: "string"},
			{Name: "cacert", Type: "string"},
			{Name: "context", Type: "string"},
			{Name: "auth-provider", Type: "string"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}},
			{Name: "ttl", Type: "string"},
		},
		Examples: []string{
			"rancher auth login --server https://rancher.ex --token-stdin --name prod --context c-1:p-1 < token.txt",
		},
		Toolset: "core",
	}
}

func (l loginCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	// Guard the CW-3 footgun: the global --context flag and this command's
	// local --context are separate, unsynchronized urfave/cli Flag objects
	// sharing a name. If the caller supplied a global override but login's
	// own --context is empty, the override would otherwise be silently
	// dropped from the written config — the global flag scopes server reads,
	// not config writes.
	if in.String("context") == "" {
		if ctxVal := config.ContextOverride(); ctxVal != "" {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"--context at the root scopes server operations, not config writes").
				WithHint("pass --context explicitly: rancher auth login --context <cluster[:project]>")
		}
	}

	// Defense-in-depth: the pipeline's validateInput normally rejects a missing
	// --server first, but Execute must be safe when called directly (tests).
	server := in.String("server")
	if server == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--server is required").
			WithHint("pass --server <url>")
	}
	u, err := url.ParseRequestURI(server)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "invalid server URL: "+server)
	}
	// Keep only scheme://host — drop path, query, fragment, AND userinfo. Dropping
	// User is a security requirement: https://admin:secret@host must not persist
	// credentials into config.yaml (secrets live only in the credential store).
	u.Path, u.RawQuery, u.Fragment, u.User = "", "", "", nil

	token, err := resolveLoginToken(ctx, u.String(), in, p)
	if err != nil {
		return runtime.Result{}, err
	}
	id, secret, ok := strings.Cut(token, ":")
	if !ok || id == "" || secret == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"invalid token (expected non-empty id:secret)").
			WithHint("get a token from the Rancher UI (format id:secret)")
	}

	name := in.String("name")
	if name == "" {
		name = "default"
	}

	cfg, err := config.Load(config.FilePath(l.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	cfg.Servers[name] = config.Server{URL: u.String(), CA: in.String("cacert")}

	var cluster, project string
	if c := in.String("context"); c != "" {
		cluster, project, _ = config.SplitContext(c)
	}
	cfg.Contexts[name] = config.Context{Server: name, Cluster: cluster, Project: project}
	cfg.CurrentContext = name

	// Login is the one place auto may re-probe and upgrade a stale "file" pin
	// to the keychain; the returned store is the one to write into.
	store, err := config.RepinForLogin(l.dir, cfg.CredentialStore, l.store)
	if err != nil {
		return runtime.Result{}, err
	}
	if err := store.SetToken(name, token); err != nil {
		return runtime.Result{}, err
	}
	if err := cfg.Write(); err != nil {
		return runtime.Result{}, err
	}

	res := runtime.Result{Kind: "LoginResult", Data: map[string]string{
		"server": name, "url": u.String(), "context": name,
	}}
	if warning := l.warnBelowMinVersion(ctx, u.String(), token, in.String("cacert")); warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	return res, nil
}

// serverVersionSettingPath is reached ONLY through the local-cluster proxy:
// management.cattle.io/v3 is an aggregated CRD API, never served at the
// Rancher root, so a bare /apis/... GET 404s here exactly as it does for
// the ext.cattle.io paths in login_ext.go.
const serverVersionSettingPath = "/k8s/clusters/local/apis/management.cattle.io/v3/settings/server-version"

// warnBelowMinVersion evaluates the server-version Setting read via
// l.rancherVersion against config.MinRancherVersion (PS-8, warn-only floor).
// Any failure to read it at all — RBAC denial, a missing setting,
// connectivity — is folded into the same "cannot determine" posture as an
// unparseable value (a dev/head build): this probe never turns a successful
// login into a failure.
func (l loginCmd) warnBelowMinVersion(ctx context.Context, server, token, cacertFlag string) string {
	if l.rancherVersion == nil {
		return config.UnknownServerVersionWarning
	}
	raw, err := l.rancherVersion(ctx, server, token, cacertFlag)
	if err != nil {
		return config.UnknownServerVersionWarning
	}
	_, warning := config.EvalMinVersionSupport(raw)
	return warning
}

// probeServerVersion is the real server-version Setting read (PS-8) via a
// direct HTTP GET with the just-stored token: login has no engine client
// factory, so — unlike auth check's existing client path — this is
// login_ext.go's direct-HTTP style, not provider.ServerVersionSetting.
func probeServerVersion(ctx context.Context, server, token, cacertFlag string) (string, error) {
	caPEM, err := config.ResolveCA(cacertFlag)
	if err != nil {
		return "", err
	}
	client, err := httpx.New(httpx.Options{CACert: caPEM})
	if err != nil {
		return "", err
	}
	return fetchServerVersionSetting(ctx, client, kube.BaseURL(server), token)
}

// fetchServerVersionSetting GETs the server-version Setting and returns its
// top-level "value" field (Setting carries its value there, not under
// spec/status).
func fetchServerVersionSetting(ctx context.Context, client *http.Client, base, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+serverVersionSettingPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, body, err := httpx.Do(client, req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", clierr.New(clierr.CodeGeneric,
			"fetching server-version setting: "+resp.Status)
	}
	return gjson.GetBytes(body, "value").String(), nil
}

func resolveLoginToken(ctx context.Context, server string, in runtime.Input, p runtime.Prompter) (string, error) {
	if in.Bool("token-stdin") {
		if in.String("auth-provider") != "" || in.String("auth-flow") != "" || in.String("ttl") != "" {
			return "", clierr.New(clierr.CodeValidation,
				"--auth-provider/--auth-flow/--ttl apply to the provider sign-in flow and are incompatible with --token-stdin").
				WithHint("drop --token-stdin to use the provider flow, or drop the provider flags to paste/pipe a token")
		}
	}

	var (
		tok string
		err error
	)
	switch {
	case in.Bool("token-stdin"):
		tok, err = p.ReadLine(ctx)
	// The provider flow is allowed without a TTY when --auth-provider is
	// explicitly named: the finer-grained gates already inside it (SAML's
	// NoInput check, basicAuth's per-prompt CanPrompt check, OAuth's lack of
	// any TTY requirement) are the real gate — mirroring how `auth get-token`
	// already runs its OAuth device-code/auth-code flows headlessly. Without
	// an explicit provider, the paste-or-provider menu itself needs a TTY.
	case p.CanPrompt() || in.String("auth-provider") != "":
		tok, err = promptForToken(ctx, server, in, p)
	default:
		return "", clierr.New(clierr.CodeValidation, "token required").
			WithHint("pipe it via --token-stdin, or run in a terminal")
	}
	if err != nil {
		// A typed clierr (from the provider flow, or an invalid menu pick) is
		// already the right error to surface — the io.EOF/generic mapping
		// below is for the plain-paste read paths only.
		var ce *clierr.Error
		if errors.As(err, &ce) {
			return "", err
		}
		if errors.Is(err, io.EOF) {
			return "", clierr.New(clierr.CodeValidation, "no token provided").
				WithHint("pipe the token via --token-stdin, or enter it at the prompt")
		}
		return "", clierr.New(clierr.CodeGeneric, "reading token: "+err.Error())
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", clierr.New(clierr.CodeValidation, "no token provided").
			WithHint("pipe the token via --token-stdin, or enter it at the prompt")
	}
	return tok, nil
}

// promptForToken offers the paste-or-provider choice (EXT-R-8): --auth-provider
// or --auth-flow already signal provider intent and skip straight to it,
// mirroring get-token's own explicit-flag shortcut past its provider menu.
func promptForToken(ctx context.Context, server string, in runtime.Input, p runtime.Prompter) (string, error) {
	if in.String("auth-provider") != "" || in.String("auth-flow") != "" {
		return resolveProviderLoginToken(ctx, server, in, p)
	}
	p.Notice("1) paste an API token\n2) sign in via an identity provider")
	choice, err := p.Prompt(ctx, "Select [1]: ")
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(choice) {
	case "", "1":
		return p.PromptSecret(ctx, "Enter token (id:secret): ")
	case "2":
		return resolveProviderLoginToken(ctx, server, in, p)
	default:
		return "", clierr.New(clierr.CodeAmbiguousSelection, "select 1 (paste) or 2 (identity provider)").
			WithHint("re-run and answer 1 or 2")
	}
}
