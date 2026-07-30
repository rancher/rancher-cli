package token

import (
	"context"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// credList renders cached exec-credential entries as a NAME SERVER CLUSTER
// EXPIRY table. The composite Key is deliberately not a column — it stays in
// the JSON/YAML payload but never surfaces raw in the table.
type credList []config.CachedCredInfo

func (credList) Columns() []string { return []string{"NAME", "SERVER", "CLUSTER", "EXPIRY"} }

func (l credList) Rows() [][]string {
	rows := make([][]string, 0, len(l))
	for _, c := range l {
		exp := ""
		if !c.Expiry.IsZero() {
			exp = c.Expiry.Format(time.RFC3339)
		}
		rows = append(rows, []string{c.Name, c.Server, c.Cluster, exp})
	}
	return rows
}

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

func (c listTokensCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	list, err := c.store.ListCachedCreds()
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "TokenList", Data: credList(list)}, nil
}
