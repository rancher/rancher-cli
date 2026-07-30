package kinds_test

import (
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
)

// publicAPIMembers is the semi-official Rancher Kubernetes Public API list,
// provided by the team 2026-07-30; update only with a citation.
//
// Format: "<K8sKind>.<group>"
//
// ClusterProxyConfig.management.cattle.io and AuditPolicy.auditlog.cattle.io
// are on the list but have no kind-table rows yet — they are listed here for
// reference and are NOT expected to produce a test failure when absent.
var publicAPIMembers = []string{
	"Project.management.cattle.io",
	"Token.ext.cattle.io",
	"Kubeconfig.ext.cattle.io",
	"User.management.cattle.io",
	"RoleTemplate.management.cattle.io",
	"GlobalRole.management.cattle.io",
	"GlobalRoleBinding.management.cattle.io",
	"ClusterRoleTemplateBinding.management.cattle.io",
	"ProjectRoleTemplateBinding.management.cattle.io",
	// No kind-table rows yet — listed for completeness, tolerated as absent:
	// "ClusterProxyConfig.management.cattle.io",
	// "AuditPolicy.auditlog.cattle.io",
}

// TestPublicAPIKindsAreContractPublic asserts the two-way conformance between
// the semi-official membership list and the kind table:
//
//  1. Every kind-table row with ContractPublic maps to a listed <K8sKind>.<group>.
//  2. Every listed entry that HAS a kind-table row is ContractPublic.
func TestPublicAPIKindsAreContractPublic(t *testing.T) {
	memberSet := make(map[string]bool, len(publicAPIMembers))
	for _, m := range publicAPIMembers {
		memberSet[m] = true
	}

	for _, k := range kinds.All() {
		key := fmt.Sprintf("%s.%s", k.K8sKind, k.GVR.Group)
		if k.Binding.Contract == safety.ContractPublic {
			assert.True(t, memberSet[key],
				"kind %q (%s) has ContractPublic but is NOT in the publicAPIMembers list — add it or downgrade its contract",
				k.Name, key)
		}
		if memberSet[key] {
			assert.Equal(t, safety.ContractPublic, k.Binding.Contract,
				"kind %q (%s) is in publicAPIMembers but does NOT have ContractPublic",
				k.Name, key)
		}
	}
}
