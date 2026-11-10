package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	verbPatch  = "patch"
	verbCreate = "create"
	verbDelete = "delete"
)

// refusalCase is one refused verb of one kind row, typed as a complete command
// line so that only the refusal can stop it.
type refusalCase struct {
	key  string
	verb string
	// name is the row's Name, the name the command line takes for the kind.
	name string
	// args is the command line after --config <dir>. A patch line carries a
	// valid inline patch and a create line carries a manifest of the kind; no
	// other check can refuse it first.
	args []string
	// manifest, when set, is the manifest file the create line reads; the
	// token {file} in args names it.
	manifest string
	// want is the whole message of the refusal, written out in full.
	want string
}

const refusalPatchBody = `[{"op":"add","path":"/metadata/labels/x","value":"y"}]`

func patchRefusal(key, name, want string, flags ...string) refusalCase {
	args := append([]string{verbPatch, name, "x", "--patch", refusalPatchBody}, flags...)
	return refusalCase{key: key, verb: verbPatch, name: name, args: args, want: want}
}

func createRefusal(key, name, manifest, want string) refusalCase {
	return refusalCase{
		key: key, verb: verbCreate, name: name, manifest: manifest, want: want,
		args: []string{"create", "resource", "-f", "{file}", "--cluster", "local", "--namespace", "default"},
	}
}

func deleteRefusal(key, name, want string) refusalCase {
	return refusalCase{
		key: key, verb: verbDelete, name: name, want: want,
		args: []string{"delete", name, "x"},
	}
}

func manifestOf(apiVersion, kind string, namespaced bool) string {
	m := "apiVersion: " + apiVersion + "\nkind: " + kind + "\nmetadata:\n  name: x\n"
	if namespaced {
		m += "  namespace: default\n"
	}
	return m
}

func unknownDelete(name string) string {
	return `unknown command "` + name + `"`
}

// refusalCases lists every refused verb of every row, by key. Each entry uses
// the row's Name, the name the command line takes for the kind, and holds the
// whole expected message written out in full, never built from the row's
// fields.
func refusalCases() []refusalCase {
	return []refusalCase{
		patchRefusal("clusterregistrationtoken", "clusterregistrationtoken",
			"clusterregistrationtoken is read-only: patching a cluster registration token is not supported; create a new one instead",
			"--namespace", "default"),
		patchRefusal("managementtoken", "management-token",
			"management-token: patch is not supported - Rancher sets these token fields itself (the key, a TTL within the server's maximum TTL, and the user the token belongs to); the CLI does not change them; use the Rancher API or UI to manage token fields"),

		patchRefusal("clusterrepo", "clusterrepo",
			"clusterrepo: patch is not supported - this CLI does not change repositories; read it with "+clierr.Rancher+" get 'clusterrepo' --cluster 'local'",
			"--cluster", "local"),
		patchRefusal("app", "app",
			"app: patch is not supported - Rancher creates and manages app objects from the Helm release; this CLI does not change them; read it with "+clierr.Rancher+" get 'app' --cluster 'local' --namespace 'default'",
			"--cluster", "local", "--namespace", "default"),
		patchRefusal("operation", "operation",
			"operation: patch is not supported - Rancher creates and manages operation objects; this CLI does not change them; read it with "+clierr.Rancher+" get 'operation' --cluster 'local' --namespace 'default'",
			"--cluster", "local", "--namespace", "default"),

		patchRefusal("etcdsnapshot", "etcd-snapshot",
			"etcd-snapshot: patch is not supported - Rancher creates and manages etcd snapshot objects from the snapshot files; this CLI does not change them; read it with "+clierr.Rancher+" get 'etcd-snapshot' --namespace 'default'",
			"--namespace", "default"),

		createRefusal("clusterrepo", "clusterrepo", manifestOf("catalog.cattle.io/v1", "ClusterRepo", false),
			"cannot create a clusterrepo from a manifest; this CLI does not add repositories"),
		createRefusal("app", "app", manifestOf("catalog.cattle.io/v1", "App", true),
			"cannot create an app from a manifest; Rancher creates an app when it installs a chart; to install one (<release> is the Helm release name, <c> the cluster, <ns> the namespace, <repo> the ClusterRepo and <chart> the chart), run `"+clierr.Rancher+" app install <release> --cluster <c> --namespace <ns> --repo <repo> --chart <chart> --dry-run` and add --apply after reading the preview"),
		createRefusal("operation", "operation", manifestOf("catalog.cattle.io/v1", "Operation", true),
			"cannot create an operation from a manifest; Rancher creates an operation for each Helm command it runs"),

		createRefusal("etcdsnapshot", "etcd-snapshot", manifestOf("rke.cattle.io/v1", "ETCDSnapshot", true),
			"cannot create an etcd-snapshot from a manifest; Rancher creates an etcd snapshot object for each snapshot file; this CLI does not take snapshots"),

		deleteRefusal("clusterrepo", "clusterrepo",
			`unknown command "clusterrepo"; clusterrepo cannot be deleted - this CLI does not delete repositories`),
		deleteRefusal("app", "app",
			"unknown command \"app\"; app cannot be deleted - deleting an app object leaves its Helm release installed; to uninstall it (<name> is its name, <c> the cluster and <ns> its namespace), run `rancher app uninstall <name> --cluster <c> --namespace <ns> --dry-run` and add --apply --yes after reading the preview"),
		deleteRefusal("operation", "operation",
			`unknown command "operation"; operation cannot be deleted - this CLI does not delete operations`),

		deleteRefusal("etcdsnapshot", "etcd-snapshot",
			`unknown command "etcd-snapshot"; etcd-snapshot cannot be deleted - an etcd snapshot object is only Rancher's record of a snapshot file on a node or in S3; deleting the object leaves the file in place`),

		createRefusal("token", "token", manifestOf("ext.cattle.io/v1", "Token", false),
			"cannot create a token from a manifest; create tokens with '"+clierr.Rancher+" create token --dry-run' and add --apply --yes after reading the preview (the token's value is printed once)"),
		createRefusal("managementtoken", "management-token", manifestOf("management.cattle.io/v3", "Token", false),
			"cannot create a management-token from a manifest; create tokens with '"+clierr.Rancher+" create token --dry-run' and add --apply --yes after reading the preview (it creates an ext.cattle.io Token)"),
		createRefusal("kubeconfig", "kubeconfig", manifestOf("ext.cattle.io/v1", "Kubeconfig", false),
			"cannot create a kubeconfig from a manifest; create kubeconfigs with '"+clierr.Rancher+" create kubeconfig --dry-run' and add --apply --yes after reading the preview (the kubeconfig is printed once)"),

		deleteRefusal("managementcluster", "cluster",
			`unknown command "cluster"; management clusters cannot be deleted directly; to delete a provisioning cluster (<name> is its name), run this and add --apply --yes after reading the preview: `+clierr.Rancher+` delete provisioning-cluster <name> --dry-run`),
		deleteRefusal("node", "node", unknownDelete("node")),
		deleteRefusal("persistentvolume", "persistentvolume", unknownDelete("persistentvolume")),
		deleteRefusal("event", "event", unknownDelete("event")),
		deleteRefusal("bundle", "bundle", unknownDelete("bundle")),
		deleteRefusal("bundledeployment", "bundledeployment", unknownDelete("bundledeployment")),
		deleteRefusal("clustergroup", "clustergroup", unknownDelete("clustergroup")),
		deleteRefusal("fleetcluster", "fleet-cluster", unknownDelete("fleet-cluster")),
		deleteRefusal("fleetworkspace", "fleet-workspace", unknownDelete("fleet-workspace")),
		deleteRefusal("setting", "setting", unknownDelete("setting")),
		deleteRefusal("feature", "feature", unknownDelete("feature")),
		deleteRefusal("roletemplate", "roletemplate", unknownDelete("roletemplate")),
		deleteRefusal("globalrole", "globalrole", unknownDelete("globalrole")),
		deleteRefusal("capicluster", "capi-cluster", unknownDelete("capi-cluster")),
		deleteRefusal("capimachine", "machine", unknownDelete("machine")),
		deleteRefusal("capimachineset", "machineset", unknownDelete("machineset")),
		deleteRefusal("capimachinedeployment", "machinedeployment", unknownDelete("machinedeployment")),
		deleteRefusal("nodedriver", "nodedriver", unknownDelete("nodedriver")),
		deleteRefusal("clusterregistrationtoken", "clusterregistrationtoken", unknownDelete("clusterregistrationtoken")),
	}
}

// expectedRefusals derives, from the row table and the override table, the
// (key, verb) pairs that are refused: patch when the row sets NoPatchReason or
// the override lists a patch refusal, create when the row sets
// NoCreateViaManifest, delete when the row is not deletable and the override
// has no delete.
func expectedRefusals() map[string]bool {
	out := map[string]bool{}
	ov := kindOverrides()
	for _, k := range kinds.All() {
		o := ov[k.Key]
		if _, refused := o.refusals[verbPatch]; refused || k.NoPatchReason != "" {
			out[k.Key+" "+verbPatch] = true
		}
		if k.NoCreateViaManifest != "" {
			out[k.Key+" "+verbCreate] = true
		}
		if !k.Deletable && o.del == nil {
			out[k.Key+" "+verbDelete] = true
		}
	}
	return out
}

// TestRefusalListMatchesTheTables asserts that the list of refused verbs
// equals, both ways, the verbs expectedRefusals derives from the two tables:
// the test fails when a kind gains or loses a refusal until the list is
// updated.
func TestRefusalListMatchesTheTables(t *testing.T) {
	listed := map[string]bool{}
	for _, c := range refusalCases() {
		id := c.key + " " + c.verb
		assert.False(t, listed[id], "%s is listed twice", id)
		listed[id] = true
		row, ok := kinds.ByKey(c.key)
		require.True(t, ok, "%s is not a row key", c.key)
		assert.Equal(t, row.Name, c.name, "%s uses the row's Name", id)
	}
	assert.Equal(t, expectedRefusals(), listed)
}

// TestEveryRefusedVerbIsATypedValidationError asserts that each refused verb of
// each row, run as a complete command line, ends with a VALIDATION error whose
// message equals the entry's expected message, and that no request reaches the
// server. Not parallel: run sets the captured command line and the test sets
// the process environment.
func TestEveryRefusedVerbIsATypedValidationError(t *testing.T) {
	exitEnv(t)
	var requests atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	dir := grConfigDir(t, srv)

	for _, c := range refusalCases() {
		t.Run(c.key+" "+c.verb, func(t *testing.T) {
			before := requests.Load()
			args := slices.Clone(c.args)
			if c.manifest != "" {
				file := filepath.Join(t.TempDir(), "manifest.yaml")
				require.NoError(t, os.WriteFile(file, []byte(c.manifest), 0o600))
				if i := slices.Index(args, "{file}"); i >= 0 {
					args[i] = file
				}
			}
			var out, errOut bytes.Buffer
			root := grRoot(t)
			root.Writer, root.ErrWriter = &out, &errOut
			got := run(t.Context(), root, append([]string{"rancher", "--config", dir}, args...))

			require.NotNil(t, got.err, "stderr: %s", errOut.String())
			assert.Equal(t, clierr.CodeValidation, got.err.Code)
			assert.Equal(t, c.want, got.err.Message)
			assert.Equal(t, before, requests.Load(), "the refusal must come before any request")
		})
	}
}
