package commands

import (
	"context"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type loginCmd struct {
	dir   string
	store config.CredStore
}

// NewLogin builds the `login` command (local).
func NewLogin(dir string, store config.CredStore) runtime.Command {
	return &loginCmd{dir: dir, store: store}
}

func (loginCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "login",
		Usage:       "Login to a Rancher server",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "serverurl", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "token", Type: "string", Required: true},
			{Name: "name", Type: "string"},
			{Name: "cacert", Type: "string"},
			{Name: "context", Type: "string"},
		},
		Examples: []string{"rancher login https://rancher.ex --token id:secret --name prod --context c-1:p-1"},
	}
}

func (loginCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}

func (l loginCmd) Execute(_ context.Context, in runtime.Input, _ safety.Effect) (runtime.Result, error) {
	// Defense-in-depth: the pipeline's validateInput normally rejects missing
	// serverurl/--token first, but Execute must be safe when called directly
	// (tests, future callers) — never index in.Args() unguarded.
	if len(in.Args()) == 0 {
		// Message matches the pipeline's validateInput so the error is identical
		// regardless of invocation path (agents may key on message text).
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "argument <serverurl> is required").
			WithHint("provide <serverurl>")
	}
	raw := in.Args()[0]
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "invalid server URL: "+raw)
	}
	// Keep only scheme://host — drop path, query, fragment, AND userinfo. Dropping
	// User is a security requirement: `https://admin:secret@host` must not persist
	// credentials into config.yaml (secrets live only in the credential store).
	u.Path, u.RawQuery, u.Fragment, u.User = "", "", "", nil

	// --token presence is enforced by the pipeline; here validate its shape.
	token := in.String("token")
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
	if ctx := in.String("context"); ctx != "" {
		cluster, project, _ = strings.Cut(ctx, ":")
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
