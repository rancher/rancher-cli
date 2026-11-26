package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// steveNamespacesInFullAndInPart is the Steve namespace collection of a caller
// who may get team-a and has a role in team-b only: Steve lists team-b with a
// few fields and no kubernetes.io/metadata.name label.
const steveNamespacesInFullAndInPart = `{"data":[` +
	`{"id":"team-a","metadata":{"name":"team-a","labels":{"kubernetes.io/metadata.name":"team-a"}}},` +
	`{"id":"team-b","metadata":{"name":"team-b","labels":{"field.cattle.io/projectId":"p-1"}}}` +
	`],"revision":"1"}`

// TestNamespaceNameCompletionOffersOnlyTheNamespacesSteveListsInFull asserts a
// namespace Steve lists with a few fields is a candidate of a --namespace value
// (the caller has a role in it) and is not a candidate of the name positional,
// because `get namespace` of it fails; and that the Kubernetes list of an
// administrator, whose items carry no labels here, is offered at both sites.
func TestNamespaceNameCompletionOffersOnlyTheNamespacesSteveListsInFull(t *testing.T) {
	t.Parallel()
	const kubernetes, steveCollection = "/k8s/clusters/c-x/api/v1/namespaces", "/k8s/clusters/c-x/v1/namespaces"
	tests := []struct {
		name       string
		flag       string
		kubernetes int
		names      []string
	}{
		{name: "the --namespace value", flag: "namespace", kubernetes: 403, names: []string{"team-a", "team-b"}},
		{name: "the name positional", flag: "", kubernetes: 403, names: []string{"team-a"}},
		{name: "the --namespace value of an administrator", flag: "namespace", kubernetes: 200, names: []string{"b", "a"}},
		{name: "the name positional of an administrator", flag: "", kubernetes: 200, names: []string{"b", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := completiontest.NewByAccept(t, func(path, _ string) (string, int) {
				switch path {
				case kubernetes:
					return listBody, tt.kubernetes
				case steveCollection:
					return steveNamespacesInFullAndInPart, 200
				}
				return "", 404
			})
			got := KindSource("namespace").Complete(t.Context(), ls.Conn(2), completion.Request{Flag: tt.flag, ClusterID: "c-x"})
			assert.Equal(t, tt.names, got.Names)
		})
	}
}

// TestListedInFullAgreesWithTheReaderOfTheSteveList asserts the check of an
// item of the Steve list that the completion makes on decoded JSON gives the
// answer steve.ReadableInFull gives on the object the namespace read decodes.
func TestListedInFullAgreesWithTheReaderOfTheSteveList(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]map[string]any{
		"the label names the namespace": {"metadata": map[string]any{"name": "a", "labels": map[string]any{"kubernetes.io/metadata.name": "a"}}},
		"other labels only":             {"metadata": map[string]any{"name": "a", "labels": map[string]any{"field.cattle.io/projectId": "p-1"}}},
		"an empty label":                {"metadata": map[string]any{"name": "a", "labels": map[string]any{"kubernetes.io/metadata.name": ""}}},
		"no labels":                     {"metadata": map[string]any{"name": "a"}},
		"no metadata":                   {},
	} {
		assert.Equal(t, steve.ReadableInFull(&unstructured.Unstructured{Object: item}), listedInFull(item), name)
	}
}
