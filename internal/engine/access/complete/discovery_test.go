package complete

import (
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

// TestCompleteQuestionFailsClosed asserts a root that fails, or answers
// another document, contributes nothing while the other root still
// answers; an unusable cluster id costs no request.
func TestCompleteQuestionFailsClosed(t *testing.T) {
	t.Parallel()
	s := completiontest.New(t, map[string]string{corePath: completiontest.DiscoveryCoreBody, groupPath: `{"kind":"APIGroupList","groups":[{"name":"apps"}]}`})
	got := QuestionSource().Complete(t.Context(), s.Conn(2), discoveryRequest("", []string{"get"}))
	assert.Equal(t, []string{"pods", "nodes", "services"}, got.Names, "the plain discovery document of the second root contributes nothing")

	s = completiontest.New(t, map[string]string{groupPath: completiontest.DiscoveryGroupsBody})
	got = QuestionSource().Complete(t.Context(), s.Conn(2), discoveryRequest("group", []string{"get", "pods"}))
	assert.Equal(t, []string{"apps", "networking.k8s.io", "gateway.example.io", "metrics.example.io"}, got.Names, "a failed core root still leaves the groups")

	conn := s.Conn(2)
	req := discoveryRequest("", []string{"get"})
	req.ClusterID = "a/b"
	got = QuestionSource().Complete(t.Context(), conn, req)
	require.Empty(t, got.Names)
	assert.Zero(t, conn.Used())
}
