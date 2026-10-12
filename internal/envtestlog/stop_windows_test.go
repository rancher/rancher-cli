//go:build windows && envtest

package envtestlog

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// controlPlaneChildren lists the etcd and kube-apiserver children of this
// process, by the file name of their program.
func controlPlaneChildren(t *testing.T) []string {
	t.Helper()
	children, err := childProcesses()
	require.NoError(t, err)
	var names []string
	for _, child := range children {
		if isControlPlaneImage(child.name) {
			names = append(names, child.name)
		}
	}
	return names
}

// TestStopEndsControlPlaneProcesses asserts that after Start, this process
// has etcd and kube-apiserver children, and that Stop returns nil and leaves
// none, and that the second env.Stop() inside Stop removes etcd's data
// directory, which envtest cannot remove while etcd runs. The first check
// keeps the second from passing on an image name that matches nothing; the
// names found are logged.
func TestStopEndsControlPlaneProcesses(t *testing.T) {
	isolate(t)
	env, _, err := Start(func() *envtest.Environment { return &envtest.Environment{} }, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Stop(env) })

	before := controlPlaneChildren(t)
	t.Logf("control plane children before Stop: %v", before)
	var etcd, apiServer int
	for _, name := range before {
		if strings.HasPrefix(strings.ToLower(name), "etcd") {
			etcd++
		} else {
			apiServer++
		}
	}
	require.GreaterOrEqual(t, etcd, 1, "no etcd child found")
	require.GreaterOrEqual(t, apiServer, 1, "no kube-apiserver child found")

	dataDir := env.ControlPlane.Etcd.DataDir
	require.NotEmpty(t, dataDir)
	require.DirExists(t, dataDir)

	require.NoError(t, Stop(env))
	assert.Empty(t, controlPlaneChildren(t))
	assert.NoDirExists(t, dataDir)
}
