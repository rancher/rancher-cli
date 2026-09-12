package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

type effectData struct {
	Action  string `json:"action"`
	Target  string `json:"target"`
	Cluster string `json:"cluster"`
	Diff    []struct {
		Path string `json:"path"`
		From any    `json:"from"`
		To   any    `json:"to"`
	} `json:"diff"`
	ConflictDetection bool `json:"conflictDetection"`
	Reversible        bool `json:"reversible"`
	Destructive       bool `json:"destructive"`
}

type effectEnvelope struct {
	Kind     string     `json:"kind"`
	Warnings []string   `json:"warnings"`
	Data     effectData `json:"data"`
}

type grEnvData struct {
	Error struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Hint    string          `json:"hint"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

type grErrEnvelope struct {
	Kind string    `json:"kind"`
	Data grEnvData `json:"data"`
}

func grWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func grWriteStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"status":     "Failure",
		"reason":     reason,
		"message":    msg,
		"code":       code,
	})
}

func grKubeList(items []map[string]any) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "List",
		"metadata":   map[string]any{"resourceVersion": "1"},
		"items":      items,
	}
}

func grClusterItem(id, display string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": id, "resourceVersion": "10"},
		"spec":     map[string]any{"displayName": display},
	}
}

func grUserItem(id string, principalIDs []string) map[string]any {
	return map[string]any{
		"metadata":     map[string]any{"name": id, "resourceVersion": "20"},
		"principalIds": principalIDs,
		"displayName":  id,
	}
}

func grProjectItem(id, clusterID string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": id, "namespace": clusterID, "resourceVersion": "30"},
		"spec":     map[string]any{"displayName": "test-project"},
		"status":   map[string]any{},
	}
}

func grBindingItem(ns, name, rv, roleField, roleVal, subjectField, subjectVal string) map[string]any {
	return map[string]any{
		"metadata":   map[string]any{"name": name, "namespace": ns, "resourceVersion": rv},
		roleField:    roleVal,
		subjectField: subjectVal,
	}
}

func grCreatedObj(ns, name string) map[string]any {
	return map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Binding",
		"metadata":   map[string]any{"name": name, "namespace": ns, "resourceVersion": "99"},
	}
}

// grIsDryRunDelete checks if the DELETE request body marks the request as dry-run.
func grIsDryRunDelete(r *http.Request) bool {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	var opts struct {
		DryRun []string `json:"dryRun"`
	}
	_ = json.Unmarshal(body, &opts)
	return slices.Contains(opts.DryRun, "All")
}

// diffPaths returns sorted JSON-pointer paths from an Effect.diff.
func grDiffPaths(diff []struct {
	Path string `json:"path"`
	From any    `json:"from"`
	To   any    `json:"to"`
}) []string {
	paths := make([]string, len(diff))
	for i, e := range diff {
		paths[i] = e.Path
	}
	sort.Strings(paths)
	return paths
}

// grDiffToMap returns a path→To map for asserting Diff values.
func grDiffToMap(diff []struct {
	Path string `json:"path"`
	From any    `json:"from"`
	To   any    `json:"to"`
}) map[string]any {
	out := make(map[string]any, len(diff))
	for _, e := range diff {
		out[e.Path] = e.To
	}
	return out
}

// serverOpts configures the per-test fake management API.
type serverOpts struct {
	// pre-populated binding lists per resource kind (nil → empty list)
	prtbItems []map[string]any
	crtbItems []map[string]any
	grbItems  []map[string]any

	// set true to 409 the second non-dry-run DELETE on PRTBs
	failSecondRealDelete bool

	// output counters (non-dry-run only)
	realCreates atomic.Int32
	realDeletes atomic.Int32

	// body of the most recent non-dry-run GRB POST (decoded)
	lastGRBCreate map[string]any
}

// grHandler builds an http.Handler that serves the management API paths
// needed by grant/revoke. User u-abc is always available; the provisioned
// user for okta_user://ghost is always absent (triggering the provision path).
func grHandler(opts *serverOpts) http.Handler {
	prov := rbac.ProvisionedUserID("okta_user://ghost")

	mux := http.NewServeMux()

	// RoleTemplate GET by name - any name is accepted, with the context
	// Rancher stores on it (the scope its name starts with: "project-member"
	// is a project role), since grant refuses a template whose context is
	// not the binding's.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates/", func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		rtContext, _, _ := strings.Cut(name, "-")
		grWriteJSON(w, map[string]any{
			"apiVersion": "management.cattle.io/v3",
			"kind":       "RoleTemplate",
			"metadata":   map[string]any{"name": name, "resourceVersion": "1"},
			"context":    rtContext,
		})
	})

	// GlobalRole GET by name - any name is accepted.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/globalroles/", func(w http.ResponseWriter, r *http.Request) {
		grWriteJSON(w, map[string]any{
			"apiVersion": "management.cattle.io/v3",
			"kind":       "GlobalRole",
			"metadata":   map[string]any{"name": "gr", "resourceVersion": "1"},
		})
	})

	// Cluster list (for ResolveCluster("c-x")).
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/clusters", func(w http.ResponseWriter, r *http.Request) {
		grWriteJSON(w, grKubeList([]map[string]any{
			grClusterItem("c-x", "test-cluster"),
		}))
	})

	// User GET - u-abc found, provisioned hash 404.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/users/u-abc", func(w http.ResponseWriter, r *http.Request) {
		grWriteJSON(w, map[string]any{
			"apiVersion":   "management.cattle.io/v3",
			"kind":         "User",
			"metadata":     map[string]any{"name": "u-abc", "resourceVersion": "20"},
			"principalIds": []string{"local://u-abc"},
			"displayName":  "u-abc",
		})
	})
	// Provisioned user not found → 404.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/users/"+prov, func(w http.ResponseWriter, r *http.Request) {
		grWriteStatus(w, http.StatusNotFound, "NotFound", "user not found")
	})

	// User LIST - for provisioning fallback (no user with okta principal).
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/users", func(w http.ResponseWriter, r *http.Request) {
		grWriteJSON(w, grKubeList([]map[string]any{
			grUserItem("u-abc", []string{"local://u-abc"}),
		}))
	})

	// Project LIST in c-x namespace.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-x/projects", func(w http.ResponseWriter, r *http.Request) {
		grWriteJSON(w, grKubeList([]map[string]any{
			grProjectItem("p-x", "c-x"),
		}))
	})

	// Project GET p-x in c-x namespace (for BackingNamespace).
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-x/projects/p-x", func(w http.ResponseWriter, r *http.Request) {
		grWriteJSON(w, map[string]any{
			"apiVersion": "management.cattle.io/v3",
			"kind":       "Project",
			"metadata":   map[string]any{"name": "p-x", "namespace": "c-x", "resourceVersion": "30"},
			"spec":       map[string]any{"displayName": "test-project"},
			"status":     map[string]any{},
		})
	})

	// PRTB LIST (GET) + CREATE (POST) share the same base path.
	// Trailing-slash pattern handles DELETE on named resources (e.g. /prtb-1).
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/p-x/projectroletemplatebindings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			items := opts.prtbItems
			if items == nil {
				items = []map[string]any{}
			}
			grWriteJSON(w, grKubeList(items))
		case http.MethodPost:
			dryRun := r.URL.Query().Get("dryRun") == "All"
			if !dryRun {
				opts.realCreates.Add(1)
			}
			grWriteJSON(w, grCreatedObj("p-x", "prtb-new"))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// PRTB DELETE by name.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/p-x/projectroletemplatebindings/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if grIsDryRunDelete(r) {
			grWriteJSON(w, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
			return
		}
		n := opts.realDeletes.Add(1)
		if opts.failSecondRealDelete && n == 2 {
			grWriteStatus(w, http.StatusConflict, "Conflict", "resourceVersion conflict")
			return
		}
		grWriteJSON(w, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
	})

	// CRTB LIST (GET) + CREATE (POST).
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-x/clusterroletemplatebindings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			items := opts.crtbItems
			if items == nil {
				items = []map[string]any{}
			}
			grWriteJSON(w, grKubeList(items))
		case http.MethodPost:
			dryRun := r.URL.Query().Get("dryRun") == "All"
			if !dryRun {
				opts.realCreates.Add(1)
			}
			grWriteJSON(w, grCreatedObj("c-x", "crtb-new"))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// CRTB DELETE by name.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-x/clusterroletemplatebindings/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if grIsDryRunDelete(r) {
			grWriteJSON(w, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
			return
		}
		opts.realDeletes.Add(1)
		grWriteJSON(w, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
	})

	// GRB LIST (GET) + CREATE (POST).
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/globalrolebindings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			items := opts.grbItems
			if items == nil {
				items = []map[string]any{}
			}
			grWriteJSON(w, grKubeList(items))
		case http.MethodPost:
			dryRun := r.URL.Query().Get("dryRun") == "All"
			if !dryRun {
				opts.realCreates.Add(1)
				body, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				var obj map[string]any
				_ = json.Unmarshal(body, &obj)
				opts.lastGRBCreate = obj
			}
			grWriteJSON(w, grCreatedObj("", "grb-new"))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// GRB DELETE by name.
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/globalrolebindings/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if grIsDryRunDelete(r) {
			grWriteJSON(w, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
			return
		}
		opts.realDeletes.Add(1)
		grWriteJSON(w, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
	})

	return mux
}

// grConfigDir creates a temp config dir pointed at srv.
func grConfigDir(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(certPath, pemFromServer(t, srv), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"currentServer: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n    ca: "+certPath+"\n"), 0o600))
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "token-abc:secret"))
	return dir
}

// grRun runs the root command and captures stdout/stderr.
func grRun(t *testing.T, root *ucli.Command, dir string, args ...string) (stdout, stderr []byte, runErr error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	argv := append([]string{"rancher", "--config", dir}, args...)
	runErr = root.Run(t.Context(), argv)
	return out.Bytes(), errOut.Bytes(), runErr
}

// TestGrantRevokeEnvelope asserts the grant/revoke serialization contract from
// build-root through the runtime pipeline to the rendered JSON envelope for
// every command/scope combination in the design matrix.
func TestGrantRevokeEnvelope(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	provID := rbac.ProvisionedUserID("okta_user://ghost")
	provWarn := "no user exists for principal \"okta_user://ghost\" - Rancher will provision user \"" + provID + "\" when the binding applies"

	t.Run("grant project preview", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "u-abc", "--role", "project-member",
			"--project", "p-x", "--cluster", "c-x", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		require.NotEmpty(t, stdout, "stdout must have Effect envelope")

		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		assert.Empty(t, env.Warnings)
		assert.Equal(t, "create", env.Data.Action)
		assert.True(t, env.Data.Destructive, "grant must be Destructive")
		assert.Equal(t, []string{"/projectName", "/roleTemplateName", "/userName", "/userPrincipalName"},
			grDiffPaths(env.Data.Diff))
		diffVals := grDiffToMap(env.Data.Diff)
		assert.Equal(t, "c-x:p-x", diffVals["/projectName"])
		assert.Equal(t, "project-member", diffVals["/roleTemplateName"])
		assert.Equal(t, "u-abc", diffVals["/userName"])
		assert.Equal(t, "local://u-abc", diffVals["/userPrincipalName"])
		for _, e := range env.Data.Diff {
			assert.Nil(t, e.From, "Diff.From must be nil for a create")
		}
	})

	t.Run("grant cluster preview", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "u-abc", "--role", "cluster-owner",
			"--cluster", "c-x", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		assert.Equal(t, []string{"/clusterName", "/roleTemplateName", "/userName", "/userPrincipalName"},
			grDiffPaths(env.Data.Diff))
		diffVals := grDiffToMap(env.Data.Diff)
		assert.Equal(t, "c-x", diffVals["/clusterName"])
		assert.Equal(t, "cluster-owner", diffVals["/roleTemplateName"])
		assert.Equal(t, "u-abc", diffVals["/userName"])
		assert.Equal(t, "local://u-abc", diffVals["/userPrincipalName"])
		for _, e := range env.Data.Diff {
			assert.Nil(t, e.From, "Diff.From must be nil for a create")
		}
	})

	t.Run("grant global preview", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "u-abc", "--role", "admin",
			"--global", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		assert.Equal(t, []string{"/globalRoleName", "/userName", "/userPrincipalName"},
			grDiffPaths(env.Data.Diff))
		diffVals := grDiffToMap(env.Data.Diff)
		assert.Equal(t, "admin", diffVals["/globalRoleName"])
		assert.Equal(t, "u-abc", diffVals["/userName"])
		assert.Equal(t, "local://u-abc", diffVals["/userPrincipalName"])
		for _, e := range env.Data.Diff {
			assert.Nil(t, e.From, "Diff.From must be nil for a create")
			assert.NotEqual(t, "/projectName", e.Path, "global grant must not emit /projectName")
			assert.NotEqual(t, "/clusterName", e.Path, "global grant must not emit /clusterName")
		}
	})

	t.Run("grant provisioning preview project", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "okta_user://ghost", "--role", "project-member",
			"--project", "p-x", "--cluster", "c-x", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		// Exact complete Diff: scope, role, and subject entries; no /userName.
		assert.Equal(t, []string{"/projectName", "/roleTemplateName", "/userPrincipalName"},
			grDiffPaths(env.Data.Diff))
		diffVals := grDiffToMap(env.Data.Diff)
		assert.Equal(t, "c-x:p-x", diffVals["/projectName"])
		assert.Equal(t, "project-member", diffVals["/roleTemplateName"])
		assert.Equal(t, "okta_user://ghost", diffVals["/userPrincipalName"])
		require.Len(t, env.Warnings, 1)
		assert.Equal(t, provWarn, env.Warnings[0])
	})

	t.Run("grant provisioning preview cluster", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "okta_user://ghost", "--role", "cluster-owner",
			"--cluster", "c-x", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		// Exact complete Diff: scope, role, and subject entries; no /userName.
		assert.Equal(t, []string{"/clusterName", "/roleTemplateName", "/userPrincipalName"},
			grDiffPaths(env.Data.Diff))
		diffVals := grDiffToMap(env.Data.Diff)
		assert.Equal(t, "c-x", diffVals["/clusterName"])
		assert.Equal(t, "cluster-owner", diffVals["/roleTemplateName"])
		assert.Equal(t, "okta_user://ghost", diffVals["/userPrincipalName"])
		require.Len(t, env.Warnings, 1)
		assert.Equal(t, provWarn, env.Warnings[0])
	})

	t.Run("grant provisioning preview global", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "okta_user://ghost", "--role", "admin",
			"--global", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		// Exact complete Diff: role and subject entries; no /userName.
		assert.Equal(t, []string{"/globalRoleName", "/userPrincipalName"},
			grDiffPaths(env.Data.Diff))
		diffVals := grDiffToMap(env.Data.Diff)
		assert.Equal(t, "admin", diffVals["/globalRoleName"])
		assert.Equal(t, "okta_user://ghost", diffVals["/userPrincipalName"])
		require.Len(t, env.Warnings, 1)
		assert.Equal(t, provWarn, env.Warnings[0])
	})

	t.Run("confirmation gate", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		// No --apply, no --dry-run → CONFIRMATION_REQUIRED.
		stdout, stderr, runErr := grRun(t, root, dir,
			"grant", "u-abc", "--role", "project-member",
			"--project", "p-x", "--cluster", "c-x", "-o", "json")

		require.Error(t, runErr)
		assert.Empty(t, stdout, "stdout must be empty on CONFIRMATION_REQUIRED")
		var env grErrEnvelope
		require.NoError(t, json.Unmarshal(stderr, &env))
		assert.Equal(t, "CONFIRMATION_REQUIRED", env.Data.Error.Code)
		// Server must not have seen a non-dry-run create.
		assert.Equal(t, int32(0), opts.realCreates.Load(), "no real create must have occurred")
	})

	t.Run("grant project apply", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		// --apply --yes triggers real create; no --dry-run.
		stdout, _, runErr := grRun(t, root, dir,
			"grant", "u-abc", "--role", "project-member",
			"--project", "p-x", "--cluster", "c-x", "--apply", "--yes", "-o", "json")

		require.NoError(t, runErr)
		require.NotEmpty(t, stdout)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(stdout, &raw))
		assert.Equal(t, "ProjectRoleTemplateBinding", raw["kind"])
		assert.Equal(t, int32(1), opts.realCreates.Load(), "exactly one non-dry-run create")
	})

	t.Run("grant global apply", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "u-abc", "--role", "admin",
			"--global", "--apply", "--yes", "-o", "json")

		require.NoError(t, runErr)
		require.NotEmpty(t, stdout)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(stdout, &raw))
		assert.Equal(t, "GlobalRoleBinding", raw["kind"])
		assert.Equal(t, int32(1), opts.realCreates.Load(), "GRB created once")

		// The GRB POST body must carry generateName "grb-" and no namespace.
		require.NotNil(t, opts.lastGRBCreate, "GRB POST body must have been captured")
		meta, _ := opts.lastGRBCreate["metadata"].(map[string]any)
		require.NotNil(t, meta, "GRB POST body must have metadata")
		assert.Equal(t, "grb-", meta["generateName"], "GRB generateName must be \"grb-\"")
		_, hasNS := meta["namespace"]
		assert.False(t, hasNS, "GRB POST body must not have metadata.namespace")
	})

	t.Run("grant group preview project", func(t *testing.T) {
		opts := &serverOpts{}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"grant", "okta_group://gid", "--role", "project-member",
			"--project", "p-x", "--cluster", "c-x", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		assert.Empty(t, env.Warnings, "group subject: no provisioning warning")
		assert.Equal(t, []string{"/groupPrincipalName", "/projectName", "/roleTemplateName"},
			grDiffPaths(env.Data.Diff))
		diffVals := grDiffToMap(env.Data.Diff)
		assert.Equal(t, "okta_group://gid", diffVals["/groupPrincipalName"])
		assert.Equal(t, "c-x:p-x", diffVals["/projectName"])
		assert.Equal(t, "project-member", diffVals["/roleTemplateName"])
	})

	t.Run("revoke multi preview", func(t *testing.T) {
		opts := &serverOpts{
			prtbItems: []map[string]any{
				grBindingItem("p-x", "prtb-1", "rv1", "roleTemplateName", "project-member", "userName", "u-abc"),
				grBindingItem("p-x", "prtb-2", "rv2", "roleTemplateName", "project-viewer", "userName", "u-abc"),
			},
		}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		// No --role → both bindings matched; dry-run deletes them.
		stdout, _, runErr := grRun(t, root, dir,
			"revoke", "u-abc", "--project", "p-x", "--cluster", "c-x",
			"--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		assert.Equal(t, "delete", env.Data.Action)
		assert.Contains(t, env.Data.Target, "(2)", "aggregate target must include count")
		require.Len(t, env.Warnings, 1)
		assert.Contains(t, env.Warnings[0], "deleted sequentially")
	})

	t.Run("revoke cluster preview", func(t *testing.T) {
		opts := &serverOpts{
			crtbItems: []map[string]any{
				grBindingItem("c-x", "crtb-1", "rv1", "roleTemplateName", "cluster-owner", "userName", "u-abc"),
			},
		}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"revoke", "u-abc", "--role", "cluster-owner",
			"--cluster", "c-x", "--dry-run", "-o", "json")

		require.NoError(t, runErr)
		var env effectEnvelope
		require.NoError(t, json.Unmarshal(stdout, &env))
		assert.Equal(t, "Effect", env.Kind)
		assert.Equal(t, "delete", env.Data.Action)
		assert.Equal(t, "c-x", env.Data.Cluster)
		assert.Contains(t, env.Data.Target, "clusterroletemplatebindings/crtb-1")
	})

	t.Run("revoke partial failure", func(t *testing.T) {
		// Three bindings; the second real delete (sorted: prtb-1 < prtb-2 <
		// prtb-3) is forced to 409. This ensures details.unattempted is
		// non-empty, asserting the full deleted/failed/unattempted split.
		opts := &serverOpts{
			prtbItems: []map[string]any{
				grBindingItem("p-x", "prtb-1", "rv1", "roleTemplateName", "project-member", "userName", "u-abc"),
				grBindingItem("p-x", "prtb-2", "rv2", "roleTemplateName", "project-viewer", "userName", "u-abc"),
				grBindingItem("p-x", "prtb-3", "rv3", "roleTemplateName", "project-owner", "userName", "u-abc"),
			},
			failSecondRealDelete: true,
		}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		// --apply --yes, no --dry-run → Plan then Execute; second real delete 409s.
		stdout, stderr, runErr := grRun(t, root, dir,
			"revoke", "u-abc", "--project", "p-x", "--cluster", "c-x",
			"--apply", "--yes", "-o", "json")

		require.Error(t, runErr)
		assert.Empty(t, stdout, "stdout must be empty on partial failure")
		var env grErrEnvelope
		require.NoError(t, json.Unmarshal(stderr, &env))
		assert.Equal(t, "CONFLICT", env.Data.Error.Code)

		// Decode details: deleted/failed/unattempted
		var details rbac.RevokePartialFailure
		require.NoError(t, json.Unmarshal(env.Data.Error.Details, &details),
			"details must decode as RevokePartialFailure")
		assert.Equal(t, []string{"prtb-1"}, details.Deleted)
		assert.Equal(t, "prtb-2", details.Failed.Name)
		assert.Equal(t, []string{"prtb-3"}, details.Unattempted, "third binding must be unattempted")
	})

	t.Run("revoke global apply", func(t *testing.T) {
		opts := &serverOpts{
			grbItems: []map[string]any{
				grBindingItem("", "grb-1", "rv1", "globalRoleName", "admin", "userName", "u-abc"),
			},
		}
		srv := httptest.NewTLSServer(grHandler(opts))
		defer srv.Close()
		dir := grConfigDir(t, srv)

		stdout, _, runErr := grRun(t, root, dir,
			"revoke", "u-abc", "--role", "admin",
			"--global", "--apply", "--yes", "-o", "json")

		require.NoError(t, runErr)
		require.NotEmpty(t, stdout)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(stdout, &raw))
		assert.Equal(t, "GlobalRoleBindingDelete", raw["kind"])
		assert.Equal(t, int32(1), opts.realDeletes.Load(), "GRB deleted once")
	})
}
