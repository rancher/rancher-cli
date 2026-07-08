package commands

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type loginCmd struct {
	dir   string
	store config.CredStore
}

// NewLogin builds the `auth login` command (local).
func NewLogin(dir string, store config.CredStore) runtime.Command {
	return &loginCmd{dir: dir, store: store}
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
		},
		Examples: []string{
			"rancher auth login --server https://rancher.ex --token-stdin --name prod --context c-1:p-1 < token.txt",
		},
	}
}

func (l loginCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
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

	token, err := resolveLoginToken(ctx, in, p)
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
		name = "rancherDefault"
	}

	cfg, err := config.Load(config.FilePath(l.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	cfg.Servers[name] = config.Server{URL: u.String(), CA: in.String("cacert")}

	var cluster, project string
	if c := in.String("context"); c != "" {
		cluster, project, _ = strings.Cut(c, ":")
	}
	cfg.Contexts[name] = config.Context{Server: name, Cluster: cluster, Project: project}
	cfg.CurrentContext = name

	if err := l.store.SetToken(name, token); err != nil {
		return runtime.Result{}, err
	}
	if err := cfg.Write(); err != nil {
		return runtime.Result{}, err
	}

	return runtime.Result{Kind: "LoginResult", Data: map[string]string{
		"server": name, "url": u.String(), "context": name,
	}}, nil
}

func resolveLoginToken(ctx context.Context, in runtime.Input, p runtime.Prompter) (string, error) {
	var (
		tok string
		err error
	)
	switch {
	case in.Bool("token-stdin"):
		tok, err = p.ReadLine(ctx)
	case p.CanPrompt():
		tok, err = p.PromptSecret(ctx, "Enter token (id:secret): ")
	default:
		return "", clierr.New(clierr.CodeValidation, "token required").
			WithHint("pipe it via --token-stdin, or run in a terminal")
	}
	if err != nil {
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
