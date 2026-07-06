package token

import (
	"context"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
)

type listTokensCmd struct{ store config.CredStore }

// NewListTokensCommand builds `auth list-tokens` (local): list cached
// exec-credential entries by name and expiry.
func NewListTokensCommand(_ string, store config.CredStore) runtime.Command {
	return &listTokensCmd{store: store}
}

func (listTokensCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth list-tokens",
		Usage:       "List cached exec-credential token entries",
		CommandType: runtime.Local,
		Examples:    []string{"rancher auth list-tokens -o json"},
	}
}

func (listTokensCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}

func (c listTokensCmd) Execute(_ context.Context, _ runtime.Input, _ safety.Effect, _ *ui.Prompter) (runtime.Result, error) {
	list, err := c.store.ListCachedCreds()
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "TokenList", Data: list}, nil
}
