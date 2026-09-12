package provisioning

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/provisioning/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCloudCredentialCompletionOffersWhatTheCommandResolves asserts the
// --cloud-credential completion source and ResolveCloudCredential agree
// over one Secret list: every id the source offers is a credential the
// command's own predicate recognizes, every display name it offers
// resolves to exactly one credential, and no display name the command
// would call ambiguous or would read as an id is offered.
func TestCloudCredentialCompletionOffersWhatTheCommandResolves(t *testing.T) {
	t.Parallel()
	body := completiontest.CloudCredentialsBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	conn := completion.NewConnection(srv.Client(), "tok", srv.URL, false, 1)
	got := complete.CreateClusterSource().Complete(t.Context(), conn, completion.Request{Flag: "cloud-credential"})

	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &decoded))
	list := &unstructured.UnstructuredList{}
	for _, item := range decoded.Items {
		list.Items = append(list.Items, unstructured.Unstructured{Object: item})
	}
	objects := map[string]*unstructured.Unstructured{}
	for i := range list.Items {
		objects[list.Items[i].GetName()] = &list.Items[i]
	}
	factory := fakeFactory{c: &fakeClient{objects: objects, list: list}}

	var want []string
	ids := map[string]bool{}
	for _, obj := range credentialObjects(list) {
		want = append(want, obj.GetName())
		ids[obj.GetName()] = true
	}
	for _, obj := range credentialObjects(list) {
		display := displayNameOf(obj)
		if ids[display] {
			continue
		}
		if ref, err := ResolveCloudCredential(t.Context(), factory, display); err == nil {
			assert.Equal(t, obj.GetName(), ref.SecretName)
			want = append(want, display)
		}
	}
	assert.ElementsMatch(t, want, got.Names)
	for _, name := range got.Names {
		_, err := ResolveCloudCredential(t.Context(), factory, name)
		assert.NoError(t, err, "%s is offered, so the command must resolve it", name)
	}
}
