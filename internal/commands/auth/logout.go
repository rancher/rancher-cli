package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

type logoutCmd struct {
	dir string
}

// NewLogout builds `auth logout` (local): delete the stored login token
// for a server and the exec-credential cache records its login made, then
// revoke on the server the tokens among them the CLI created and can prove
// it created (token.DeleteAndRecord). Keeps the server config entries;
// `auth delete-token` remains for clearing cache entries outside a logout.
func NewLogout(dir string) runtime.Command {
	return &logoutCmd{dir: dir}
}

func (logoutCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "auth logout",
		Usage: "Clear stored login credentials for a server",
		Desc: "Deletes the server entry's stored login token in every backend and the exec-credential cache records " +
			"made from its login, then deletes on the server the tokens among them the CLI created and recorded: " +
			"the login token, its superseded tokens and the cached cluster tokens. Read-only mode set by config " +
			"or the environment does not stop those deletes. data.revoked lists the tokens the server confirmed " +
			"deleted; data.notRevoked lists the " +
			"rest with a reason and a retry: nextLogin (the next login the CLI creates for the entry deletes it), " +
			"manual (recovery holds the commands) or none (a pasted token, not the CLI's). A token the command " +
			"could not revoke because the server's certificate failed verification is listed with code TLS_TRUST " +
			"and the certificate's details. Exits 0 when every " +
			"local delete succeeds, whatever the server answered. A Ctrl-C or termination signal that stops the " +
			"local delete, or leaves a token unreached, ends it with INTERRUPTED (exit 130), with both lists in " +
			"the error's details; a signal after the local delete that leaves no token unsent does not stop it.",
		CommandType:  runtime.Local,
		ServerAccess: runtime.ServerAccessWrite,
		Args:         []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:        []runtime.ArgSpec{{Name: "all", Type: "bool", Desc: "log out of all configured servers"}},
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
			"specify a server name or --all, not both; log out of one server with: "+clierr.Rancher+" auth logout <name>; or of all with: "+clierr.Rancher+" auth logout --all")
	}

	var (
		targets   []string
		d         *token.Deletion
		notices   []string
		localErr  error
		urlByName = map[string]string{}
	)
	if err := config.WithLock(cmdCtx, l.dir, func() error {
		cfg, err := config.Load(config.FilePath(l.dir))
		if err != nil {
			return err
		}
		targets, err = logoutTargets(l.dir, cfg, all, name)
		if err != nil {
			return err
		}
		for _, s := range targets {
			urlByName[s] = config.DisplayServerURL(cfg.Servers[s].URL)
		}
		policy, perr := config.EffectivePolicy(cfg.CredentialStore)
		if perr != nil {
			return perr
		}
		// The directory's unfinished store change goes first, after the
		// logout's own validation and before its first write.
		prelude, perr := config.PrepareCredentialDelete(cmdCtx, l.dir, cfg)
		if perr != nil {
			return perr
		}
		d, notices, localErr, err = l.deleteLocally(cmdCtx, cfg, targets, all, policy, slices.Clone(prelude))
		return err
	}); err != nil {
		return runtime.Result{}, err
	}

	// The revokes run outside every lock, from what the delete read.
	d.RevokeTokens(cmdCtx)
	notices = append(notices, d.Settle(cmdCtx)...)
	rep := d.Report()
	notices = append(notices, d.Warnings()...)
	if localErr != nil {
		return runtime.Result{}, l.localFailure(localErr, d, rep, logoutRerun(targets, all), notices)
	}
	if d.Cancelled() {
		return runtime.Result{}, clierr.New(clierr.CodeInterrupted,
			"stopped before every token was revoked; the logout itself completed, and the details list each token").
			WithDetails(LogoutAllPartialFailure{LoggedOut: d.DeletedTargets(), DeletedFrom: d.DeletedFrom, DeletedCache: d.DeletedCache,
				Revoked: rep.Revoked, NotRevoked: rep.NotRevoked}).
			WithWarnings(notices...)
	}

	slices.Sort(targets)
	deletedCache := d.DeletedCache
	text := renderLogoutText(urlByName, d.DeletedFrom, len(deletedCache), d.TextLines())
	return runtime.Result{Kind: "LogoutResult", Data: map[string]any{
		"loggedOut": targets, "deletedFrom": d.DeletedFrom, "deletedCache": deletedCache,
		"revoked": rep.Revoked, "notRevoked": rep.NotRevoked,
	},
		Text:       text,
		StyledText: func(s style.Style) string { return styleLogoutText(text, len(d.DeletedFrom), s) },
		Warnings:   notices,
	}, nil
}

// deleteLocally is the logout's lock span after the prelude: the local
// delete (token.DeleteAndRecord) with every backend's login delete, then
// the last write, which resumes the targets' cache writes, and under --all
// the orphaned-helper sweep. err is a failure before anything was deleted;
// localErr one after, which the revokes still follow.
func (l logoutCmd) deleteLocally(ctx context.Context, cfg *config.Config, targets []string, all bool, policy string, notices []string) (d *token.Deletion, out []string, localErr, err error) {
	cmd := token.DeleteForLogout
	if all {
		cmd = token.DeleteForLogoutAll
	}
	cache := config.NewFileStore(l.dir)
	d, localErr = token.DeleteAndRecord(ctx, cfg, token.LocalDelete{
		Dir: l.dir, Command: cmd, Targets: targets, Cache: cache, Rerun: logoutRerun(targets, all),
		DeleteLogin: func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
			return config.DeleteLoginToken(ctx, l.dir, ref, policy)
		},
	})
	if ns, ok := cache.(config.NoticeSource); ok {
		notices = append(notices, ns.DrainNotices()...)
	}
	if d == nil {
		return nil, nil, nil, config.WithNotices(localErr, notices)
	}
	notices = append(notices, d.Notices...)
	if d.LoginErr != nil {
		// The entries whose login delete failed hold their login again,
		// unless the token was gone after the failure; every target's pause
		// stays for the next exec-credential cache miss.
		return d, notices, localErr, nil
	}
	notices = l.writeLast(cfg, targets, notices)
	if all {
		// The sweep runs whatever the cache delete did: it reaches helper
		// credentials whose server name never made it into config.yaml (a
		// crash or failed config write between a successful helper store
		// and cfg.Write), which nothing above can see, and the command's
		// promise is to erase every credential it can still reach. Each
		// deleted record is merged into DeletedFrom under its server name
		// and reported unproven: nothing names its token.
		orphans, sweepErr := config.DeleteOrphanedHelperCredentials(ctx, l.dir)
		for _, e := range orphans {
			d.DeletedFrom[e.Name] = append(d.DeletedFrom[e.Name], "helper:"+e.Helper)
			d.AddUnproven(e.CanonicalURL)
		}
		if sweepErr != nil {
			localErr = &sweepFailure{cache: d.CacheErr, sweep: sweepErr}
		}
	}
	return d, notices, localErr, nil
}

// logoutRerun is the logout that finishes one whose local delete did not
// complete: the same entry, or --all.
func logoutRerun(targets []string, all bool) string {
	if all {
		return clierr.Rancher + " auth logout --all"
	}
	return clierr.Rancher + " auth logout " + clierr.ShellQuote(strings.Join(targets, " "))
}

// sweepFailure is a logout --all whose orphaned-helper sweep failed, with
// the cache delete's own failure when it failed too.
type sweepFailure struct{ cache, sweep error }

func (e *sweepFailure) Error() string { return e.sweep.Error() }

// Unwrap lets errors.Is reach both causes.
func (e *sweepFailure) Unwrap() []error { return []error{e.cache, e.sweep} }

// localFailure shapes a local delete that failed after it began deleting:
// the step that failed, what was already deleted, and both revocation
// lists, and the logout to run again. A signal that stopped a step is
// reported as INTERRUPTED with the same details: records may already be
// written and tokens deleted, and the bare cancellation would carry
// neither list. Every other failure reaches the helpers below, which
// therefore never see a cancellation.
func (l logoutCmd) localFailure(err error, d *token.Deletion, rep token.RevocationReport, rerun string, notices []string) error {
	details := LogoutAllPartialFailure{LoggedOut: d.DeletedTargets(), DeletedFrom: d.DeletedFrom, DeletedCache: d.DeletedCache,
		Revoked: rep.Revoked, NotRevoked: rep.NotRevoked, rerun: rerun}
	sweep, swept := errors.AsType[*sweepFailure](err)
	if errors.Is(err, context.Canceled) {
		return clierr.New(clierr.CodeInterrupted,
			"stopped before the logout completed; run it again to finish, and the details list what was logged out and each token").
			WithDetails(details).
			WithWarnings(notices...)
	}
	switch {
	case d.LoginErr != nil:
		return deleteFailure(fmt.Sprintf("failed to log out %q", d.LoginErr.Target), err, details, notices)
	case swept && sweep.cache != nil:
		return deleteBothFailure(sweep.cache, sweep.sweep, details, notices)
	case swept:
		return deleteFailure("failed to delete orphaned helper credentials", sweep.sweep, details, notices)
	}
	return deleteFailure("failed to delete exec-credential cache", d.CacheErr, details, notices)
}

// renderLogoutText is the TTY view of a logout: which servers, which
// credential backends were touched, how many cached exec credentials
// went, and what the server did with each token. Silent success is for
// scripts (exit code + JSON envelope); a person deserves the
// confirmation. It renders every deletedFrom entry, not just the requested
// targets: logout --all's orphaned-helper sweep reports records whose
// server name never made it into config (no URL known) - the text must not
// diverge from the JSON envelope for exactly the crash-recovery case that
// sweep exists to catch. Logout never clears the current-server pointer
// (including --all): the entry survives, credential-less, and the pointer
// keeps naming it.
func renderLogoutText(urlByName map[string]string, deletedFrom map[string][]string, cacheCount int, revocation []string) string {
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
	for _, line := range revocation {
		b.WriteString(line + "\n")
	}
	if b.Len() == 0 {
		// The only way names and cacheCount are both empty is --all against a
		// config with zero servers and no cache: ShapeText requires non-empty
		// Text on every default-format success, so this case needs its own
		// line too.
		b.WriteString("no servers configured to log out of\n")
	}
	return b.String()
}

// styleLogoutText applies auth logout's bold spans to text, the plain
// rendering renderLogoutText already produced: each server's opener line
// ("logged out of <name> (<url>)"), bold, whole line - its detail line
// stays plain. openers is renderLogoutText's own per-server block count
// (len(deletedFrom), the same map it iterates), so the opener lines are the
// even-indexed lines up to openers*2 - a structural position, not a scan
// for the "logged out of" text - leaving the cache-count line and the
// revocation lines after that range untouched.
func styleLogoutText(text string, openers int, s style.Style) string {
	return runtime.StyleLines(text, s, func(i int, body string) (string, bool) {
		if i%2 != 0 || i >= openers*2 {
			return "", false
		}
		return s.Bold(body), true
	})
}

// CompleteArgs implements runtime.ShellCompleter: `auth logout <server> <TAB>`
// completes server names from the already-loaded config snapshot.
func (logoutCmd) CompleteArgs(_ context.Context, view runtime.ConfigView, _ []string) []string {
	return view.ServerNames()
}

// logoutTargets resolves which servers a logout request applies to. --all
// adds every entry the directory holds a credential identity for on top of
// the configured ones: an entry removed from config.yaml by hand leaves its
// keychain credential behind, and the identity record is the only thing
// that still names it.
func logoutTargets(dir string, cfg *config.Config, all bool, name string) ([]string, error) {
	switch {
	case all:
		targets := make([]string, 0, len(cfg.Servers))
		for s := range cfg.Servers {
			targets = append(targets, s)
		}
		stored, err := config.IdentityEntries(dir)
		if err != nil {
			return nil, err
		}
		for _, s := range stored {
			if _, configured := cfg.Servers[s]; !configured {
				targets = append(targets, s)
			}
		}
		return targets, nil
	case name != "":
		if _, ok := cfg.Servers[name]; !ok {
			return nil, register.ServerNotFound(cfg, name)
		}
		return []string{name}, nil
	default:
		// No arg: the current server. CurrentServerEntry guards an empty or
		// dangling pointer; re-wrap its login-specific hint.
		cur, _, cerr := cfg.CurrentServerEntry()
		if cerr != nil {
			return nil, clierr.New(clierr.CodeNotLoggedIn,
				"no current server; specify a server name or --all; log out of one server with: "+clierr.Rancher+" auth logout <name>; or of all with: "+clierr.Rancher+" auth logout --all")
		}
		return []string{cur}, nil
	}
}

// writeLast is the logout's last write: every pause a resumer may clear,
// the targets' own included. A failed write is a notice, never a failure:
// the tokens are already deleted, and the pause stays for the next
// exec-credential cache miss or credential change.
func (l logoutCmd) writeLast(cfg *config.Config, targets []string, notices []string) []string {
	if !config.ResumeCacheWrites(l.dir, cfg, targets...) {
		return notices
	}
	if err := config.WriteStep("resume", cfg); err != nil {
		return append(notices, fmt.Sprintf("cache writes for %s stay paused (%v); the next exec-credential cache miss or credential change resumes them", strings.Join(targets, ", "), err))
	}
	return notices
}

// LogoutAllPartialFailure is a logout's structured payload for a
// mid-sequence failure: everything already deleted before the failing step,
// so a caller decoding the error sees exactly what succeeded rather than a
// bare failure that looks like nothing happened - the credentials for
// LoggedOut/DeletedFrom/DeletedCache are already gone regardless of which
// later step failed - and what the server did with each token.
type LogoutAllPartialFailure struct {
	LoggedOut    []string                `json:"loggedOut"`
	DeletedFrom  map[string][]string     `json:"deletedFrom,omitempty"`
	DeletedCache []string                `json:"deletedCache,omitempty"`
	Revoked      []token.RevokedToken    `json:"revoked"`
	NotRevoked   []token.NotRevokedToken `json:"notRevoked"`
	// rerun is the logout to run again when the local delete did not
	// complete, empty when it did.
	rerun string
}

// TextDetails projects which servers were already logged out, which
// credential backends were already removed per server, how many cached
// kubectl credentials went and what the server did with each token,
// closing, when the local delete did not complete, with the logout to run
// again: a failed run never undoes what it already deleted.
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
	for _, r := range p.Revoked {
		lines = append(lines, "revoked: "+r.Ref.String()+" on "+r.Server)
	}
	for _, n := range p.NotRevoked {
		lines = append(lines, n.DetailLines()...)
	}
	if p.rerun != "" {
		lines = append(lines, "those credentials are gone regardless of this failure - run "+p.rerun+" to finish")
	}
	return lines
}

// deleteFailure shapes a delete-step failure other than a cancellation,
// which localFailure reports as INTERRUPTED. The failure keeps its
// classification (a lock timeout stays LOCK_TIMEOUT and retryable) under
// the step-prefixed message, plus the
// LogoutAllPartialFailure details so nothing already deleted is vaporized
// just because a later step in the same --all run failed - its TextDetails
// projection's first line already names what was already logged out, so no
// separate hint repeats the same fact. notices carries every notice
// (keychain-unreachable degrades, say) already recorded by a step that ran
// successfully before this one failed - the pipeline discards Result on an
// error return, so this is their only ride back to the operator.
func deleteFailure(step string, err error, details LogoutAllPartialFailure, notices []string) error {
	return clierr.Wrap(err, fmt.Sprintf("%s: %v", step, err)).
		WithDetails(details).
		WithWarnings(notices...)
}

// deleteBothFailure reports a run where both final cleanups failed. The
// two causes ride one error so a caller can still match either, but the
// error is built here rather than wrapped: clierr.Wrap adopts whichever
// cause is already classified, which would take that cause's code and
// retry flag and drop the other from the chain, and a joined error
// spells itself with a newline between its causes, which the text
// renderer strips and which would run the two messages together. A
// cancelled step never reaches it: localFailure reports it as INTERRUPTED.
func deleteBothFailure(cacheErr, sweepErr error, details LogoutAllPartialFailure, notices []string) error {
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf(
		"failed to delete exec-credential cache (%v) and orphaned helper credentials (%v)", cacheErr, sweepErr)).
		WithCause(errors.Join(cacheErr, sweepErr)).
		WithDetails(details).
		WithWarnings(notices...)
}
