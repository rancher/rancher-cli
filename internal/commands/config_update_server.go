package commands

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type configUpdateServerCmd struct {
	dir          string
	tlsPreflight tlsPreflightFunc
}

// NewConfigUpdateServer builds `config update-server <name> [--url ...]
// [--cacert ... | --clear-cacert]` (local): moves an existing entry's
// address or trust without the credential purge delete-server would force.
// A URL change still clears the stored login token and its exec-credential
// cache (Decision 5 — a bearer token minted by one host must never be
// presented to another); a CA-only change keeps them.
func NewConfigUpdateServer(dir string) runtime.Command {
	return &configUpdateServerCmd{dir: dir, tlsPreflight: tlsPreflightDefault}
}

func (configUpdateServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config update-server",
		Usage:       "Change a configured server's URL or CA",
		CommandType: runtime.Local,
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
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "nothing to update").
			WithHint("pass --url, --cacert, or --clear-cacert")
	}

	preCfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	existing, ok := preCfg.Servers[name]
	if !ok {
		return runtime.Result{}, serverNotFoundWithCandidates(preCfg, name)
	}

	src, serr := prepareUpdateSource(existing)
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

	probe, perr := prepareRegistration(ctx, c.tlsPreflight, resultURL, resultCA)
	if perr != nil {
		return runtime.Result{}, perr
	}

	var (
		out                       updateOutcome
		deletedFrom, deletedCache []string
		notices                   []string
	)
	if err := config.WithLock(ctx, c.dir, func() error {
		cfg, err := config.Load(config.FilePath(c.dir))
		if err != nil {
			return err
		}
		plan, verr := validateUpdate(cfg, name, src, probe)
		if verr != nil {
			return verr
		}
		if plan.credentialsCleared {
			// Delete-first ordering, as delete-server: a purge failure must
			// leave config.yaml untouched, never a half-migrated entry
			// pointed at the new address with the old token still live.
			policy := config.EffectivePolicy(cfg.CredentialStore)
			ref := config.ServerRef{Name: name, URL: src.canonicalURL}
			deletedFrom, deletedCache, notices, err = config.DeleteTokenEverywhere(ctx, c.dir, ref, policy)
			if err != nil {
				return wrapUnlessCanceled(err, func(err error) error {
					return clierr.Wrap(err,
						fmt.Sprintf("failed to clear credentials for %q; server config left untouched: %v", name, err)).
						WithWarnings(notices...)
				})
			}
		}
		out, err = applyUpdate(cfg, plan)
		if err != nil && plan.credentialsCleared {
			// The purge already ran (delete-first ordering) and cannot be
			// undone; a write failure here must say so plainly rather than
			// looking like an ordinary write error against still-live
			// credentials — the entry still names the OLD URL, but its
			// token is already gone.
			return clierr.Wrap(err, fmt.Sprintf(
				"credentials for %q were cleared, but writing the updated server entry failed: %v",
				name, err)).WithHint(fmt.Sprintf("rancher auth login %s", shquote(name))).
				WithWarnings(notices...)
		}
		return err
	}); err != nil {
		return runtime.Result{}, err
	}

	changed := out.changedFields
	if changed == nil {
		changed = []string{}
	}
	sort.Strings(deletedFrom)
	sort.Strings(deletedCache)
	res := runtime.Result{Kind: "ServerUpdated", Data: map[string]any{
		"name": out.name, "url": out.canonicalURL, "ca": caPresence(out.caSelector),
		"currentServer": out.currentServer, "unchanged": out.unchanged,
		"changed": changed, "credentialsCleared": out.credentialsCleared,
		// Same single-server deletedFrom shape as config delete-server
		// (map keyed "backend") — this and delete-server report the same
		// DeleteTokenEverywhere primitive's output and should look alike.
		"deletedFrom": map[string][]string{"backend": deletedFrom}, "deletedCache": deletedCache,
	}}
	if probe.versionWarning != "" {
		res.Warnings = append(res.Warnings, probe.versionWarning)
	}
	res.Warnings = append(res.Warnings, notices...)
	if out.unchanged {
		res.Text = "server " + out.name + " unchanged\n"
		return res, nil
	}
	var lines []string
	if slices.Contains(changed, "url") {
		lines = append(lines, "url: "+out.canonicalURL+" (changed)")
	}
	if slices.Contains(changed, "ca") {
		if caPresence(out.caSelector) != "none" {
			lines = append(lines, "ca: pinned (changed)")
		} else {
			lines = append(lines, "ca: cleared (changed)")
		}
	}
	if out.credentialsCleared {
		lines = append(lines, fmt.Sprintf("credentials cleared — log in again: rancher auth login %s", shquote(out.name)))
	}
	if len(deletedCache) > 0 {
		lines = append(lines, fmt.Sprintf("%d cached kubectl credentials removed", len(deletedCache)))
	}
	if out.pointerRepaired {
		lines = append(lines, "current server set to "+out.name)
	}
	res.Text = runtime.TextLines("server "+out.name+" updated", lines...)
	return res, nil
}

// CompleteArgs implements runtime.ShellCompleter: `config update-server
// <TAB>` completes server names, the same candidate set as
// use-server/delete-server.
func (configUpdateServerCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	return view.ServerNames()
}
