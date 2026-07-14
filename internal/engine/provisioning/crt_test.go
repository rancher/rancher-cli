package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crtInput is a minimal runtime.Input fake carrying exactly what
// NewGetCRT/NewRefuseCRTPatch's Execute/Plan read: positional args, the
// --namespace string flag, and the --wait bool flag.
type crtInput struct {
	args []string
	ns   string
	wait bool
}

func (i crtInput) String(name string) string {
	if name == "namespace" {
		return i.ns
	}
	return ""
}
func (i crtInput) Bool(name string) bool {
	if name == "wait" {
		return i.wait
	}
	return false
}
func (i crtInput) Args() []string { return i.args }

var _ runtime.Input = crtInput{}

// fakeRootClient is a real-server-over-mock RootClient: it issues a real
// HTTP GET against an httptest.Server and maps the response the same way
// production's rootClient does (engine.RootClient's documented contract —
// 404 -> NOT_FOUND, else non-2xx -> GENERIC), so crt.go's own decode/guard
// logic — not a hand-rolled substitute for it — is what these tests
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

	res, err := rc.Execute(context.Background(), crtInput{args: []string{"default-token"}, ns: "c-aaa"}, nil)
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
		field := field
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
			res, err := rc.Execute(context.Background(), crtInput{args: []string{"default-token"}, ns: "c-aaa"}, nil)

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
	res, err := rc.Execute(context.Background(), crtInput{args: []string{"missing"}, ns: "c-aaa"}, nil)

	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, runtime.Result{}, res)
}

// TestNewGetCRTMalformedBody is spec D-10's transport case (review R2-5):
// a body that is not valid JSON must decode to a typed CodeGeneric error
// naming "decoding" and the path — never a panic, never an empty/zero
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
		res, err := rc.Execute(context.Background(), crtInput{}, nil)
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
// output — the leak assertion.
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
	res, err := rc.Execute(context.Background(), crtInput{}, nil)
	require.NoError(t, err)
	all, ok := res.Data.(crtRows)
	require.True(t, ok)
	require.Len(t, all, 2)

	// --namespace c-aaa: only that namespace's row, and c-bbb's token is
	// absent from the ENTIRE marshaled output, not merely "the right one is
	// present" — the real leak check.
	res, err = rc.Execute(context.Background(), crtInput{ns: "c-aaa"}, nil)
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
	assert.False(t, strings.Contains(out, "c-bbb"), "the other namespace must never leak into filtered output")
}

// TestNewRefuseCRTPatchRejectsAsReadOnly proves the patch override never
// touches the network: Plan refuses immediately with a VALIDATION error
// naming the row read-only.
func TestNewRefuseCRTPatchRejectsAsReadOnly(t *testing.T) {
	t.Parallel()
	cmd := NewRefuseCRTPatch(crtKind(t))
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "NewRefuseCRTPatch must be a MutatingCommand")

	eff, exec, err := mc.Plan(context.Background(), crtInput{args: []string{"default-token"}, ns: "c-aaa"})
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok, "error must be a typed *clierr.Error, got %T", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "read-only")
	assert.Nil(t, exec)
	assert.Equal(t, "", eff.Target)
}

// TestNewGetCRTDescribeMirrorsGenericGet pins the schema-parity contract:
// the override's Descriptor must be indistinguishable from what
// resources.NewGet(k, ...) would have generated for this row, since only
// execution is overridden (golden manifest must stay unchanged).
func TestNewGetCRTDescribeMirrorsGenericGet(t *testing.T) {
	t.Parallel()
	k := crtKind(t)
	d := NewGetCRT(k, fakeRootFactory{}).Describe()
	assert.Equal(t, "get clusterregistrationtoken", d.Name)
	assert.ElementsMatch(t, []string{"clusterregistrationtokens", "crt"}, d.Aliases)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)
	assert.False(t, d.ProjectScope)
	require.Len(t, d.Flags, 4)
	assert.Equal(t, "namespace", d.Flags[0].Name)
	assert.False(t, d.Flags[0].Required)
}

func TestNewRefuseCRTPatchDescribeMirrorsGenericPatch(t *testing.T) {
	t.Parallel()
	k := crtKind(t)
	d := NewRefuseCRTPatch(k).Describe()
	assert.Equal(t, "patch clusterregistrationtoken", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
	assert.Equal(t, "false", d.Destructive)
	require.Len(t, d.Flags, 6)
	assert.Equal(t, "namespace", d.Flags[0].Name)
	assert.True(t, d.Flags[0].Required)
}
