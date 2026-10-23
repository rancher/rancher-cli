package provisioning

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPauseAutoscalingCapability(t *testing.T) {
	t.Parallel()
	d := NewPauseAutoscaling(nil).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Equal(t, "cluster-autoscaling", d.Capability.Feature)
}

func TestPauseAutoscalingSetsAnnotation(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", nil, false)}
	cmd := NewPauseAutoscaling(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive, "provisioningcluster is Sensitive: every family mutation is Destructive")

	v, _, _ := unstructured.NestedString(client.updated.Object, "metadata", "annotations", clusterAutoscalerPausedAnnotation)
	assert.Equal(t, "true", v)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "cluster autoscaler paused for c1")
}

func TestPauseAutoscalingAlreadyPausedChangesNothing(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", nil, true)}
	cmd := NewPauseAutoscaling(fakeFactory{c: client})

	_, exec, err := planFamily(t, cmd, ccInput{a: []string{"c1"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "already paused for c1")
}

func TestResumeAutoscalingCapability(t *testing.T) {
	t.Parallel()
	d := NewResumeAutoscaling(nil).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Equal(t, "cluster-autoscaling", d.Capability.Feature)
}

func TestResumeAutoscalingClearsAnnotation(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", nil, true)}
	cmd := NewResumeAutoscaling(fakeFactory{c: client})

	_, exec, err := planFamily(t, cmd, ccInput{a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	_, found, _ := unstructured.NestedString(client.updated.Object, "metadata", "annotations", clusterAutoscalerPausedAnnotation)
	assert.False(t, found)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "cluster autoscaler resumed for c1")
}

func TestResumeAutoscalingNotPausedChangesNothing(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", nil, false)}
	cmd := NewResumeAutoscaling(fakeFactory{c: client})

	_, exec, err := planFamily(t, cmd, ccInput{a: []string{"c1"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "not paused for c1; nothing to resume")
}

func TestPauseAutoscalingClusterArgRequired(t *testing.T) {
	t.Parallel()
	cmd := NewPauseAutoscaling(fakeFactory{c: &familyClient{}})
	_, _, err := planFamily(t, cmd, ccInput{})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestPauseAutoscalingHasNoPoolFlag(t *testing.T) {
	t.Parallel()
	d := NewPauseAutoscaling(nil).Describe()
	for _, f := range d.Flags {
		assert.NotEqual(t, "pool", f.Name, "pause is cluster-wide only")
	}
}
