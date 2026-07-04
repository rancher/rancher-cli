package commands

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type contextCurrentCmd struct{ dir string }

// NewContextCurrent builds `context current` (local).
func NewContextCurrent(dir string) runtime.Command { return &contextCurrentCmd{dir: dir} }

func (contextCurrentCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "config current-context", Usage: "Show the current context", CommandType: runtime.Local}
}
func (contextCurrentCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}
func (c contextCurrentCmd) Execute(_ context.Context, _ runtime.Input, _ safety.Effect) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	if cfg.CurrentContext == "" {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "no current context").
			WithHint("run: rancher auth login <SERVERURL>")
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "current context \""+cfg.CurrentContext+"\" not found").
			WithHint("run: rancher config use-context <name> or rancher auth login")
	}
	return runtime.Result{Kind: "Context", Data: map[string]string{
		"name": cfg.CurrentContext, "server": ctx.Server, "cluster": ctx.Cluster, "project": ctx.Project,
	}}, nil
}

type contextSetCmd struct{ dir string }

// NewContextSet builds `config set-context` (local).
func NewContextSet(dir string) runtime.Command { return &contextSetCmd{dir: dir} }

func (contextSetCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config set-context",
		Usage:       "Set the current context's cluster/project scope",
		CommandType: runtime.Local,
		Flags: []runtime.ArgSpec{
			{Name: "current", Type: "bool"},
			{Name: "cluster", Type: "string"},
			{Name: "project", Type: "string"},
		},
		Examples: []string{"rancher config set-context --current --cluster c-1 --project p-1"},
	}
}
func (contextSetCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}
func (c contextSetCmd) Execute(_ context.Context, in runtime.Input, _ safety.Effect) (runtime.Result, error) {
	if !in.Bool("current") {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--current is required").
			WithHint("run: rancher config set-context --current --cluster <c> [--project <p>]")
	}
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	if cfg.CurrentContext == "" {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "no current context").
			WithHint("run: rancher auth login <SERVERURL>")
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "current context \""+cfg.CurrentContext+"\" not found").
			WithHint("run: rancher config use-context <name> or rancher auth login")
	}
	if cl := in.String("cluster"); cl != "" {
		ctx.Cluster = cl
	}
	if pr := in.String("project"); pr != "" {
		ctx.Project = pr
	}
	cfg.Contexts[cfg.CurrentContext] = ctx
	if err := cfg.Write(); err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "Context", Data: map[string]string{
		"name": cfg.CurrentContext, "cluster": ctx.Cluster, "project": ctx.Project,
	}}, nil
}

type contextUseCmd struct{ dir string }

// NewContextUse builds `context use <name>` (local): activate a named context.
// This is the primitive that lets a user pick among contexts targeting the same
// server (which `context switch` — scope only — cannot do).
func NewContextUse(dir string) runtime.Command { return &contextUseCmd{dir: dir} }

func (contextUseCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config use-context",
		Usage:       "Activate a named context",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:    []string{"rancher config use-context acme-prod"},
	}
}
func (contextUseCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}
func (c contextUseCmd) Execute(_ context.Context, in runtime.Input, _ safety.Effect) (runtime.Result, error) {
	if len(in.Args()) == 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "argument <name> is required").
			WithHint("provide <name>")
	}
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	name := in.Args()[0]
	if _, ok := cfg.Contexts[name]; !ok {
		cands := make([]clierr.Candidate, 0, len(cfg.Contexts))
		for cn := range cfg.Contexts {
			cands = append(cands, clierr.Candidate{ID: cn, Name: cn})
		}
		return runtime.Result{}, clierr.New(clierr.CodeNotFound, "context not found: "+name).
			WithCandidates(cands)
	}
	cfg.CurrentContext = name
	if err := cfg.Write(); err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "Context", Data: map[string]string{"current": name}}, nil
}
