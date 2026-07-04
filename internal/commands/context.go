package commands

import (
	"context"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type contextCurrentCmd struct{ dir string }

// NewContextCurrent builds `context current` (local).
func NewContextCurrent(dir string) runtime.Command { return &contextCurrentCmd{dir: dir} }

func (contextCurrentCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "context current", Usage: "Show the current context", CommandType: runtime.Local}
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
			WithHint("run: rancher login <SERVERURL>")
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "current context \""+cfg.CurrentContext+"\" not found").
			WithHint("run: rancher context use <name> or rancher login")
	}
	return runtime.Result{Kind: "Context", Data: map[string]string{
		"name": cfg.CurrentContext, "server": ctx.Server, "cluster": ctx.Cluster, "project": ctx.Project,
	}}, nil
}

type contextSwitchCmd struct{ dir string }

// NewContextSwitch builds `context switch <cluster:project>` (local).
func NewContextSwitch(dir string) runtime.Command { return &contextSwitchCmd{dir: dir} }

func (contextSwitchCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "context switch",
		Usage:       "Switch the current context scope",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "cluster:project", Type: "string", Required: true}},
	}
}
func (contextSwitchCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}
func (c contextSwitchCmd) Execute(_ context.Context, in runtime.Input, _ safety.Effect) (runtime.Result, error) {
	// Defense-in-depth guard (Execute must be safe when called directly). Message
	// text matches the pipeline's validateInput ("argument <name> is required") so
	// the error is identical regardless of invocation path.
	if len(in.Args()) == 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"argument <cluster:project> is required").
			WithHint("provide <cluster:project>")
	}
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	if cfg.CurrentContext == "" {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "no current context").
			WithHint("run: rancher login <SERVERURL>")
	}
	// Match Effective()'s RANCHER_CONTEXT semantics: "c-1:p-1" sets both; a
	// colon-less "c-1" sets only the cluster and LEAVES the project unchanged.
	cluster, project, hasProject := strings.Cut(in.Args()[0], ":")
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeContextNotSet, "current context \""+cfg.CurrentContext+"\" not found").
			WithHint("run: rancher context use <name> or rancher login")
	}
	ctx.Cluster = cluster
	if hasProject {
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

// NewContextUse builds `context use <name>` (local): activate a named context.
// This is the primitive that lets a user pick among contexts targeting the same
// server (which `context switch` — scope only — cannot do).
func NewContextUse(dir string) runtime.Command { return &contextUseCmd{dir: dir} }

func (contextUseCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "context use",
		Usage:       "Activate a named context",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:    []string{"rancher context use acme-prod"},
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
