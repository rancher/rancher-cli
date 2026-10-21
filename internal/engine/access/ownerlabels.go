package access

import (
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/rbac"
)

// The labels Rancher writes on the RoleBindings and ClusterRoleBindings it
// creates for a ClusterRoleTemplateBinding or ProjectRoleTemplateBinding.
// Both grammars exist: the classic one is the default before Rancher
// v2.16, the aggregation one from v2.16 on (the aggregated-roletemplates
// feature), marked by aggregationLabel.
const (
	// rtbOwnerLabel carries "<namespace>_<name>" of the owning binding.
	rtbOwnerLabel = "authz.cluster.cattle.io/rtb-owner-updated"
	// ownerUserValue and membershipOwnerValue are the values of a label
	// whose KEY is "<namespace>_<name>" of the owning binding.
	ownerUserValue       = "owner-user"
	membershipOwnerValue = "membership-binding-owner"
	// crtbOwnerPrefix and prtbOwnerPrefix open an aggregation-grammar key,
	// "<prefix>-<name>" run through SafeConcatName; the value is "true".
	crtbOwnerPrefix = "authz.cluster.cattle.io/crtb-owner"
	prtbOwnerPrefix = "authz.cluster.cattle.io/prtb-owner"
	// aggregationLabel marks a binding the aggregation grammar wrote.
	aggregationLabel = "management.cattle.io/roletemplate-aggregation"
)

// ownerKind is the management object a back-reference names.
type ownerKind string

const (
	ownerCRTB ownerKind = "ClusterRoleTemplateBinding"
	ownerPRTB ownerKind = "ProjectRoleTemplateBinding"
)

// ownerRef is a classic-grammar back-reference: the management object by
// namespace and name, the kind decided by the namespace (the cluster id
// names a ClusterRoleTemplateBinding, anything else a
// ProjectRoleTemplateBinding in the project's backing namespace).
type ownerRef struct {
	Kind            ownerKind
	Namespace, Name string
}

// aggregationKey is an aggregation-grammar back-reference: the label key
// as written, matchable against candidate names, never parsed.
type aggregationKey struct {
	Kind ownerKind
	Key  string
}

// classicOwners reads every classic back-reference on a binding's labels:
// the rtbOwnerLabel value and every key whose value is ownerUserValue or
// membershipOwnerValue, each of the form "<namespace>_<name>" split at
// the first "_" (neither half contains one). The result is sorted by
// kind, namespace, name and deduplicated.
func classicOwners(labels map[string]string, clusterID string) []ownerRef {
	var refs []ownerRef
	add := func(raw string) {
		ns, name, found := strings.Cut(raw, "_")
		if !found || ns == "" || name == "" {
			return
		}
		kind := ownerPRTB
		if ns == clusterID {
			kind = ownerCRTB
		}
		refs = append(refs, ownerRef{Kind: kind, Namespace: ns, Name: name})
	}
	if v, ok := labels[rtbOwnerLabel]; ok {
		add(v)
	}
	for k, v := range labels {
		if v == ownerUserValue || v == membershipOwnerValue {
			add(k)
		}
	}
	slices.SortFunc(refs, func(a, b ownerRef) int {
		return strings.Compare(string(a.Kind)+"\x00"+a.Namespace+"\x00"+a.Name, string(b.Kind)+"\x00"+b.Namespace+"\x00"+b.Name)
	})
	return slices.Compact(refs)
}

// classicReferenced reports whether the labels carry any classic-grammar
// back-reference at all, parsable or not: the rtbOwnerLabel key, or a key
// whose value is ownerUserValue or membershipOwnerValue.
func classicReferenced(labels map[string]string) bool {
	if _, ok := labels[rtbOwnerLabel]; ok {
		return true
	}
	for _, v := range labels {
		if v == ownerUserValue || v == membershipOwnerValue {
			return true
		}
	}
	return false
}

// aggregationKeys reads every aggregation-grammar back-reference on a
// binding's labels. Without the aggregation marker the same-shaped keys
// are not this grammar and nothing is returned. Sorted by kind then key.
func aggregationKeys(labels map[string]string) []aggregationKey {
	if labels[aggregationLabel] != "true" {
		return nil
	}
	var keys []aggregationKey
	for k, v := range labels {
		if v != "true" {
			continue
		}
		switch {
		case strings.HasPrefix(k, crtbOwnerPrefix+"-"):
			keys = append(keys, aggregationKey{Kind: ownerCRTB, Key: k})
		case strings.HasPrefix(k, prtbOwnerPrefix+"-"):
			keys = append(keys, aggregationKey{Kind: ownerPRTB, Key: k})
		}
	}
	slices.SortFunc(keys, func(a, b aggregationKey) int {
		return strings.Compare(string(a.Kind)+"\x00"+a.Key, string(b.Kind)+"\x00"+b.Key)
	})
	return keys
}

// aggregationKeyFor is the label key Rancher writes for a binding name
// under the aggregation grammar.
func aggregationKeyFor(kind ownerKind, name string) string {
	prefix := crtbOwnerPrefix
	if kind == ownerPRTB {
		prefix = prtbOwnerPrefix
	}
	return rbac.SafeConcatName(prefix, name)
}

// matchAggregationKey returns, in candidate order, every candidate name
// whose aggregation key equals key. Several matches are possible: every
// project's creator binding is named creator-project-owner, so the key
// alone cannot tell them apart and the caller narrows by subject and
// project.
func matchAggregationKey(kind ownerKind, key string, candidates []string) []string {
	var out []string
	for _, c := range candidates {
		if aggregationKeyFor(kind, c) == key {
			out = append(out, c)
		}
	}
	return out
}
