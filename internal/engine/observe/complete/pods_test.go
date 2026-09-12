package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// podsBody is the two-pod list every path of the fixture answers.
const podsBody = `{"items":[{"metadata":{"name":"pod-b"}},{"metadata":{"name":"pod-a"}}]}`

func podRequest(tail ...string) completion.Request {
	var positionals []string
	if len(tail) > 0 && tail[0] != "" {
		positionals = tail[:1]
	}
	return completion.Request{Flag: "pod", Tail: tail, Positionals: positionals, ClusterID: "c-x"}
}

// TestPodSourceClaim asserts a local-only target claims with no cluster
// need and any credential, a downstream-only target with the cluster
// resolved and any credential, and a target without a static namespace,
// an unknown target, a missing target, and every other site are
// declined.
func TestPodSourceClaim(t *testing.T) {
	t.Parallel()
	src := PodSource()
	local := completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}
	downstream := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}
	for _, tt := range []struct {
		target string
		want   completion.Requirements
	}{
		{"server", local}, {"webhook", local}, {"api-extension", local},
		{"agent", downstream}, {"kube-api-auth", downstream},
	} {
		reqs, ok := src.Claim(podRequest(tt.target, "--pod"))
		require.True(t, ok, tt.target)
		assert.Equal(t, tt.want, reqs, tt.target)
	}
	for _, req := range []completion.Request{
		podRequest("fleet-agent", "--pod"),
		podRequest("nope", "--pod"),
		podRequest(),
		{Flag: "container", Positionals: []string{"server"}},
		{Index: 0},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestCompletePods asserts a local-only target lists the management
// cluster's pods in the target's namespace under its selector, a
// downstream-only target the resolved cluster's, names raw in list
// order; an unusable cluster id answers nothing without a request.
func TestCompletePods(t *testing.T) {
	t.Parallel()
	src := PodSource()
	s := completiontest.NewFixed(t, podsBody)
	got := src.Complete(t.Context(), s.Conn(1), podRequest("webhook", "--pod"))
	assert.Equal(t, []string{"pod-b", "pod-a"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/api/v1/namespaces/cattle-system/pods?labelSelector=app%3Drancher-webhook"}, s.Requested())

	s = completiontest.NewFixed(t, podsBody)
	got = src.Complete(t.Context(), s.Conn(1), podRequest("agent", "--cluster", "c-x", "--pod"))
	assert.Equal(t, []string{"pod-b", "pod-a"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/c-x/api/v1/namespaces/cattle-system/pods?labelSelector=app%3Dcattle-cluster-agent"}, s.Requested())

	conn := s.Conn(1)
	req := podRequest("agent", "--pod")
	req.ClusterID = "a/b"
	got = src.Complete(t.Context(), conn, req)
	assert.Empty(t, got.Names)
	assert.Zero(t, conn.Used())
}

// TestCompletePodsFailsEmpty asserts a refused listing answers nothing.
func TestCompletePodsFailsEmpty(t *testing.T) {
	t.Parallel()
	s := completiontest.NewStatus(t, 403)
	got := PodSource().Complete(t.Context(), s.Conn(1), podRequest("server", "--pod"))
	assert.Empty(t, got.Names)
}
