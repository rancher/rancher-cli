package commands

import (
	"context"
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type logoutCmd struct {
	dir string
}

// NewLogout builds `auth logout` (local): clear the stored login token for a
// server and drain matching exec-credential cache entries via
// config.DrainToken — the single drain point (ARCHITECTURE.md). Keeps the
// server/context config entries; `auth delete-token` remains for clearing
// cache entries outside a logout.
func NewLogout(dir string) runtime.Command {
	return &logoutCmd{dir: dir}
}

func (logoutCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth logout",
		Usage:       "Clear stored login credentials for a server",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:       []runtime.ArgSpec{{Name: "all", Type: "bool"}},
		Examples: []string{
			"rancher auth logout",
			"rancher auth logout prod",
			"rancher auth logout --all",
		},
		Toolset: "core",
	}
}

func (l logoutCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	all := in.Bool("all")
	name := ""
	if a := in.Args(); len(a) > 0 {
		name = a[0]
	}
	if all && name != "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"specify a server name or --all, not both").
			WithHint("rancher auth logout <name>  OR  rancher auth logout --all")
	}

	cfg, err := config.Load(config.FilePath(l.dir))
	if err != nil {
		return runtime.Result{}, err
	}

	targets, err := logoutTargets(cfg, all, name)
	if err != nil {
		return runtime.Result{}, err
	}

	deletedFrom, deletedCache, err := l.drainTargets(cfg, targets)
	if err != nil {
		return runtime.Result{}, err
	}

	if err := clearCurrentContext(cfg, all, targets); err != nil {
		return runtime.Result{}, err
	}

	sort.Strings(targets)
	sort.Strings(deletedCache)
	return runtime.Result{Kind: "LogoutResult", Data: map[string]any{
		"loggedOut": targets, "deletedFrom": deletedFrom, "deletedCache": deletedCache,
	}}, nil
}

// logoutTargets resolves which servers a logout request applies to.
func logoutTargets(cfg *config.Config, all bool, name string) ([]string, error) {
	switch {
	case all:
		targets := make([]string, 0, len(cfg.Servers))
		for s := range cfg.Servers {
			targets = append(targets, s)
		}
		return targets, nil
	case name != "":
		if _, ok := cfg.Servers[name]; !ok {
			cands := make([]clierr.Candidate, 0, len(cfg.Servers))
			for s := range cfg.Servers {
				cands = append(cands, clierr.Candidate{ID: s, Name: s})
			}
			return nil, clierr.New(clierr.CodeNotFound, "server not found: "+name).
				WithCandidates(cands)
		}
		return []string{name}, nil
	default:
		// No arg: the current context's server. CurrentServer guards empty/stale
		// CurrentContext and a missing server; re-wrap its login-specific hint.
		cur, _, cerr := cfg.CurrentServer()
		if cerr != nil {
			return nil, clierr.New(clierr.CodeContextNotSet,
				"no current context; specify a server name or --all").
				WithHint("rancher auth logout <name>  OR  rancher auth logout --all")
		}
		return []string{cur}, nil
	}
}

// drainTargets removes each target's login token from both backends AND its
// exec-credential cache entries via config.DrainToken (the single drain
// point), reporting the token backends touched per server so a partial
// failure names what was already drained, plus every cache key removed
// across all targets.
func (l logoutCmd) drainTargets(cfg *config.Config, targets []string) (map[string][]string, []string, error) {
	deletedFrom := make(map[string][]string, len(targets))
	var deletedCache []string
	done := make([]string, 0, len(targets))
	for _, s := range targets {
		from, cache, derr := config.DrainToken(l.dir, s, cfg.Servers[s].URL)
		if derr != nil {
			return nil, nil, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("failed to log out %q: %v", s, derr)).
				WithHint(fmt.Sprintf("already logged out: %v", done))
		}
		deletedFrom[s] = from
		deletedCache = append(deletedCache, cache...)
		done = append(done, s)
	}
	return deletedFrom, deletedCache, nil
}

// clearCurrentContext unsets the current context when it no longer resolves to
// a logged-in server: always under --all when one is set, otherwise only when
// the current context targets one of the servers just logged out.
func clearCurrentContext(cfg *config.Config, all bool, targets []string) error {
	if cfg.CurrentContext == "" {
		return nil
	}
	if all {
		cfg.CurrentContext = ""
		return cfg.Write()
	}
	cur, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return nil
	}
	for _, s := range targets {
		if cur.Server == s {
			cfg.CurrentContext = ""
			return cfg.Write()
		}
	}
	return nil
}
