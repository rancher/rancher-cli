package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestShortNamespaceFlagReachesEveryDeclarationFamily asserts that -n x on
// each family of commands that declare --namespace reaches the command as
// --namespace x: the command's request path holds /namespaces/ns1/. It
// asserts no exit code, since the recorder's canned answer ends the
// commands differently. Not parallel: it sets the process environment and
// the captured command line.
func TestShortNamespaceFlagReachesEveryDeclarationFamily(t *testing.T) {
	restoreRawArgv(t)
	rec := methodRecorderEnv(t)
	manifest := filepath.Join(t.TempDir(), "cm.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm1\n"), 0o600))
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"get", []string{"get", "pod", "web", "-n", "ns1", "--cluster", "local", "-o", "json"}},
		{"delete", []string{"delete", "pod", "web", "-n", "ns1", "--cluster", "local", "-o", "json"}},
		{"patch", []string{"patch", "pod", "web", "-n", "ns1", "--cluster", "local", "--patch", `[{"op":"add","path":"/metadata/labels","value":{"a":"b"}}]`, "-o", "json"}},
		{"create resource", []string{"create", "resource", "-f", manifest, "-n", "ns1", "--cluster", "local", "-o", "json"}},
		{"create secret", []string{"create", "secret", "s1", "-n", "ns1", "--cluster", "local", "--from-literal", "k=v", "-o", "json"}},
		{"inspect pod", []string{"inspect", "pod", "web", "-n", "ns1", "--cluster", "local", "-o", "json"}},
		{"inspect deployment", []string{"inspect", "deployment", "web", "-n", "ns1", "--cluster", "local", "-o", "json"}},
		{"usage", []string{"usage", "-n", "ns1", "--cluster", "local", "-o", "json"}},
		{"cluster scale", []string{"cluster", "scale", "prod", "-n", "ns1", "--pool", "p", "--nodes", "2", "-o", "json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			rec.take()
			_, _, _ = runBounded(t, root, tc.argv)
			requests := rec.take()
			assert.True(t, slices.ContainsFunc(requests, func(r string) bool { return strings.Contains(r, "/namespaces/ns1/") }), "no request path holds /namespaces/ns1/: %v", requests)
		})
	}
}

// TestInspectPodStillRequiresNamespace asserts that inspect pod, typed with
// neither -n nor --namespace, is refused as VALIDATION at exit 2 naming
// --namespace, and makes no request. Not parallel: it sets the process
// environment and the captured command line.
func TestInspectPodStillRequiresNamespace(t *testing.T) {
	restoreRawArgv(t)
	rec := methodRecorderEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code := runBounded(t, root, []string{"inspect", "pod", "web", "--cluster", "c1", "-o", "json"})
	assert.Equal(t, 2, code)
	assert.Empty(t, rec.take(), "no request")
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--namespace")
}
