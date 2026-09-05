package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crtInput is a minimal runtime.Input fake carrying exactly what
// NewGetCRT/NewRefuseCRTPatch's Execute/Plan read: positional args, the
// --namespace and --selector string flags, and the --wait bool flag.
type crtInput struct {
	args     []string
	ns       string
	selector string
	wait     bool
}

func (i crtInput) String(name string) string {
	switch name {
	case "namespace":
		return i.ns
	case "selector":
		return i.selector
	}
	return ""
}
func (i crtInput) Bool(name string) bool {
	if name == "wait" {
		return i.wait
	}
	return false
}
func (i crtInput) Strings(string) []string { return nil }
func (i crtInput) IsSet(string) bool       { return false }
func (i crtInput) Args() []string          { return i.args }

var _ runtime.Input = crtInput{}

// fakeRootClient is a real-server-over-mock RootClient: it issues a real
// HTTP GET against an httptest.Server and maps the response the same way
// production's rootClient does (engine.RootClient's documented contract -
// 404 -> NOT_FOUND, else non-2xx -> GENERIC), so crt.go's own decode/guard
// logic - not a hand-rolled substitute for it - is what these tests
// exercise.
type fakeRootClient struct {
	srv *httptest.Server
}

func (f fakeRootClient) GetJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		return nil, clierr.New(clierr.CodeGeneric, "rancher root GET "+path+": "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return body, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, clierr.New(clierr.CodeNotFound, fmt.Sprintf("rancher root GET %s: not found", path))
	default:
		return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("rancher root GET %s: %s", path, resp.Status))
	}
}

var _ engine.RootClient = fakeRootClient{}

type fakeRootFactory struct{ rc engine.RootClient }

func (f fakeRootFactory) Root() (engine.RootClient, error) { return f.rc, nil }

var _ engine.RootClientFactory = fakeRootFactory{}

func rootsFor(srv *httptest.Server) engine.RootClientFactory {
	return fakeRootFactory{rc: fakeRootClient{srv: srv}}
}

func crtKind(t *testing.T) kinds.Kind {
	t.Helper()
	k, ok := kinds.Lookup("clusterregistrationtoken")
	require.True(t, ok, "clusterregistrationtoken kind must exist")
	return k
}

// splicedStatus builds a status block as Steve's own formatter would emit
// it: every one of the seven command/manifest fields carries token, plus
// the raw token itself and an expiry.
func splicedStatus(token string) map[string]any {
	return map[string]any{
		"command":                    "curl --insecure -sfL https://rancher.example/v3/import/" + token + ".yaml | kubectl apply -f -",
		"insecureCommand":            "curl -sfL https://rancher.example/v3/import/" + token + ".yaml | kubectl apply -f -",
		"nodeCommand":                "sudo docker run -d --privileged --net=host --name=cattle-agent rancher/rancher-agent --server https://rancher.example --token " + token,
		"insecureNodeCommand":        "sudo docker run -d --name=cattle-agent rancher/rancher-agent --token " + token,
		"windowsNodeCommand":         "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + token,
		"insecureWindowsNodeCommand": "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + token,
		"manifestUrl":                "https://rancher.example/v3/import/" + token + ".yaml",
		"token":                      token,
		"expiresAt":                  "2099-01-01T00:00:00Z",
	}
}

func crtObject(ns, name string, status map[string]any) map[string]any {
	return map[string]any{
		"id":       ns + "/" + name,
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{"clusterName": ns},
		"status":   status,
	}
}

func writeJSONFixture(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(v))
}

// TestNewGetCRTNamedReturnsSplicedFields proves the single-object path
// carries the server-spliced fields straight through into the command
// Result: the real token, never the k8s proxy's own "{token}" placeholder.
func TestNewGetCRTNamedReturnsSplicedFields(t *testing.T) {
	t.Parallel()
	obj := crtObject("c-aaa", "default-token", splicedStatus("tok-aaa"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, crtCollectionPath+"/c-aaa/default-token", r.URL.Path)
		writeJSONFixture(t, w, obj)
	}))
	defer srv.Close()

	cmd := NewGetCRT(crtKind(t), rootsFor(srv))
	rc, ok := cmd.(runtime.ReadCommand)
	require.True(t, ok)

	res, err := rc.Execute(t.Context(), crtInput{args: []string{"default-token"}, ns: "c-aaa"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "ClusterRegistrationToken", res.Kind)
	rows, ok := res.Data.(crtRows)
	require.True(t, ok)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, "default-token", row.Name)
	assert.Equal(t, "c-aaa", row.Namespace)
	assert.Equal(t, "c-aaa", row.ClusterName)
	assert.Equal(t, "tok-aaa", row.Token)
	assert.Contains(t, row.Command, "tok-aaa")
	assert.NotContains(t, row.Command, placeholderToken)
	assert.Contains(t, row.NodeCommand, "tok-aaa")
	assert.NotContains(t, row.ManifestURL, placeholderToken)
}

// TestNewGetCRTPlaceholderGuardTable is table-driven over each of the seven
// formatter-protected fields carrying the literal "{token}" placeholder in
// turn: the guard must name that exact field and never let the placeholder
// reach the Result as data.
func TestNewGetCRTPlaceholderGuardTable(t *testing.T) {
	fields := []string{
		"command",
		"insecureCommand",
		"nodeCommand",
		"insecureNodeCommand",
		"windowsNodeCommand",
		"insecureWindowsNodeCommand",
		"manifestUrl",
	}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			status := splicedStatus("tok-aaa")
			status[field] = placeholderToken // this one field never got spliced
			obj := crtObject("c-aaa", "default-token", status)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSONFixture(t, w, obj)
			}))
			defer srv.Close()

			cmd := NewGetCRT(crtKind(t), rootsFor(srv))
			rc := cmd.(runtime.ReadCommand)
			res, err := rc.Execute(t.Context(), crtInput{args: []string{"default-token"}, ns: "c-aaa"}, nil)

			require.Error(t, err)
			ce, ok := err.(*clierr.Error)
			require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Contains(t, ce.Message, field, "error must name the exact unspliced field")
			assert.Equal(t, runtime.Result{}, res, "an unspliced placeholder must never surface as a Result")

			// The literal placeholder must never appear anywhere reachable as data.
			b, _ := json.Marshal(res)
			assert.NotContains(t, string(b), placeholderToken)
		})
	}
}

// TestNewGetCRTNotFound proves a 404 on the single-object path maps to
// CodeNotFound (engine.RootClient's own documented 404 mapping, exercised
// end-to-end through NewGetCRT).
func TestNewGetCRTNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cmd := NewGetCRT(crtKind(t), rootsFor(srv))
	rc := cmd.(runtime.ReadCommand)
	res, err := rc.Execute(t.Context(), crtInput{args: []string{"missing"}, ns: "c-aaa"}, nil)

	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, runtime.Result{}, res)
}

// TestNewGetCRTMalformedBody is spec D-10's transport case (review R2-5):
// a body that is not valid JSON must decode to a typed CodeGeneric error
// naming "decoding" and the path - never a panic, never an empty/zero
// Result standing in for the failure silently.
func TestNewGetCRTMalformedBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json{"))
	}))
	defer srv.Close()

	cmd := NewGetCRT(crtKind(t), rootsFor(srv))
	rc := cmd.(runtime.ReadCommand)

	assert.NotPanics(t, func() {
		res, err := rc.Execute(t.Context(), crtInput{}, nil)
		require.Error(t, err)
		ce, ok := err.(*clierr.Error)
		require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Contains(t, ce.Message, "decoding")
		assert.Contains(t, ce.Message, crtCollectionPath)
		assert.Equal(t, runtime.Result{}, res)
	})
}

// TestNewGetCRTNamespaceFiltering is R3-3's namespace-filtering contract: the
// Steve read always GETs the root collection and filters DECODED objects
// locally. A bare list returns every namespace; --namespace narrows to one,
// and the OTHER namespace's token must appear NOWHERE in the filtered
// output - the leak assertion.
func TestNewGetCRTNamespaceFiltering(t *testing.T) {
	t.Parallel()
	objs := []map[string]any{
		crtObject("c-aaa", "default-token", splicedStatus("tok-aaa")),
		crtObject("c-bbb", "default-token", splicedStatus("tok-bbb")),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, crtCollectionPath, r.URL.Path)
		writeJSONFixture(t, w, map[string]any{"data": objs})
	}))
	defer srv.Close()

	cmd := NewGetCRT(crtKind(t), rootsFor(srv))
	rc := cmd.(runtime.ReadCommand)

	// Bare list: both namespaces present.
	res, err := rc.Execute(t.Context(), crtInput{}, nil)
	require.NoError(t, err)
	all, ok := res.Data.(crtRows)
	require.True(t, ok)
	require.Len(t, all, 2)

	// --namespace c-aaa: only that namespace's row, and c-bbb's token is
	// absent from the ENTIRE marshaled output, not merely "the right one is
	// present" - the real leak check.
	res, err = rc.Execute(t.Context(), crtInput{ns: "c-aaa"}, nil)
	require.NoError(t, err)
	filtered, ok := res.Data.(crtRows)
	require.True(t, ok)
	require.Len(t, filtered, 1)
	assert.Equal(t, "c-aaa", filtered[0].Namespace)
	assert.Equal(t, "tok-aaa", filtered[0].Token)

	b, err := json.Marshal(filtered)
	require.NoError(t, err)
	out := string(b)
	assert.Contains(t, out, "tok-aaa")
	assert.NotContains(t, out, "tok-bbb", "the other namespace's token must never leak into filtered output")
	assert.NotContains(t, out, "c-bbb", "the other namespace must never leak into filtered output")
}

// TestNewGetCRTListDegradesBadRowInsteadOfAborting is finding 3's fix: a
// list containing one row with an unspliced "{token}" placeholder must NOT
// abort the whole result (the pre-fix behavior - one bad row hid every
// healthy row). Instead the bad row's offending field is blanked, a warning
// names the row and field, and the two healthy rows survive untouched. The
// placeholder itself must still never reach output as data anywhere in the
// marshaled result - the fix changes the mechanism (abort -> blank+warn),
// never the placeholder ban itself.
func TestNewGetCRTListDegradesBadRowInsteadOfAborting(t *testing.T) {
	t.Parallel()
	badStatus := splicedStatus("tok-bad")
	badStatus["command"] = placeholderToken // this one field never got spliced
	objs := []map[string]any{
		crtObject("c-aaa", "good-1", splicedStatus("tok-aaa")),
		crtObject("c-bbb", "bad-1", badStatus),
		crtObject("c-ccc", "good-2", splicedStatus("tok-ccc")),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONFixture(t, w, map[string]any{"data": objs})
	}))
	defer srv.Close()

	cmd := NewGetCRT(crtKind(t), rootsFor(srv))
	rc := cmd.(runtime.ReadCommand)
	res, err := rc.Execute(t.Context(), crtInput{}, nil)
	require.NoError(t, err, "one bad row must not abort the whole list")

	rows, ok := res.Data.(crtRows)
	require.True(t, ok)
	require.Len(t, rows, 3, "all three rows, including the bad one, must be present")

	byName := map[string]crtRow{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	good1, bad1, good2 := byName["good-1"], byName["bad-1"], byName["good-2"]

	assert.Contains(t, good1.Command, "tok-aaa", "a healthy row before the bad one is unaffected")
	assert.Contains(t, good2.Command, "tok-ccc", "a healthy row after the bad one is unaffected")

	assert.Equal(t, "", bad1.Command, "the offending field must be blanked, never left carrying the placeholder")
	assert.Contains(t, bad1.NodeCommand, "tok-bad", "only the offending field is blanked; the row's other healthy fields survive")

	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "c-bbb/bad-1", "the warning must name the offending row")
	assert.Contains(t, res.Warnings[0], "command", "the warning must name the offending field")

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.NotContains(t, string(b), placeholderToken, "the placeholder must never reach output as data")
}

// TestNewGetCRTNamedGetOnBadRowStaysStrict is finding 3's regression proof
// for the branch the fix must NOT touch: a single-object get ("named"
// branch, with a name arg) on a row carrying an unspliced placeholder must
// still fail loud with the typed CodeGeneric error - the list-degrade
// behavior above applies only to the no-name-arg list path.
// TestNewGetCRTPlaceholderGuardTable already exercises this exhaustively
// over all seven guarded fields; this test asserts the same contract narrowly
// so a future change to the list path can't accidentally soften the
// named-get path without a dedicated test catching it.
func TestNewGetCRTNamedGetOnBadRowStaysStrict(t *testing.T) {
	t.Parallel()
	status := splicedStatus("tok-aaa")
	status["command"] = placeholderToken
	obj := crtObject("c-aaa", "default-token", status)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONFixture(t, w, obj)
	}))
	defer srv.Close()

	cmd := NewGetCRT(crtKind(t), rootsFor(srv))
	rc := cmd.(runtime.ReadCommand)
	res, err := rc.Execute(t.Context(), crtInput{args: []string{"default-token"}, ns: "c-aaa"}, nil)

	require.Error(t, err, "a single-object get on a bad row must still fail loud, not degrade")
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "command")
	assert.Equal(t, runtime.Result{}, res, "an unspliced placeholder must never surface as a Result on the named path")
}

// TestNewRefuseCRTPatchRejectsAsReadOnly proves the patch override never
// touches the network: Plan refuses immediately with a VALIDATION error
// naming the row read-only.
func TestNewRefuseCRTPatchRejectsAsReadOnly(t *testing.T) {
	t.Parallel()
	cmd := NewRefuseCRTPatch(crtKind(t))
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "NewRefuseCRTPatch must be a MutatingCommand")

	eff, exec, err := mc.Plan(t.Context(), crtInput{args: []string{"default-token"}, ns: "c-aaa"})
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "read-only")
	assert.Nil(t, exec)
	assert.Equal(t, "", eff.Target)
}

// TestNewGetCRTDescribeMirrorsGenericGet asserts the schema-parity contract
// (finding 6): the override's Descriptor must be STRUCT-EQUAL to what the
// LIVE resources.NewGet(k, ...) generates for this row - not to a
// hand-duplicated literal that can silently drift out of sync with the
// generator (resources/get.go's getFlags) while still passing. Mirrors
// deletecluster_test.go's TestDeleteProvisioningClusterDescribeMirrorsGeneric
// pattern. Describe() never touches its clients argument, so nil is safe
// here (verified by reading getCmd.Describe()/patchCmd.Describe()).
func TestNewGetCRTDescribeMirrorsGenericGet(t *testing.T) {
	t.Parallel()
	k := crtKind(t)
	generic := resources.NewGet(k, nil).Describe()
	override := NewGetCRT(k, nil).Describe()
	assert.Equal(t, generic, override)
	assert.Equal(t, "get clusterregistrationtoken", override.Name)
}

// TestNewRefuseCRTPatchDescribeMirrorsGenericPatch is finding 6's
// struct-equality parity test for the patch override, against the LIVE
// resources.NewPatch(k, ...) generator.
func TestNewRefuseCRTPatchDescribeMirrorsGenericPatch(t *testing.T) {
	t.Parallel()
	k := crtKind(t)
	generic := resources.NewPatch(k, nil, nil).Describe()
	override := NewRefuseCRTPatch(k).Describe()
	assert.Equal(t, generic, override)
	assert.Equal(t, "patch clusterregistrationtoken", override.Name)
}

// TestNewGetCRTSelectorFiltersTheDecodedList asserts the override honors
// the --selector it inherits from the generic get descriptor: the list is
// fetched whole (this override never assumes Steve's query grammar) and
// filtered by labels locally, composing with --namespace.
func TestNewGetCRTSelectorFiltersTheDecodedList(t *testing.T) {
	t.Parallel()
	prod := crtObject("c-aaa", "default-token", splicedStatus("tok-aaa"))
	prod["metadata"].(map[string]any)["labels"] = map[string]any{"env": "prod", "team": "a"}
	// dev carries an unspliced command so the test can show that a row the
	// selector excludes never emits a placeholder warning, while a kept bad
	// row still degrades in place.
	devStatus := splicedStatus("tok-bbb")
	devStatus["command"] = placeholderToken
	dev := crtObject("c-bbb", "default-token", devStatus)
	dev["metadata"].(map[string]any)["labels"] = map[string]any{"env": "dev"}
	bare := crtObject("c-ccc", "default-token", splicedStatus("tok-ccc"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONFixture(t, w, map[string]any{"data": []map[string]any{prod, dev, bare}})
	}))
	defer srv.Close()
	rc := NewGetCRT(crtKind(t), rootsFor(srv)).(runtime.ReadCommand)

	res, err := rc.Execute(t.Context(), crtInput{selector: "env=prod"}, nil)
	require.NoError(t, err)
	rows := res.Data.(crtRows)
	require.Len(t, rows, 1)
	assert.Equal(t, "c-aaa", rows[0].Namespace)
	assert.Empty(t, res.Warnings, "a row the selector excludes is never inspected for placeholders")

	res, err = rc.Execute(t.Context(), crtInput{selector: "env=dev"}, nil)
	require.NoError(t, err)
	rows = res.Data.(crtRows)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Command, "a kept bad row still degrades in place")
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "c-bbb/default-token")

	res, err = rc.Execute(t.Context(), crtInput{selector: "env"}, nil)
	require.NoError(t, err)
	assert.Len(t, res.Data.(crtRows), 2, "existence selector keeps every labeled row")

	res, err = rc.Execute(t.Context(), crtInput{selector: "!env"}, nil)
	require.NoError(t, err)
	rows = res.Data.(crtRows)
	require.Len(t, rows, 1)
	assert.Equal(t, "c-ccc", rows[0].Namespace)

	res, err = rc.Execute(t.Context(), crtInput{ns: "c-bbb", selector: "env=prod"}, nil)
	require.NoError(t, err)
	assert.Empty(t, res.Data.(crtRows), "--namespace and --selector both apply")
}

// TestNewGetCRTSelectorValidation asserts the override refuses a
// malformed selector and a selector on a named get exactly as the generic
// get does, before any request.
func TestNewGetCRTSelectorValidation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request must be made")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	rc := NewGetCRT(crtKind(t), rootsFor(srv)).(runtime.ReadCommand)

	_, err := rc.Execute(t.Context(), crtInput{selector: "env in (prod"}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "--selector")

	_, err = rc.Execute(t.Context(), crtInput{args: []string{"default-token"}, ns: "c-aaa", selector: "env=prod"}, nil)
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "--selector does not apply to a named clusterregistrationtoken")
}
