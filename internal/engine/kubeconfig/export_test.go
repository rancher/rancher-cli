package kubeconfig

import (
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/token"
)

// NoOwnerNoticeForTest is the notice the kubeconfig for clusterID prints
// when the login of entry answers for the identified entry and records no
// owner.
func NoOwnerNoticeForTest(cfg *config.Config, identified, entry, clusterID string, plugin bool) string {
	_, notice := lookupCall{cfg: cfg, clusterID: clusterID}.recordedOwner(token.StoredLogin{
		Outcome: token.LookupAnswered, Identified: identified, Entry: entry, PluginEntry: plugin,
	})
	return notice
}

// NoStoredLoginNoticeForTest is the notice the kubeconfig for clusterID prints
// when no stored login answers for it.
func NoStoredLoginNoticeForTest(cfg *config.Config, dir, name, clusterID, serverID string) string {
	return lookupNotice(lookupCall{cfg: cfg, dir: dir, name: name, clusterID: clusterID, serverID: serverID}, token.StoredLogin{Outcome: token.LookupNone})
}

// ReservedNameRefusalForTest is the message that refuses a --name auth
// get-token refuses.
func ReservedNameRefusalForTest(name, clusterID, serverURL string) string {
	err := reservedName(name, clusterID, serverURL, "", config.CASource{})
	if err == nil {
		return ""
	}
	return err.Error()
}
