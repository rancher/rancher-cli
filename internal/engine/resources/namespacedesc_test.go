package resources

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namespaceFlagOf returns the --namespace flag of flags.
func namespaceFlagOf(t *testing.T, flags []runtime.ArgSpec) runtime.ArgSpec {
	t.Helper()
	for _, f := range flags {
		if f.Name == FlagNamespace {
			return f
		}
	}
	require.FailNow(t, "no --namespace flag", "flags: %v", flagNamesOf(flags))
	return runtime.ArgSpec{}
}

func flagNamesOf(flags []runtime.ArgSpec) []string {
	names := make([]string, 0, len(flags))
	for _, f := range flags {
		names = append(names, f.Name)
	}
	return names
}

func mustLookup(t *testing.T, name string) kinds.Kind {
	t.Helper()
	k, ok := kinds.Lookup(name)
	require.True(t, ok, name)
	return k
}

// TestProjectRoleTemplateBindingNamespaceDescription asserts the
// --namespace description of the get, patch and delete of
// projectroletemplatebinding.
func TestProjectRoleTemplateBindingNamespaceDescription(t *testing.T) {
	t.Parallel()
	k := mustLookup(t, "projectroletemplatebinding")
	const want = "the project's backing `namespace` on the local cluster; or pass --project with --cluster"
	for verb, flags := range map[string][]runtime.ArgSpec{
		"get":    getFlags(k),
		"patch":  patchFlags(k),
		"delete": deleteFlags(k),
	} {
		assert.Equal(t, want, namespaceFlagOf(t, flags).Desc, verb)
	}
}

// TestClusterRegistrationTokenNamespaceFlagRequired asserts --namespace is
// optional on get and required on patch of clusterregistrationtoken, and the
// only flag before the wait flags on get.
func TestClusterRegistrationTokenNamespaceFlagRequired(t *testing.T) {
	t.Parallel()
	k := mustLookup(t, "clusterregistrationtoken")
	get := getFlags(k)
	assert.Equal(t, []string{FlagNamespace, flagWait, flagFor, flagTimeout}, flagNamesOf(get))
	assert.False(t, namespaceFlagOf(t, get).Required)
	patch := patchFlags(k)
	assert.Equal(t, []string{FlagNamespace, flagPatch, flagPatchFile, flagPatchType}, flagNamesOf(patch))
	assert.True(t, namespaceFlagOf(t, patch).Required)
}

// TestWorkspaceRowFlagOrder asserts the flags of get, patch and delete of
// gitrepo: --namespace first, never required.
func TestWorkspaceRowFlagOrder(t *testing.T) {
	t.Parallel()
	k := mustLookup(t, "gitrepo")
	assert.Equal(t, []string{FlagNamespace, flagWait, flagFor, flagTimeout}, flagNamesOf(getFlags(k)))
	assert.Equal(t, []string{FlagNamespace, flagPatch, flagPatchFile, flagPatchType}, flagNamesOf(patchFlags(k)))
	assert.Equal(t, []string{FlagNamespace, flagWait, flagTimeout}, flagNamesOf(deleteFlags(k)))
	for verb, flags := range map[string][]runtime.ArgSpec{
		"get":    getFlags(k),
		"patch":  patchFlags(k),
		"delete": deleteFlags(k),
	} {
		assert.False(t, namespaceFlagOf(t, flags).Required, verb)
	}
}

// namespaceCanBeOutsideAWorkspace lists each workspace row whose objects can be
// outside a Fleet workspace, with the reason. A row listed here sets
// NamespaceDesc; every other workspace row shows the Fleet workspace
// description with its DefaultNamespace.
var namespaceCanBeOutsideAWorkspace = map[string]string{
	"bundledeployment":      "Fleet keeps a BundleDeployment in the cluster namespace of its Fleet cluster",
	"etcdsnapshot":          "Rancher keeps the etcd snapshots of a cluster it does not provision in the namespace named by the cluster's id",
	"capicluster":           "a CAPI cluster that Rancher does not provision can be created in any namespace",
	"capimachine":           "a CAPI machine is in the namespace of its CAPI cluster, which can be any namespace when Rancher does not provision the cluster",
	"capimachineset":        "a CAPI machine set is in the namespace of its CAPI cluster, which can be any namespace when Rancher does not provision the cluster",
	"capimachinedeployment": "a CAPI machine deployment is in the namespace of its CAPI cluster, which can be any namespace when Rancher does not provision the cluster",
	"uiplugin":              "Rancher installs extensions into cattle-ui-plugin-system, not a Fleet workspace, and the Rancher UI reads the UIPlugins of every namespace",
}

// TestEveryManagementRowWithAPlainOrWorkspaceNamespaceDescribesIt asserts
// each Management row with a plain namespace sets NamespaceDesc, each row
// with a workspace namespace has a DefaultNamespace, and no other row sets
// NamespaceDesc.
func TestEveryManagementRowWithAPlainOrWorkspaceNamespaceDescribesIt(t *testing.T) {
	t.Parallel()
	for _, k := range kinds.All() {
		managementNamespace := k.Scope == kinds.Management && (k.Policy == kinds.NSPlain || k.Policy == kinds.NSWorkspace)
		switch {
		case k.Scope == kinds.Management && k.Policy == kinds.NSPlain:
			assert.NotEmpty(t, k.NamespaceDesc, "%s is Management with a plain namespace: set NamespaceDesc", k.Key)
		case k.Scope == kinds.Management && k.Policy == kinds.NSWorkspace:
			assert.NotEmpty(t, k.DefaultNamespace, "%s is Management with a workspace namespace: set DefaultNamespace", k.Key)
		}
		if !managementNamespace {
			assert.Empty(t, k.NamespaceDesc, "%s sets NamespaceDesc but is not Management with a plain or workspace namespace", k.Key)
		}
	}
}

// TestWorkspaceRowsThatCanBeOutsideAFleetWorkspaceAreListedWithAReason asserts
// the workspace rows that set NamespaceDesc are exactly the rows of
// namespaceCanBeOutsideAWorkspace, and each of them describes the value
// differently from the Fleet workspace description.
func TestWorkspaceRowsThatCanBeOutsideAFleetWorkspaceAreListedWithAReason(t *testing.T) {
	t.Parallel()
	found := map[string]bool{}
	for _, k := range kinds.All() {
		if k.Policy != kinds.NSWorkspace {
			continue
		}
		found[k.Key] = true
		reason, listed := namespaceCanBeOutsideAWorkspace[k.Key]
		if !listed {
			assert.Empty(t, k.NamespaceDesc, "%s sets NamespaceDesc: list it in namespaceCanBeOutsideAWorkspace with the reason its objects can be outside a Fleet workspace", k.Key)
			continue
		}
		assert.NotEmpty(t, reason, k.Key)
		assert.NotEmpty(t, k.NamespaceDesc, "%s is listed in namespaceCanBeOutsideAWorkspace and needs its own NamespaceDesc", k.Key)
		assert.NotEqual(t, fleetWorkspaceDesc(k), k.NamespaceDesc, k.Key)
	}
	for key := range namespaceCanBeOutsideAWorkspace {
		assert.True(t, found[key], "%s is listed in namespaceCanBeOutsideAWorkspace but is not a workspace row", key)
	}
}

func fleetWorkspaceDesc(k kinds.Kind) string {
	return "Fleet workspace (`namespace`) (default: " + k.DefaultNamespace + ")"
}

// TestManagementRowNamespaceDescriptions asserts, for each row it lists, the
// --namespace description, whether the flag is required, that it is the first
// flag and that its alias is -n. It reads the get and patch flags of each row
// and the delete flags of each row that has a delete.
func TestManagementRowNamespaceDescriptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key      string
		want     string
		required map[string]bool
	}{
		{"clusterregistrationtoken", "cluster `id` (c-...), the namespace of its cluster registration tokens", map[string]bool{"get": false, "patch": true}},
		{"bundledeployment", "cluster namespace of a Fleet cluster (`namespace`): its status.namespace, such as cluster-fleet-...", map[string]bool{"get": false, "patch": false}},
		{"etcdsnapshot", "`namespace` of the cluster's snapshots: for a cluster Rancher provisions, the namespace of its provisioning cluster (default: fleet-default); otherwise the cluster's id (c-...)", map[string]bool{"get": false, "patch": false}},
		{"capimachine", "`namespace` of the CAPI object: for a cluster Rancher provisions, the namespace of its provisioning cluster (default: fleet-default); otherwise the namespace the CAPI cluster was created in", map[string]bool{"get": false, "patch": false}},
		{"gitrepo", "Fleet workspace (`namespace`) (default: fleet-default)", map[string]bool{"get": false, "patch": false, "delete": false}},
		{"amazonec2config", "Fleet workspace (`namespace`) (default: fleet-default)", map[string]bool{"get": false, "patch": false, "delete": false}},
	} {
		k := mustLookup(t, tc.key)
		verbs := map[string][]runtime.ArgSpec{
			"get":   getFlags(k),
			"patch": patchFlags(k),
		}
		if k.Deletable {
			verbs["delete"] = deleteFlags(k)
		}
		assert.Equal(t, len(tc.required), len(verbs), "%s: the verbs of the row", tc.key)
		for verb, flags := range verbs {
			f := namespaceFlagOf(t, flags)
			assert.Equal(t, tc.want, f.Desc, "%s %s", verb, tc.key)
			assert.Equal(t, tc.required[verb], f.Required, "%s %s", verb, tc.key)
			assert.Equal(t, FlagNamespace, flags[0].Name, "%s %s", verb, tc.key)
			assert.Equal(t, []string{"n"}, f.Aliases, "%s %s", verb, tc.key)
		}
	}
}
