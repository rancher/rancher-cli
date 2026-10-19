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
