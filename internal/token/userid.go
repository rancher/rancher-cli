package token

import (
	"errors"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rancher/rancher-cli/credhelper"
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
// cache, whose key carries it: typed empty, not a user id, and set on a
// call without --name are refused.
func readUserID(in runtime.Input, name, clusterID string) (string, error) {
	userID := in.String(userIDFlag)
	switch {
	case userID == "" && in.IsSet(userIDFlag):
		return "", clierr.New(clierr.CodeValidation, "--user-id is empty; pass a value or omit --user-id")
	case userID == "":
		return "", nil
	case !ValidUserID(userID):
		return "", clierr.New(clierr.CodeValidation,
			"--user-id "+clierr.ShellQuote(userID)+" is not a Rancher user id (a lowercase name such as u-abc12)")
	case name == "":
		return "", clierr.New(clierr.CodeValidation,
			"--user-id needs --name: regenerate the kubeconfig with "+clierr.Rancher+" kubeconfig --cluster "+clierr.ShellQuote(clusterID)+
				", which writes --name, and --user-id when the login it uses records its owner").WithCause(errUserIDNeedsName)
	}
	return userID, nil
}

// errUserIDNeedsName marks the refusal of --user-id without --name.
var errUserIDNeedsName = errors.New("--user-id needs --name")

// withNewEntryLogin completes the refusal of --user-id without --name for
// a directory that has no server entry for the call's server: rancher
// kubeconfig, its remedy, needs one, so the refusal names the login of a
// new entry there first, with the call's CA, a piped token of the user,
// and the entry made current. It reads config.yaml, on the refused call
// only. A server the call does not name and the directory does not know
// leaves the refusal as it is.
func (t tokenCmd) withNewEntryLogin(err error, in runtime.Input) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || !errors.Is(err, errUserIDNeedsName) {
		return err
	}
	cfg, lerr := config.Load(config.FilePath(t.dir))
	if lerr != nil {
		cfg = nil
	}
	server := applyDefaultScheme(in.String("server"))
	if server == "" && cfg != nil {
		server = cfg.EffectiveConnection().URL
	}
	serverID, cerr := credhelper.CanonicalServerURL(server)
	if cerr != nil || (cfg != nil && len(cfg.MatchingServerNames(serverID)) > 0) {
		return err
	}
	u := userCheck{userID: in.String(userIDFlag), clusterID: in.String("cluster"), serverID: serverID, caSource: in.String("cacert")}
	return ce.WithMessage("--user-id needs --name, and no server entry in this directory is registered for " + serverID + "; log in under a new one: " +
		u.loginOfNewEntry(cfg) + " - the kubeconfig it writes carries --name, and --user-id when the login records its owner")
}

// userIDPrefixRefused is the refusal of a --name in the namespace of the
// cache keys of calls with --user-id: a call without --user-id whose
// --name began with the prefix would read and write those keys.
func userIDPrefixRefused() *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		"--name values starting with '"+config.UserIDCachePrefix+"' are reserved for the exec plugin's cache slots of calls with --user-id; use a different name")
}

// RecordedUserID is the --user-id a call with --name name, --server server
// and --cluster clusterID can be given as cfg stands: the recorded owner of
// the entry the name identifies at server, when it is a user id; else "".
// An empty server is the directory's current one, as the plugin reads it.
// The plugin accepts the value: with that entry's own login it passes the
// owner check, and without one the call falls back or signs in.
func RecordedUserID(cfg *config.Config, server, name, clusterID string) string {
	if cfg == nil || name == "" {
		return ""
	}
	server = applyDefaultScheme(server)
	if server == "" {
		server = cfg.EffectiveConnection().URL
	}
	id := config.IdentifyEntry(cfg, server, name, clusterID, false)
	if id.Entry == "" || id.OtherURL {
		return ""
	}
	owner := cfg.Servers[id.Entry].TokenUserID
	if !ValidUserID(owner) {
		return ""
	}
	return owner
}
