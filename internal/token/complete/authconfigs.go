package complete

import (
	"context"
	"slices"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// supportedTypes are the provider types the sign-in commands can sign in
// with, spelled as the server's list spells them. The sign-in code keeps its
// own list, and a test asserts the two are equal, so a suggestion never
// names an auth config the command would drop.
var supportedTypes = []string{
	"localProvider",
	"freeIpaProvider",
	"openLdapProvider",
	"activeDirectoryProvider",
	"pingProvider",
	"adfsProvider",
	"keyCloakProvider",
	"oktaProvider",
	"shibbolethProvider",
	"genericSAMLProvider",
	"azureADProvider",
}

// SupportedTypes returns the provider types the source offers auth configs
// of.
func SupportedTypes() []string {
	return slices.Clone(supportedTypes)
}

// AuthConfigSource is the source auth login, auth get-token and the legacy
// token command hand the surface for --auth-provider. It claims that flag's
// value only, needs no credential and one request, and offers the names of
// the enabled auth configs of a supported type: the ids of the server's
// unauthenticated provider list, each of which --auth-provider takes.
func AuthConfigSource() completion.Source {
	return completion.Source{Claim: claimAuthConfig, Complete: completeAuthConfigs}
}

func claimAuthConfig(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "auth-provider" {
		return completion.Requirements{}, false
	}
	return completion.PublicListing(), true
}

func completeAuthConfigs(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	p, err := completion.AuthProvidersPath()
	if err != nil {
		return completion.Result{}
	}
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	var names []string
	for _, entry := range list.Data {
		if entry.ID != "" && slices.Contains(supportedTypes, entry.Type) {
			names = append(names, entry.ID)
		}
	}
	return completion.Result{Names: names}
}
