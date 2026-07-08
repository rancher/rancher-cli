package commands

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type purgeLegacyCmd struct{}

// NewConfigPurgeLegacy builds `config purge-legacy` (local).
func NewConfigPurgeLegacy() runtime.Command { return &purgeLegacyCmd{} }

func (purgeLegacyCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config purge-legacy",
		Usage:       "Delete the migrated legacy cli2.json (plaintext credentials)",
		CommandType: runtime.Local,
		Toolset:     "core",
	}
}
func (purgeLegacyCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	path, ok := config.FindLegacy()
	if !ok {
		return runtime.Result{Kind: "PurgeLegacy", Data: map[string]any{"removed": false, "reason": "no legacy file found"}}, nil
	}
	if err := config.PurgeLegacy(path); err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric, err.Error())
	}
	return runtime.Result{Kind: "PurgeLegacy", Data: map[string]any{"removed": true, "path": path}}, nil
}
