package runtime

import (
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func namespaceObjects(outcome string, names ...string) []safety.Object {
	var out []safety.Object
	for _, n := range names {
		out = append(out, safety.Object{Kind: "namespace", Name: n, Outcome: outcome})
	}
	return out
}

// TestObjectLinesShowsAtMostFiveNamesAndACount asserts one outcome with
// more than five names lists five and counts the rest, and an outcome with
// five or fewer lists them all.
func TestObjectLinesShowsAtMostFiveNamesAndACount(t *testing.T) {
	t.Parallel()
	objs := append(namespaceObjects(safety.OutcomeRancherKeeps, "ns1", "ns2", "ns3", "ns4", "ns5", "ns6", "ns7", "ns8"),
		namespaceObjects(safety.OutcomeRancherDeletes, "legacy")...)
	assert.Equal(t, []string{
		"namespaces:",
		"  rancher keeps: ns1, ns2, ns3, ns4, ns5 and 3 more",
		"  rancher deletes: legacy",
	}, ObjectLines(objs, "project"))
	five := namespaceObjects(safety.OutcomeRancherKeeps, "a", "b", "c", "d", "e")
	assert.Equal(t, []string{"namespaces:", "  rancher keeps: a, b, c, d, e"}, ObjectLines(five, "project"))
}

// TestObjectLinesOrdersOutcomesAndShowsReasons asserts the outcome lines
// follow one fixed order whatever the order of the objects, and a not
// deleted or failed name carries its reason in parentheses.
func TestObjectLinesOrdersOutcomesAndShowsReasons(t *testing.T) {
	t.Parallel()
	objs := []safety.Object{
		{Kind: "namespace", Name: "old-ns", Outcome: safety.OutcomeBeingDeleted},
		{Kind: "namespace", Name: "default", Outcome: safety.OutcomeNotDeleted, Message: "Kubernetes never deletes it"},
		{Kind: "namespace", Name: "apps", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "namespace", Name: "tools", Outcome: safety.OutcomeWillBeDeleted},
		{Kind: "project", Name: "p-1", Outcome: safety.OutcomeWillBeDeleted},
	}
	assert.Equal(t, []string{
		"namespaces:",
		"  will be deleted: apps, tools",
		"  not deleted: default (Kubernetes never deletes it)",
		"  being deleted: old-ns",
	}, ObjectLines(objs, "project"), "the headline kind is not listed")
	assert.Empty(t, ObjectLines(nil, "project"))
}

// TestDryRunTextListsTheEffectObjects asserts the preview body carries the
// object lines between the changes and the gate line, and an Effect with no
// objects renders as before.
func TestDryRunTextListsTheEffectObjects(t *testing.T) {
	t.Parallel()
	eff := safety.Effect{
		Action: safety.ActionDelete, DisplayKind: "project", DisplayName: "p-1", Cluster: "c-abc", Destructive: true,
		Objects: append(namespaceObjects(safety.OutcomeRancherKeeps, "apps"), namespaceObjects(safety.OutcomeRancherDeletes, "legacy")...),
	}
	assert.Equal(t, "delete project/p-1\n"+
		"  cluster: c-abc\n"+
		"  namespaces:\n"+
		"    rancher keeps: apps\n"+
		"    rancher deletes: legacy\n"+
		"  dry-run preview - re-run with --apply --yes to apply the change\n", dryRunText(eff))
	eff.Objects = nil
	assert.Equal(t, "delete project/p-1\n  cluster: c-abc\n  dry-run preview - re-run with --apply --yes to apply the change\n", dryRunText(eff))
}

// TestEffectObjectsMarshalAsAnArrayOnlyWhenPresent asserts the JSON key
// "objects" holds the items with their fields and is absent for an Effect
// that lists none.
func TestEffectObjectsMarshalAsAnArrayOnlyWhenPresent(t *testing.T) {
	t.Parallel()
	eff := safety.Effect{Action: safety.ActionDelete, DisplayKind: "project", DisplayName: "p-1",
		Objects: []safety.Object{{Kind: "namespace", Name: "default", Outcome: safety.OutcomeNotDeleted, Message: "Kubernetes never deletes it"}}}
	raw, err := json.Marshal(eff)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, []any{map[string]any{"kind": "namespace", "name": "default", "outcome": "not deleted", "message": "Kubernetes never deletes it"}}, decoded["objects"])

	eff.Objects = nil
	raw, err = json.Marshal(eff)
	require.NoError(t, err)
	decoded = nil
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.NotContains(t, decoded, "objects")
}

// TestObjectLinesGroupAndCompareByTheValuesAsGiven asserts the filter on the
// kind and the outcome is for printing only: a kind that differs from the
// headline kind only by a character the filter removes gets its own line,
// and two outcomes that differ only by one stay two lines, each with its own
// names and message; no line holds the character.
func TestObjectLinesGroupAndCompareByTheValuesAsGiven(t *testing.T) {
	t.Parallel()
	t.Run("kind", func(t *testing.T) {
		t.Parallel()
		objs := []safety.Object{
			{Kind: "Cluster", Name: "same", Outcome: safety.OutcomeRancherKeeps},
			{Kind: "Clu\x1bster", Name: "other", Outcome: safety.OutcomeRancherKeeps},
		}
		lines := ObjectLines(objs, "Cluster")
		assert.Equal(t, []string{"Clusters:", "  rancher keeps: other"}, lines)
	})
	t.Run("outcome", func(t *testing.T) {
		t.Parallel()
		objs := []safety.Object{
			{Kind: "namespace", Name: "a", Outcome: safety.OutcomeFailed, Message: "first"},
			{Kind: "namespace", Name: "b", Outcome: "fail\x1bed", Message: "second"},
		}
		lines := ObjectLines(objs, "project")
		assert.Equal(t, []string{"namespaces:", "  failed: a (first)", "  failed: b"}, lines)
	})
}
