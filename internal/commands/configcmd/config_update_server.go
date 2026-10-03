package configcmd

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
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
		Name:        "config update-server",
		Usage:       "Change a configured server's URL or CA",
		CommandType: runtime.Local,
		Network:     true,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "url", Type: "string", Desc: "new Rancher server `url` (scheme required)"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM"},
			{Name: "clear-cacert", Type: "bool", Desc: "return the entry to system roots"},
		},
		Examples: []string{"rancher config update-server qa1 --cacert /etc/rancher/ca.pem"},
		Shape:    runtime.ShapeText,
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

	resultURL := existing.URL
	if url != "" {
		resultURL = url
	}
	resultCA := existing.CA
	switch {
	case clearCacert:
		resultCA = ""
	case cacert != "":
		resultCA = cacert
	}

	canonicalResultURL, perr := register.CanonicalizeServerURL(resultURL)
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
		if plan.CredentialsCleared {
			// Delete-first ordering, as delete-server: a purge failure must
			// leave config.yaml untouched but for the change's first write,
			// never a half-migrated entry pointed at the new address with
			// the old token still live. That first write moves two fields
			// ahead of the purge: the entry's cache binding id advances and
			// its cache writes pause. A get-token that read the old id and
			// the old credential a moment earlier would otherwise write a
			// record the purge cannot see and the entry would still
			// accept, and one that loads after it and reads the credential
			// before the delete writes nothing. The address and every other
			// field stay behind the contract; the entry write resumes.
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
			entry := cfg.Servers[name]
			if berr := config.StartCredentialChange(&entry); berr != nil {
				return config.WithNotices(berr, prelude)
			}
			cfg.Servers[name] = entry
			if werr := config.WriteStep("cachebinding", cfg); werr != nil {
				return config.WithNotices(werr, prelude)
			}
			ref := config.ServerRef{Name: name, URL: src.CanonicalURL}
			var deleted []string
			deletedFrom, deletedCache, deleted, err = config.DeleteTokenEverywhere(ctx, c.dir, ref, policy)
			notices = slices.Concat(prelude, deleted)
			if err != nil {
				return clierr.WrapUnlessCanceled(err, func(err error) error {
					return clierr.Wrap(err,
						fmt.Sprintf("failed to clear credentials for %q; the server address is unchanged, though its cache binding id has already been advanced and its cache writes paused: %v", name, err)).
						WithWarnings(notices...)
				})
			}
			// The entry write that follows is the change's last.
			config.ResumeCacheWrites(c.dir, cfg, name)
		}
		out, err = register.ApplyUpdate(cfg, plan)
		if err != nil && plan.CredentialsCleared {
			// The purge already ran (delete-first ordering) and cannot be
			// undone; a write failure here must say so plainly rather than
			// looking like an ordinary write error against still-live
			// credentials - the entry still names the OLD URL, but its
			// token is already gone.
			return clierr.Wrap(err, fmt.Sprintf(
				"credentials for %q were cleared, but writing the updated server entry failed: %v",
				name, err)).WithHint(fmt.Sprintf("rancher auth login %s", clierr.ShellQuote(name))).
				WithWarnings(notices...)
		}
		return err
	}); err != nil {
		return runtime.Result{}, err
	}

	changed := out.ChangedFields
	if changed == nil {
		changed = []string{}
	}
	sort.Strings(deletedFrom)
	sort.Strings(deletedCache)
	res := runtime.Result{Kind: "ServerUpdated", Data: map[string]any{
		"name": out.Name, "url": out.CanonicalURL, "ca": register.CAPresence(out.CASelector),
		"currentServer": out.CurrentServer, "unchanged": out.Unchanged,
		"changed": changed, "credentialsCleared": out.CredentialsCleared,
		// Same single-server deletedFrom shape as config delete-server
		// (map keyed "backend") - this and delete-server report the same
		// DeleteTokenEverywhere primitive's output and should look alike.
		"deletedFrom": map[string][]string{"backend": deletedFrom}, "deletedCache": deletedCache,
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
	if slices.Contains(changed, "url") {
		lines = append(lines, "url: "+out.CanonicalURL+" (changed)")
	}
	if slices.Contains(changed, "ca") {
		if register.CAPresence(out.CASelector) != "none" {
			lines = append(lines, "ca: pinned (changed)")
		} else {
			lines = append(lines, "ca: cleared (changed)")
		}
	}
	credentialsClearedLine := ""
	if out.CredentialsCleared {
		credentialsClearedLine = fmt.Sprintf("credentials cleared - log in again: rancher auth login %s", clierr.ShellQuote(out.Name))
		lines = append(lines, credentialsClearedLine)
	}
	if len(deletedCache) > 0 {
		lines = append(lines, cachedCredsRemovedLine(len(deletedCache)))
	}
	if out.PointerRepaired {
		lines = append(lines, "current server set to "+out.Name)
	}
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
	q := clierr.ShellQuote(name)
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"%s is one of the exec plugin's own login entries, so its URL cannot change; remove it with %s auth logout %s if it holds a login, then %s config delete-server %s, and the exec plugin creates a new entry for the new URL",
		q, clierr.Rancher, q, clierr.Rancher, q))
}

// CompleteArgs implements runtime.ShellCompleter: `config update-server
// <TAB>` completes server names, the same candidate set as
// use-server/delete-server.
func (configUpdateServerCmd) CompleteArgs(_ context.Context, view runtime.ConfigView, _ []string) []string {
	return view.ServerNames()
}
