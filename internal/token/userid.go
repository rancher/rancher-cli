package token

import (
	"slices"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// userIDFlag is auth get-token's flag naming the Rancher user every token
// the call hands kubectl must belong to.
const userIDFlag = "user-id"

// ValidUserID reports whether id can be a Rancher user id: a
// management.cattle.io User name, which is a Kubernetes object name. Such
// a name holds no "/", which the cache key's form relies on.
func ValidUserID(id string) bool {
	return len(validation.IsDNS1123Subdomain(id)) == 0
}

// readUserID is the call's --user-id, checked before anything reads the
// cache, whose key carries it: typed empty and not a user id are refused.
func readUserID(in runtime.Input) (string, error) {
	userID := in.String(userIDFlag)
	switch {
	case userID == "" && in.IsSet(userIDFlag):
		return "", clierr.New(clierr.CodeValidation, "--user-id is empty; pass a value or omit --user-id")
	case userID == "":
		return "", nil
	case !ValidUserID(userID):
		return "", clierr.New(clierr.CodeValidation,
			"--user-id "+clierr.ShellQuote(userID)+" is not a Rancher user id (a lowercase name such as u-abc12)")
	}
	return userID, nil
}

// ValidAuthConfigName reports whether name can be an auth config's name: a
// Kubernetes object name, which holds neither ":" nor "/", which the cache
// key's form relies on.
func ValidAuthConfigName(name string) bool {
	return len(validation.IsDNS1123Subdomain(name)) == 0
}

// readAuthConfig is the call's --auth-provider when it goes with --user-id,
// checked before anything reads the cache, whose key carries it: the value,
// untrimmed, must be an auth config name. A provider type such as
// openLdapProvider is not one; a provider name such as openldap is, and is
// then matched exactly. Without --user-id the flag keeps its meaning of
// choosing a sign-in and nothing is checked or returned.
func readAuthConfig(in runtime.Input, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
	authConfig := in.String("auth-provider")
	if authConfig != "" && !ValidAuthConfigName(authConfig) {
		return "", clierr.New(clierr.CodeValidation,
			"--auth-provider "+clierr.ShellQuote(authConfig)+" is not an auth config name (a lowercase name such as corp-ldap)")
	}
	return authConfig, nil
}

// userIDPrefixRefused is the refusal of a --name in the namespace of the
// cache keys of calls with --user-id: a call without --user-id whose
// --name began with the prefix would read and write those keys.
func userIDPrefixRefused() *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		"--name values starting with '"+config.UserIDCachePrefix+"' are reserved for the exec plugin's cache slots of calls with --user-id; use a different name")
}

// RecordedUserIDs are the --user-id values a call with --name, --server
// server and --cluster clusterID can be given as cfg stands, sorted: when
// the name identifies an entry at server, that entry's recorded owner;
// otherwise the distinct recorded owners of the entries for server. Only a
// recorded owner that is a user id counts. An empty server is the
// directory's current one, as the plugin reads it. The plugin accepts each
// value as a user id: an owner has a login to find or leads to a sign-in,
// and an entry the name identifies passes its own owner check. A value can
// still be refused for the auth config the call names.
func RecordedUserIDs(cfg *config.Config, server, name, clusterID string) []string {
	if cfg == nil {
		return nil
	}
	server = ApplyDefaultScheme(server)
	if server == "" {
		server = cfg.EffectiveConnection().URL
	}
	var owners []string
	if id := config.IdentifyEntry(cfg, server, name, clusterID, false); id.Entry != "" {
		if id.OtherURL {
			return nil
		}
		owners = []string{cfg.Servers[id.Entry].TokenUserID}
	} else {
		for _, n := range cfg.MatchingServerNames(server) {
			owners = append(owners, cfg.Servers[n].TokenUserID)
		}
	}
	owners = slices.DeleteFunc(owners, func(o string) bool { return !ValidUserID(o) })
	slices.Sort(owners)
	return slices.Compact(owners)
}
