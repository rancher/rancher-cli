package engine

import (
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newObj(m map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: m}
}

// plainRedaction hides nothing.
var plainRedaction = redactionFor(testGVR)

func walkOf(t *testing.T, obj map[string]any) []safety.DiffEntry {
	t.Helper()
	diff, err := CreateDiff(testGVR, obj, plainRedaction)
	require.NoError(t, err)
	return diff
}

// TestCreateDiffListsEveryLeafInDiffOrder asserts the walk lists one entry
// per leaf with from null, keys sorted at each level and list items by
// index. A walk that stops at lists, or that orders items as text, fails it:
// the eleventh list entry comes after the tenth.
func TestCreateDiffListsEveryLeafInDiffOrder(t *testing.T) {
	t.Parallel()
	items := make([]any, 11)
	for i := range items {
		items[i] = fmt.Sprintf("v%d", i)
	}
	diff := walkOf(t, map[string]any{
		"spec": map[string]any{
			"z":     "last",
			"a":     int64(1),
			"x":     items,
			"flag":  true,
			"pools": []any{map[string]any{"name": "p", "quantity": float64(2)}},
		},
	})
	var paths []string
	for _, e := range diff {
		assert.Nil(t, e.From, e.Path)
		paths = append(paths, e.Path)
	}
	assert.Equal(t, []string{
		"/spec/a", "/spec/flag",
		"/spec/pools/0/name", "/spec/pools/0/quantity",
		"/spec/x/0", "/spec/x/1", "/spec/x/2", "/spec/x/3", "/spec/x/4", "/spec/x/5",
		"/spec/x/6", "/spec/x/7", "/spec/x/8", "/spec/x/9", "/spec/x/10",
		"/spec/z",
	}, paths)
	assert.Equal(t, safety.DiffEntry{Path: "/spec/a", To: int64(1)}, diff[0])
	assert.Equal(t, safety.DiffEntry{Path: "/spec/x/10", To: "v10"}, diff[14])
}

// TestCreateDiffEscapesKeys asserts a key with "/" or "~" is one escaped
// pointer segment.
func TestCreateDiffEscapesKeys(t *testing.T) {
	t.Parallel()
	diff := walkOf(t, map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{"app.kubernetes.io/name": "web", "a~b": "c"},
		},
	})
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/metadata/labels/app.kubernetes.io~1name", To: "web"},
		{Path: "/metadata/labels/a~0b", To: "c"},
	}, diff)
}

// TestCreateDiffSkipsIdentityFieldsByPath asserts the four identity fields
// are left out and a map the skip empties is not a leaf: a submission that
// sets nothing past its identity has no entries.
func TestCreateDiffSkipsIdentityFieldsByPath(t *testing.T) {
	t.Parallel()
	assert.Empty(t, walkOf(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "n1"},
	}), "name only: no entry, and no /metadata entry")
	assert.Empty(t, walkOf(t, map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "User",
		"metadata":   map[string]any{"generateName": "u-"},
	}))
	assert.Empty(t, walkOf(t, map[string]any{}))
}

// TestCreateDiffKeepsNestedKindAndName asserts only the identity paths are
// skipped: a kind or name key deeper in the object is a field the command
// sets.
func TestCreateDiffKeepsNestedKindAndName(t *testing.T) {
	t.Parallel()
	diff := walkOf(t, map[string]any{
		"kind": "Cluster",
		"spec": map[string]any{
			"name": "top",
			"kind": "inner",
			"x":    map[string]any{"machineConfigRef": map[string]any{"kind": "DigitaloceanConfig", "name": "pool"}},
		},
		"metadata": map[string]any{"name": "c1", "labels": map[string]any{"name": "l", "kind": "k"}},
	})
	var paths []string
	for _, e := range diff {
		paths = append(paths, e.Path)
	}
	assert.Equal(t, []string{
		"/metadata/labels/kind", "/metadata/labels/name",
		"/spec/kind", "/spec/name",
		"/spec/x/machineConfigRef/kind", "/spec/x/machineConfigRef/name",
	}, paths)
}

// TestCreateDiffLeafRules asserts what counts as a leaf: a null, an empty
// list at any depth and an empty map below the top level are entries; an
// empty top-level map is not.
func TestCreateDiffLeafRules(t *testing.T) {
	t.Parallel()
	diff := walkOf(t, map[string]any{
		"metadata": map[string]any{},
		"spec": map[string]any{
			"rkeConfig": map[string]any{},
			"rules":     []any{},
			"nothing":   nil,
			"nested":    map[string]any{"deep": []any{}},
		},
		"rules": []any{},
		"empty": map[string]any{},
	})
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/rules", To: []any{}},
		{Path: "/spec/nested/deep", To: []any{}},
		{Path: "/spec/nothing", To: nil},
		{Path: "/spec/rkeConfig", To: map[string]any{}},
		{Path: "/spec/rules", To: []any{}},
	}, diff)
}

// TestCreateDiffKeepsNamespaceLabelsAndAnnotations asserts where the object
// lands and what a command labels it with are fields of the preview.
func TestCreateDiffKeepsNamespaceLabelsAndAnnotations(t *testing.T) {
	t.Parallel()
	diff := walkOf(t, map[string]any{
		"metadata": map[string]any{
			"name":        "n1",
			"namespace":   "team-a",
			"labels":      map[string]any{"field.cattle.io/projectId": "p-1"},
			"annotations": map[string]any{"field.cattle.io/creatorId": "u-1"},
		},
	})
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/metadata/annotations/field.cattle.io~1creatorId", To: "u-1"},
		{Path: "/metadata/labels/field.cattle.io~1projectId", To: "p-1"},
		{Path: "/metadata/namespace", To: "team-a"},
	}, diff)
}

// TestCreateDiffLeavesOutServerOwnedPaths asserts the resource version and
// the managed fields never appear, whatever they hold.
func TestCreateDiffLeavesOutServerOwnedPaths(t *testing.T) {
	t.Parallel()
	diff := walkOf(t, map[string]any{
		"metadata": map[string]any{
			"namespace":       "ns",
			"resourceVersion": "7",
			"managedFields":   []any{map[string]any{"manager": "cli.rancher.io", "fieldsV1": map[string]any{"f:spec": map[string]any{}}}},
		},
	})
	assert.Equal(t, []safety.DiffEntry{{Path: "/metadata/namespace", To: "ns"}}, diff)
}

// TestCreateDiffHidesRedactedValues asserts the walk applies the redaction:
// each leaf under a hidden path keeps its path and shows <redacted>.
func TestCreateDiffHidesRedactedValues(t *testing.T) {
	t.Parallel()
	secretRow := rowRedaction(t, "secret")
	diff, err := CreateDiff(secretRow.GVR(), map[string]any{
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "s", "namespace": "ns"},
		"stringData": map[string]any{"password": "hunter2", "empty": ""},
		"type":       "Opaque",
	}, secretRow)
	require.NoError(t, err)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/metadata/namespace", To: "ns"},
		{Path: "/stringData/empty", To: "<redacted>"},
		{Path: "/stringData/password", To: "<redacted>"},
		{Path: "/type", To: "Opaque"},
	}, diff)
}

// TestCreateDiffRefusesAMissingOrForeignRedaction asserts the walk is an
// internal error, before anything is built, for a zero redaction and for the
// redaction of another resource.
func TestCreateDiffRefusesAMissingOrForeignRedaction(t *testing.T) {
	t.Parallel()
	obj := map[string]any{"spec": map[string]any{"x": "y"}}
	diff, err := CreateDiff(testGVR, obj, kinds.Redaction{})
	require.ErrorContains(t, err, "internal error: create preview of projects has no redaction for its kind")
	assert.Nil(t, diff)
	diff, err = CreateDiff(testGVR, obj, rowRedaction(t, "secret"))
	require.ErrorContains(t, err, "internal error: create preview of projects uses the redaction for secrets")
	assert.Nil(t, diff)
}

// TestPlanCreateRefusesAMissingOrForeignRedaction asserts the planner
// refuses both before any request: the client sees no call.
func TestPlanCreateRefusesAMissingOrForeignRedaction(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		redaction kinds.Redaction
		want      string
	}{
		"zero":    {kinds.Redaction{}, "internal error: create preview of projects has no redaction for its kind"},
		"foreign": {rowRedaction(t, "secret"), "internal error: create preview of projects uses the redaction for secrets"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &obsClient{}
			obj := newObj(map[string]any{"metadata": map[string]any{"name": "p"}})
			_, _, _, err := PlanCreateObserved(t.Context(), client, testGVR, "c-x", obj, tc.redaction, "c-x", "project", "Project", false)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, client.dryRuns, "no request before the refusal")
			_, _, err = PlanCreate(t.Context(), client, testGVR, "c-x", obj, tc.redaction, "c-x", "project", "Project", false)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, client.dryRuns)
		})
	}
}

// TestPlanUpdateRefusesAMissingOrForeignRedaction asserts both update
// planners refuse a zero redaction and the redaction of another resource
// before any request: the client sees no call.
func TestPlanUpdateRefusesAMissingOrForeignRedaction(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		redaction kinds.Redaction
		want      string
	}{
		"zero":    {kinds.Redaction{}, "internal error: update preview of projects has no redaction for its kind"},
		"foreign": {rowRedaction(t, "secret"), "internal error: update preview of projects uses the redaction for secrets"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &obsClient{}
			obj := newObj(map[string]any{"metadata": map[string]any{"name": "p"}})
			_, _, _, err := PlanUpdateObserved(t.Context(), client, testGVR, "c-x", obj, tc.redaction, "c-x", "project", "Project", nil, false)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, client.dryRuns, "no request before the refusal")
			_, _, err = PlanUpdate(t.Context(), client, testGVR, "c-x", obj, tc.redaction, "c-x", "project", "Project", nil, false)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, client.dryRuns)
		})
	}
}

// TestPlanUpdateHidesTheRedactedValuesInTheDiff asserts the update planner
// hides, in the diff it receives, the values the redaction covers and keeps
// the others.
func TestPlanUpdateHidesTheRedactedValuesInTheDiff(t *testing.T) {
	t.Parallel()
	obj := newObj(map[string]any{"metadata": map[string]any{"name": "s"}})
	diff := []safety.DiffEntry{
		{Path: "/data/key", From: "old", To: "new"},
		{Path: "/spec/replicas", From: 1, To: 2},
	}
	eff, _, err := PlanUpdate(t.Context(), &obsClient{}, secretRedaction.GVR(), "ns", obj, secretRedaction, "c-x", "secret", "Secret", diff, true)
	require.NoError(t, err)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/data/key", From: Redacted, To: Redacted},
		{Path: "/spec/replicas", From: 1, To: 2},
	}, eff.Diff)
}

// TestPlanCreateSetsTheDiffFromTheSubmission asserts the planner's diff
// comes from the submission, not from the server's answer: a field only the
// answer holds is absent, and a field only the submission holds is listed.
func TestPlanCreateSetsTheDiffFromTheSubmission(t *testing.T) {
	t.Parallel()
	client := &obsClient{dryRunResult: newObj(map[string]any{
		"metadata": map[string]any{"name": "p", "uid": "u-1"},
		"spec":     map[string]any{"defaulted": "by-server"},
	})}
	obj := newObj(map[string]any{
		"metadata": map[string]any{"name": "p", "namespace": "c-x"},
		"spec":     map[string]any{"displayName": "p"},
	})
	eff, _, err := PlanCreate(t.Context(), client, testGVR, "c-x", obj, plainRedaction, "c-x", "project", "Project", false)
	require.NoError(t, err)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/metadata/namespace", To: "c-x"},
		{Path: "/spec/displayName", To: "p"},
	}, eff.Diff)
}
