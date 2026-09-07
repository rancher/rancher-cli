package access

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/engine/rbac"
)

// TestClassicOwnersReadsBothLabelForms asserts the rtb-owner-updated value
// and the <ns>_<name> keys resolve to CRTB or PRTB references by the
// namespace half, that several keys yield several references in sorted
// order, and that unrelated labels and malformed values are ignored.
func TestClassicOwnersReadsBothLabelForms(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		rtbOwnerLabel:                                "c-m-746z46nj_crtb-5jxi7g32u5",
		"c-m-746z46nj-p-k7bbr_prtb-nwujaaihho":       ownerUserValue,
		"c-m-746z46nj-p-dwxmn_creator-project-owner": ownerUserValue,
		"c-m-746z46nj_creator-cluster-owner":         membershipOwnerValue,
		"cattle.io/creator":                          "norman",
		"kubernetes.io/bootstrapping":                "rbac-defaults",
		"no-underscore":                              ownerUserValue,
		"_leading":                                   ownerUserValue,
		"trailing_":                                  ownerUserValue,
		"c-m-746z46nj_x":                             "some-other-value",
	}
	got := classicOwners(labels, "c-m-746z46nj")
	assert.Equal(t, []ownerRef{
		{Kind: ownerCRTB, Namespace: "c-m-746z46nj", Name: "creator-cluster-owner"},
		{Kind: ownerCRTB, Namespace: "c-m-746z46nj", Name: "crtb-5jxi7g32u5"},
		{Kind: ownerPRTB, Namespace: "c-m-746z46nj-p-dwxmn", Name: "creator-project-owner"},
		{Kind: ownerPRTB, Namespace: "c-m-746z46nj-p-k7bbr", Name: "prtb-nwujaaihho"},
	}, got)
	assert.Empty(t, classicOwners(map[string]string{"cattle.io/creator": "norman"}, "c-m-746z46nj"))
	assert.Empty(t, classicOwners(nil, "c-m-746z46nj"))
}

// TestAggregationKeysAndMatching asserts the crtb-owner-* and prtb-owner-*
// keys are collected only when the aggregation marker is present, that a
// candidate name matches its key whether or not the key was truncated,
// and that a shared key (every project's creator binding) yields every
// candidate carrying that name.
func TestAggregationKeysAndMatching(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		aggregationLabel: "true",
		aggregationKeyFor(ownerCRTB, "c-m-g9vv28t9-fleet-default-owner"): "true",
		aggregationKeyFor(ownerPRTB, "creator-project-owner"):            "true",
		"authz.cluster.cattle.io/impersonator":                           "true",
	}
	keys := aggregationKeys(labels)
	assert.Equal(t, []aggregationKey{
		{Kind: ownerCRTB, Key: "authz.cluster.cattle.io/crtb-owner-c-m-g9vv28t9-fleet-def-f6563"},
		{Kind: ownerPRTB, Key: "authz.cluster.cattle.io/prtb-owner-creator-project-owner"},
	}, keys)
	assert.Equal(t, "authz.cluster.cattle.io/crtb-owner-c-m-g9vv28t9-fleet-def-f6563",
		rbac.SafeConcatName(crtbOwnerPrefix, "c-m-g9vv28t9-fleet-default-owner"))
	assert.Equal(t, []string{"c-m-g9vv28t9-fleet-default-owner"},
		matchAggregationKey(ownerCRTB, keys[0].Key, []string{"creator-cluster-owner", "c-m-g9vv28t9-fleet-default-owner", "crtb-dx6zh"}))
	assert.Equal(t, []string{"creator-project-owner", "creator-project-owner"},
		matchAggregationKey(ownerPRTB, keys[1].Key, []string{"creator-project-owner", "prtb-jx99v", "creator-project-owner"}))
	assert.Empty(t, matchAggregationKey(ownerCRTB, keys[0].Key, []string{"other"}))
	// Without the marker the same keys are not the aggregation grammar.
	delete(labels, aggregationLabel)
	assert.Empty(t, aggregationKeys(labels))
}
