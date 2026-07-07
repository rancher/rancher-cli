package commands

import (
	"context"
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
)

type logoutCmd struct {
	dir   string
	store config.CredStore
}

// NewLogout builds `auth logout` (local): clear the stored login token for a
// server. Keeps the server/context config entries and never touches the exec-cred
// cache (that is `auth delete-token`'s domain).
func NewLogout(dir string, store config.CredStore) runtime.Command {
	return &logoutCmd{dir: dir, store: store}
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
	}
}

func (logoutCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}

func (l logoutCmd) Execute(_ context.Context, in runtime.Input, _ safety.Effect, _ *ui.Prompter) (runtime.Result, error) {
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

	targets := []string{}
	switch {
	case all:
		for s := range cfg.Servers {
			targets = append(targets, s)
		}
	case name != "":
		if _, ok := cfg.Servers[name]; !ok {
			cands := make([]clierr.Candidate, 0, len(cfg.Servers))
			for s := range cfg.Servers {
				cands = append(cands, clierr.Candidate{ID: s, Name: s})
			}
			return runtime.Result{}, clierr.New(clierr.CodeNotFound, "server not found: "+name).
				WithCandidates(cands)
		}
		targets = []string{name}
	default:
		// No arg: the current context's server. CurrentServer guards empty/stale
		// CurrentContext and a missing server; re-wrap its login-specific hint.
		cur, _, cerr := cfg.CurrentServer()
		if cerr != nil {
			return runtime.Result{}, clierr.New(clierr.CodeContextNotSet,
				"no current context; specify a server name or --all").
				WithHint("rancher auth logout <name>  OR  rancher auth logout --all")
		}
		targets = []string{cur}
	}

	done := make([]string, 0, len(targets))
	for _, s := range targets {
		if derr := l.store.DeleteToken(s); derr != nil {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("failed to log out %q: %v", s, derr)).
				WithHint(fmt.Sprintf("already logged out: %v", done))
		}
		done = append(done, s)
	}

	// Unset the current context if it no longer resolves to a logged-in server.
	switch {
	case all:
		if cfg.CurrentContext != "" {
			cfg.CurrentContext = ""
			if werr := cfg.Write(); werr != nil {
				return runtime.Result{}, werr
			}
		}
	default:
		if cfg.CurrentContext != "" {
			if cur, ok := cfg.Contexts[cfg.CurrentContext]; ok {
				for _, s := range targets {
					if cur.Server == s {
						cfg.CurrentContext = ""
						if werr := cfg.Write(); werr != nil {
							return runtime.Result{}, werr
						}
						break
					}
				}
			}
		}
	}

	sort.Strings(targets)
	return runtime.Result{Kind: "LogoutResult", Data: map[string]any{"loggedOut": targets}}, nil
}
