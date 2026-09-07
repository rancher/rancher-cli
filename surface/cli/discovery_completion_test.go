package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestFetchDiscoveryDropsStaleGroups asserts completion's discovery goes
// through the shared parser: a group version the server marked stale
// contributes no suggestion, while the healthy groups' resources and
// subresources are offered.
func TestFetchDiscoveryDropsStaleGroups(t *testing.T) {
	t.Parallel()
	var accepts atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepts.Store(r.Header.Get("Accept"))
		var doc apidiscoveryv2.APIGroupDiscoveryList
		doc.Kind = "APIGroupDiscoveryList"
		switch r.URL.Path {
		case "/k8s/clusters/c-1/api":
			doc.Items = []apidiscoveryv2.APIGroupDiscovery{{ObjectMeta: metav1.ObjectMeta{Name: ""}, Versions: []apidiscoveryv2.APIVersionDiscovery{{
				Version: "v1", Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
				Resources: []apidiscoveryv2.APIResourceDiscovery{{Resource: "pods", Scope: apidiscoveryv2.ScopeNamespace, Verbs: []string{"get"},
					Subresources: []apidiscoveryv2.APISubresourceDiscovery{{Subresource: "log", Verbs: []string{"get"}}}}},
			}}}}
		case "/k8s/clusters/c-1/apis":
			doc.Items = []apidiscoveryv2.APIGroupDiscovery{
				{ObjectMeta: metav1.ObjectMeta{Name: "apps"}, Versions: []apidiscoveryv2.APIVersionDiscovery{{Version: "v1", Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
					Resources: []apidiscoveryv2.APIResourceDiscovery{{Resource: "deployments", Scope: apidiscoveryv2.ScopeNamespace, Verbs: []string{"get"}}}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "dead.example"}, Versions: []apidiscoveryv2.APIVersionDiscovery{{Version: "v1beta1", Freshness: apidiscoveryv2.DiscoveryFreshnessStale,
					Resources: []apidiscoveryv2.APIResourceDiscovery{{Resource: "widgets", Scope: apidiscoveryv2.ScopeNamespace, Verbs: []string{"get"}}}}}},
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()
	got := fetchDiscovery(t.Context(), srv.Client(), TokenCapability{Token: "t", ServerURL: srv.URL}, "c-1")
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.Group+"/"+r.Name)
	}
	assert.Equal(t, []string{"/pods", "/pods/log", "apps/deployments"}, names)
	assert.Contains(t, accepts.Load(), "as=APIGroupDiscoveryList")
}
