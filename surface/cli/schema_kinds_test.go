package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaManifest runs the real root's `schema -o json` and decodes the
// envelope's data into the typed manifest — never string-matching the
// rendered JSON. Not parallel-safe (t.Setenv).
func schemaManifest(t *testing.T) runtime.Manifest {
	t.Helper()
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))

	var env struct {
		Data runtime.Manifest `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	return env.Data
}

// TestSchemaKindVerbSurfaceMatchesRegisteredCommands is the SE-17 invariant:
// over the full kinds × {get,patch,delete} domain, a verb whose state is
// available OR refused has a registered generated command "<verb> <name>"
// (a refusal is a registered command that declines at Plan), and an absent
// verb has none — both directions, derived from manifest data alone.
func TestSchemaKindVerbSurfaceMatchesRegisteredCommands(t *testing.T) {
	m := schemaManifest(t)
	require.Len(t, m.Kinds, len(kinds.All()), "every kind-table row must reach the manifest")

	registered := map[string]bool{}
	for _, c := range m.Commands {
		registered[c.Name] = true
	}
	verbs := []string{"get", "patch", "delete"}
	for _, k := range m.Kinds {
		require.Len(t, k.Verbs, len(verbs), "kind %q must state all three generated verbs", k.Name)
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

// TestSchemaKindsManifestFields pins the SE-8 kind rows: the refused verb
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
	assert.Equal(t, string(safety.TierCRD), get.Tier)
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
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	}
	out := schemaKinds([]kinds.Kind{row}, nil)
	require.Len(t, out, 1)

	del := out[0].Verbs["delete"]
	assert.Equal(t, runtime.VerbAbsent, del.State)
	assert.Empty(t, del.Note, "no NoDeleteReason means a plain absence")
	assert.Empty(t, del.Tier, "an absent verb carries no binding")
	assert.Empty(t, del.Stability)

	assert.Equal(t, runtime.VerbAvailable, out[0].Verbs["get"].State)
	assert.Equal(t, runtime.VerbAvailable, out[0].Verbs["patch"].State)
}
