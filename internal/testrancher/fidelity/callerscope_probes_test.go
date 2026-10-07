package fidelity

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// normalizedFor is body after probe name's normalization, as JSON.
func normalizedFor(t *testing.T, name string, body string) string {
	t.Helper()
	var p Probe
	for _, q := range All() {
		if q.Name == name {
			p = q
		}
	}
	require.Equal(t, name, p.Name)
	var b map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &b))
	Normalize(b, append(DefaultNormalizeRules(), p.Normalize...))
	out, err := json.Marshal(b)
	require.NoError(t, err)
	return string(out)
}

// TestOwnerSteveNamespacesRecordNoProbeFixture asserts that the owner's
// namespace collection records the same with and without a namespace a
// probe created, with the count of the namespaces kept.
func TestOwnerSteveNamespacesRecordNoProbeFixture(t *testing.T) {
	t.Parallel()
	ns := func(name string) string {
		return `{"id":"` + name + `","type":"namespace","metadata":{"name":"` + name + `"},"spec":{}}`
	}
	clean := `{"type":"collection","revision":"9","count":2,"data":[` + ns("default") + `,` + ns("kube-system") + `]}`
	withFixture := `{"type":"collection","revision":"12","count":3,"data":[` + ns("default") + `,` + ns("e2e-fid-explain") + `,` + ns("kube-system") + `]}`
	got := normalizedFor(t, "access-downstream/owner-steve", withFixture)
	assert.Equal(t, normalizedFor(t, "access-downstream/owner-steve", clean), got)
	assert.Contains(t, got, `"count":2`)
	assert.NotContains(t, got, "e2e-fid-")
}

// TestOwnerProjectsRecordTheSameOnEveryBoot asserts that the owner's
// steve project collection records the same for other generated project names,
// in another order, and with a project a probe created.
func TestOwnerProjectsRecordTheSameOnEveryBoot(t *testing.T) {
	t.Parallel()
	project := func(name, display string) string {
		return `{"id":"c-m-test/` + name + `","type":"management.cattle.io.project","metadata":{"name":"` + name +
			`","namespace":"c-m-test"},"spec":{"displayName":"` + display + `"}}`
	}
	boot1 := `{"type":"collection","revision":"9","count":2,"data":[` + project("p-abcde", "Default") + `,` + project("p-fghij", "System") + `]}`
	boot2 := `{"type":"collection","revision":"14","count":3,"data":[` + project("p-e2e-fid-explain", "e2e-fid-explain") + `,` +
		project("p-zzzzz", "System") + `,` + project("p-kkkkk", "Default") + `]}`
	got := normalizedFor(t, "access-downstream/owner-management-gate", boot2)
	assert.Equal(t, normalizedFor(t, "access-downstream/owner-management-gate", boot1), got)
	assert.Contains(t, got, `"count":2`)
	assert.NotContains(t, got, "p-")
}

// TestAdminProjectsGoldenIgnoresAProbeProject asserts that the recorded
// admin-steve-projects-by-cluster projects body normalizes back to itself,
// and to the same body with a project another probe created added.
func TestAdminProjectsGoldenIgnoresAProbeProject(t *testing.T) {
	t.Parallel()
	const name = "access-downstream/admin-steve-projects-by-cluster"
	g, err := LoadGolden(GoldenPath(filepath.Join("testdata", "downstream"), name))
	require.NoError(t, err)
	i := slices.IndexFunc(g.Observations, func(o ObservationResult) bool { return o.Name == "projects" })
	require.GreaterOrEqual(t, i, 0)
	want, err := json.Marshal(g.Observations[i].Body)
	require.NoError(t, err)

	var withFixture map[string]any
	require.NoError(t, json.Unmarshal(want, &withFixture))
	withFixture["count"] = withFixture["count"].(float64) + 1
	withFixture["data"] = append(withFixture["data"].([]any), map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Project", "type": "management.cattle.io.project",
		"id": "{downstream}/p-e2e-fid-explain", "metadata": map[string]any{"name": "p-e2e-fid-explain", "namespace": "{downstream}"},
		"spec": map[string]any{"clusterName": "{downstream}", "displayName": "e2e-fid-explain"}})
	fixtureBody, err := json.Marshal(withFixture)
	require.NoError(t, err)

	assert.JSONEq(t, string(want), normalizedFor(t, name, string(want)), "the golden normalizes to itself")
	assert.JSONEq(t, string(want), normalizedFor(t, name, string(fixtureBody)), "a probe's project is not recorded")
}
