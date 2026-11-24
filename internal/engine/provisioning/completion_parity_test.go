package provisioning

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCloudCredentialCompletionOffersWhatTheCommandResolves asserts the
// completion source of each mutation that takes a cloud credential (create
// cluster's --cloud-credential, the credential of patch and delete) and
// ResolveCloudCredential agree over one Secret list: a credential id is
// offered exactly when the command resolves it, and a display name that is no
// id, which the command refuses as not found, is never offered.
func TestCloudCredentialCompletionOffersWhatTheCommandResolves(t *testing.T) {
	t.Parallel()
	body := completiontest.CloudCredentialsBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

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
	for _, obj := range credentialObjects(list) {
		id := obj.GetName()
		ref, _, err := ResolveCloudCredential(t.Context(), factory, id, lookupForChange)
		if err == nil {
			assert.Equal(t, id, ref.SecretName)
			want = append(want, id)
			continue
		}
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code, "%s is refused for no other reason", id)
	}
	for name, got := range changingSources(t, srv.Client(), srv.URL) {
		assert.ElementsMatch(t, want, got.Names, name)
		for _, obj := range credentialObjects(list) {
			display := displayNameOf(obj)
			if _, isID := objects[display]; isID {
				continue
			}
			assert.NotContains(t, got.Names, display, name)
			_, _, err := ResolveCloudCredential(t.Context(), factory, display, lookupForChange)
			assert.True(t, clierr.IsNotFound(err), "the display name %q is not an id", display)
		}
	}
}

// changingSources returns what the completion source of each mutation that
// takes a cloud credential offers for its credential value: create cluster's
// --cloud-credential, and the <credential> argument of patch and delete.
// A connection allows one listing, so each source gets its own.
func changingSources(t *testing.T, client *http.Client, url string) map[string]completion.Result {
	t.Helper()
	ctx := t.Context()
	conn := func() *completion.Connection { return completion.NewConnection(client, "tok", url, false, 1) }
	return map[string]completion.Result{
		"create cluster":          complete.CreateClusterSource().Complete(ctx, conn(), completion.Request{Flag: "cloud-credential"}),
		"patch cloud-credential":  complete.PatchCloudCredentialSource().Complete(ctx, conn(), completion.Request{Index: 0}),
		"delete cloud-credential": complete.DeleteCloudCredentialSource().Complete(ctx, conn(), completion.Request{Index: 0}),
	}
}

// TestCompletionAndTheResolverKeepTheSameSecrets asserts that, for Secrets of
// each shape, the credential the resolver accepts is a credential the
// completion of every mutation that takes a cloud credential offers, and the
// reverse, and that each offered value resolves to its Secret for the
// commands.
func TestCompletionAndTheResolverKeepTheSameSecrets(t *testing.T) {
	t.Parallel()
	marker := map[string]any{"amazonec2credentialConfig-accessKey": "x"}
	for _, tc := range []struct {
		name   string
		secret *unstructured.Unstructured
		want   bool
	}{
		{"a Secret with a marker key and no driver in front", secretObj("plain", nil, map[string]any{"credentialConfig-x": "x"}), false},
		{"a Secret whose first marker key has no driver in front", secretObj("mixed", nil, map[string]any{"credentialConfig-x": "x", "zcredentialConfig-y": "y"}), false},
		{"a Secret named cc- with no keys", secretObj("cc-named", nil, nil), true},
		{"a Secret with a driver marker key", secretObj("azure1", nil, map[string]any{"azurecredentialConfig-id": "x"}), true},
		{"a Secret with no marker key", secretObj("none", nil, map[string]any{"other": "x"}), false},
		{"a Secret with a driver marker key and a display name", secretObj("named", map[string]string{displayNameAnnotation: "Named"}, marker), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*tc.secret}}
			body, err := json.Marshal(map[string]any{"items": []any{tc.secret.Object}})
			require.NoError(t, err)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)
			factory := fakeFactory{c: &fakeClient{objects: map[string]*unstructured.Unstructured{tc.secret.GetName(): tc.secret}, list: list}}

			assert.Equal(t, tc.want, len(credentialObjects(list)) == 1, "the resolver keeps the Secret")
			for name, got := range changingSources(t, srv.Client(), srv.URL) {
				assert.Equal(t, tc.want, slices.Contains(got.Names, tc.secret.GetName()), "%s offers the Secret", name)
				for _, offered := range got.Names {
					ref, _, err := ResolveCloudCredential(t.Context(), factory, offered, lookupForChange)
					require.NoError(t, err, "%s offers %q", name, offered)
					assert.Equal(t, offered, ref.SecretName)
					stored, err := readStoredCredential(t.Context(), factory, offered, "", lookupForChange)
					require.NoError(t, err, "%s offers %q", name, offered)
					assert.Equal(t, offered, stored.ref.SecretName)
				}
			}
		})
	}
}
