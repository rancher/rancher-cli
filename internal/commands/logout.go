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
// server and delete matching exec-credential cache entries via
// config.DeleteTokenEverywhere — the single delete point (ARCHITECTURE.md). Keeps the
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
		Flags:       []runtime.ArgSpec{{Name: "all", Type: "bool", Desc: "log out of all configured servers"}},
		Examples: []string{
			"rancher auth logout",
			"rancher auth logout prod",
			"rancher auth logout --all",
		},
	}
}

func (l logoutCmd) Execute(cmdCtx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
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

	var (
		targets      []string
		deletedFrom  map[string][]string
		deletedCache []string
	)
	if err := config.WithLock(cmdCtx, l.dir, func() error {
		cfg, err := config.Load(config.FilePath(l.dir))
		if err != nil {
			return err
		}
		targets, err = logoutTargets(cfg, all, name)
		if err != nil {
			return err
		}
		policy := config.EffectivePolicy(cfg.CredentialStore)
		if all {
			deletedFrom, deletedCache, err = l.deleteAll(cmdCtx, cfg, targets, policy)
		} else {
			deletedFrom, deletedCache, err = l.deleteTargets(cmdCtx, cfg, targets, policy)
		}
		if err != nil {
			return err
		}
		return clearCurrentContext(cfg, all, targets)
	}); err != nil {
		return runtime.Result{}, err
	}

	sort.Strings(targets)
	sort.Strings(deletedCache)
	return runtime.Result{Kind: "LogoutResult", Data: map[string]any{
		"loggedOut": targets, "deletedFrom": deletedFrom, "deletedCache": deletedCache,
	}}, nil
}

// CompleteArgs implements runtime.ShellCompleter: `auth logout <server> <TAB>`
// completes server names from the already-loaded config snapshot (AC-14).
func (logoutCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	return view.ServerNames()
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
			return nil, serverNotFoundWithCandidates(cfg, name)
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

// deleteTargets removes each target's login token from both backends AND its
// exec-credential cache entries via config.DeleteTokenEverywhere (the single delete
// point), reporting the token backends touched per server so a partial
// failure names what was already deleted, plus every cache key removed
// across all targets.
func (l logoutCmd) deleteTargets(ctx context.Context, cfg *config.Config, targets []string, policy string) (map[string][]string, []string, error) {
	deletedFrom := make(map[string][]string, len(targets))
	var deletedCache []string
	done := make([]string, 0, len(targets))
	for _, s := range targets {
		from, cache, derr := config.DeleteTokenEverywhere(ctx, l.dir, config.ServerRef{Name: s, URL: cfg.Servers[s].URL}, policy)
		if derr != nil {
			return nil, nil, deleteFailure(fmt.Sprintf("failed to log out %q", s), derr, done)
		}
		deletedFrom[s] = from
		deletedCache = append(deletedCache, cache...)
		done = append(done, s)
	}
	return deletedFrom, deletedCache, nil
}

// deleteAll is `logout --all`'s delete path: it deletes every target's login
// token (both backends, same as deleteTargets) but wipes the WHOLE
// exec-credential cache once via config.DeleteAllExecCache rather than
// looping a per-server host-scoped delete — cheaper, and it also catches
// orphaned/bare cache entries that match no configured server (which no
// per-server delete, keyed to a currently-configured server's host, could
// ever reach). See DeleteAllExecCache's doc for why.
func (l logoutCmd) deleteAll(ctx context.Context, cfg *config.Config, targets []string, policy string) (map[string][]string, []string, error) {
	deletedFrom := make(map[string][]string, len(targets))
	done := make([]string, 0, len(targets))
	for _, s := range targets {
		from, derr := config.DeleteLoginToken(ctx, l.dir, config.ServerRef{Name: s, URL: cfg.Servers[s].URL}, policy)
		if derr != nil {
			return nil, nil, deleteFailure(fmt.Sprintf("failed to log out %q", s), derr, done)
		}
		deletedFrom[s] = from
		done = append(done, s)
	}
	deletedCache, err := config.DeleteAllExecCache(l.dir)
	if err != nil {
		return nil, nil, deleteFailure("failed to delete exec-credential cache", err, done)
	}

	// Reach helper credentials whose server name never made it into
	// config.yaml (a crash or failed config write between a successful helper
	// store and cfg.Write): no per-server delete above could see them. Merge
	// each deleted record into deletedFrom under its server name so the
	// envelope stays stable.
	orphans, rerr := config.DeleteOrphanedHelperCredentials(ctx, l.dir)
	for _, e := range orphans {
		deletedFrom[e.Name] = append(deletedFrom[e.Name], "helper:"+e.Helper)
	}
	if rerr != nil {
		return nil, nil, deleteFailure("failed to delete orphaned helper credentials", rerr, done)
	}
	return deletedFrom, deletedCache, nil
}

// deleteFailure shapes a delete-step failure: the caller's own cancellation
// (Ctrl-C during a contended lock wait inside the delete) passes through
// unchanged so errors.Is(err, context.Canceled) keeps working — dressing it
// as a delete failure would send the user diagnosing helper problems.
// Anything else keeps its classification (a lock timeout stays LOCK_TIMEOUT
// and retryable) under the step-prefixed message and the already-done hint.
func deleteFailure(step string, err error, done []string) error {
	return wrapUnlessCanceled(err, func(err error) error {
		return clierr.Wrap(err, fmt.Sprintf("%s: %v", step, err)).
			WithHint(fmt.Sprintf("already logged out: %v", done))
	})
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
