package commands

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type loginCmd struct {
	dir   string
	store config.CredStore
	// rancherVersion reads the server-version Setting for the PS-8 floor
	// check, injectable for tests; defaults to provider.ServerVersionSetting
	// — the same PS-8 read `auth check` already uses (finding 7), rather
	// than login re-implementing the local-cluster proxy path plus response
	// decode via raw HTTP. The original "no engine client factory inside a
	// Local command" objection doesn't apply here: ServerVersionSetting
	// takes a bare config.Effective, no factory/cache construct, and login
	// already has everything an Effective needs by this point (the server
	// URL, --cacert, and the just-resolved/stored token).
	rancherVersion func(ctx context.Context, eff config.Effective) (string, error)
}

// NewLogin builds the `auth login` command (local).
func NewLogin(dir string, store config.CredStore) runtime.Command {
	return &loginCmd{dir: dir, store: store, rancherVersion: provider.ServerVersionSetting}
}

func (loginCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth login",
		Usage:       "Login to a Rancher server",
		CommandType: runtime.Local,
		Flags: []runtime.ArgSpec{
			{Name: "server", Type: "string", Required: true, Desc: "Rancher server `url` (scheme required)"},
			{Name: "token-stdin", Type: "bool", Desc: "read the API token (id:secret) from stdin"},
			{Name: "name", Type: "string", Desc: "`name` for the saved server entry"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM"},
			{Name: "context", Type: "string", Desc: "initial scope `cluster[:project]` for the new context"},
			{Name: "auth-provider", Type: "string", Desc: "auth provider `name` to sign in with"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}, Desc: "identity-provider `flow`", Env: []string{"RANCHER_CLI_OAUTH_AUTH_FLOW"}},
			{Name: "ttl", Type: "string", Desc: "session-token lifetime `duration`, e.g. 90d"},
		},
		Examples: []string{
			"rancher auth login --server https://rancher.ex --token-stdin --name prod --context c-1:p-1 < token.txt",
		},
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
	var warnings []string
	if err := store.SetToken(config.ServerRef{Name: name, URL: u.String()}, token); err != nil {
		if !errors.Is(err, config.ErrTokenStoreUnsupported) {
			return runtime.Result{}, err
		}
		// Read-only helper: a declared, expected outcome — proceed exactly as
		// on success, but say the token was not persisted.
		warnings = append(warnings,
			"credential helper is read-only; token not persisted — future commands will re-acquire it through the helper")
	}
	if err := cfg.Write(); err != nil {
		return runtime.Result{}, err
	}

	res := runtime.Result{Kind: "LoginResult", Data: map[string]string{
		"server": name, "url": u.String(), "context": name,
	}}
	res.Warnings = warnings
	versionEff := config.Effective{ServerName: name, URL: u.String(), CA: in.String("cacert"), Token: token}
	if warning := l.warnBelowMinVersion(ctx, versionEff); warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	return res, nil
}

// warnBelowMinVersion evaluates the server-version Setting read via
// l.rancherVersion against config.MinRancherVersion (PS-8, warn-only floor).
// Any failure to read it at all — RBAC denial, a missing setting,
// connectivity — is folded into the same "cannot determine" posture as an
// unparseable value (a dev/head build): this probe never turns a successful
// login into a failure.
func (l loginCmd) warnBelowMinVersion(ctx context.Context, eff config.Effective) string {
	if l.rancherVersion == nil {
		return config.UnknownServerVersionWarning
	}
	raw, err := l.rancherVersion(ctx, eff)
	if err != nil {
		return config.UnknownServerVersionWarning
	}
	_, warning := config.EvalMinVersionSupport(raw)
	return warning
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
