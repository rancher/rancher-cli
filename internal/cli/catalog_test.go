package cli

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestDeleteHintNamesTheReasonForEveryTypedWord asserts the message of a
// delete of a catalog kind, typed as the name or as the plural alias, with the
// row's reason in it. Not parallel: it sets the process environment and the
// captured command line.
func TestDeleteHintNamesTheReasonForEveryTypedWord(t *testing.T) {
	exitEnv(t)
	for word, want := range map[string]string{
		"apps":         `unknown command "apps"; app cannot be deleted - deleting an app object leaves its Helm release installed; this CLI does not uninstall apps`,
		"operations":   `unknown command "operations"; operation cannot be deleted - this CLI does not delete operations`,
		"clusterrepos": `unknown command "clusterrepos"; clusterrepo cannot be deleted - this CLI does not delete repositories`,
	} {
		t.Run(word, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			var out, errOut bytes.Buffer
			root.Writer, root.ErrWriter = &out, &errOut
			got := run(t.Context(), root, []string{"rancher", "delete", word, "x", "-o", "json"})
			require.NotNil(t, got.err)

			var env errEnvelope
			require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
			assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
			decoded := clierr.New(env.Data.Error.Code, env.Data.Error.Message)
			require.ErrorContains(t, decoded, want)
			assert.Equal(t, "rancher schema", env.Data.Error.Hint, "the generic hint stays")
		})
	}
}

// TestNoPatchReasonAndAPatchOverrideAreRefused asserts that a row with both
// NoPatchReason and a patch override makes BuildRoot fail. Not parallel: it
// replaces the package override table.
func TestNoPatchReasonAndAPatchOverrideAreRefused(t *testing.T) {
	orig := kindOverrideTable
	overrides := make(map[string]kindOverride, len(orig)+1)
	maps.Copy(overrides, orig)
	overrides["app"] = kindOverride{patch: func(k kinds.Kind, _ runDeps) runtime.Command { return resources.NewRefusePatch(k) }}
	kindOverrideTable = overrides
	t.Cleanup(func() { kindOverrideTable = orig })

	_, err := BuildRoot("v-test")
	require.ErrorContains(t, err, `kind "app" sets NoPatchReason and also has a patch override`)

	require.NoError(t, checkPatchRefusals(kinds.All(), orig), "the production tables have no such row")
}

// TestNoPatchReasonRowsShowPatchAsARefusal asserts that the schema manifest
// lists patch of each catalog kind as refused with the row's reason, and that
// the patch binding of the row is kept and resolves to the registered
// refusal command.
func TestNoPatchReasonRowsShowPatchAsARefusal(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	patchGroup := findCommand(root.Commands, "patch")
	require.NotNil(t, patchGroup)
	bindings := kinds.Bindings()

	for _, key := range []string{"clusterrepo", "app", "operation"} {
		row, ok := kinds.ByKey(key)
		require.True(t, ok, key)
		require.NotEmpty(t, row.NoPatchReason, key)

		var verb runtime.VerbState
		for _, sk := range schemaKinds(kinds.All(), kindOverrides()) {
			if sk.Key == key {
				verb = sk.Verbs["patch"]
			}
		}
		assert.Equal(t, runtime.VerbRefused, verb.State, key)
		assert.Equal(t, row.NoPatchReason, verb.Note, key)

		assert.Contains(t, bindings, "patch "+row.Name, key)
		assert.NotNil(t, findCommand(patchGroup.Commands, row.Name), "patch %s is registered", row.Name)
	}
	for _, key := range []string{"pod", "gitrepo"} {
		row, _ := kinds.ByKey(key)
		assert.Empty(t, row.NoPatchReason, key)
	}
}

// TestGetOperationDescriptionStatesTheDifferences asserts that the schema
// description of get operation names both differences from Rancher's table.
func TestGetOperationDescriptionStatesTheDifferences(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var desc string
	for _, c := range m.Commands {
		if c.Name == "get operation" {
			desc = c.Desc
		}
	}
	assert.Contains(t, desc, "COMMAND shows the command's words joined with commas")
	assert.Contains(t, desc, `TARGET-NAMESPACE is the namespace of the pod that ran the command, where the Rancher UI's "Tgt Namespace" is the release's namespace`)
}

// catalogServer answers the reads of the catalog tests.
type catalogServer struct{}

func (s *catalogServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	const base = "/k8s/clusters/local/apis/catalog.cattle.io/v1/namespaces/default/"
	switch r.URL.Path {
	case base + "apps/x":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "catalog.cattle.io/v1", "kind": "App",
			"metadata": map[string]any{"name": "x", "namespace": "default"},
			"spec": map[string]any{
				"name": "rel", "values": map[string]any{"password": "topsecret"},
				"chart": map[string]any{"metadata": map[string]any{"name": "c"}, "values": map[string]any{"k": "chartdefault"}},
				"info":  map[string]any{"status": "deployed", "notes": "notes-text"},
			},
		})
	case base + "operations/x":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "catalog.cattle.io/v1", "kind": "Operation",
			"metadata": map[string]any{"name": "x", "namespace": "default"},
			"status":   map[string]any{"podNamespace": "cattle-system", "command": []any{"upgrade", "rancher-webhook"}, "token": "op-token-value"},
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not found","reason":"NotFound","details":{},"code":404}`))
	}
}

// TestGetOfAnAppAndAnOperationHidesTheRedactedPaths asserts that get -o json
// and -o yaml of an app lack its three redacted paths and of an operation lack
// status.token, while the other fields stay. Not parallel: it sets the
// process environment and the captured command line.
func TestGetOfAnAppAndAnOperationHidesTheRedactedPaths(t *testing.T) {
	exitEnv(t)
	srv := httptest.NewTLSServer(&catalogServer{})
	defer srv.Close()
	dir := grConfigDir(t, srv)

	getObject := func(t *testing.T, format, kind string) map[string]any {
		t.Helper()
		root := grRoot(t)
		var out, errOut bytes.Buffer
		root.Writer, root.ErrWriter = &out, &errOut
		got := run(t.Context(), root, []string{"rancher", "--config", dir, "get", kind, "x", "--cluster", "local", "--namespace", "default", "-o", format})
		require.Nil(t, got.err, "stderr: %s", errOut.String())
		var env struct {
			Data map[string]any `json:"data"`
		}
		if format == "yaml" {
			require.NoError(t, yaml.Unmarshal(out.Bytes(), &env))
		} else {
			require.NoError(t, json.Unmarshal(out.Bytes(), &env))
		}
		for _, secret := range []string{"topsecret", "chartdefault", "notes-text", "op-token-value"} {
			assert.NotContains(t, out.String(), secret)
		}
		return env.Data
	}

	for _, format := range []string{"json", "yaml"} {
		t.Run("app "+format, func(t *testing.T) {
			spec, _ := getObject(t, format, "app")["spec"].(map[string]any)
			require.NotNil(t, spec)
			assert.NotContains(t, spec, "values")
			assert.NotContains(t, spec["chart"], "values")
			assert.NotContains(t, spec["info"], "notes")
			assert.Equal(t, "rel", spec["name"])
			assert.Equal(t, "deployed", spec["info"].(map[string]any)["status"])
		})
		t.Run("operation "+format, func(t *testing.T) {
			status, _ := getObject(t, format, "operation")["status"].(map[string]any)
			require.NotNil(t, status)
			assert.NotContains(t, status, "token")
			assert.Equal(t, "cattle-system", status["podNamespace"])
		})
	}
}

// TestGetAppUnderAProjectIsLimitedToItsNamespaces asserts that get app with
// --project shows the apps of the project's namespaces only, and that a named
// get in a namespace outside the project is refused before it reads the
// app. The fake puts the namespace default in the project p-xyz of c-abc, and
// answers a list of apps with one app in default and one in another namespace.
func TestGetAppUnderAProjectIsLimitedToItsNamespaces(t *testing.T) {
	h := newContractHarness(t)
	apps := "/k8s/clusters/c-abc/apis/catalog.cattle.io/v1/apps"
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apps {
			h.srv.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		item := func(ns string) map[string]any {
			return map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "App", "metadata": map[string]any{"name": "app-in-" + ns, "namespace": ns}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "AppList",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{item("default"), item("elsewhere")}})
	}))
	defer front.Close()
	t.Setenv(config.EnvURL, front.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, front)))

	r := h.run(t, "get", "app", "--cluster", "c-abc", "--project", "p-xyz", "-o", "json")
	require.Nil(t, r.err, "stderr: %s", r.stderr)
	var env struct {
		Data []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &env))
	require.Len(t, env.Data, 1)
	assert.Equal(t, "app-in-default", env.Data[0].Metadata.Name)

	r = h.run(t, "get", "app", "x", "--cluster", "c-def", "--project", "p-def", "--namespace", "default", "-o", "json")
	require.NotNil(t, r.err)
	assert.Equal(t, clierr.CodeValidation, r.err.Code)
	assert.Zero(t, r.count("/k8s/clusters/c-def/apis/catalog.cattle.io/v1/namespaces/default/apps/x"), "the app is not read")
}
