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
	// drain is the credential/exec-cache drain, injectable for tests
	// (the authcheck preflight-seam pattern, authcheck.go); defaults to
	// config.DrainToken — the single drain point (ARCHITECTURE.md), whose
	// only other caller is auth logout.
	drain func(dir, server, url string) (fromBackends, drainedCache []string, err error)
}

// NewConfigDeleteServer builds `config delete-server <name>` (local): removes
// a configured server, every context field-matching it, and drains its
// stored credentials — the orphaned-creds cleanup nothing else performs
// today. It does NOT revoke the server-side token (that is `auth logout
// --revoke`, ledgered/out of scope).
func NewConfigDeleteServer(dir string) runtime.Command {
	return &configDeleteServerCmd{dir: dir, drain: config.DrainToken}
}

func (configDeleteServerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config delete-server",
		Usage:       "Delete a configured server, its contexts, and its stored credentials",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Examples:    []string{"rancher config delete-server prod"},
		Toolset:     "core",
	}
}

// Execute's ordering is safety-critical and pinned: drain credentials FIRST,
// and abort on drain failure before touching cfg or writing anything — the
// config file stays byte-identical to before the call. Only after a
// successful drain does the single config write (server + field-matched
// contexts removed, current context cleared if it was among them) happen.
// A drain failure must never leave a live orphaned token behind a
// disappeared server entry — the exact bug this command exists to kill; the
// benign failure direction is creds gone + server still configured, never
// the reverse.
func (d configDeleteServerCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	name := in.Args()[0]

	cfg, err := config.Load(config.FilePath(d.dir))
	if err != nil {
		return runtime.Result{}, err
	}

	srv, ok := cfg.Servers[name]
	if !ok {
		return runtime.Result{}, serverNotFoundWithCandidates(cfg, name)
	}

	deletedFrom, deletedCache, err := d.drain(d.dir, name, srv.URL)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("failed to drain credentials for %q; server config left untouched: %v", name, err))
	}

	var removedContexts []string
	for cn, cctx := range cfg.Contexts {
		if cctx.Server == name {
			removedContexts = append(removedContexts, cn)
		}
	}
	sort.Strings(removedContexts)

	currentCleared := false
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

	if err := cfg.Write(); err != nil {
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
