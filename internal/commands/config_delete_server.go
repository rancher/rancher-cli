package commands

import (
	"context"
	"fmt"
	"sort"

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
	delete func(ctx context.Context, dir string, ref config.ServerRef, policy string) (fromBackends, deletedCache []string, err error)
}

// NewConfigDeleteServer builds `config delete-server <name>` (local): removes
// a configured server, every context field-matching it, and deletes its
// stored credentials — the orphaned-creds cleanup nothing else performs
// today. It does NOT revoke the server-side token (that is `auth logout
// --revoke`, ledgered/out of scope).
func NewConfigDeleteServer(dir string) runtime.Command {
	return &configDeleteServerCmd{dir: dir, delete: config.DeleteTokenEverywhere}
}

func (configDeleteServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config delete-server",
		Usage:       "Delete a configured server, its contexts, and its stored credentials",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:    []string{"rancher config delete-server prod"},
	}
}

// Execute's ordering is safety-critical and pinned: delete credentials FIRST,
// and abort on delete failure before touching cfg or writing anything — the
// config file stays byte-identical to before the call. Only after a
// successful delete does the single config write (server + field-matched
// contexts removed, current context cleared if it was among them) happen.
// A delete failure must never leave a live orphaned token behind a
// disappeared server entry — the exact bug this command exists to kill; the
// benign failure direction is creds gone + server still configured, never
// the reverse.
func (d configDeleteServerCmd) Execute(cmdCtx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]

	var (
		srv             config.Server
		removedContexts []string
		currentCleared  bool
		deletedFrom     []string
		deletedCache    []string
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
		deletedFrom, deletedCache, err = d.delete(cmdCtx, d.dir, config.ServerRef{Name: name, URL: srv.URL}, policy)
		if err != nil {
			return wrapUnlessCanceled(err, func(err error) error {
				return clierr.Wrap(err,
					fmt.Sprintf("failed to delete credentials for %q; server config left untouched: %v", name, err))
			})
		}
		for cn, cctx := range cfg.Contexts {
			if cctx.Server == name {
				removedContexts = append(removedContexts, cn)
			}
		}
		sort.Strings(removedContexts)
		for _, cn := range removedContexts {
			if cn == cfg.CurrentContext {
				currentCleared = true
				break
			}
		}
		for _, cn := range removedContexts {
			delete(cfg.Contexts, cn)
		}
		delete(cfg.Servers, name)
		if currentCleared {
			cfg.CurrentContext = ""
		}
		return cfg.Write()
	}); err != nil {
		return runtime.Result{}, err
	}

	sort.Strings(deletedFrom)
	sort.Strings(deletedCache)
	return runtime.Result{Kind: "DeletedServer", Data: map[string]any{
		"server":                name,
		"url":                   srv.URL,
		"contexts":              removedContexts,
		"deletedFrom":           map[string][]string{"backend": deletedFrom},
		"deletedCache":          deletedCache,
		"currentContextCleared": currentCleared,
	}}, nil
}

// CompleteArgs implements runtime.ShellCompleter: `config delete-server <TAB>`
// completes server names from the already-loaded config snapshot (AC-19).
func (configDeleteServerCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	return view.ServerNames()
}
