package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// SecretNameSource is the source oidc-client rotate-secret and
// remove-secret hand the surface: the second positional, the secret
// names of the client typed as the first, read from that one OIDCClient's
// status.clientSecrets. It claims the site once a client is typed, needs
// a management token and one request, and declines every other site (the
// client positional is the kind's own).
func SecretNameSource() completion.Source {
	return completion.Source{Claim: claimSecretName, Complete: completeSecretNames}
}

func claimSecretName(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "" || req.Index != 1 || len(req.Positionals) == 0 {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

func completeSecretNames(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	k, ok := kinds.ByKey("oidcclient")
	if !ok {
		return completion.Result{}
	}
	p, err := completion.ManagementNamedResourcePath(k.GVR.Group, k.GVR.Version, "", k.GVR.Resource, req.Positionals[0])
	if err != nil {
		return completion.Result{}
	}
	var obj struct {
		Status struct {
			ClientSecrets map[string]any `json:"clientSecrets"`
		} `json:"status"`
	}
	if err := conn.GetJSON(ctx, p, "", &obj); err != nil {
		return completion.Result{}
	}
	names := make([]string, 0, len(obj.Status.ClientSecrets))
	for name := range obj.Status.ClientSecrets {
		names = append(names, name)
	}
	return completion.Result{Names: names}
}
