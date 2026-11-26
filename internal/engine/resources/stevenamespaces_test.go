package resources

import (
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const steveNamespacesPath = "/k8s/clusters/c-abc/v1/namespaces"

// refusedListWarning is the warning of a list read through Steve with no
// --project: the Kubernetes API refused the list of every namespace.
var refusedListWarning = "the Kubernetes API refused the list of every namespace (403); showing the namespaces the Steve API lists for you; " +
	"list the projects you can see with " + clierr.Rancher + " access projects --cluster 'c-abc'"

// steveNamespaceFull is a namespace of a project as Steve returns it to the
// caller who may get it: the whole object plus what Steve adds.
func steveNamespaceFull(name, project string, labels map[string]any) map[string]any {
	meta := map[string]any{
		"name": name, "resourceVersion": "7", "creationTimestamp": "2026-11-24T09:00:00Z", "uid": "u-" + name,
		"fields": []any{name, "Active", "3h"}, "relationships": []any{}, "state": map[string]any{"name": "active"},
		"labels":      map[string]any{"kubernetes.io/metadata.name": name, "field.cattle.io/projectId": project},
		"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:" + project},
	}
	maps.Copy(meta["labels"].(map[string]any), labels)
	return map[string]any{"id": name, "type": "namespace", "apiVersion": "v1", "kind": "Namespace", "metadata": meta,
		"spec": map[string]any{"finalizers": []any{"kubernetes"}}, "status": map[string]any{"phase": "Active"},
		"links": map[string]any{"self": "https://rancher.invalid/self"}}
}

// steveNamespaceFewFields is a namespace Steve returns for a caller who cannot
// get it: the name, the resourceVersion, the phase, the project annotation and
// the label fleet.cattle.io/managed, with no uid, creation time, name label or
// fields.
func steveNamespaceFewFields(name, project string) map[string]any {
	meta := map[string]any{"name": name, "resourceVersion": "7", "labels": map[string]any{"fleet.cattle.io/managed": "true"}}
	if project != "" {
		meta["annotations"] = map[string]any{"field.cattle.io/projectId": "c-abc:" + project}
	}
	return map[string]any{"id": name, "type": "namespace", "apiVersion": "v1", "kind": "Namespace", "metadata": meta,
		"spec": map[string]any{}, "status": map[string]any{"phase": "Active"}}
}

// projectOwnerNamespaces is what Steve lists for a project owner: two
// namespaces of project p-1, one of project p-2, one of p-1 with a few fields
// only and one with a few fields only that belongs to no project.
func projectOwnerNamespaces() []map[string]any {
	return []map[string]any{
		steveNamespaceFull("team-a", "p-1", map[string]any{"team": "a"}),
		steveNamespaceFull("team-b", "p-1", map[string]any{"team": "b"}),
		steveNamespaceFull("other", "p-2", nil),
		steveNamespaceFewFields("p1-seen", "p-1"),
		steveNamespaceFewFields("seen-only", ""),
	}
}

// ownerFixture is the client and factory of a caller whom the Kubernetes API
// refuses every namespace read with kubernetesErr and to whom the Steve API
// responds as serve says.
func ownerFixture(kubernetesErr error, serve func(path string, n int) (int, any)) (*refusedClient, *steveFactory) {
	client := &refusedClient{getErr: kubernetesErr, tableErr: kubernetesErr, listErr: kubernetesErr}
	return client, &steveFactory{client: client, root: &steveRoot{serve: serve}}
}

// steveNamespacesAnswer lists items and serves each by name, as Steve does: a
// namespace it lists with a few fields only (one steve.ReadableInFull refuses)
// is 403 by name.
func steveNamespacesAnswer(items ...map[string]any) func(string, int) (int, any) {
	return func(path string, _ int) (int, any) {
		if path == steveNamespacesPath {
			return http.StatusOK, steveCollection(items...)
		}
		for _, item := range items {
			if path == steveNamespacesPath+"/"+item["id"].(string) {
				if !steve.ReadableInFull(&unstructured.Unstructured{Object: item}) {
					return http.StatusForbidden, nil
				}
				return http.StatusOK, item
			}
		}
		return http.StatusNotFound, nil
	}
}

// runNamespaces runs `get namespace` as a text table or as objects, with
// --cluster c-abc and the flags in s, in the context of project p-1 when
// project is set.
func runNamespaces(t *testing.T, f engine.ClusterClientFactory, textTable, project bool, s map[string]string, args ...string) (runtime.Result, error) {
	t.Helper()
	flags := map[string]string{"cluster": "c-abc"}
	maps.Copy(flags, s)
	ctx := t.Context()
	if project {
		ctx, _ = projectContext(t)
		flags["project"] = "p-1"
	}
	return getCommand(t, "namespace", f).Execute(ctx, tableInput{args: args, s: flags, textTable: textTable}, nil)
}

// objectNames lists the names of the objects of a result read as objects.
func objectNames(t *testing.T, res runtime.Result) []string {
	t.Helper()
	var out []string
	for _, item := range decodeList(t, res) {
		out = append(out, item["metadata"].(map[string]any)["name"].(string))
	}
	return out
}

// tableNames lists the first cell of each row of a result read as a text table.
func tableNames(t *testing.T, res runtime.Result) []string {
	t.Helper()
	st, ok := res.Data.(serverTable)
	require.True(t, ok, "got %T", res.Data)
	var out []string
	for _, row := range st.Rows() {
		out = append(out, row[0])
	}
	return out
}

// TestGetNamespacesAProjectOwnerReadsThroughSteve asserts, for the object
// output and the text table, with and without --project and --selector, the
// namespaces a get returns after the Kubernetes API refused the list and the
// warnings it carries: the refusal and the command that lists the projects,
// unless --project was typed; a count of the namespaces shown with a few fields
// only; and, under --selector, a count of the ones left out.
func TestGetNamespacesAProjectOwnerReadsThroughSteve(t *testing.T) {
	t.Parallel()
	const (
		shown   = "1 namespace is shown with only the fields the Steve API returns for a namespace you have no permission to get"
		shown2  = "2 namespaces are shown with only the fields the Steve API returns for a namespace you have no permission to get"
		left    = "1 namespace is left out of the list: the Steve API returns only a few fields for a namespace you have no permission to get, and --selector needs its labels"
		left2   = "2 namespaces are left out of the list: the Steve API returns only a few fields for a namespace you have no permission to get, and --selector needs their labels"
		columns = "NAME STATUS AGE"
	)
	tests := []struct {
		name     string
		project  bool
		selector string
		names    []string
		warnings []string
	}{
		{name: "every namespace", names: []string{"other", "p1-seen", "seen-only", "team-a", "team-b"}, warnings: []string{refusedListWarning, shown2}},
		{name: "a project", project: true, names: []string{"p1-seen", "team-a", "team-b"}, warnings: []string{shown}},
		{name: "a selector", selector: "team=a", names: []string{"team-a"}, warnings: []string{refusedListWarning, left2}},
		{name: "a selector no namespace with a few fields only could match", selector: "!team", names: []string{"other"}, warnings: []string{refusedListWarning, left2}},
		{name: "a project and a selector", project: true, selector: "team in (a,b)", names: []string{"team-a", "team-b"}, warnings: []string{left}},
	}
	for _, tt := range tests {
		for _, textTable := range []bool{false, true} {
			form := "objects"
			if textTable {
				form = "text table"
			}
			t.Run(tt.name+", "+form, func(t *testing.T) {
				t.Parallel()
				denied := kubernetesStatus(http.StatusForbidden, false)
				client, f := ownerFixture(denied, steveNamespacesAnswer(projectOwnerNamespaces()...))
				s := map[string]string{}
				if tt.selector != "" {
					s["selector"] = tt.selector
				}
				res, err := runNamespaces(t, f, textTable, tt.project, s)
				require.NoError(t, err)
				assert.Equal(t, tt.warnings, res.Warnings)
				assert.Equal(t, []string{steveNamespacesPath}, f.root.requested(), "one Steve page, and no selector or query")
				if textTable {
					assert.Equal(t, tt.names, tableNames(t, res))
					assert.Equal(t, 1, client.listTableCalls())
					st := res.Data.(serverTable)
					assert.Equal(t, columns, joinCells(st.Columns()))
					for _, row := range st.Rows() {
						assert.Equal(t, "Active", row[1], row[0])
					}
					return
				}
				assert.Equal(t, tt.names, objectNames(t, res))
				assert.Equal(t, 1, client.listCalls)
				for _, item := range decodeList(t, res) {
					assertKubernetesShape(t, item)
				}
			})
		}
	}
}

func joinCells(cells []string) string {
	out := ""
	for i, c := range cells {
		if i > 0 {
			out += " "
		}
		out += c
	}
	return out
}

// TestGetNamespacesTextTableCellsComeFromTheObjects asserts the AGE cell of a
// namespace read through Steve is the age of its creation time, and blank for a
// namespace with no creation time.
func TestGetNamespacesTextTableCellsComeFromTheObjects(t *testing.T) {
	t.Parallel()
	_, f := ownerFixture(kubernetesStatus(http.StatusForbidden, false),
		steveNamespacesAnswer(steveNamespaceFull("team-a", "p-1", nil), steveNamespaceFewFields("seen-only", "")))
	res, err := runNamespaces(t, f, true, false, nil)
	require.NoError(t, err)
	rows := res.Data.(serverTable).Rows()
	require.Len(t, rows, 2)
	assert.Equal(t, "seen-only", rows[0][0])
	assert.Empty(t, rows[0][2], "no creation time, no age")
	assert.Equal(t, "team-a", rows[1][0])
	assert.NotEmpty(t, rows[1][2])
}

// TestGetNamespacesJSONKeepsWhatSteveReturnedForANamespaceWithAFewFields
// asserts the objects of the list are in the Kubernetes shape and a
// namespace with a few fields only keeps the fields Steve returned, with none
// added.
func TestGetNamespacesJSONKeepsWhatSteveReturnedForANamespaceWithAFewFields(t *testing.T) {
	t.Parallel()
	_, f := ownerFixture(kubernetesStatus(http.StatusForbidden, false), steveNamespacesAnswer(steveNamespaceFewFields("seen-only", "")))
	res, err := runNamespaces(t, f, false, false, nil)
	require.NoError(t, err)
	items := decodeList(t, res)
	require.Len(t, items, 1)
	meta := items[0]["metadata"].(map[string]any)
	assert.Equal(t, map[string]any{"name": "seen-only", "resourceVersion": "7", "labels": map[string]any{"fleet.cattle.io/managed": "true"}}, meta)
	assert.Equal(t, map[string]any{"phase": "Active"}, items[0]["status"])
}

// TestGetNamespacesAnEmptySteveListIsNotAnError asserts a caller to whom Steve
// lists no namespace gets an empty list with the refusal warning, and under
// --project an empty list with no warning. Neither body has a count key, as
// Steve sends none for an empty list.
func TestGetNamespacesAnEmptySteveListIsNotAnError(t *testing.T) {
	t.Parallel()
	bodies := map[string]any{
		"a recorded empty list": recordedSteveBody(t, "standard-steve-empty.json", "clusters"),
		"the list of a cluster member with no project": map[string]any{
			"type": "collection", "links": map[string]any{"self": "https://rancher.invalid/k8s/clusters/c-abc/v1/namespaces"},
			"actions": map[string]any{}, "resourceType": "namespace", "revision": "1639249", "data": []any{},
		},
	}
	for name, empty := range bodies {
		for _, textTable := range []bool{false, true} {
			_, f := ownerFixture(kubernetesStatus(http.StatusForbidden, false), func(string, int) (int, any) { return http.StatusOK, empty })
			res, err := runNamespaces(t, f, textTable, false, nil)
			require.NoError(t, err, name)
			assert.Equal(t, []string{refusedListWarning}, res.Warnings, name)
			if textTable {
				assert.Empty(t, tableNames(t, res), name)
			} else {
				assert.Empty(t, decodeList(t, res), name)
			}

			res, err = runNamespaces(t, f, textTable, true, nil)
			require.NoError(t, err, name)
			assert.Empty(t, res.Warnings, name)
		}
	}
}

// TestGetNamespaceOneReadsThroughSteveAfterAForbiddenGet asserts a get of one
// namespace the Kubernetes API refuses reads that namespace through Steve, as
// objects and as a text table, with no warning, and that a namespace Steve lists
// with a few fields only, which Steve refuses by name, returns the Kubernetes
// API's error.
func TestGetNamespaceOneReadsThroughSteveAfterAForbiddenGet(t *testing.T) {
	t.Parallel()
	for _, textTable := range []bool{false, true} {
		denied := kubernetesStatus(http.StatusForbidden, false)
		_, f := ownerFixture(denied, steveNamespacesAnswer(projectOwnerNamespaces()...))
		res, err := runNamespaces(t, f, textTable, false, nil, "team-a")
		require.NoError(t, err)
		assert.Empty(t, res.Warnings)
		assert.Equal(t, []string{steveNamespacesPath + "/team-a"}, f.root.requested())
		if textTable {
			assert.Equal(t, []string{"team-a"}, tableNames(t, res))
		} else {
			assert.Equal(t, "team-a", decodeOne(t, res)["metadata"].(map[string]any)["name"])
		}

		_, f = ownerFixture(denied, steveNamespacesAnswer(projectOwnerNamespaces()...))
		_, err = runNamespaces(t, f, textTable, false, nil, "seen-only")
		assert.Equal(t, error(denied), err, "text table %t", textTable)
		assert.Equal(t, []string{steveNamespacesPath + "/seen-only"}, f.root.requested())
	}
}

// TestGetNamespacesKeepTheKubernetesErrorWhenSteveRefuses asserts that when
// both APIs refuse, a list and a get of one namespace, as objects and as a
// text table, return the Kubernetes API's error and no result.
func TestGetNamespacesKeepTheKubernetesErrorWhenSteveRefuses(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		for _, textTable := range []bool{false, true} {
			denied := kubernetesStatus(http.StatusForbidden, true)
			_, f := ownerFixture(denied, func(string, int) (int, any) { return status, nil })
			_, err := runNamespaces(t, f, textTable, false, nil)
			assert.Equal(t, error(denied), err, "list, steve %d, text table %t", status, textTable)
			_, err = runNamespaces(t, f, textTable, false, nil, "team-a")
			assert.Equal(t, error(denied), err, "one, steve %d, text table %t", status, textTable)
		}
	}
}

// TestGetNamespacesReadSteveOnlyForAKubernetes403 asserts a Kubernetes 401,
// 404 or 500 on the list or on the get of one namespace, as objects and as a
// text table, is returned unchanged and sends nothing to Steve.
func TestGetNamespacesReadSteveOnlyForAKubernetes403(t *testing.T) {
	t.Parallel()
	for _, code := range []int32{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		for _, textTable := range []bool{false, true} {
			failure := kubernetesStatus(code, false)
			_, f := ownerFixture(failure, steveNamespacesAnswer(projectOwnerNamespaces()...))
			_, err := runNamespaces(t, f, textTable, false, nil)
			assert.Equal(t, error(failure), err, "list %d, text table %t", code, textTable)
			_, err = runNamespaces(t, f, textTable, false, nil, "team-a")
			assert.Equal(t, error(failure), err, "one %d, text table %t", code, textTable)
			assert.Zero(t, f.rootCalls, "status %d, text table %t", code, textTable)
		}
	}
}

// TestGetNamespacesAnAdministratorReadsKubernetesOnly asserts a caller to whom
// the Kubernetes API responds gets that response, as objects and as a text
// table, with and without --selector, and as objects with --project, with none
// of the warnings of a read through Steve, and that no request goes to Steve.
func TestGetNamespacesAnAdministratorReadsKubernetesOnly(t *testing.T) {
	t.Parallel()
	objs := []unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": "default",
		"annotations": map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}}}}}
	tests := []struct {
		name      string
		textTable bool
		project   bool
		selector  string
	}{
		{name: "objects"},
		{name: "text table", textTable: true},
		{name: "objects with a selector", selector: "team=a"},
		{name: "text table with a selector", textTable: true, selector: "team=a"},
		{name: "objects of a project", project: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &memClient{items: objs, table: engine.TableAnswer{Table: tableFixture(t, "pods")}}
			f := &steveFactory{client: client, root: &steveRoot{serve: func(string, int) (int, any) { return http.StatusInternalServerError, nil }}}
			s := map[string]string{}
			if tt.selector != "" {
				s["selector"] = tt.selector
			}
			res, err := runNamespaces(t, f, tt.textTable, tt.project, s)
			require.NoError(t, err)
			assert.Empty(t, res.Warnings)
			if !tt.project {
				_, err = runNamespaces(t, f, tt.textTable, false, nil, "default")
				require.NoError(t, err)
			}
			assert.Zero(t, f.rootCalls)
		})
	}
}

// TestGetWaitOnANamespaceReadsItThroughSteve asserts a wait on a namespace the
// Kubernetes API refuses polls the Steve object by name until the condition
// holds, asks the Kubernetes API for the object once, shows the namespace as
// objects or as a text table built from the Steve object, with no warning, and
// sends no table request after the refusal.
// Not parallel: withFastPoll sets the poll interval.
func TestGetWaitOnANamespaceReadsItThroughSteve(t *testing.T) {
	withFastPoll(t)
	for _, textTable := range []bool{false, true} {
		denied := kubernetesStatus(http.StatusForbidden, true)
		client := &refusedClient{getErr: denied, tableErr: denied, listErr: denied}
		serve := func(path string, count int) (int, any) {
			if path != steveNamespacesPath+"/team-a" {
				return http.StatusNotFound, nil
			}
			ready := "False"
			if count >= 3 {
				ready = "True"
			}
			item := steveNamespaceFull("team-a", "p-1", nil)
			item["status"] = map[string]any{"phase": "Active", "conditions": []any{map[string]any{"type": "Ready", "status": ready}}}
			return http.StatusOK, item
		}
		f := &steveFactory{client: client, root: &steveRoot{serve: serve}}
		in := tableInput{input: waitInput([]string{"team-a"}, map[string]string{"cluster": "c-abc", "for": "condition=Ready"}), textTable: textTable}
		res, err := getCommand(t, "namespace", f).Execute(t.Context(), in, nil)
		require.NoError(t, err, "text table %t", textTable)
		assert.Empty(t, res.Warnings)
		assert.NotEmpty(t, res.Summary)
		requested := f.root.requested()
		assert.GreaterOrEqual(t, len(requested), 3)
		for _, p := range requested {
			assert.Equal(t, steveNamespacesPath+"/team-a", p)
		}
		assert.Equal(t, 1, client.getCalls, "after the first 403 only Steve is asked")
		assert.Empty(t, client.getTableNS, "no table is asked for after the refusal")
		if textTable {
			assert.Equal(t, []string{"team-a"}, tableNames(t, res))
		} else {
			assert.Equal(t, "team-a", decodeOne(t, res)["metadata"].(map[string]any)["name"])
		}
	}
}

// TestNamespaceTableAgeIsTheAgeOfTheOtherTablesOfGet asserts the AGE cell of the
// table built from Steve's objects is the cell ageCell gives the same object: a
// valid time, no time, a time that is not RFC 3339 and a time in the future.
func TestNamespaceTableAgeIsTheAgeOfTheOtherTablesOfGet(t *testing.T) {
	t.Parallel()
	build, ok := kinds.SteveTable("namespace")
	require.True(t, ok)
	at := now()
	for name, created := range map[string]any{
		"minutes":    at.Add(-90 * time.Minute).UTC().Format(time.RFC3339),
		"days":       at.Add(-50 * time.Hour).UTC().Format(time.RFC3339),
		"the future": at.Add(3 * time.Hour).UTC().Format(time.RFC3339),
		"not a time": "yesterday",
		"empty":      "",
		"missing":    nil,
	} {
		meta := map[string]any{"name": "x"}
		if created != nil {
			meta["creationTimestamp"] = created
		}
		obj := unstructured.Unstructured{Object: map[string]any{"metadata": meta}}
		table := build([]unstructured.Unstructured{obj}, at)
		require.Len(t, table.Rows, 1, name)
		assert.Equal(t, ageCell(obj.Object), table.Rows[0].Cells[2], name)
	}
}
