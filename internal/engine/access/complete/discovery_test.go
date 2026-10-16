package complete

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	corePath  = "/k8s/clusters/c-x/api"
	groupPath = "/k8s/clusters/c-x/apis"
)

// bothRoots serves the shared discovery documents on the two roots of
// cluster c-x.
func bothRoots(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, map[string]string{corePath: completiontest.DiscoveryCoreBody, groupPath: completiontest.DiscoveryGroupsBody})
}

func discoveryRequest(flag string, positionals []string, tail ...string) completion.Request {
	req := completion.Request{Flag: flag, ClusterID: "c-x", Positionals: positionals, Tail: tail}
	if flag == "" {
		req.Index = 1
	}
	return req
}

// TestQuestionSourceClaims asserts the three discovery-backed sites are
// claimed with the discovery declaration - --subresource only once a
// resource is typed - and nothing is claimed beside --non-resource-url,
// for the verb positional, for --as, or for another flag.
func TestQuestionSourceClaims(t *testing.T) {
	t.Parallel()
	src := QuestionSource()
	tests := []struct {
		name string
		req  completion.Request
		ok   bool
	}{
		{name: "resource positional", req: completion.Request{Index: 1}, ok: true},
		{name: "group", req: completion.Request{Flag: "group"}, ok: true},
		{name: "subresource with a resource typed", req: completion.Request{Flag: "subresource", Positionals: []string{"get", "pods"}}, ok: true},
		{name: "subresource without a resource typed", req: completion.Request{Flag: "subresource", Positionals: []string{"get"}}},
		{name: "verb positional", req: completion.Request{Index: 0}},
		{name: "resource positional beside --non-resource-url", req: completion.Request{Index: 1, Tail: []string{"get", "--non-resource-url", "/healthz"}}},
		{name: "group beside --non-resource-url=", req: completion.Request{Flag: "group", Tail: []string{"--non-resource-url=/healthz"}}},
		{name: "--as", req: completion.Request{Flag: "as"}},
		{name: "--cluster", req: completion.Request{Flag: "cluster"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reqs, ok := src.Claim(tt.req)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, discoveryListing(), reqs)
			}
		})
	}
}

// TestCompleteQuestionReadsTheTwoRoots asserts the resource positional
// answers every base resource from both roots in their preferred version
// (a stale first version skipped, a stale-only group dropped), --group
// the named groups, both with exactly two requests under the aggregated
// Accept header.
func TestCompleteQuestionReadsTheTwoRoots(t *testing.T) {
	t.Parallel()
	s := bothRoots(t)
	src := QuestionSource()

	conn := s.Conn(2)
	got := src.Complete(t.Context(), conn, discoveryRequest("", []string{"get"}))
	assert.Equal(t, []string{"pods", "nodes", "services", "deployments", "ingresses", "ingresses", "gauges"}, got.Names)
	assert.Equal(t, 2, conn.Used())
	assert.Equal(t, []string{corePath, groupPath}, s.Requested())
	for _, accept := range s.Accepts() {
		assert.Equal(t, aggregatedDiscoveryAccept, accept)
	}

	got = src.Complete(t.Context(), s.Conn(2), discoveryRequest("group", []string{"get", "pods"}))
	assert.Equal(t, []string{"apps", "networking.k8s.io", "gateway.example.io", "metrics.example.io"}, got.Names)
}

// TestCompleteQuestionSubresources asserts --subresource follows the
// commands' resolution rules: plural, singular and short name; a
// qualified name; --group; the core group named "core"; a bare name two
// non-core groups serve answers nothing; a qualified ambiguous name
// answers its group's; conflicting group spellings, an unknown resource
// and a stale group's resource answer nothing; a <resource>/<name>
// spelling resolves its resource, and a name the commands reject
// answers nothing.
func TestCompleteQuestionSubresources(t *testing.T) {
	t.Parallel()
	s := bothRoots(t)
	src := QuestionSource()
	tests := []struct {
		name     string
		resource string
		tail     []string
		want     []string
	}{
		{name: "plural", resource: "pods", want: []string{"log", "status", "exec"}},
		{name: "singular", resource: "pod", want: []string{"log", "status", "exec"}},
		{name: "short name", resource: "po", want: []string{"log", "status", "exec"}},
		{name: "cluster-scoped", resource: "nodes", want: []string{"status", "proxy"}},
		{name: "qualified", resource: "deployments.apps", want: []string{"scale", "status"}},
		{name: "group flag", resource: "deployments", tail: []string{"--group", "apps"}, want: []string{"scale", "status"}},
		{name: "core named", resource: "pods", tail: []string{"--group=core"}, want: []string{"log", "status", "exec"}},
		{name: "ambiguous bare name", resource: "ingresses"},
		{name: "qualified ambiguous name", resource: "ingresses.networking.k8s.io", want: []string{"status"}},
		{name: "ambiguous name under a group", resource: "ingresses", tail: []string{"--group", "gateway.example.io"}, want: []string{"conditions"}},
		{name: "conflicting group spellings", resource: "deployments.apps", tail: []string{"--group", "batch"}},
		{name: "unknown resource", resource: "nonesuch"},
		{name: "stale group's resource", resource: "widgets"},
		{name: "preferred version of a partly stale group", resource: "gauges", want: []string{"status"}},
		{name: "resource with a name", resource: "pods/web", want: []string{"log", "status", "exec"}},
		{name: "qualified resource with a name", resource: "deployments.apps/web", want: []string{"scale", "status"}},
		{name: "resource with an empty name", resource: "pods/"},
		{name: "resource with a name carrying a slash", resource: "pods/a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := src.Complete(t.Context(), s.Conn(2), discoveryRequest("subresource", []string{"get", tt.resource}, tt.tail...))
			assert.Equal(t, tt.want, got.Names)
		})
	}
}

// legacyGroupsRoot serves the core root as the aggregated document and
// the groups root the way the local cluster of Rancher v2.15.0 and
// v2.15.1 does: 406 to the aggregated Accept header, the legacy group
// list to plain JSON, and one legacy document per group version; the dead
// group's document is 503.
func legacyGroupsRoot(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.NewByAccept(t, func(path, accept string) (string, int) {
		aggregated := strings.Contains(accept, "APIGroupDiscoveryList")
		switch {
		case path == corePath && aggregated:
			return completiontest.DiscoveryCoreBody, http.StatusOK
		case path == groupPath && aggregated:
			return `{"kind":"Status","code":406}`, http.StatusNotAcceptable
		case path == groupPath:
			return completiontest.DiscoveryLegacyGroupsBody, http.StatusOK
		}
		if body, ok := completiontest.DiscoveryLegacyVersionBodies[strings.TrimPrefix(path, "/k8s/clusters/c-x")]; ok {
			return body, http.StatusOK
		}
		if path == "/k8s/clusters/c-x/apis/dead.example/v1beta1" {
			return "service unavailable", http.StatusServiceUnavailable
		}
		return "not found", http.StatusNotFound
	})
}

// TestCompleteQuestionWalksALegacyRoot asserts a root that does not
// serve the aggregated document is walked the legacy way - the plain
// root document, then one document per group in its preferred version -
// and answers the same resources, groups and subresources the
// aggregated document of the same cluster answers; the dead group, whose
// document fails, contributes nothing; the requests are the aggregated
// attempt, the plain root, and one per group.
func TestCompleteQuestionWalksALegacyRoot(t *testing.T) {
	t.Parallel()
	limit := 2 + 2*(1+completion.DiscoveryGroupLimit)
	src := QuestionSource()
	aggregated := bothRoots(t)
	legacy := legacyGroupsRoot(t)

	for _, tc := range []struct {
		name string
		req  completion.Request
	}{
		{"resources", discoveryRequest("", []string{"get"})},
		{"groups", discoveryRequest("group", []string{"get", "pods"})},
		{"subresources", discoveryRequest("subresource", []string{"get", "deployments"})},
		{"ambiguous subresources by group", discoveryRequest("subresource", []string{"get", "ingresses"}, "--group", "gateway.example.io")},
		{"subresources of the preferred version", discoveryRequest("subresource", []string{"get", "gauges"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := src.Complete(t.Context(), aggregated.Conn(limit), tc.req)
			got := src.Complete(t.Context(), legacy.Conn(limit), tc.req)
			assert.Equal(t, want.Names, got.Names)
		})
	}

	fresh := legacyGroupsRoot(t)
	conn := fresh.Conn(limit)
	got := src.Complete(t.Context(), conn, discoveryRequest("", []string{"get"}))
	assert.Equal(t, []string{"pods", "nodes", "services", "deployments", "ingresses", "ingresses", "gauges"}, got.Names)
	assert.False(t, conn.Exceeded())
	assert.Equal(t, 1+2+5, conn.Used(), "core aggregated, groups aggregated then plain, one document per group")
	requested := fresh.Requested()
	assert.Equal(t, []string{corePath, groupPath, groupPath}, requested[:3])
	assert.ElementsMatch(t, []string{
		"/k8s/clusters/c-x/apis/apps/v1", "/k8s/clusters/c-x/apis/networking.k8s.io/v1", "/k8s/clusters/c-x/apis/gateway.example.io/v1",
		"/k8s/clusters/c-x/apis/metrics.example.io/v1", "/k8s/clusters/c-x/apis/dead.example/v1beta1",
	}, requested[3:])
	accepts := fresh.Accepts()
	assert.Equal(t, aggregatedDiscoveryAccept, accepts[1])
	assert.Equal(t, "application/json", accepts[2])
}

// TestCompleteQuestionLegacyCoreRoot asserts the core root, when it does
// not serve the aggregated document, is walked through its versions
// document to the preferred version's document.
func TestCompleteQuestionLegacyCoreRoot(t *testing.T) {
	t.Parallel()
	s := completiontest.NewByAccept(t, func(path, accept string) (string, int) {
		switch {
		case path == corePath && strings.Contains(accept, "APIGroupDiscoveryList"):
			return "", http.StatusNotAcceptable
		case path == corePath:
			return completiontest.DiscoveryLegacyCoreBody, http.StatusOK
		case path == corePath+"/v1":
			return completiontest.DiscoveryLegacyVersionBodies["/api/v1"], http.StatusOK
		case path == groupPath:
			return completiontest.DiscoveryGroupsBody, http.StatusOK
		}
		return "", http.StatusNotFound
	})
	got := QuestionSource().Complete(t.Context(), s.Conn(8), discoveryRequest("subresource", []string{"get", "pods"}))
	assert.Equal(t, []string{"log", "status", "exec"}, got.Names)
}

// TestCompleteQuestionLegacyWalkIsBounded asserts a legacy root with more
// groups than the seam's limit has only the first limit groups read, so
// the source never exceeds its allowance and still answers what it read.
func TestCompleteQuestionLegacyWalkIsBounded(t *testing.T) {
	t.Parallel()
	var groups strings.Builder
	for i := range completion.DiscoveryGroupLimit + 5 {
		if i > 0 {
			groups.WriteString(",")
		}
		fmt.Fprintf(&groups, `{"name":"g%03d.example.io","preferredVersion":{"version":"v1"}}`, i)
	}
	s := completiontest.NewByAccept(t, func(path, accept string) (string, int) {
		switch {
		case path == corePath:
			return completiontest.DiscoveryCoreBody, http.StatusOK
		case path == groupPath && strings.Contains(accept, "APIGroupDiscoveryList"):
			return "", http.StatusNotAcceptable
		case path == groupPath:
			return `{"kind":"APIGroupList","groups":[` + groups.String() + `]}`, http.StatusOK
		}
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/k8s/clusters/c-x/apis/"), "/v1")
		return `{"kind":"APIResourceList","resources":[{"name":"` + strings.ReplaceAll(name, ".", "-") + `s","singularName":"x"}]}`, http.StatusOK
	})
	limit := 2 + 2*(1+completion.DiscoveryGroupLimit)
	conn := s.Conn(limit)
	got := QuestionSource().Complete(t.Context(), conn, discoveryRequest("group", []string{"get", "pods"}))
	assert.Len(t, got.Names, completion.DiscoveryGroupLimit)
	assert.Equal(t, "g000.example.io", got.Names[0])
	assert.False(t, conn.Exceeded())
	assert.Equal(t, 1+2+completion.DiscoveryGroupLimit, conn.Used())
}

// TestCompleteQuestionFailsClosed asserts a root that fails contributes
// nothing while the other root still answers; a legacy group without a
// version, and a group whose document fails, contribute nothing; an
// unusable cluster id costs no request.
func TestCompleteQuestionFailsClosed(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, map[string]string{corePath: completiontest.DiscoveryCoreBody, groupPath: `{"kind":"APIGroupList","groups":[{"name":"apps"},{"name":"batch","preferredVersion":{"version":"v1"}}]}`})
	got := QuestionSource().Complete(t.Context(), s.Conn(8), discoveryRequest("", []string{"get"}))
	assert.Equal(t, []string{"pods", "nodes", "services"}, got.Names, "a version-less group and a group whose document is not served contribute nothing")

	s = completiontest.New(t, map[string]string{groupPath: completiontest.DiscoveryGroupsBody})
	got = QuestionSource().Complete(t.Context(), s.Conn(8), discoveryRequest("group", []string{"get", "pods"}))
	assert.Equal(t, []string{"apps", "networking.k8s.io", "gateway.example.io", "metrics.example.io"}, got.Names, "a failed core root still leaves the groups")

	conn := s.Conn(8)
	req := discoveryRequest("", []string{"get"})
	req.ClusterID = "a/b"
	got = QuestionSource().Complete(t.Context(), conn, req)
	require.Empty(t, got.Names)
	assert.Zero(t, conn.Used())
}
