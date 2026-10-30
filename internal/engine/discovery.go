package engine

import (
	"errors"
	"slices"
	"strings"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DiscoveryIncomplete is the error ServerResources returns TOGETHER with a
// partial resource list when the server could not describe every API
// group: an aggregated API whose backend is down, a group version whose
// discovery document answered 503. The list is what the server could
// describe; Failed names what it could not, so a caller can tell a
// complete inventory from a survivor instead of resolving against the
// survivor as if it were the whole cluster. A caller that treats the
// error as fatal loses nothing but the partial list.
type DiscoveryIncomplete struct {
	Failed map[schema.GroupVersion]error
}

func (e *DiscoveryIncomplete) Error() string {
	return "API discovery incomplete: the server could not list the resources of API groups " + strings.Join(e.Groups(), ", ")
}

// Groups returns the failed groups' names ("core" for the empty group),
// sorted and deduplicated across versions.
func (e *DiscoveryIncomplete) Groups() []string {
	seen := map[string]bool{}
	var out []string
	for gv := range e.Failed {
		g := gv.Group
		if g == "" {
			g = "core"
		}
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

// GroupFailed reports whether any version of group (empty or "core" for
// the core group) is among the failed.
func (e *DiscoveryIncomplete) GroupFailed(group string) bool {
	if group == "core" {
		group = ""
	}
	for gv := range e.Failed {
		if gv.Group == group {
			return true
		}
	}
	return false
}

// AggregatedDiscoveryAccept asks a Kubernetes API server for aggregated
// discovery: the whole inventory in one APIGroupDiscoveryList per root
// (/api, /apis), each group carrying its versions' resources inside.
// v2 is GA since Kubernetes 1.30 and v2beta1 shares its wire shape, so
// both are accepted and decoded with the v2 types. No plain
// application/json fallback is listed on purpose: a server that does not
// serve the document must be recognizable by its answer, not silently
// handed a legacy list.
const AggregatedDiscoveryAccept = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList," +
	"application/json;g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList"

// IsAggregatedDiscovery reports whether a discovery answer's Content-Type
// names the aggregated document. A server that does not serve it answers
// the same request with something else (the local cluster of Rancher
// v2.15.0 and v2.15.1: 406 to the aggregated-only header, a legacy
// APIGroupList to a header that also lists plain JSON), so the answer's
// content type, not the request, decides which parser applies.
func IsAggregatedDiscovery(contentType string) bool {
	return strings.Contains(contentType, "as=APIGroupDiscoveryList")
}

// ResourcesFromAggregated projects an APIGroupDiscoveryList onto the
// port's shape. Per group, the first version not marked stale (the server
// lists the preferred version first) contributes its resources and
// "<resource>/<sub>" entries for its subresources. Every version the
// server marked Stale is a failed group version: the API server keeps a
// stale entry when the group's backend stopped answering, and whatever
// it cached is not evidence of what the group serves now.
func ResourcesFromAggregated(doc *apidiscoveryv2.APIGroupDiscoveryList) ([]APIResource, map[schema.GroupVersion]error) {
	var out []APIResource
	failed := map[schema.GroupVersion]error{}
	for _, g := range doc.Items {
		picked := false
		for _, v := range g.Versions {
			if v.Freshness == apidiscoveryv2.DiscoveryFreshnessStale {
				failed[schema.GroupVersion{Group: g.Name, Version: v.Version}] = errors.New("the API server marked this version stale: its backend did not answer")
				continue
			}
			if picked {
				continue
			}
			picked = true
			for _, r := range v.Resources {
				base := APIResource{Group: g.Name, Version: v.Version, Name: r.Resource, SingularName: r.SingularResource,
					Kind: kindOf(r.ResponseKind), ShortNames: r.ShortNames, Namespaced: r.Scope == apidiscoveryv2.ScopeNamespace, Verbs: r.Verbs}
				out = append(out, base)
				for _, sub := range r.Subresources {
					out = append(out, APIResource{Group: g.Name, Version: v.Version, Name: r.Resource + "/" + sub.Subresource,
						Kind: kindOf(sub.ResponseKind), Namespaced: base.Namespaced, Verbs: sub.Verbs})
				}
			}
		}
	}
	return out, failed
}

// ResourcesFromLegacy projects the legacy discovery documents (one
// APIResourceList per group version - the provider reads the server's
// preferred version of each group, the one version the aggregated path
// also picks) onto the port's shape. A subresource arrives as its own
// entry named "<resource>/<sub>" and stays that way; a list whose
// groupVersion does not parse contributes nothing.
func ResourcesFromLegacy(lists []*metav1.APIResourceList) []APIResource {
	out := []APIResource{}
	for _, l := range lists {
		if l == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, res := range l.APIResources {
			out = append(out, APIResource{
				Group: gv.Group, Version: gv.Version, Name: res.Name, SingularName: res.SingularName,
				Kind: res.Kind, ShortNames: res.ShortNames, Namespaced: res.Namespaced, Verbs: res.Verbs,
			})
		}
	}
	return out
}

// kindOf reads the optional responseKind of a discovery entry.
func kindOf(gvk *metav1.GroupVersionKind) string {
	if gvk == nil {
		return ""
	}
	return gvk.Kind
}

// ErrDiscoveryIncompleteFor builds the port's partial-answer error, nil
// when nothing failed - so a provider can return it unconditionally.
func ErrDiscoveryIncompleteFor(failed map[schema.GroupVersion]error) error {
	if len(failed) == 0 {
		return nil
	}
	return &DiscoveryIncomplete{Failed: failed}
}

var _ error = (*DiscoveryIncomplete)(nil)
