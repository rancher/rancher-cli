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

type configDeleteServerCmd struct {
	dir string
	// delete is the credential/exec-cache delete, injectable for tests
	// (the authcheck preflight-seam pattern, authcheck.go); defaults to
	// config.DeleteTokenEverywhere — the single delete point (ARCHITECTURE.md), whose
	// only other caller is auth logout.
	delete func(ctx context.Context, dir string, ref config.ServerRef, policy string) (fromBackends, deletedCache, notices []string, err error)
}

// NewConfigDeleteServer builds `config delete-server <name>` (local): removes
// a configured server (clearing the current-server pointer when it names it)
// and deletes its stored credentials — the orphaned-creds cleanup nothing
// else performs today. It does NOT revoke the server-side token (that is
// `auth logout --revoke`, ledgered/out of scope).
func NewConfigDeleteServer(dir string) runtime.Command {
	return &configDeleteServerCmd{dir: dir, delete: config.DeleteTokenEverywhere}
}

func (configDeleteServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config delete-server",
		Usage:       "Delete a configured server and its stored credentials",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:    []string{"rancher config delete-server prod"},
		Shape:       runtime.ShapeText,
	}
}

// Execute's ordering is safety-critical: delete credentials FIRST, and abort
// on delete failure before touching cfg or writing anything — the config
// file stays byte-identical to before the call. Only after a successful
// delete does the single config write (server removed, current-server
// pointer cleared if it named the deleted server) happen. A delete failure
// must never leave a live orphaned token behind a disappeared server entry —
// the exact bug this command exists to kill; the benign failure direction is
// creds gone + server still configured, never the reverse.
func (d configDeleteServerCmd) Execute(cmdCtx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]

	var (
		srv            config.Server
		currentCleared bool
		deletedFrom    []string
		deletedCache   []string
		notices        []string
	)
	if err := config.WithLock(cmdCtx, d.dir, func() error {
		cfg, err := config.Load(config.FilePath(d.dir))
		if err != nil {
			return err
		}
		var ok bool
		srv, ok = cfg.Servers[name]
		if !ok {
			return serverNotFoundWithCandidates(cfg, name)
		}
		policy := config.EffectivePolicy(cfg.CredentialStore)
		deletedFrom, deletedCache, notices, err = d.delete(cmdCtx, d.dir, config.ServerRef{Name: name, URL: srv.URL}, policy)
		if err != nil {
			return wrapUnlessCanceled(err, func(err error) error {
				return clierr.Wrap(err,
					fmt.Sprintf("failed to delete credentials for %q; server config left untouched: %v", name, err)).
					WithWarnings(notices...)
			})
		}
		delete(cfg.Servers, name)
		if cfg.CurrentServer == name {
			cfg.CurrentServer = ""
			currentCleared = true
		}
		return cfg.Write()
	}); err != nil {
		return runtime.Result{}, err
	}

	sort.Strings(deletedFrom)
	sort.Strings(deletedCache)
	res := runtime.Result{Kind: "DeletedServer", Data: map[string]any{
		"server":               name,
		"url":                  srv.URL,
		"deletedFrom":          map[string][]string{"backend": deletedFrom},
		"deletedCache":         deletedCache,
		"currentServerCleared": currentCleared,
	}, Warnings: notices}
	const pointerClearedLine = "was current — pointer cleared"
	var lines []string
	if currentCleared {
		lines = append(lines, pointerClearedLine)
	}
	if len(deletedFrom) > 0 {
		lines = append(lines, "credentials removed ("+strings.Join(deletedFrom, ", ")+")")
	}
	if len(deletedCache) > 0 {
		lines = append(lines, cachedCredsRemovedLine(len(deletedCache)))
	}
	tr := runtime.TextLines("server "+name+" deleted", lines...)
	if currentCleared {
		tr = runtime.AccentLine(tr, pointerClearedLine)
	}
	res.Text, res.StyledText = tr.Text, tr.StyledText
	return res, nil
}

// CompleteArgs implements runtime.ShellCompleter: `config delete-server <TAB>`
// completes server names from the already-loaded config snapshot (AC-19).
func (configDeleteServerCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	return view.ServerNames()
}
