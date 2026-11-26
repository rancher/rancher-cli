package observe

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestSelectPodNotFoundCutsItsCandidates asserts that a NOT_FOUND error for a
// name no pod of the target has lists the first clierr.CandidateLimit pod names
// in sorted order and the number of pods, and that up to
// clierr.CandidateLimit pods are all listed with no total.
func TestSelectPodNotFoundCutsItsCandidates(t *testing.T) {
	t.Parallel()
	for _, n := range []int{clierr.CandidateLimit, clierr.CandidateLimit + 1, 300} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			pods := make([]unstructured.Unstructured, 0, n)
			for i := n - 1; i >= 0; i-- {
				pods = append(pods, unstructuredPodWithContainers(fmt.Sprintf("agent-%03d", i), "main"))
			}
			_, err := selectPod(pods, "agent-none")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			require.Len(t, ce.Candidates, min(n, clierr.CandidateLimit))
			for i, c := range ce.Candidates {
				assert.Equal(t, fmt.Sprintf("agent-%03d", i), c.ID)
			}
			if n > clierr.CandidateLimit {
				assert.Equal(t, n, ce.CandidatesTotal)
			} else {
				assert.Zero(t, ce.CandidatesTotal)
			}
		})
	}
}

// TestSelectPodNotFoundListsTheMatchesFirst asserts the NOT_FOUND error for a
// name no pod of the target has lists the pods whose name contains it, whatever
// the case, before the others, each part by name.
func TestSelectPodNotFoundListsTheMatchesFirst(t *testing.T) {
	t.Parallel()
	pods := make([]unstructured.Unstructured, 0, 300)
	for i := 297; i >= 0; i-- {
		pods = append(pods, unstructuredPodWithContainers(fmt.Sprintf("agent-%03d", i), "main"))
	}
	pods = append(pods, unstructuredPodWithContainers("web-1", "main"), unstructuredPodWithContainers("db-WEB", "main"))
	_, err := selectPod(pods, "web")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	var got []string
	for _, c := range ce.Candidates {
		got = append(got, c.ID)
	}
	assert.Equal(t, []string{"db-WEB", "web-1", "agent-000", "agent-001", "agent-002", "agent-003", "agent-004", "agent-005", "agent-006", "agent-007"}, got)
	assert.Equal(t, 300, ce.CandidatesTotal)
}
