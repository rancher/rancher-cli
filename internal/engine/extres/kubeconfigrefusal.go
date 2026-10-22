package extres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// kubeconfigTokenFloor is the first Rancher version whose Kubeconfig store
// accepts the token this CLI creates for a login. It is parsed for the
// version comparison (a missing patch reads as 0) and shown in the message.
const kubeconfigTokenFloor = "2.15"

// kubeconfigTokenLookupText opens the 403 that Rancher's Kubeconfig store
// answers when it cannot find the request token. Other Rancher code
// produces the bare phrase "error getting request token" without this
// prefix, so only the whole prefix matches.
const kubeconfigTokenLookupText = "kubeconfigs.ext.cattle.io is forbidden: error getting request token "

// refuseUnsupportedTokenLookup turns the Kubeconfig store's failed lookup of
// the request token into CAPABILITY_UNSUPPORTED, for the one request that
// creates a Kubeconfig. Rancher before 2.15 looks the request token up as a
// token of the older management API only, so a bearer that is an ext token
// (the kind this CLI creates when it logs in, or one given in
// RANCHER_CLI_TOKEN) is refused with a 403 that reads as a permission
// failure. The same text appears on newer servers for a lookup that really
// failed, so the error is reclassified only when the server reports a
// version below 2.15, and the version is asked for only after the request
// has already failed. Every other error is returned unchanged: one that is
// not the RBAC error carrying the store's text, a server version that
// cannot be read or parsed (a dev build or a bare "head"; a release branch
// head such as v2.14-head parses), and a factory that cannot report it.
// v2.15.0 alpha builds before the fix report as 2.15.0 and keep the RBAC
// error. The refusal carries the login command as its hint only when a stored
// login that auth login accepts is in use. The server's text stays in the
// cause and is not printed, as RefuseUnsupportedWorkflow does.
//
// The call after the real create is reachable only when the token vanishes
// between the dry-run and the create; the dry-run reaches the same lookup
// and fails first.
func refuseUnsupportedTokenLookup(ctx context.Context, clients engine.ClusterClientFactory, err error) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeRBAC || !strings.Contains(ce.Message, kubeconfigTokenLookupText) {
		return err
	}
	versions, ok := clients.(engine.ServerVersionProvider)
	if !ok {
		return err
	}
	server, verr := versions.ServerVersion(ctx)
	if verr != nil {
		return err
	}
	if below, known := config.VersionBelow(server, kubeconfigTokenFloor); !known || !below {
		return err
	}
	server = strings.TrimSpace(server)
	var hint string
	if namer, ok := clients.(engine.StoredLoginNamer); ok {
		if entry, nerr := namer.StoredLoginName(); nerr == nil && entry != "" {
			// "" for a plugin login entry, which auth login refuses.
			if hint = config.LoginHintFor(entry); hint != "" {
				hint += " --token-stdin"
			}
		}
	}
	remedy := "set RANCHER_CLI_TOKEN to an API token"
	if hint != "" {
		remedy = "an API token (pipe id:secret to " + hint + ")"
	}
	return clierr.New(clierr.CodeCapabilityUnsupported,
		fmt.Sprintf("Rancher %s cannot create a kubeconfig with the token this command used; use Rancher %s or later, or %s",
			server, kubeconfigTokenFloor, remedy)).
		WithHint(hint).
		WithDetails(clierr.CapabilityUnsupportedDetails{Feature: "create kubeconfig", Server: server}).
		WithCause(err)
}
