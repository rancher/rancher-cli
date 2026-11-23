package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// ExtensionUninstallSource is the live source of `extension uninstall`, whose
// registered name is command: the chart name, from the Apps of the extension
// namespace (one request). The command accepts an App of any status, with or
// without a UIPlugin, and the source offers every App. It claims no other site.
func ExtensionUninstallSource(command string) completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Command != command || req.Flag != "" || req.Index != 0 {
				return completion.Requirements{}, false
			}
			return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}, true
		},
		Complete: func(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
			p, err := completion.NamespacedPath("local", "catalog.cattle.io", "v1", extension.Namespace, "apps")
			if err != nil {
				return completion.Result{}
			}
			var list struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"items"`
			}
			if err := conn.GetJSON(ctx, p, "", &list); err != nil {
				return completion.Result{}
			}
			names := make([]string, 0, len(list.Items))
			for _, it := range list.Items {
				names = append(names, it.Metadata.Name)
			}
			return completion.Result{Names: names}
		},
	}
}
