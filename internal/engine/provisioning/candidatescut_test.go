package provisioning

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// credentialsOf returns n cloud credentials named cc-000 and up, in
// descending order of their names, each with display name name-<number>
// unless display is set, in which case every credential has that display name.
func credentialsOf(n int, display string) *unstructured.UnstructuredList {
	var objs []*unstructured.Unstructured
	for i := n - 1; i >= 0; i-- {
		name := display
		if name == "" {
			name = fmt.Sprintf("name-%03d", i)
		}
		objs = append(objs, secretObj(fmt.Sprintf("cc-%03d", i), map[string]string{displayNameAnnotation: name},
			map[string]any{"amazonec2credentialConfig-x": "x"}))
	}
	return listOf(objs...)
}

// requireFirstCredentials asserts ce lists the first clierr.CandidateLimit of
// n cloud credentials by id, each id written with the namespace prefix, and
// records n as the total only when the list is longer than the limit.
func requireFirstCredentials(t *testing.T, ce *clierr.Error, n int) {
	t.Helper()
	require.Len(t, ce.Candidates, min(n, clierr.CandidateLimit))
	for i, c := range ce.Candidates {
		assert.Equal(t, fmt.Sprintf("cattle-global-data:cc-%03d", i), c.ID)
	}
	if n > clierr.CandidateLimit {
		assert.Equal(t, n, ce.CandidatesTotal)
	} else {
		assert.Zero(t, ce.CandidatesTotal)
	}
}

// TestCloudCredentialNotFoundCutsItsCandidates asserts the NOT_FOUND error of
// a cloud credential lookup lists the first clierr.CandidateLimit credentials
// by id and the size of the full list, for the read lookup and for patch and
// delete, and that the NOT_FOUND of patch or delete for a value that is the
// display name of other credentials cuts the list of those credentials the same
// way.
func TestCloudCredentialNotFoundCutsItsCandidates(t *testing.T) {
	t.Parallel()
	for _, n := range []int{clierr.CandidateLimit, clierr.CandidateLimit + 1, 300} {
		for name, lookup := range map[string]credentialLookup{"read": lookupForRead, "change": lookupForChange} {
			t.Run(name+" "+strconv.Itoa(n), func(t *testing.T) {
				t.Parallel()
				client := &fakeClient{list: credentialsOf(n, "")}
				_, _, err := ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cc-missing", lookup)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeNotFound, ce.Code)
				if lookup == lookupForRead {
					// The read lookup lists the credentials without the prefix.
					require.Len(t, ce.Candidates, min(n, clierr.CandidateLimit))
					assert.Equal(t, "cc-000", ce.Candidates[0].ID)
					if n > clierr.CandidateLimit {
						assert.Equal(t, n, ce.CandidatesTotal)
					} else {
						assert.Zero(t, ce.CandidatesTotal)
					}
					return
				}
				requireFirstCredentials(t, ce, n)
			})
		}
		t.Run("display name of change "+strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{list: credentialsOf(n, "web")}
			_, _, err := ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "web", lookupForChange)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			requireFirstCredentials(t, ce, n)
		})
	}
}

// matchCredentials returns 300 cloud credentials named cc-000 and up, in
// descending order of their names, where cc-250 has display name my-WEB and
// cc-100 has display name webapp.
func matchCredentials() *unstructured.UnstructuredList {
	var objs []*unstructured.Unstructured
	for i := 299; i >= 0; i-- {
		name := fmt.Sprintf("name-%03d", i)
		switch i {
		case 250:
			name = "my-WEB"
		case 100:
			name = "webapp"
		}
		objs = append(objs, secretObj(fmt.Sprintf("cc-%03d", i), map[string]string{displayNameAnnotation: name},
			map[string]any{"amazonec2credentialConfig-x": "x"}))
	}
	return listOf(objs...)
}

// TestCloudCredentialNotFoundListsTheMatchesFirst asserts the NOT_FOUND error
// of a cloud credential lookup, for the read lookup and for patch and delete,
// lists the credentials whose id or display name contains the typed value
// before the others, each part by id.
func TestCloudCredentialNotFoundListsTheMatchesFirst(t *testing.T) {
	t.Parallel()
	want := []string{"cc-100", "cc-250", "cc-000", "cc-001", "cc-002", "cc-003", "cc-004", "cc-005", "cc-006", "cc-007"}
	for name, lookup := range map[string]credentialLookup{"read": lookupForRead, "change": lookupForChange} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{list: matchCredentials()}
			_, _, err := ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "web", lookup)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			var got []string
			for _, c := range ce.Candidates {
				got = append(got, strings.TrimPrefix(c.ID, "cattle-global-data:"))
			}
			assert.Equal(t, want, got)
			assert.Equal(t, 300, ce.CandidatesTotal)
		})
	}
}
