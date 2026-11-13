package configcmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

type configUpdateServerCmd struct {
	dir          string
	tlsPreflight register.TLSPreflightFunc
}

// NewConfigUpdateServer builds `config update-server <name> [--url ...]
// [--cacert ... | --clear-cacert]` (local): moves an existing entry's
// address or trust without the credential purge delete-server would force.
// A URL change still clears the stored login token and its exec-credential
// cache (a bearer token created by one host must never be
// presented to another); a CA-only change keeps them.
func NewConfigUpdateServer(dir string) runtime.Command {
	return &configUpdateServerCmd{dir: dir, tlsPreflight: register.TLSPreflight}
}

func (configUpdateServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "config update-server",
		Usage: "Change a configured server's URL or CA",
		Desc: "A URL change deletes the entry's stored credentials and the exec-credential cache records made from its " +
			"login, and sends nothing to the old address: the tokens the CLI created there stay recorded on the entry " +
			"with the old server named, or on another entry that still names it, and data.notRevoked lists each with " +
			"its recovery. A CA-only change keeps the credentials.",
		CommandType:  runtime.Local,
		ServerAccess: runtime.ServerAccessRead,
		Args:         []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "url", Type: "string", Desc: "new Rancher server `url` (scheme required)"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM"},
			{Name: "clear-cacert", Type: "bool", Desc: "return the entry to system roots"},
		},
		Examples:    []string{"rancher config update-server qa1 --cacert /etc/rancher/ca.pem"},
		Shape:       runtime.ShapeText,
		SecretInput: true,
	}
}

func (c configUpdateServerCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]
	url := in.String("url")
	cacert := in.String("cacert")
	clearCacert := in.Bool("clear-cacert")

	if cacert != "" && clearCacert {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--cacert and --clear-cacert are mutually exclusive")
	}
	if url == "" && cacert == "" && !clearCacert {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"nothing to update; pass --url, --cacert, or --clear-cacert")
	}

	preCfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	existing, ok := preCfg.Servers[name]
	if !ok {
		return runtime.Result{}, register.ServerNotFound(preCfg, name)
	}

	src, serr := register.PrepareUpdateSource(existing)
	if serr != nil {
		return runtime.Result{}, serr
	}
	src.ClearCA = clearCacert

	resultURL, urlEntry := existing.URL, name
	if url != "" {
		resultURL, urlEntry = url, ""
	}
	resultCA := existing.CA
	switch {
	case clearCacert:
		resultCA = ""
	case cacert != "":
		resultCA = cacert
	}

	canonicalResultURL, perr := register.CanonicalizeServerURL(resultURL, urlEntry)
	if perr != nil {
		return runtime.Result{}, perr
	}
	if config.IsExecLoginEntryName(name) && canonicalResultURL != existing.URL {
		return runtime.Result{}, execLoginURLChangeRefused(name)
	}
	probe, perr := register.Prepare(ctx, c.tlsPreflight, canonicalResultURL, resultCA)
	if perr != nil {
		return runtime.Result{}, perr
	}

	var (
		out                       register.UpdateOutcome
		deletedFrom, deletedCache []string
		notices                   []string
		del                       *token.Deletion
	)
	if err := config.WithLock(ctx, c.dir, func() error {
		cfg, err := config.Load(config.FilePath(c.dir))
		if err != nil {
			return err
		}
		plan, verr := register.ValidateUpdate(cfg, name, src, probe)
		if verr != nil {
			return verr
		}
		after := register.PlannedEntry(cfg.Servers[name], plan)
		if plan.CredentialsCleared {
			// Delete-first ordering, as delete-server: a purge failure must
			// leave config.yaml untouched but for the change's first write,
			// never a half-migrated entry pointed at the new address with
			// the old token still live. That first write (token.DeleteAndRecord)
			// moves the entry's cache binding id ahead of the purge and
			// pauses its cache writes, and records the old server's tokens:
			// on the moved entry with that server named, so no login of the
			// new address tries them, or on another entry that still names
			// it. A get-token that read the old id and the old credential a
			// moment earlier would otherwise write a record the purge cannot
			// see and the entry would still accept, and one that loads after
			// it and reads the credential before the delete writes nothing.
			// Nothing is sent to the old address. The address and every
			// other field stay behind the contract; the entry write resumes.
			policy, perr := config.EffectivePolicy(cfg.CredentialStore)
			if perr != nil {
				return perr
			}
			// The directory's unfinished store change goes first, after
			// the plan that says credentials clear and before any write.
			prelude, perr := config.PrepareCredentialDelete(ctx, c.dir, cfg)
			if perr != nil {
				return perr
			}
			cache := config.NewFileStore(c.dir)
			var lerr error
			del, lerr = token.DeleteAndRecord(ctx, cfg, token.LocalDelete{
				Dir: c.dir, Command: token.DeleteForUpdateServer, Targets: []string{name}, Updated: &after, Cache: cache,
				Rerun: clierr.NewCommand("config update-server").Arg(name).FlagValue("--url", canonicalResultURL),
				DeleteLogin: func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
					return config.DeleteLoginToken(ctx, c.dir, ref, policy)
				},
			})
			notices = slices.Clone(prelude)
			if ns, ok := cache.(config.NoticeSource); ok {
				notices = append(notices, ns.DrainNotices()...)
			}
			if del == nil {
				return config.WithNotices(lerr, notices)
			}
			notices = append(notices, del.Notices...)
			deletedFrom, deletedCache = del.DeletedFrom[name], del.DeletedCache
			if lerr != nil {
				rep := del.Report()
				return clierr.WrapUnlessCanceled(lerr, func(err error) error {
					return clierr.Wrap(err,
						fmt.Sprintf("failed to clear credentials for %q; the server address is unchanged, though its cache binding id has already been advanced and its cache writes paused: %v", name, err)).
						WithDetails(rep).
						WithWarnings(append(notices, del.Warnings()...)...)
				})
			}
			// The entry write that follows is the change's last.
			config.ResumeCacheWrites(c.dir, cfg, name)
		}
		if plan.RewriteURL && !plan.CredentialsCleared {
			// Only the user information leaves the URL, so no credential is
			// cleared and no revoke plan runs. The entry becomes usable at its
			// server, which stops the plugin from serving the cached tokens
			// bound to the other usable entries there, and the next read would
			// delete them. They are kept under keys no read deletes, before the
			// entry is written; nothing is sent.
			kept, kerr := config.KeepStaleCachedCreds(c.dir, cfg, name, after)
			if kerr != nil {
				if _, unreadable := errors.AsType[*config.CacheReadError](kerr); !unreadable {
					return kerr
				}
				notices = append(notices, "the token cache could not be read; no cached token was kept: "+kerr.Error())
			}
			for _, k := range kept {
				notices = append(notices, keptTokenNotice(k))
			}
		}
		out, err = register.ApplyUpdate(cfg, plan)
		if err == nil && del != nil {
			// The warnings of the purge judge the entry by the config as it
			// stands now, with the new URL. The Deletion's view is the config
			// ApplyUpdate has just changed unless a settle span reloaded it,
			// which an update that sends no request never reaches; the
			// refresh keeps the judgement right if that changes.
			del.Refresh(cfg)
		}
		if err != nil && plan.CredentialsCleared {
			// The purge already ran (delete-first ordering) and cannot be
			// undone; a write failure here must say so plainly rather than
			// looking like an ordinary write error against still-live
			// credentials - the entry still names the OLD URL, but its
			// token is already gone.
			return clierr.Wrap(err, fmt.Sprintf(
				"credentials for %q were cleared, but writing the updated server entry failed: %v",
				name, err)).WithHintCommand(clierr.NewCommand(config.LoginWords).Arg(name)).
				WithWarnings(notices...)
		}
		if err != nil && plan.RewriteURL {
			// The records the move kept stay kept; the warnings say which.
			return config.WithNotices(err, notices)
		}
		return err
	}); err != nil {
		return runtime.Result{}, err
	}

	changed := out.ChangedFields
	if changed == nil {
		changed = []string{}
	}
	if out.UserinfoRemoved {
		// The stored URL changed, though the server did not: the envelope
		// says so, and no credential is cleared.
		changed = append([]string{"url"}, changed...)
	}
	notRevoked := []token.NotRevokedToken{}
	var revocationLines []string
	if del != nil {
		notRevoked = del.Report().NotRevoked
		notices = append(notices, del.Warnings()...)
		revocationLines = del.TextLines()
	}
	sort.Strings(deletedFrom)
	sort.Strings(deletedCache)
	res := runtime.Result{Kind: "ServerUpdated", Data: map[string]any{
		"name": out.Name, "url": out.CanonicalURL, "ca": register.CAPresence(out.CASelector),
		"currentServer": out.CurrentServer, "unchanged": out.Unchanged,
		"changed": changed, "credentialsCleared": out.CredentialsCleared,
		// Same single-server deletedFrom shape as config delete-server
		// (map keyed "backend") - this and delete-server report the same
		// local delete's output and should look alike.
		"deletedFrom": map[string][]string{"backend": deletedFrom}, "deletedCache": deletedCache,
		"notRevoked": notRevoked,
	}}
	if probe.VersionWarning != "" {
		res.Warnings = append(res.Warnings, probe.VersionWarning)
	}
	res.Warnings = append(res.Warnings, notices...)
	if out.Unchanged {
		tr := runtime.TextLines("server " + out.Name + " unchanged")
		res.Text, res.StyledText = tr.Text, tr.StyledText
		return res, nil
	}
	var lines []string
	if slices.Contains(out.ChangedFields, "url") {
		lines = append(lines, "url: "+out.CanonicalURL+" (changed)")
	}
	if out.UserinfoRemoved {
		lines = append(lines, "url: "+out.CanonicalURL+" (user name and password removed)")
	}
	if slices.Contains(out.ChangedFields, "ca") {
		if register.CAPresence(out.CASelector) != "none" {
			lines = append(lines, "ca: pinned (changed)")
		} else {
			lines = append(lines, "ca: cleared (changed)")
		}
	}
	credentialsClearedLine := ""
	if out.CredentialsCleared {
		credentialsClearedLine = clierr.Text("credentials cleared", clierr.Lead(" - log in again: "), clierr.NewCommand(config.LoginWords).Arg(out.Name))
		lines = append(lines, credentialsClearedLine)
	}
	if len(deletedCache) > 0 {
		lines = append(lines, cachedCredsRemovedLine(len(deletedCache)))
	}
	if out.PointerRepaired {
		lines = append(lines, "current server set to "+out.Name)
	}
	lines = append(lines, revocationLines...)
	tr := runtime.TextLines("server "+out.Name+" updated", lines...)
	if credentialsClearedLine != "" {
		tr = runtime.AccentLine(tr, credentialsClearedLine)
	}
	res.Text, res.StyledText = tr.Text, tr.StyledText
	return res, nil
}

// execLoginURLChangeRefused refuses a URL change on one of the exec
// plugin's own login entries. The change clears the entry's login and
// with it the record that the plugin made the entry, which leaves an
// ordinary entry under a name auth login refuses: no command could log it
// in again. The plugin makes a new entry for a new URL by itself.
func execLoginURLChangeRefused(name string) error {
	remove := clierr.NewSequence().
		Then("", clierr.NewCommand("auth logout").Arg(name)).
		Then("then delete it: ", deleteServerCommand(name))
	return clierr.New(clierr.CodeValidation, clierr.Text(fmt.Sprintf(
		"%s is one of the exec plugin's own login entries, and its URL cannot change; the exec plugin creates a new entry for the new URL; to remove this entry, log it out first if it holds a login",
		clierr.ShellQuote(name)), clierr.Lead(": "), remove))
}

// CompleteArgs implements runtime.ShellCompleter: `config update-server
// <TAB>` completes server names, the same candidate set as
// use-server/delete-server.
func (configUpdateServerCmd) CompleteArgs(_ context.Context, view runtime.ConfigView, _ []string) []string {
	return view.ServerNames()
}

// keptTokenNotice is the warning for a token the rewrite of a URL kept in the
// token cache: the record is no longer served, and auth list-tokens shows it.
// A record of a call with a user has no entry that could send a revoke, so
// only the others name auth delete-token, which revokes the token through a
// usable entry at its server.
func keptTokenNotice(k config.KeptCachedCred) string {
	text := "the token " + clierr.IDText(k.Ref.String()) + " of a cached credential for " + config.DisplayServerURL(k.Server) +
		" stays in the token cache and is no longer served; "
	list := clierr.NewCommand("auth list-tokens")
	if k.Kind == config.CacheRecordOwner {
		return clierr.Text(text, clierr.Lead("to list it, run: "), list)
	}
	return clierr.Text(text, clierr.Lead("to list it, run: "), list, clierr.Lead("; to revoke it, run: "), clierr.NewCommand("auth delete-token").Arg(k.Key))
}
