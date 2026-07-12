package commands

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type contextCurrentCmd struct{ dir string }

// NewContextCurrent builds `config current-context` (local).
func NewContextCurrent(dir string) runtime.Command { return &contextCurrentCmd{dir: dir} }

func (contextCurrentCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "config current-context", Usage: "Show the current context", CommandType: runtime.Local, Toolset: "core"}
}
func (c contextCurrentCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	if cfg.CurrentContext == "" {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "no current context").
			WithHint(config.LoginHint)
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
	// `--current` is optional: Phase 1 only targets the current context, so the flag
	// is accepted for kubectl parity and reserved for the future named-context
	// variant. It is NOT marked Required — a required bool cannot be enforced by
	// validateInput (it checks a flag's String() value, always "" for a bool). The
	// meaningful requirement is that at least one scope flag is set (below).
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
		Toolset:  "core",
	}
}
func (c contextSetCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	cluster, project := in.String("cluster"), in.String("project")
	if ce := rejectShadowedGlobalOverride(cluster, project); ce != nil {
		return runtime.Result{}, ce
	}
	// Require at least one scope flag so a bare `set-context [--current]` is a
	// clear error, not a silent no-op that rewrites the config unchanged.
	// (Clearing a field back to empty is not supported in Phase 1 — it needs
	// Input.IsSet to tell "flag omitted" from "flag set to empty".)
	if cluster == "" && project == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"provide --cluster and/or --project").
			WithHint("run: rancher config set-context --current --cluster <c> [--project <p>]")
	}
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	if cfg.CurrentContext == "" {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "no current context").
			WithHint(config.LoginHint)
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "current context \""+cfg.CurrentContext+"\" not found").
			WithHint("run: rancher config use-context <name> or rancher auth login")
	}
	if cluster != "" {
		ctx.Cluster = cluster
	}
	if project != "" {
		ctx.Project = project
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

// NewContextUse builds `config use-context <name>` (local): activate a named context.
// This is the primitive that lets a user pick among contexts targeting the same
// server (which `config set-context` — scope only — cannot do).
func NewContextUse(dir string) runtime.Command { return &contextUseCmd{dir: dir} }

func (contextUseCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config use-context",
		Usage:       "Activate a named context",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:    []string{"rancher config use-context acme-prod"},
		Toolset:     "core",
	}
}
func (c contextUseCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
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

// CompleteArgs implements runtime.ShellCompleter: `config use-context <TAB>`
// completes context names. dir is the resolved config dir the TAB-time
// callback passes in — never c.dir, which is empty on the zero-deps probe
// instance adaptEntry type-asserts at wiring time, so reading it here would
// pass unit tests built with a real dir and return empty forever at real
// TAB time.
func (contextUseCmd) CompleteArgs(_ context.Context, dir string) []string {
	return contextNames(dir)
}

// rejectShadowedGlobalOverride guards against the CW-3 footgun: the global
// --context flag and this command's local --cluster/--project flags are
// separate, unsynchronized urfave/cli Flag objects that happen to share a
// name axis. If the caller supplied a global --context cluster and/or
// project but this command's own matching local flag is empty, the global
// value would otherwise be silently dropped from the write with no error —
// the global flag scopes server reads, not config writes. localCluster/
// localProject are this command's own (possibly empty) --cluster/--project
// values.
func rejectShadowedGlobalOverride(localCluster, localProject string) *clierr.Error {
	var globalCluster, globalProject string
	if ctxVal := config.ContextOverride(); ctxVal != "" {
		globalCluster, globalProject, _ = config.SplitContext(ctxVal)
	}
	if localCluster == "" && globalCluster != "" {
		return clierr.New(clierr.CodeValidation,
			"--context at the root scopes server operations, not config writes").
			WithHint("pass --cluster explicitly: rancher config set-context --current --cluster <c>")
	}
	if localProject == "" && globalProject != "" {
		return clierr.New(clierr.CodeValidation,
			"--context at the root scopes server operations, not config writes").
			WithHint("pass --project explicitly: rancher config set-context --current --project <p>")
	}
	return nil
}
