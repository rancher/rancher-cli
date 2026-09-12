package access

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/access/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
)

// decodeDiscovery decodes a shared discovery document for the commands'
// parser, so both readers see the very bytes a server would send.
func decodeDiscovery(t *testing.T, raw string) *apidiscoveryv2.APIGroupDiscoveryList {
	t.Helper()
	var doc apidiscoveryv2.APIGroupDiscoveryList
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	return &doc
}

// TestQuestionCompletionResolvesLikeTheCommands asserts the access
// questions' completion source and the commands' own discovery agree
// over the same two documents: the same base resources, the same named
// groups, the same subresources for every spelling of a resource (or
// nothing where the command would refuse), the same two requests under
// the same Accept header.
func TestQuestionCompletionResolvesLikeTheCommands(t *testing.T) {
	t.Parallel()
	coreRaw, groupsRaw := completiontest.DiscoveryCoreBody, completiontest.DiscoveryGroupsBody
	core, groups := decodeDiscovery(t, coreRaw), decodeDiscovery(t, groupsRaw)
	var mu sync.Mutex
	var paths, accepts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		accepts = append(accepts, r.Header.Get("Accept"))
		mu.Unlock()
		switch r.URL.Path {
		case "/k8s/clusters/c-x/api":
			_, _ = w.Write([]byte(coreRaw))
		case "/k8s/clusters/c-x/apis":
			_, _ = w.Write([]byte(groupsRaw))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	conn := func() *completion.Connection { return completion.NewConnection(srv.Client(), "tok", srv.URL, false, 2) }
	src := complete.QuestionSource()

	coreRes, _ := engine.ResourcesFromAggregated(core)
	groupRes, _ := engine.ResourcesFromAggregated(groups)
	all := append(coreRes, groupRes...)
	var wantResources, wantGroups []string
	for _, r := range all {
		if strings.Contains(r.Name, "/") {
			continue
		}
		wantResources = append(wantResources, r.Name)
		if r.Group != "" {
			wantGroups = append(wantGroups, r.Group)
		}
	}

	got := src.Complete(t.Context(), conn(), completion.Request{Index: 1, ClusterID: "c-x", Positionals: []string{"get"}})
	assert.Equal(t, wantResources, got.Names)
	require.Equal(t, []string{"/k8s/clusters/c-x/api", "/k8s/clusters/c-x/apis"}, paths)
	for _, a := range accepts {
		assert.Equal(t, engine.AggregatedDiscoveryAccept, a)
	}

	got = src.Complete(t.Context(), conn(), completion.Request{Flag: "group", ClusterID: "c-x", Positionals: []string{"get"}})
	assert.Equal(t, wantGroups, got.Names)

	for _, tc := range []struct{ arg, group string }{
		{"pods", ""}, {"pod", ""}, {"po", ""}, {"nodes", ""}, {"deployments.apps", ""}, {"deployments", "apps"},
		{"pods", "core"}, {"ingresses", ""}, {"ingresses.networking.k8s.io", ""}, {"ingresses", "gateway.example.io"},
		{"deployments.apps", "batch"}, {"nonesuch", ""}, {"widgets", ""}, {"gauges", ""},
		{"pods/web", ""}, {"deployments.apps/web", ""}, {"pods/", ""}, {"pods/a/b", ""},
	} {
		t.Run(tc.arg+" "+tc.group, func(t *testing.T) {
			// The commands read <resource>[/<name>] before resolving and
			// reject an empty name or a name with a slash.
			var want []string
			resource, name, hasName := strings.Cut(tc.arg, "/")
			if !hasName || (name != "" && !strings.Contains(name, "/")) {
				if ref, err := ResolveResourceIn(all, resource, tc.group); err == nil {
					want = ref.Subresources
				}
			}
			var tail []string
			if tc.group != "" {
				tail = []string{"--group", tc.group}
			}
			got := src.Complete(t.Context(), conn(), completion.Request{Flag: "subresource", ClusterID: "c-x", Positionals: []string{"get", tc.arg}, Tail: tail})
			names := slices.Clone(got.Names)
			slices.Sort(names)
			assert.Equal(t, want, names)
		})
	}
}
