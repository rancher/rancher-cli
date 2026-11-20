package catalog

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	catalogcomplete "github.com/rancher/rancher-cli/internal/engine/catalog/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestAddRepositoryAcceptsExactlyWhatCompletionOffers asserts, on the same
// fixtures, that each repository name the completion source offers is accepted
// by the plan and each name it leaves out is refused: the Prime repository on
// every edition, and a repository already added by URL or by name.
func TestAddRepositoryAcceptsExactlyWhatCompletionOffers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prime   string
		edition engine.Edition
		items   []*unstructured.Unstructured
		offered []string
	}{
		{"prime, nothing added", "true", engine.EditionPrime, nil, []string{"partner-extensions", "rancher-ui-plugins"}},
		{"community", "false", engine.EditionCommunity, nil, []string{"partner-extensions"}},
		{"unknown edition", "", engine.EditionUnknown, nil, []string{"partner-extensions"}},
		{"partner added by git repo", "true", engine.EditionPrime, []*unstructured.Unstructured{repoWith("p", map[string]any{"gitRepo": partnerURL}, "")}, []string{"rancher-ui-plugins"}},
		{"partner added by url", "false", engine.EditionCommunity, []*unstructured.Unstructured{repoWith("p", map[string]any{"url": partnerURL}, "")}, nil},
		{"prime repository added by status url", "true", engine.EditionPrime, []*unstructured.Unstructured{repoWith("q", map[string]any{"gitRepo": "https://example.com/x"}, primeURL)}, []string{"partner-extensions"}},
		{"name taken by another URL", "true", engine.EditionPrime, []*unstructured.Unstructured{repoWith("partner-extensions", map[string]any{"gitRepo": "https://example.com/x"}, "")}, []string{"rancher-ui-plugins"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			version := `{"Version":"v2.15.3"}`
			if tt.prime != "" {
				version = `{"Version":"v2.15.3","RancherPrime":"` + tt.prime + `"}`
			}
			list := map[string]any{"items": []any{}}
			for _, it := range tt.items {
				list["items"] = append(list["items"].([]any), it.Object)
			}
			body, err := json.Marshal(list)
			require.NoError(t, err)
			srv := completiontest.New(t, map[string]string{
				"/rancherversion": version,
				"/k8s/clusters/local/apis/catalog.cattle.io/v1/clusterrepos": string(body),
			})
			got := catalogcomplete.AddRepositorySource(addRepositoryName).Complete(t.Context(), srv.Conn(2), completion.Request{Command: addRepositoryName})
			assert.ElementsMatch(t, tt.offered, got.Names)

			for _, name := range []string{"partner-extensions", "rancher-ui-plugins"} {
				clients := &editionClients{addClients: &addClients{cluster: &addCluster{items: tt.items}}, edition: tt.edition}
				_, _, err := plannedAdd(t, clients, name)
				accepted := err == nil
				assert.Equal(t, slices.Contains(tt.offered, name), accepted, "plan of %s: error %v", name, err)
			}
		})
	}
}
