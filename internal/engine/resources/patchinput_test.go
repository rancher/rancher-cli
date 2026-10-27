package resources

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

const configMapJSON = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app","namespace":"default","resourceVersion":"7","labels":{"tier":"web"}},"data":{"a":"1","b":"2"}}`

// planTyped plans `patch <kind> <name>` against obj with the inline patch p
// and --type typ (unset when empty), and returns the client so a test can
// read the requests.
func planTyped(t *testing.T, kind, name, obj, p, typ string) (safety.Effect, *patchClient, error) {
	t.Helper()
	client := &patchClient{obj: objFromJSON(t, obj)}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	s := map[string]string{"cluster": "prod", "namespace": "default", "patch": p}
	if typ != "" {
		s["type"] = typ
	}
	eff, _, err := planPatch(t, NewPatch(mustKind(t, kind), fac, nil), input{args: []string{name}, s: s})
	return eff, client, err
}

func diffAt(eff safety.Effect, path string) (safety.DiffEntry, bool) {
	for _, d := range eff.Diff {
		if d.Path == path {
			return d, true
		}
	}
	return safety.DiffEntry{}, false
}

// TestPatchMergeAppliesRFC7386 asserts that --type merge adds and replaces
// keys, removes a key given null, merges nested objects, and updates with
// the fetched resourceVersion as the precondition.
func TestPatchMergeAppliesRFC7386(t *testing.T) {
	t.Parallel()
	eff, client, err := planTyped(t, "configmap", "app", configMapJSON,
		`{"data":{"a":"9","b":null,"c":"3"},"metadata":{"labels":{"app":"x"}}}`, "merge")
	require.NoError(t, err)

	d, ok := diffAt(eff, "/data/a")
	require.True(t, ok, "diff: %+v", eff.Diff)
	assert.Equal(t, "9", d.To)
	d, ok = diffAt(eff, "/data/b")
	require.True(t, ok)
	assert.Nil(t, d.To, "null removes the key")
	_, ok = diffAt(eff, "/data/c")
	assert.True(t, ok)
	_, ok = diffAt(eff, "/metadata/labels/app")
	assert.True(t, ok)
	_, ok = diffAt(eff, "/metadata/labels/tier")
	assert.False(t, ok, "a nested object merges; tier stays")

	require.Len(t, client.updates, 1)
	assert.Equal(t, "7", client.updates[0].obj.GetResourceVersion())
	assert.Equal(t, "web", client.updates[0].obj.GetLabels()["tier"])
}

// TestPatchMergeEmptyObjectChangesNothing asserts that {} plans with an
// empty diff.
func TestPatchMergeEmptyObjectChangesNothing(t *testing.T) {
	t.Parallel()
	eff, _, err := planTyped(t, "configmap", "app", configMapJSON, `{}`, "merge")
	require.NoError(t, err)
	assert.Empty(t, eff.Diff)
}

// TestPatchJSONKeepsEmptyPatches asserts that null and [] are still an
// empty json patch.
func TestPatchJSONKeepsEmptyPatches(t *testing.T) {
	t.Parallel()
	for _, p := range []string{`null`, `[]`} {
		eff, _, err := planTyped(t, "configmap", "app", configMapJSON, p, "")
		require.NoError(t, err, p)
		assert.Empty(t, eff.Diff, p)
	}
}

// TestPatchInputRefusals asserts each plain reason, under each type, and
// that no refusal sends a request.
func TestPatchInputRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, typ, patch, want string
	}{
		{"one op as an object", "", `{"op":"add","path":"/data/c","value":"3"}`, "invalid patch: the patch is one RFC 6902 operation; put it inside a JSON array"},
		{"an object without --type", "", `{"data":{"a":"9"}}`, "invalid patch: a JSON object is a merge patch; pass --type merge, or give RFC 6902 operations as a JSON array"},
		{"an object with --type json", "json", `{}`, "invalid patch: a JSON object is a merge patch; pass --type merge"},
		{"a string", "", `"x"`, "invalid patch: a JSON string is not a patch; give RFC 6902 operations as a JSON array"},
		{"a number", "", `42`, "invalid patch: a JSON number is not a patch"},
		{"a negative number", "", `-1`, "invalid patch: a JSON number is not a patch"},
		{"an object after whitespace", "", " \n {\"data\":{}}", "invalid patch: a JSON object is a merge patch"},
		{"an array after whitespace in a merge patch", "merge", "\n\t[]", "invalid patch: a merge patch is a JSON object, not a JSON array"},
		{"a negative number in a merge patch", "merge", `-2`, "invalid patch: a merge patch is a JSON object, not a JSON number"},
		{"a boolean", "", `true`, "invalid patch: a JSON boolean is not a patch"},
		{"an element that is not an object", "", `[{"op":"remove","path":"/data/a"},1]`, `invalid patch: patch operation 1 is a JSON number, not an object with "op" and "path"`},
		{"a path that is not a string", "", `[{"op":"remove","path":1}]`, `invalid patch: patch operation 0 has a "path" that is not a string`},
		{"an op that is not a string", "", `[{"op":true,"path":"/a"}]`, `invalid patch: patch operation 0 has an "op" that is not a string`},
		{"a from that is not a string", "", `[{"op":"move","from":[],"path":"/a"}]`, `invalid patch: patch operation 0 has a "from" that is not a string`},
		{"an element that is a string", "", `["x"]`, `invalid patch: patch operation 0 is a JSON string, not an object with "op" and "path"`},
		{"an element that is an array", "", `[[]]`, `invalid patch: patch operation 0 is a JSON array, not an object with "op" and "path"`},
		{"an element that is a boolean", "", `[false]`, `invalid patch: patch operation 0 is a JSON boolean, not an object with "op" and "path"`},
		{"an element that is null", "", `[null]`, `invalid patch: patch operation 0 is a JSON null, not an object with "op" and "path"`},
		{"a from that is not a string on add", "", `[{"op":"add","path":"/a","value":1,"from":2}]`, `invalid patch: patch operation 0 has a "from" that is not a string`},
		{"no op", "", `[{"path":"/data/a"}]`, `invalid patch: patch operation 0 has no "op"`},
		{"no path", "", `[{"op":"remove"}]`, `invalid patch: patch operation 0 has no "path"`},
		{"add without value", "", `[{"op":"add","path":"/data/c"}]`, `invalid patch: patch operation 0 (add) has no "value"`},
		{"replace without value", "", `[{"op":"remove","path":"/data/a"},{"op":"replace","path":"/data/b"}]`, `invalid patch: patch operation 1 (replace) has no "value"`},
		{"test without value", "", `[{"op":"test","path":"/data/a"}]`, `invalid patch: patch operation 0 (test) has no "value"`},
		{"move without from", "", `[{"op":"move","path":"/data/c"}]`, `invalid patch: patch operation 0 (move) has no "from"`},
		{"copy without from", "", `[{"op":"copy","path":"/data/c"}]`, `invalid patch: patch operation 0 (copy) has no "from"`},
		{"an unknown op", "", `[{"op":"bogus","path":"/data/a"}]`, `invalid patch: patch operation 0 has the unknown operation "bogus"; use add, remove, replace, move, copy or test`},
		{"a BOM-only file", "", "\xef\xbb\xbf", "invalid patch: the patch is empty"},
		{"blank", "", " \n\t", "invalid patch: the patch is empty"},
		{"blank merge", "merge", " ", "invalid patch: the patch is empty"},
		{"cut off", "", `[{"op":"add"`, "invalid patch: not valid JSON at byte 12; the patch must be JSON (YAML is not accepted)"},
		{"YAML", "merge", "data:\n  a: \"9\"\n", "invalid patch: not valid JSON at byte 1; the patch must be JSON (YAML is not accepted)"},
		{"trailing data", "", `[] []`, "invalid patch: not valid JSON at byte 4;"},
		{"a repeated key in a merge patch", "merge", `{"data":{"a":"1"},"data":{"b":"2"}}`, `invalid patch: the key "data" appears twice in one JSON object; name each key once`},
		{"a repeated key in an op", "", `[{"op":"add","path":"/data/c","value":"1","value":"2"}]`, `invalid patch: the key "value" appears twice in one JSON object; name each key once`},
		{"a merge array", "merge", `[{"op":"add","path":"/a","value":1}]`, "invalid patch: a merge patch is a JSON object, not a JSON array; for RFC 6902 operations in a JSON array leave out --type or pass --type json"},
		{"a merge string", "merge", `"x"`, "invalid patch: a merge patch is a JSON object, not a JSON string"},
		{"a merge number", "merge", `1`, "invalid patch: a merge patch is a JSON object, not a JSON number"},
		{"a merge boolean", "merge", `false`, "invalid patch: a merge patch is a JSON object, not a JSON boolean"},
		{"a merge null", "merge", `null`, "invalid patch: a merge patch is a JSON object, not a JSON null"},
		{"a merge resourceVersion", "merge", `{"metadata":{"resourceVersion":"9"}}`, "the merge patch targets /metadata/resourceVersion; the server sets this field and a patch cannot change it"},
		{"a merge managedFields removal", "merge", `{"metadata":{"managedFields":null}}`, "the merge patch targets /metadata/managedFields; the server sets this field and a patch cannot change it"},
		{"a merge resourceVersion removal", "merge", `{"metadata":{"resourceVersion":null}}`, "the merge patch targets /metadata/resourceVersion;"},
		{"a merge naming both server fields", "merge", `{"metadata":{"managedFields":[],"resourceVersion":"1"}}`, "the merge patch targets /metadata/resourceVersion;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, client, err := planTyped(t, "configmap", "app", configMapJSON, tc.patch, tc.typ)
			requireValidation(t, err)
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "Go value", "no decoder text")
			assert.Empty(t, client.getName, "refused before any request")
		})
	}
}

// TestPatchInputTrimsByteOrderMark asserts that a UTF-8 byte-order mark at
// the start of a patch is removed before the patch is read.
func TestPatchInputTrimsByteOrderMark(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ typ, patch string }{
		{"", "\xef\xbb\xbf" + `[{"op":"replace","path":"/data/a","value":"9"}]`},
		{"merge", "\xef\xbb\xbf" + `{"data":{"a":"9"}}`},
	} {
		eff, _, err := planTyped(t, "configmap", "app", configMapJSON, tc.patch, tc.typ)
		require.NoError(t, err, tc.typ)
		d, ok := diffAt(eff, "/data/a")
		require.True(t, ok)
		assert.Equal(t, "9", d.To)
	}
}

// TestPatchMergeSendsTheFetchedServerFields asserts that a merge patch
// sends the fetched resourceVersion and managed fields, as fetched, in the
// update.
func TestPatchMergeSendsTheFetchedServerFields(t *testing.T) {
	t.Parallel()
	obj := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app","namespace":"default","resourceVersion":"7","managedFields":[{"manager":"kubectl","operation":"Update"}]},"data":{"a":"1"}}`
	_, client, err := planTyped(t, "configmap", "app", obj, `{"metadata":{"labels":{"x":"y"}}}`, "merge")
	require.NoError(t, err)
	require.Len(t, client.updates, 1)
	sent := client.updates[0].obj
	assert.Equal(t, "7", sent.GetResourceVersion())
	require.Len(t, sent.GetManagedFields(), 1)
	assert.Equal(t, "kubectl", sent.GetManagedFields()[0].Manager)
}

// TestPatchRefusesAnObjectWithoutName asserts that a patch that removes the
// object's name or metadata is VALIDATION naming metadata.name, under
// both types, before the update.
func TestPatchRefusesAnObjectWithoutName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ typ, patch string }{
		{"merge", `{"metadata":null}`},
		{"merge", `{"metadata":"x"}`},
		{"merge", `{"metadata":{"name":null}}`},
		{"", `[{"op":"remove","path":"/metadata"}]`},
		{"", `[{"op":"remove","path":"/metadata/name"}]`},
		{"", `[{"op":"replace","path":"/metadata/name","value":""}]`},
	} {
		_, client, err := planTyped(t, "configmap", "app", configMapJSON, tc.patch, tc.typ)
		requireValidation(t, err)
		require.ErrorContains(t, err, "patched object is not valid: the patched object has no name; a patch must keep metadata.name", tc.patch)
		assert.Empty(t, client.updates, tc.patch)
	}
}

// TestPatchedObjectErrorsCarryNoData asserts that a patch that leaves the
// object without kind, or replaces it with a non-object, is refused with a
// text that holds none of the object's data, for a Secret under both types.
func TestPatchedObjectErrorsCarryNoData(t *testing.T) {
	t.Parallel()
	secret := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"default","resourceVersion":"5"},"data":{"password":"c2VjcmV0LXZhbHVl"}}`
	for _, tc := range []struct{ typ, patch, want string }{
		{"", `[{"op":"remove","path":"/kind"}]`, "patched object is not valid: the patched object has no kind; a patch must keep kind"},
		{"merge", `{"kind":null}`, "patched object is not valid: the patched object has no kind; a patch must keep kind"},
		{"", `[{"op":"replace","path":"","value":[1]}]`, "patched object is not valid: the patch replaces the object with a JSON array; the object must stay a JSON object"},
	} {
		_, client, err := planTyped(t, "secret", "db", secret, tc.patch, tc.typ)
		requireValidation(t, err)
		require.ErrorContains(t, err, tc.want)
		assert.NotContains(t, err.Error(), "c2VjcmV0LXZhbHVl", "the Secret's data never reaches the error")
		assert.Empty(t, client.updates)
	}
}

// TestPatchMergeSecretDiffIsRedacted asserts that a merge patch of a
// Secret's data shows paths only: each side that has a value reads
// <redacted>, and a side the patch adds or removes stays null.
func TestPatchMergeSecretDiffIsRedacted(t *testing.T) {
	t.Parallel()
	secret := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"default","resourceVersion":"5"},"data":{"password":"b2xk"}}`
	for _, tc := range []struct {
		patch    string
		wantPath string
		from, to any
	}{
		{`{"data":null}`, "/data", "<redacted>", nil},
		{`{"data":{"password":"bmV3"}}`, "/data/password", "<redacted>", "<redacted>"},
		{`{"stringData":{"k":"v"}}`, "/stringData", nil, "<redacted>"},
	} {
		eff, _, err := planTyped(t, "secret", "db", secret, tc.patch, "merge")
		require.NoError(t, err, tc.patch)
		require.Len(t, eff.Diff, 1, tc.patch)
		assert.Equal(t, safety.DiffEntry{Path: tc.wantPath, From: tc.from, To: tc.to}, eff.Diff[0], tc.patch)
	}
}

// TestPatchSecretDiffHidesTheLastAppliedAnnotation asserts a patch that adds
// or removes the whole annotations map of a Secret, whose last-applied
// annotation holds a copy of the applied object, shows that annotation as
// <redacted> and the other annotations as they are.
func TestPatchSecretDiffHidesTheLastAppliedAnnotation(t *testing.T) {
	t.Parallel()
	const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"
	withAnnotations := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"default","resourceVersion":"5","annotations":{"` + lastApplied + `":"copy-of-the-secret","owner":"team-a"}},"data":{"k":"b2xk"}}`
	eff, _, err := planTyped(t, "secret", "db", withAnnotations, `{"metadata":{"annotations":null}}`, "merge")
	require.NoError(t, err)
	d, ok := diffAt(eff, "/metadata/annotations")
	require.True(t, ok, "diff: %+v", eff.Diff)
	assert.Equal(t, map[string]any{lastApplied: "<redacted>", "owner": "team-a"}, d.From)
	assert.Nil(t, d.To)

	withoutAnnotations := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"default","resourceVersion":"5"},"data":{"k":"b2xk"}}`
	eff, _, err = planTyped(t, "secret", "db", withoutAnnotations,
		`{"metadata":{"annotations":{"`+lastApplied+`":"copy-of-the-secret","owner":"team-a"}}}`, "merge")
	require.NoError(t, err)
	d, ok = diffAt(eff, "/metadata/annotations")
	require.True(t, ok, "diff: %+v", eff.Diff)
	assert.Nil(t, d.From)
	assert.Equal(t, map[string]any{lastApplied: "<redacted>", "owner": "team-a"}, d.To)
}

// TestPatchMachineConfigDiffHidesItsSecretFields asserts a patch of a
// machine config shows the secret fields of its driver as <redacted> and the
// other fields as they are.
func TestPatchMachineConfigDiffHidesItsSecretFields(t *testing.T) {
	t.Parallel()
	obj := `{"apiVersion":"rke-machine-config.cattle.io/v1","kind":"Amazonec2Config","metadata":{"name":"aws","namespace":"default","resourceVersion":"5"},"region":"us-east-1","secretKey":"old-secret"}`
	eff, _, err := planTyped(t, "amazonec2config", "aws", obj,
		`{"region":"us-west-2","secretKey":"new-secret","SessionToken":"token"}`, "merge")
	require.NoError(t, err)
	for path, want := range map[string]safety.DiffEntry{
		"/region":       {Path: "/region", From: "us-east-1", To: "us-west-2"},
		"/secretKey":    {Path: "/secretKey", From: "<redacted>", To: "<redacted>"},
		"/SessionToken": {Path: "/SessionToken", To: "<redacted>"},
	} {
		d, ok := diffAt(eff, path)
		require.True(t, ok, "diff: %+v", eff.Diff)
		assert.Equal(t, want, d, path)
	}
}

// TestPatchMergeWarnsWhenListEntriesAreDropped asserts the warning for a
// merge patch that removes entries of a list the object has, and its absence
// for an identical list, an append, an empty or absent fetched list, a
// value of another type, a null, and a json patch.
func TestPatchMergeWarnsWhenListEntriesAreDropped(t *testing.T) {
	t.Parallel()
	deploy := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default","resourceVersion":"3"},"spec":{"replicas":1,"empty":[],"one":[1],"dup":[1,1],"ports":[{"port":80,"name":"http"}],"a/b":[1],"template":{"spec":{"containers":[{"name":"web","image":"v1"},{"name":"sidecar","image":"s1"}]}}}}`
	containers := "/spec/template/spec/containers"
	for _, tc := range []struct {
		name, typ, patch string
		want             []string
	}{
		{"two of two dropped", "merge", `{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"v2"}]}}}}`,
			[]string{"the merge patch removes 2 of 2 entries of the list at " + containers + "; a merge patch replaces a whole list and does not merge its entries"}},
		{"one of two kept", "merge", `{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"v1"},{"name":"sidecar","image":"s2"}]}}}}`,
			[]string{"the merge patch removes 1 of 2 entries of the list at " + containers + "; a merge patch replaces a whole list and does not merge its entries"}},
		{"the only entry dropped", "merge", `{"spec":{"one":[]}}`,
			[]string{"the merge patch removes the only entry of the list at /spec/one; a merge patch replaces a whole list and does not merge its entries"}},
		{"identical list of objects with integers", "merge", `{"spec":{"ports":[{"port":80,"name":"http"}]}}`, nil},
		{"an integer written as a float", "merge", `{"spec":{"ports":[{"port":80.0,"name":"http"}]}}`, nil},
		{"a list replaced by an object", "merge", `{"spec":{"ports":{"port":80}}}`,
			[]string{"the merge patch removes the only entry of the list at /spec/ports; a merge patch replaces a whole list and does not merge its entries"}},
		{"a list replaced by a value", "merge", `{"spec":{"template":{"spec":{"containers":"none"}}}}`,
			[]string{"the merge patch removes 2 of 2 entries of the list at " + containers + "; a merge patch replaces a whole list and does not merge its entries"}},
		{"two lists, in key order", "merge", `{"spec":{"one":[],"a/b":[]}}`,
			[]string{
				"the merge patch removes the only entry of the list at /spec/a~1b; a merge patch replaces a whole list and does not merge its entries",
				"the merge patch removes the only entry of the list at /spec/one; a merge patch replaces a whole list and does not merge its entries",
			}},
		{"an escaped key", "merge", `{"spec":{"a/b":[2]}}`,
			[]string{"the merge patch removes the only entry of the list at /spec/a~1b; a merge patch replaces a whole list and does not merge its entries"}},
		{"a duplicate entry dropped", "merge", `{"spec":{"dup":[1]}}`,
			[]string{"the merge patch removes 1 of 2 entries of the list at /spec/dup; a merge patch replaces a whole list and does not merge its entries"}},
		{"duplicates kept", "merge", `{"spec":{"dup":[1,1]}}`, nil},
		{"duplicates kept and one added", "merge", `{"spec":{"dup":[1,1,1]}}`, nil},
		{"identical list", "merge", `{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"v1"},{"name":"sidecar","image":"s1"}]}}}}`, nil},
		{"append", "merge", `{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"v1"},{"name":"sidecar","image":"s1"},{"name":"x","image":"x"}]}}}}`, nil},
		{"empty fetched list", "merge", `{"spec":{"empty":[1]}}`, nil},
		{"absent fetched path", "merge", `{"spec":{"tolerations":[{"key":"k"}]}}`, nil},
		{"another type at the path", "merge", `{"spec":{"replicas":[1]}}`, nil},
		{"null for a list", "merge", `{"spec":{"template":{"spec":{"containers":null}}}}`, nil},
		{"a json patch", "", `[{"op":"replace","path":"` + containers + `","value":[]}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eff, _, err := planTyped(t, "deployment", "web", deploy, tc.patch, tc.typ)
			require.NoError(t, err)
			var got []string
			for _, w := range eff.Warnings {
				if strings.HasPrefix(w, "the merge patch") {
					got = append(got, w)
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPatchMergeRunsEnrichers asserts that the per-kind enrichers see the
// merged object.
func TestPatchMergeRunsEnrichers(t *testing.T) {
	t.Parallel()
	client := &patchClient{obj: objFromJSON(t, configMapJSON)}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	var seen string
	enrich := func(_, patched *unstructured.Unstructured, eff *safety.Effect) {
		seen, _, _ = unstructured.NestedString(patched.Object, "data", "a")
		eff.Warnings = append(eff.Warnings, "enriched")
	}
	eff, _, err := planPatch(t, NewPatch(mustKind(t, "configmap"), fac, nil, enrich), input{args: []string{"app"}, s: map[string]string{
		"cluster": "prod", "namespace": "default", "patch": `{"data":{"a":"9"}}`, "type": "merge",
	}})
	require.NoError(t, err)
	assert.Equal(t, "9", seen)
	assert.Contains(t, eff.Warnings, "enriched")
}

// TestPatchMergeAppliesALoneOpObject asserts that a single op object under
// --type merge is applied as the merge patch the type names: its members
// become fields of the object.
func TestPatchMergeAppliesALoneOpObject(t *testing.T) {
	t.Parallel()
	eff, _, err := planTyped(t, "configmap", "app", configMapJSON, `{"op":"add","path":"/data/c","value":"3"}`, "merge")
	require.NoError(t, err)
	_, ok := diffAt(eff, "/op")
	assert.True(t, ok, "diff: %+v", eff.Diff)
}

// TestPatchRefusesTheInputBeforeTheClusterLookup asserts that `patch
// cluster` checks the patch before it looks the cluster name up: with a
// lookup that fails, a patch that cannot be used still gets its own
// refusal.
func TestPatchRefusesTheInputBeforeTheClusterLookup(t *testing.T) {
	t.Parallel()
	client := &patchClient{obj: objFromJSON(t, `{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c1"}}`)}
	fac := &recFactory{client: client, resolveErr: map[string]error{"prod": errors.New("lookup reached")}}
	for _, tc := range []struct{ typ, patch, want string }{
		{"merge", `[1]`, "invalid patch: a merge patch is a JSON object, not a JSON array"},
		{"", `[1]`, `invalid patch: patch operation 0 is a JSON number`},
	} {
		_, _, err := planPatch(t, NewPatch(mustKind(t, "cluster"), fac, nil), input{args: []string{"prod"}, s: map[string]string{
			"patch": tc.patch, "type": tc.typ,
		}})
		require.ErrorContains(t, err, tc.want)
	}
	assert.Empty(t, client.getName)
}

// TestPatchServerOwnedRefusalNeedsTheExactPath asserts that the
// server-owned refusal names only those fields themselves: the same names
// elsewhere pass it, and a metadata that is not an object gets the
// patched-object refusal instead.
func TestPatchServerOwnedRefusalNeedsTheExactPath(t *testing.T) {
	t.Parallel()
	_, _, err := planTyped(t, "configmap", "app", configMapJSON, `{"data":{"metadata":{"resourceVersion":"1"}}}`, "merge")
	require.NoError(t, err)
	_, _, err = planTyped(t, "configmap", "app", configMapJSON, `{"metadata":"x"}`, "merge")
	require.ErrorContains(t, err, "the patched object has no name")
	assert.NotContains(t, errText(err), "the merge patch targets")
}

// TestPatchApplyErrorNamesTheCheck asserts the text of an op that does not
// apply to the fetched object.
func TestPatchApplyErrorNamesTheCheck(t *testing.T) {
	t.Parallel()
	_, _, err := planTyped(t, "configmap", "app", configMapJSON, `[{"op":"replace","path":"/data/zz","value":"1"}]`, "")
	requireValidation(t, err)
	require.ErrorContains(t, err, "cannot apply patch: ")
	require.ErrorContains(t, err, "; check the patch operations against the resource")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestPatchMergeWarningComparesWithTheStoredObject asserts that the
// dropped-entry warning reads the server's dry-run answer: a container the
// patch keeps in short form is not counted as dropped once the server
// fills its defaults, as it filled the fetched one.
func TestPatchMergeWarningComparesWithTheStoredObject(t *testing.T) {
	t.Parallel()
	deploy := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default","resourceVersion":"3"},"spec":{"template":{"spec":{"containers":[{"name":"web","image":"v1","imagePullPolicy":"IfNotPresent"},{"name":"sidecar","image":"s1","imagePullPolicy":"IfNotPresent"}]}}}}`
	fillDefaults := func(obj *unstructured.Unstructured) *unstructured.Unstructured {
		out := obj.DeepCopy()
		containers, _, _ := unstructured.NestedSlice(out.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			if m, ok := c.(map[string]any); ok {
				if _, set := m["imagePullPolicy"]; !set {
					m["imagePullPolicy"] = "IfNotPresent"
				}
			}
		}
		_ = unstructured.SetNestedSlice(out.Object, containers, "spec", "template", "spec", "containers")
		return out
	}
	for _, tc := range []struct {
		name, patch string
		want        []string
	}{
		{"one of two kept in short form", `{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"v1"}]}}}}`,
			[]string{"the merge patch removes 1 of 2 entries of the list at /spec/template/spec/containers; a merge patch replaces a whole list and does not merge its entries"}},
		{"both resent in short form", `{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"v1"},{"name":"sidecar","image":"s1"}]}}}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &patchClient{obj: objFromJSON(t, deploy), dryRunAnswer: fillDefaults}
			fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
			eff, _, err := planPatch(t, NewPatch(mustKind(t, "deployment"), fac, nil), input{args: []string{"web"}, s: map[string]string{
				"cluster": "prod", "namespace": "default", "patch": tc.patch, "type": "merge",
			}})
			require.NoError(t, err)
			var got []string
			for _, w := range eff.Warnings {
				if strings.HasPrefix(w, "the merge patch") {
					got = append(got, w)
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPatchMergeWarningComesBeforeTheEnrichers asserts that the per-kind
// enrichers run after the dropped-entry warning, so their notes come last.
func TestPatchMergeWarningComesBeforeTheEnrichers(t *testing.T) {
	t.Parallel()
	deploy := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default","resourceVersion":"3"},"spec":{"list":[1,2]}}`
	client := &patchClient{obj: objFromJSON(t, deploy)}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	var seenAtEnrich int
	enrich := func(_, _ *unstructured.Unstructured, eff *safety.Effect) {
		seenAtEnrich = len(eff.Warnings)
		eff.Warnings = append(eff.Warnings, "enriched")
	}
	eff, _, err := planPatch(t, NewPatch(mustKind(t, "deployment"), fac, nil, enrich), input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "default", "patch": `{"spec":{"list":[1]}}`, "type": "merge",
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, seenAtEnrich, "the warning is in place when the enrichers run")
	require.Len(t, eff.Warnings, 2)
	assert.Equal(t, "enriched", eff.Warnings[1])
}

// TestPatchMergeWarningFallsBackToTheLocalMerge asserts that a client
// whose dry-run answers no object still gets the warning, read from the
// locally merged object.
func TestPatchMergeWarningFallsBackToTheLocalMerge(t *testing.T) {
	t.Parallel()
	deploy := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default","resourceVersion":"3"},"spec":{"list":[1,2]}}`
	client := &patchClient{obj: objFromJSON(t, deploy), dryRunAnswer: func(*unstructured.Unstructured) *unstructured.Unstructured { return nil }}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	eff, _, err := planPatch(t, NewPatch(mustKind(t, "deployment"), fac, nil), input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "default", "patch": `{"spec":{"list":[1]}}`, "type": "merge",
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"the merge patch removes 1 of 2 entries of the list at /spec/list; a merge patch replaces a whole list and does not merge its entries"}, eff.Warnings)
}

// TestPatchErrorsCarryNoValue asserts that no refusal of a patch quotes a
// value the patch carries: a malformed op ahead of an op whose value is the
// canary, a failing test op, an op on a path that is missing, an op the
// server owns, and a merge patch of a server-owned field. A path or key is
// an identifier and may be named.
func TestPatchErrorsCarryNoValue(t *testing.T) {
	t.Parallel()
	const canary = "s3cretcanary9"
	for _, tc := range []struct{ name, typ, patch, want string }{
		{"a malformed op, then an op holding the canary", "",
			`[{"op":"add","path":"/data/a"},{"op":"add","path":"/data/b","value":"` + canary + `"}]`,
			`patch operation 0 (add) has no "value"`},
		{"an add with a null value, then an op holding the canary", "",
			`[{"op":"add","path":"/data/a","value":null},{"op":"remove","path":"/data/zz"},{"op":"add","path":"/data/b","value":"` + canary + `"}]`,
			"cannot apply patch"},
		{"a test op that fails", "",
			`[{"op":"test","path":"/data/a","value":"` + canary + `"}]`,
			"testing value /data/a failed"},
		{"an op on a path that is missing", "",
			`[{"op":"add","path":"/nothing/here/` + "x" + `","value":"` + canary + `"}]`,
			"cannot apply patch"},
		{"a replace with a value that is not an object", "",
			`[{"op":"replace","path":"","value":"` + canary + `"}]`,
			"cannot apply patch"},
		{"a server-owned field in an op value", "",
			`[{"op":"add","path":"/metadata","value":{"resourceVersion":"1","note":"` + canary + `"}}]`,
			"patch operation 0 targets /metadata/resourceVersion; the server sets this field"},
		{"a server-owned field in a merge patch", "merge",
			`{"metadata":{"resourceVersion":"1","labels":{"a":"` + canary + `"}}}`,
			"the merge patch targets /metadata/resourceVersion"},
		{"an unknown op", "",
			`[{"op":"bogus","path":"/data/a","value":"` + canary + `"}]`,
			`patch operation 0 has the unknown operation "bogus"`},
		{"a patch that is not JSON", "",
			`[{"op":"add","path":"/data/a","value":"` + canary + `"`,
			"not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, client, err := planTyped(t, "configmap", "app", configMapJSON, tc.patch, tc.typ)
			requireValidation(t, err)
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), canary)
			assert.Empty(t, client.updates)
		})
	}
}
