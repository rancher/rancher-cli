package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaManifest returns the full schema manifest directly from the registry
// for manifest-invariant assertions. The bare schema -o json now returns the
// TOC view, not the full manifest, so tests that need the full manifest
// (SE-17/SE-18 invariants) bypass the presentation layer entirely.
func schemaManifest(t *testing.T) runtime.Manifest {
	t.Helper()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	return m
}

// TestSchemaKindVerbSurfaceMatchesRegisteredCommands is the SE-17 invariant:
// over the full kinds × {get,patch,delete} domain, a verb whose state is
// available OR refused has a registered generated command "<verb> <name>"
// (a refusal is a registered command that declines at Plan), and an absent
// verb has none - both directions, derived from manifest data alone.
// Kinds may carry additional verb states beyond the three core generated verbs
// (e.g. a "create" refused state for kinds that reject create-resource); those
// extra entries are not checked here.
func TestSchemaKindVerbSurfaceMatchesRegisteredCommands(t *testing.T) {
	m := schemaManifest(t)
	require.Len(t, m.Kinds, len(kinds.All()), "every kind-table row must reach the manifest")

	registered := map[string]bool{}
	for _, c := range m.Commands {
		registered[c.Name] = true
	}
	verbs := []string{"get", "patch", "delete"}
	for _, k := range m.Kinds {
		for _, verb := range verbs {
			vs, ok := k.Verbs[verb]
			require.True(t, ok, "kind %q missing verb %q", k.Name, verb)
			name := verb + " " + k.Name
			switch vs.State {
			case runtime.VerbAvailable, runtime.VerbRefused:
				assert.True(t, registered[name], "%q is %s but has no registered command", name, vs.State)
			case runtime.VerbAbsent:
				assert.False(t, registered[name], "%q is absent but a command is registered", name)
			default:
				t.Errorf("kind %q verb %q has invalid state %q", k.Name, verb, vs.State)
			}
		}
	}
}

// TestSchemaManagementTokenCreateRefused verifies that the schema manifest
// surfaces the management-token "create" verb as refused, so agents can
// discover that create-resource rejects v3 Token manifests without running a
// live command. Only kinds that actively refuse create via NoCreateViaManifest
// carry this verb state; the absence of "create" in other kinds' verb maps
// does not mean create is available - it means no reviewed restriction was
// recorded (agents should treat absence as unknown, not available).
func TestSchemaManagementTokenCreateRefused(t *testing.T) {
	m := schemaManifest(t)
	var mt *runtime.SchemaKind
	for i := range m.Kinds {
		if m.Kinds[i].Key == "managementtoken" {
			mt = &m.Kinds[i]
			break
		}
	}
	require.NotNil(t, mt, "managementtoken kind must appear in the manifest")
	create, ok := mt.Verbs["create"]
	require.True(t, ok, "managementtoken must carry a 'create' verb state in the manifest")
	assert.Equal(t, runtime.VerbRefused, create.State)
	assert.NotEmpty(t, create.Note, "refused create must carry the restriction note")
	assert.Equal(t, string(safety.ContractInternal), create.Contract)
	assert.Equal(t, string(safety.Deprecated), create.Stability)
}

// TestSchemaKindsManifestFields asserts the SE-8 kind rows: the refused verb
// (CRT patch), the override-added verb (provisioning-cluster delete), a
// reasoned absence (cluster delete), and the identity/routing fields.
func TestSchemaKindsManifestFields(t *testing.T) {
	m := schemaManifest(t)
	byName := map[string]runtime.SchemaKind{}
	for _, k := range m.Kinds {
		byName[k.Name] = k
	}

	crt, ok := byName["clusterregistrationtoken"]
	require.True(t, ok)
	patch := crt.Verbs["patch"]
	assert.Equal(t, runtime.VerbRefused, patch.State)
	assert.Equal(t, "read-only: consumed, not edited", patch.Note)

	pc, ok := byName["provisioning-cluster"]
	require.True(t, ok)
	assert.Equal(t, runtime.VerbAvailable, pc.Verbs["delete"].State,
		"delete provisioning-cluster ships via override despite Deletable: false")

	cluster, ok := byName["cluster"]
	require.True(t, ok)
	cdel := cluster.Verbs["delete"]
	assert.Equal(t, runtime.VerbAbsent, cdel.State)
	row, found := kinds.Lookup("managementcluster")
	require.True(t, found)
	require.NotEmpty(t, row.NoDeleteReason, "the cluster row carries a reviewed no-delete reason")
	assert.Equal(t, row.NoDeleteReason, cdel.Note)

	pod := byName["pod"]
	assert.Equal(t, "pod", pod.Key)
	assert.Contains(t, pod.Aliases, "po")
	assert.Equal(t, "", pod.Group)
	assert.Equal(t, "v1", pod.Version)
	assert.Equal(t, "pods", pod.Resource)
	assert.Equal(t, string(kinds.Downstream), pod.Scope)
	assert.Equal(t, string(kinds.NSPlain), pod.NamespacePolicy)
	assert.True(t, pod.Completable)
	get := pod.Verbs["get"]
	assert.Equal(t, runtime.VerbAvailable, get.State)
	assert.Equal(t, string(safety.ContractInternal), get.Contract)
	assert.Equal(t, string(safety.Beta), get.Stability)

	gitrepo := byName["gitrepo"]
	assert.Equal(t, "fleet-default", gitrepo.DefaultNamespace)
	assert.Equal(t, "condition=Ready", gitrepo.WaitDefault)

	assert.True(t, byName["secret"].Sensitive)
}

// TestSchemaKindsBuilderPlainAbsence exercises the one shape absent from
// production data: a non-deletable row WITHOUT a reviewed reason renders a
// plain absence (empty note). Builder-level, on a synthetic row.
func TestSchemaKindsBuilderPlainAbsence(t *testing.T) {
	t.Parallel()
	row := kinds.Kind{
		Key: "widget", Name: "widget", Deletable: false,
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	}
	out := schemaKinds([]kinds.Kind{row}, nil)
	require.Len(t, out, 1)

	del := out[0].Verbs["delete"]
	assert.Equal(t, runtime.VerbAbsent, del.State)
	assert.Empty(t, del.Note, "no NoDeleteReason means a plain absence")
	assert.Empty(t, del.Contract, "an absent verb carries no binding")
	assert.Empty(t, del.Stability)

	assert.Equal(t, runtime.VerbAvailable, out[0].Verbs["get"].State)
	assert.Equal(t, runtime.VerbAvailable, out[0].Verbs["patch"].State)
}
