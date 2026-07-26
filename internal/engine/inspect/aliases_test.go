package inspect_test

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/inspect"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
)

func TestInspectPodAliasesMatchKindsTable(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("pod")
	assert.True(t, ok)
	d := inspect.NewPod(nil).Describe()
	assert.Equal(t, k.Aliases, d.Aliases,
		"inspect pod Aliases must equal the kinds table pod aliases")
}

func TestInspectDeploymentAliasesMatchKindsTable(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("deployment")
	assert.True(t, ok)
	d := inspect.NewDeployment(nil).Describe()
	assert.Equal(t, k.Aliases, d.Aliases,
		"inspect deployment Aliases must equal the kinds table deployment aliases")
}

func TestInspectProjectAliasesMatchKindsTable(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("project")
	assert.True(t, ok)
	d := inspect.NewInspectProject(nil).Describe()
	assert.Equal(t, k.Aliases, d.Aliases,
		"inspect project Aliases must equal the kinds table project aliases")
}

func TestInspectClusterAliasesMatchKindsTable(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("cluster")
	assert.True(t, ok)
	d := inspect.NewInspectCluster(nil, nil).Describe()
	assert.Equal(t, k.Aliases, d.Aliases,
		"inspect cluster Aliases must equal the kinds table cluster aliases")
}
