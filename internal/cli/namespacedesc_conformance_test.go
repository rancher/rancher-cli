package cli

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genericNamespaceDesc is the description of --namespace on a command that
// does not say what its value is.
var genericNamespaceDesc = engine.NamespaceFlag(false).Desc

// genericNamespaceCommands lists the commands, outside the generated get,
// patch and delete of a downstream row, whose --namespace is an ordinary
// namespace of the typed cluster and keeps the generic description, each
// with the reason.
var genericNamespaceCommands = map[string]string{
	"create resource":    "the namespace of a manifest that names none; the kind comes from the manifest",
	"inspect pod":        "an ordinary namespace on the typed cluster",
	"inspect deployment": "an ordinary namespace on the typed cluster",
	"usage":              "an ordinary namespace on the typed cluster",
}

// manifestNamespaceFlags returns the --namespace flag of every command of
// the manifest that has one, by command name.
func manifestNamespaceFlags(t *testing.T) map[string]runtime.ArgSpec {
	t.Helper()
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	out := map[string]runtime.ArgSpec{}
	for _, c := range m.Commands {
		for _, f := range c.Flags {
			if f.Name == engine.FlagNamespace {
				out[c.Name] = f
			}
		}
	}
	return out
}

// generatedVerbKind returns the kind of a generated get, patch or delete,
// whose name is the verb and the kind's name.
func generatedVerbKind(command string) (kinds.Kind, bool) {
	verb, name, found := strings.Cut(command, " ")
	if !found || !isGeneratedVerb(verb) {
		return kinds.Kind{}, false
	}
	k, ok := kinds.Lookup(name)
	return k, ok && k.Name == name
}

// TestGeneratedVerbsDescribeTheirNamespaceValue asserts the --namespace
// description of every generated get, patch and delete in the manifest. A
// Management row with a plain namespace shows its NamespaceDesc. A workspace row
// shows its NamespaceDesc when it sets one, and the Fleet workspace description
// with its default otherwise. A downstream row shows the generic description.
// The test reads the commands in the manifest; the overrides of a kind are
// checked together with the generated verbs.
func TestGeneratedVerbsDescribeTheirNamespaceValue(t *testing.T) {
	flags := manifestNamespaceFlags(t)
	checked := map[string]int{}
	for command, f := range flags {
		k, ok := generatedVerbKind(command)
		if !ok {
			continue
		}
		switch {
		case k.Scope == kinds.Management && k.Policy == kinds.NSPlain:
			require.NotEmpty(t, k.NamespaceDesc, command)
			assert.Equal(t, k.NamespaceDesc, f.Desc, command)
			checked["management plain"]++
		case k.Scope == kinds.Management && k.Policy == kinds.NSWorkspace:
			derived := "Fleet workspace (`namespace`) (default: " + k.DefaultNamespace + ")"
			if k.NamespaceDesc == "" {
				assert.Equal(t, derived, f.Desc, command)
				checked["workspace"]++
				break
			}
			assert.Equal(t, k.NamespaceDesc, f.Desc, command)
			assert.NotEqual(t, derived, f.Desc, command)
			checked["workspace with its own description"]++
		case k.Scope == kinds.Downstream && k.Policy == kinds.NSPlain:
			assert.Equal(t, genericNamespaceDesc, f.Desc, command)
			checked["downstream plain"]++
		default:
			assert.Failf(t, "unexpected --namespace", "%s: a row with this scope and namespace policy takes no --namespace", command)
		}
	}
	for _, kind := range []string{"management plain", "workspace", "workspace with its own description", "downstream plain"} {
		assert.Positive(t, checked[kind], "no command of the manifest was checked for the group %q", kind)
	}
}

// TestGenericNamespaceDescriptionIsOnlyOnOrdinaryNamespaces asserts every
// command of the manifest whose --namespace has the generic description is a
// generated verb of a downstream row or is listed with the reason its value
// is an ordinary namespace, and that each listed command exists and still
// has the generic description.
func TestGenericNamespaceDescriptionIsOnlyOnOrdinaryNamespaces(t *testing.T) {
	flags := manifestNamespaceFlags(t)
	for command, f := range flags {
		if f.Desc != genericNamespaceDesc {
			continue
		}
		if k, ok := generatedVerbKind(command); ok && k.Scope == kinds.Downstream && k.Policy == kinds.NSPlain {
			continue
		}
		reason, listed := genericNamespaceCommands[command]
		assert.True(t, listed, "%q describes --namespace as %q: say what the value is, or list the command with the reason it is an ordinary namespace", command, genericNamespaceDesc)
		assert.NotEmpty(t, reason, command)
	}
	for command, reason := range genericNamespaceCommands {
		assert.NotEmpty(t, reason, command)
		f, ok := flags[command]
		require.True(t, ok, "%q is listed but has no --namespace in the manifest", command)
		assert.Equal(t, genericNamespaceDesc, f.Desc, "%q is listed but describes --namespace differently", command)
	}
}

// TestNamespaceCompletionOffersNoCandidatesForManagementRows asserts no source
// claims the --namespace value of a generated verb of a Management row, with and
// without a typed --cluster. The value is a namespace of the local cluster, and
// the shared source lists the namespaces of the typed cluster instead. The
// --namespace of a generated verb of a downstream row is still claimed.
func TestNamespaceCompletionOffersNoCandidatesForManagementRows(t *testing.T) {
	flags := manifestNamespaceFlags(t)
	var management, downstream int
	for command := range flags {
		k, ok := generatedVerbKind(command)
		if !ok {
			continue
		}
		for _, cluster := range []string{"", "c-aaa11"} {
			req := completion.Request{Command: command, Flag: engine.FlagNamespace, Cluster: cluster}
			_, _, owned := claimLiveSource(req, runtime.Descriptor{Name: command}, nil, flagSources, kindSources, positionalKinds)
			switch k.Scope {
			case kinds.Management:
				assert.False(t, owned, "%s --namespace with --cluster %q is claimed by a source", command, cluster)
				management++
			case kinds.Downstream:
				assert.True(t, owned, "%s --namespace with --cluster %q is claimed by no source", command, cluster)
				downstream++
			}
		}
	}
	assert.Positive(t, management)
	assert.Positive(t, downstream)
}
