package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var secretRedaction = func() kinds.Redaction {
	k, _ := kinds.ByKey("secret")
	return k.Redaction()
}()

// TestRedactDiffKeepsAMissingSideNull asserts a hidden entry keeps its path,
// shows <redacted> for each side that has a value and leaves a side that has
// none as null: an addition reads as one and a removal reads as one.
func TestRedactDiffKeepsAMissingSideNull(t *testing.T) {
	t.Parallel()
	got := RedactDiff([]safety.DiffEntry{
		{Path: "/data/added", From: nil, To: "bmV3"},
		{Path: "/data/removed", From: "b2xk", To: nil},
		{Path: "/data/changed", From: "b2xk", To: "bmV3"},
		{Path: "/stringData/k", From: nil, To: "v"},
		{Path: "/data", From: map[string]any{"password": "b2xk"}, To: nil},
		{Path: "/data", From: nil, To: map[string]any{"password": "b2xk"}},
		{Path: "/type", From: "Opaque", To: "kubernetes.io/basic-auth"},
	}, secretRedaction)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/data/added", To: "<redacted>"},
		{Path: "/data/removed", From: "<redacted>"},
		{Path: "/data/changed", From: "<redacted>", To: "<redacted>"},
		{Path: "/stringData/k", To: "<redacted>"},
		{Path: "/data", From: "<redacted>"},
		{Path: "/data", To: "<redacted>"},
		{Path: "/type", From: "Opaque", To: "kubernetes.io/basic-auth"},
	}, got)
}

// TestRedactDiffHidesStringDataEntries asserts the Secret payload paths are
// covered for both /data and /stringData, at the path and below it, and that
// a name that only starts with one is not.
func TestRedactDiffHidesStringDataEntries(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/data", "/data/k", "/stringData", "/stringData/k", "/stringData/k/nested"} {
		got := RedactDiff([]safety.DiffEntry{{Path: path, To: "v"}}, secretRedaction)
		assert.Equal(t, "<redacted>", got[0].To, path)
	}
	for _, path := range []string{"/dataExtra", "/stringDataExtra/k", "/metadata/data"} {
		got := RedactDiff([]safety.DiffEntry{{Path: path, To: "v"}}, secretRedaction)
		assert.Equal(t, "v", got[0].To, path)
	}
	plain := RedactDiff([]safety.DiffEntry{{Path: "/data/k", To: "v"}}, rowRedaction(t, "namespace"))
	assert.Equal(t, "v", plain[0].To, "a kind that is not Sensitive has no payload paths")
}

// TestRedactDiffHidesKindPathsAtOrUnder asserts a RedactPaths path hides the
// entry at the path and everything under it, and only those.
func TestRedactDiffHidesKindPathsAtOrUnder(t *testing.T) {
	t.Parallel()
	r := kinds.RowlessRedaction(testGVR, "a test resource with no kind row", []string{"spec", "newPassword"}, []string{"password"})
	got := RedactDiff([]safety.DiffEntry{
		{Path: "/spec/newPassword", To: "a"},
		{Path: "/spec/newPassword/x", To: "b"},
		{Path: "/password", To: "c"},
		{Path: "/spec/newPasswordHint", To: "d"},
		{Path: "/spec/userID", To: "e"},
	}, r)
	assert.Equal(t, []any{"<redacted>", "<redacted>", "<redacted>", "d", "e"},
		[]any{got[0].To, got[1].To, got[2].To, got[3].To, got[4].To})
}

// TestRedactDiffHidesTheLastAppliedAnnotation asserts the annotation that
// holds a copy of the applied object is hidden for every kind that hides
// anything, and for none that hides nothing.
func TestRedactDiffHidesTheLastAppliedAnnotation(t *testing.T) {
	t.Parallel()
	entry := safety.DiffEntry{Path: "/metadata/annotations/" + escapePointerToken(kinds.LastAppliedAnnotation), To: `{"data":{"k":"v"}}`}
	for name, r := range map[string]kinds.Redaction{
		"sensitive":    secretRedaction,
		"redact paths": rowRedaction(t, "user"),
		"redact paths only": kinds.RowlessRedaction(schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "passwordchangerequests"},
			"no kind row", []string{"spec", "newPassword"}),
		"machine config": rowRedaction(t, "amazonec2config"),
	} {
		got := RedactDiff([]safety.DiffEntry{entry}, r)
		assert.Equal(t, "<redacted>", got[0].To, name)
	}
	got := RedactDiff([]safety.DiffEntry{entry}, rowRedaction(t, "namespace"))
	assert.Equal(t, `{"data":{"k":"v"}}`, got[0].To, "a kind with nothing to hide shows the annotation")
}

// TestRedactDiffReplacesHiddenValuesInsideAnAncestorEntry asserts an entry
// above a hidden path (a patch that adds or removes /metadata/annotations,
// /metadata or the whole object in one step) carries a copy of its value with
// only the hidden values replaced, and leaves the original value untouched.
func TestRedactDiffReplacesHiddenValuesInsideAnAncestorEntry(t *testing.T) {
	t.Parallel()
	annotations := map[string]any{
		kinds.LastAppliedAnnotation: `{"data":{"k":"v"}}`,
		"owner":                     "team-a",
	}
	whole := map[string]any{
		"metadata": map[string]any{"name": "s", "annotations": annotations},
		"data":     map[string]any{"k": "dg=="},
		"type":     "Opaque",
	}
	got := RedactDiff([]safety.DiffEntry{
		{Path: "/metadata/annotations", From: nil, To: annotations},
		{Path: "/metadata/annotations", From: annotations, To: nil},
		{Path: "/metadata", From: nil, To: whole["metadata"]},
		{Path: "", From: whole, To: nil},
	}, secretRedaction)

	wantAnnotations := map[string]any{kinds.LastAppliedAnnotation: "<redacted>", "owner": "team-a"}
	assert.Equal(t, wantAnnotations, got[0].To)
	assert.Equal(t, wantAnnotations, got[1].From)
	assert.Nil(t, got[0].From)
	assert.Nil(t, got[1].To)
	assert.Equal(t, map[string]any{"name": "s", "annotations": wantAnnotations}, got[2].To)
	assert.Equal(t, map[string]any{
		"metadata": map[string]any{"name": "s", "annotations": wantAnnotations},
		"data":     "<redacted>",
		"type":     "Opaque",
	}, got[3].From)
	assert.Equal(t, `{"data":{"k":"v"}}`, annotations[kinds.LastAppliedAnnotation], "the original annotation is not changed")
	assert.Equal(t, map[string]any{"k": "dg=="}, whole["data"], "the original object is not changed")
}

// TestRedactDiffLeavesAnUnaffectedAncestorEntryAlone asserts an entry whose
// value holds nothing hidden is the value it was.
func TestRedactDiffLeavesAnUnaffectedAncestorEntryAlone(t *testing.T) {
	t.Parallel()
	labels := map[string]any{"app": "web"}
	got := RedactDiff([]safety.DiffEntry{{Path: "/metadata/labels", To: labels}}, secretRedaction)
	assert.Equal(t, labels, got[0].To)
}

// fleetHelm is a Helm options map with values, template values and the
// fields beside them that stay visible.
func fleetHelm(n string) map[string]any {
	return map[string]any{
		"chart":          "c" + n,
		"values":         map[string]any{"password": "s3cret" + n},
		"templateValues": map[string]any{"token": "t0ken" + n},
		"valuesFrom":     []any{map[string]any{"secretKeyRef": map[string]any{"name": "vals" + n}}},
	}
}

// redactedFleetHelm is fleetHelm with the values and template values hidden.
func redactedFleetHelm(n string) map[string]any {
	h := fleetHelm(n)
	h["values"] = Redacted
	h["templateValues"] = Redacted
	return h
}

// TestRedactDiffHidesFleetHelmValuesInLists asserts the helmop and bundle
// rows hide the values and template values of each target in a diff: an
// entry whose path is a hidden path, an entry whose value is the whole
// targets list (a merge patch that sets spec.targets), an entry for one added
// target, and an entry for an added helm map. An entry with no From or no To
// keeps it nil, the values outside the hidden paths stay, and the list in the
// input entry, which the patch and the fetched object may share, is not
// modified.
func TestRedactDiffHidesFleetHelmValuesInLists(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"helmop", "bundle"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			r := rowRedaction(t, key)
			targets := []any{
				map[string]any{"name": "a", "helm": fleetHelm("0")},
				map[string]any{"name": "b", "helm": fleetHelm("1")},
			}
			before := runtime.DeepCopyJSONValue(targets)
			got := RedactDiff([]safety.DiffEntry{
				{Path: "/spec/targets/0/helm/values", From: map[string]any{"password": "old"}, To: map[string]any{"password": "new"}},
				{Path: "/spec/targets/0/helm/templateValues", To: map[string]any{"token": "new"}},
				{Path: "/spec/targets", From: nil, To: targets},
				{Path: "/spec/targets/1", To: map[string]any{"name": "b", "helm": fleetHelm("1")}},
				{Path: "/spec/targets/0/helm", To: fleetHelm("0")},
				{Path: "/spec/targets/0/name", From: "a", To: "c"},
			}, r)
			assert.Equal(t, []safety.DiffEntry{
				{Path: "/spec/targets/0/helm/values", From: Redacted, To: Redacted},
				{Path: "/spec/targets/0/helm/templateValues", To: Redacted},
				{Path: "/spec/targets", To: []any{
					map[string]any{"name": "a", "helm": redactedFleetHelm("0")},
					map[string]any{"name": "b", "helm": redactedFleetHelm("1")},
				}},
				{Path: "/spec/targets/1", To: map[string]any{"name": "b", "helm": redactedFleetHelm("1")}},
				{Path: "/spec/targets/0/helm", To: redactedFleetHelm("0")},
				{Path: "/spec/targets/0/name", From: "a", To: "c"},
			}, got)
			assert.Equal(t, before, targets, "the original list and its items are unchanged")
			assert.NotEqual(t, reflect.ValueOf(targets).Pointer(), reflect.ValueOf(got[2].To).Pointer(), "the entry holds a copy of the list")
		})
	}
}

// TestRedactDiffHidesBundleDeploymentHelmValues asserts the bundledeployment
// row hides the values and template values of its options and staged options
// in a diff: an entry whose path is a hidden path, an entry for an added helm
// map and an entry for the whole options map.
func TestRedactDiffHidesBundleDeploymentHelmValues(t *testing.T) {
	t.Parallel()
	got := RedactDiff([]safety.DiffEntry{
		{Path: "/spec/options/helm/values", From: map[string]any{"password": "old"}, To: nil},
		{Path: "/spec/stagedOptions/helm/templateValues/token", To: "new"},
		{Path: "/spec/stagedOptions/helm", To: fleetHelm("0")},
		{Path: "/spec/options", To: map[string]any{"helm": fleetHelm("1"), "namespace": "ns"}},
	}, rowRedaction(t, "bundledeployment"))
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/spec/options/helm/values", From: Redacted},
		{Path: "/spec/stagedOptions/helm/templateValues/token", To: Redacted},
		{Path: "/spec/stagedOptions/helm", To: redactedFleetHelm("0")},
		{Path: "/spec/options", To: map[string]any{"helm": redactedFleetHelm("1"), "namespace": "ns"}},
	}, got)
}

// machineConfigRow is the kind row of a machine config kind.
func machineConfigRows(t *testing.T) []kinds.Kind {
	t.Helper()
	var rows []kinds.Kind
	for _, k := range kinds.All() {
		if k.GVR.Group == kinds.MachineConfigGroup {
			rows = append(rows, k)
		}
	}
	require.Len(t, rows, 5, "the five machine config kinds")
	return rows
}

// TestEveryMachineConfigSecretFieldIsHidden asserts, for every machine config
// kind and every name in its secret list, that a scalar value, a map value
// and a list value never reach the preview, under the name as listed and
// with its case changed, and that the path does.
func TestEveryMachineConfigSecretFieldIsHidden(t *testing.T) {
	t.Parallel()
	for _, row := range machineConfigRows(t) {
		fields, ok := kinds.MachineConfigFieldsFor(row.Name)
		require.True(t, ok, row.Name)
		require.NotEmpty(t, fields.Secret, row.Name)
		for _, name := range fields.Secret {
			for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name[:1]) + strings.ToUpper(name[1:])} {
				for shape, value := range map[string]any{
					"scalar": "s3cret-" + name,
					"map":    map[string]any{"k": "m3cret-" + name, "n": map[string]any{"deep": "d3cret-" + name}},
					"list":   []any{"l3cret-" + name, map[string]any{"k": "i3cret-" + name}},
				} {
					label := fmt.Sprintf("%s %s %s", row.Name, spelling, shape)
					diff, err := CreateDiff(row.GVR, map[string]any{
						"kind":     row.K8sKind,
						"metadata": map[string]any{"name": "mc", "namespace": "fleet-default"},
						"region":   "nyc3",
						spelling:   value,
					}, row.Redaction())
					require.NoError(t, err, label)
					raw, err := json.Marshal(diff)
					require.NoError(t, err, label)
					assert.NotContains(t, string(raw), "cret-"+name, label)
					hidden := 0
					for _, e := range diff {
						switch {
						case e.Path == "/"+spelling || strings.HasPrefix(e.Path, "/"+spelling+"/"):
							assert.Equal(t, "<redacted>", e.To, "%s: %s", label, e.Path)
							hidden++
						case e.Path == "/region":
							assert.Equal(t, "nyc3", e.To, "%s: other fields print as given", label)
						}
					}
					assert.Positive(t, hidden, "%s: the path is listed", label)
				}
			}
		}
	}
}

// TestMachineConfigRedactionHidesOnlyTheTopLevelField asserts a secret name
// hides a field of the object root and not a field of the same name below
// another key, and that the identifiers of a driver stay visible.
func TestMachineConfigRedactionHidesOnlyTheTopLevelField(t *testing.T) {
	t.Parallel()
	row, _ := kinds.Lookup("azureconfig")
	got := RedactDiff([]safety.DiffEntry{
		{Path: "/clientSecret", To: "a"},
		{Path: "/clientsecret", To: "b"},
		{Path: "/tags/clientSecret", To: "c"},
		{Path: "/clientId", To: "d"},
		{Path: "/tenantId", To: "e"},
	}, row.Redaction())
	assert.Equal(t, []any{"<redacted>", "<redacted>", "c", "d", "e"},
		[]any{got[0].To, got[1].To, got[2].To, got[3].To, got[4].To})
}
