package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
// server config entries; `auth delete-token` remains for clearing
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
		Shape: runtime.ShapeText,
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
		notices      []string
		urlByName    = map[string]string{}
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
		for _, s := range targets {
			urlByName[s] = cfg.Servers[s].URL
		}
		policy := config.EffectivePolicy(cfg.CredentialStore)
		if all {
			deletedFrom, deletedCache, notices, err = l.deleteAll(cmdCtx, cfg, targets, policy)
		} else {
			deletedFrom, deletedCache, notices, err = l.deleteTargets(cmdCtx, cfg, targets, policy)
		}
		return err
	}); err != nil {
		return runtime.Result{}, err
	}

	sort.Strings(targets)
	sort.Strings(deletedCache)
	return runtime.Result{Kind: "LogoutResult", Data: map[string]any{
		"loggedOut": targets, "deletedFrom": deletedFrom, "deletedCache": deletedCache,
	}, Text: renderLogoutText(urlByName, deletedFrom, len(deletedCache)), Warnings: notices}, nil
}

// renderLogoutText is the TTY view of a logout: which servers, which
// credential backends were touched, and how many cached exec credentials
// went. Silent success is for scripts (exit code + JSON envelope); a person
// deserves the confirmation. It renders every deletedFrom entry, not just
// the requested targets: logout --all's orphaned-helper sweep reports
// records whose server name never made it into config (no URL known) — the
// text must not diverge from the JSON envelope for exactly the
// crash-recovery case that sweep exists to catch. Logout never clears the
// current-server pointer (including --all): the entry survives,
// credential-less, and the pointer keeps naming it.
func renderLogoutText(urlByName map[string]string, deletedFrom map[string][]string, cacheCount int) string {
	names := make([]string, 0, len(deletedFrom))
	for s := range deletedFrom {
		names = append(names, s)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, s := range names {
		if url := urlByName[s]; url != "" {
			fmt.Fprintf(&b, "logged out of %s (%s)\n", s, url)
		} else {
			fmt.Fprintf(&b, "logged out of %s\n", s)
		}
		if from := deletedFrom[s]; len(from) > 0 {
			fmt.Fprintf(&b, "  token removed from: %s\n", strings.Join(from, ", "))
		} else {
			b.WriteString("  no stored token found\n")
		}
	}
	if cacheCount > 0 {
		fmt.Fprintf(&b, "exec-credential cache entries removed: %d\n", cacheCount)
	}
	return strings.TrimSuffix(b.String(), "\n")
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
		// No arg: the current server. CurrentServerEntry guards an empty or
		// dangling pointer; re-wrap its login-specific hint.
		cur, _, cerr := cfg.CurrentServerEntry()
		if cerr != nil {
			return nil, clierr.New(clierr.CodeNotLoggedIn,
				"no current server; specify a server name or --all").
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
func (l logoutCmd) deleteTargets(ctx context.Context, cfg *config.Config, targets []string, policy string) (map[string][]string, []string, []string, error) {
	deletedFrom := make(map[string][]string, len(targets))
	var deletedCache, notices []string
	done := make([]string, 0, len(targets))
	for _, s := range targets {
		from, cache, ns, derr := config.DeleteTokenEverywhere(ctx, l.dir, config.ServerRef{Name: s, URL: cfg.Servers[s].URL}, policy)
		notices = append(notices, ns...)
		if derr != nil {
			return nil, nil, nil, deleteFailure(fmt.Sprintf("failed to log out %q", s), derr, done, deletedFrom, deletedCache, notices)
		}
		deletedFrom[s] = from
		deletedCache = append(deletedCache, cache...)
		done = append(done, s)
	}
	return deletedFrom, deletedCache, notices, nil
}

// deleteAll is `logout --all`'s delete path: it deletes every target's login
// token (both backends, same as deleteTargets) but wipes the WHOLE
// exec-credential cache once via config.DeleteAllExecCache rather than
// looping a per-server host-scoped delete — cheaper, and it also catches
// orphaned/bare cache entries that match no configured server (which no
// per-server delete, keyed to a currently-configured server's host, could
// ever reach). See DeleteAllExecCache's doc for why.
func (l logoutCmd) deleteAll(ctx context.Context, cfg *config.Config, targets []string, policy string) (map[string][]string, []string, []string, error) {
	deletedFrom := make(map[string][]string, len(targets))
	var notices []string
	done := make([]string, 0, len(targets))
	for _, s := range targets {
		from, ns, derr := config.DeleteLoginToken(ctx, l.dir, config.ServerRef{Name: s, URL: cfg.Servers[s].URL}, policy)
		notices = append(notices, ns...)
		if derr != nil {
			return nil, nil, nil, deleteFailure(fmt.Sprintf("failed to log out %q", s), derr, done, deletedFrom, nil, notices)
		}
		deletedFrom[s] = from
		done = append(done, s)
	}
	deletedCache, cacheNotices, err := config.DeleteAllExecCache(l.dir)
	notices = append(notices, cacheNotices...)
	if err != nil {
		return nil, nil, nil, deleteFailure("failed to delete exec-credential cache", err, done, deletedFrom, nil, notices)
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
		return nil, nil, nil, deleteFailure("failed to delete orphaned helper credentials", rerr, done, deletedFrom, deletedCache, notices)
	}
	return deletedFrom, deletedCache, notices, nil
}

// LogoutAllPartialFailure is deleteAll's structured payload for a
// mid-sequence failure: everything already deleted before the failing step,
// so a caller decoding the error sees exactly what succeeded rather than a
// bare failure that looks like nothing happened — the credentials for
// LoggedOut/DeletedFrom/DeletedCache are already gone regardless of which
// later step failed.
type LogoutAllPartialFailure struct {
	LoggedOut    []string            `json:"loggedOut"`
	DeletedFrom  map[string][]string `json:"deletedFrom,omitempty"`
	DeletedCache []string            `json:"deletedCache,omitempty"`
}

// TextDetails projects which servers were already logged out, which
// credential backends were already removed per server, and how many cached
// kubectl credentials went, closing with the standing recovery note: a
// failed --all run never undoes what it already deleted.
func (p LogoutAllPartialFailure) TextDetails() []string {
	lines := []string{"already logged out: " + strings.Join(p.LoggedOut, ", ")}
	servers := make([]string, 0, len(p.DeletedFrom))
	for s := range p.DeletedFrom {
		servers = append(servers, s)
	}
	sort.Strings(servers)
	for _, s := range servers {
		lines = append(lines, fmt.Sprintf("credentials removed: %s: %s", s, strings.Join(p.DeletedFrom[s], ", ")))
	}
	if len(p.DeletedCache) > 0 {
		lines = append(lines, "cached kubectl credentials removed: "+strings.Join(p.DeletedCache, ", "))
	}
	lines = append(lines, "those credentials are gone regardless of this failure — re-run auth logout --all to finish")
	return lines
}

// deleteFailure shapes a delete-step failure: the caller's own cancellation
// (Ctrl-C during a contended lock wait inside the delete) passes through
// unchanged so errors.Is(err, context.Canceled) keeps working — dressing it
// as a delete failure would send the user diagnosing helper problems.
// Anything else keeps its classification (a lock timeout stays LOCK_TIMEOUT
// and retryable) under the step-prefixed message, plus the
// LogoutAllPartialFailure details so nothing already deleted is vaporized
// just because a later step in the same --all run failed — its TextDetails
// projection's first line already names what was already logged out, so no
// separate hint repeats the same fact. notices carries every notice
// (keychain-unreachable degrades, say) already recorded by a step that ran
// successfully before this one failed — the pipeline discards Result on an
// error return, so this is their only ride back to the operator.
func deleteFailure(step string, err error, done []string, deletedFrom map[string][]string, deletedCache []string, notices []string) error {
	return wrapUnlessCanceled(err, func(err error) error {
		return clierr.Wrap(err, fmt.Sprintf("%s: %v", step, err)).
			WithDetails(LogoutAllPartialFailure{LoggedOut: done, DeletedFrom: deletedFrom, DeletedCache: deletedCache}).
			WithWarnings(notices...)
	})
}
