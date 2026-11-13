package resources

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// stalledOperation is a Stalled=True object that carries a release name and
// namespace in its status, as a Rancher operation does.
func stalledOperation(release, namespace string) *unstructured.Unstructured {
	obj := multiConditionObj([3]string{"Reconciling", "False", ""}, [3]string{"Stalled", "True", " exit code: 123"})
	status := obj.Object["status"].(map[string]any)
	if release != "" {
		status["releaseName"] = release
	}
	if namespace != "" {
		status["namespace"] = namespace
	}
	return obj
}

// waitStalled runs a wait on obj with the operation row's Stalled settings and
// returns the error it ends with.
func waitStalled(t *testing.T, obj *unstructured.Unstructured, rule stalledRule) *clierr.Error {
	t.Helper()
	var ce *clierr.Error
	withFastPoll(t)
	synctest.Test(t, func(t *testing.T) {
		client := &seqClient{objs: []*unstructured.Unstructured{obj}}
		cond := waitCond{kind: "condition", name: "Reconciling", status: "False"}
		_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "helm-operation-b85sw", cond, 5*time.Second, rule)
		require.ErrorAs(t, err, &ce)
	})
	return ce
}

func operationRule(t *testing.T, cluster string) stalledRule {
	t.Helper()
	k := mustKind(t, "operation")
	return stalledRule{fail: k.WaitFailsOnStalled, remedy: k.StalledRemedy, cluster: cluster}
}

// TestStalledOperationOffersTheGetAppCommand asserts the operation row's
// Stalled error is the generic stalled message with the command that shows the
// release, built from the observed status and the cluster as typed.
func TestStalledOperationOffersTheGetAppCommand(t *testing.T) {
	ce := waitStalled(t, stalledOperation("monitoring", "cattle-monitoring-system"), operationRule(t, "c-m-abc"))
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, "helm-operation-b85sw stalled:  exit code: 123", ce.Message)
	assert.Equal(t, clierr.Rancher+" get app 'monitoring' --cluster 'c-m-abc' --namespace 'cattle-monitoring-system'", ce.Hint)
}

// TestStalledOperationRemedyQuotesServerAndTypedValues asserts a release,
// namespace or cluster with a space or a quote is shell-quoted, and a release
// that starts with a dash ends the flags.
func TestStalledOperationRemedyQuotesServerAndTypedValues(t *testing.T) {
	ce := waitStalled(t, stalledOperation("my app", "my ns"), operationRule(t, "my cluster"))
	assert.Equal(t, clierr.Rancher+" get app 'my app' --cluster 'my cluster' --namespace 'my ns'", ce.Hint)

	ce = waitStalled(t, stalledOperation("-rel", "ns"), operationRule(t, "local"))
	assert.Equal(t, clierr.Rancher+" get app --cluster 'local' --namespace 'ns' -- '-rel'", ce.Hint)
}

// TestStalledOperationLeavesOutTheCommandForAValueNoShellReadsAsWritten asserts
// a release, namespace or typed cluster that cannot be printed as one shell
// word leaves the command out of the hint and puts the note in the message.
func TestStalledOperationLeavesOutTheCommandForAValueNoShellReadsAsWritten(t *testing.T) {
	for name, tc := range map[string]struct {
		release, namespace, cluster string
	}{
		"release apostrophe":       {"re'l", "ns", "c-m-abc"},
		"namespace right quote":    {"rel", "n\u2019s", "c-m-abc"},
		"cluster zero width space": {"rel", "ns", "c\u200b-m"},
		"release double backslash": {"re\\\\l", "ns", "c-m-abc"},
	} {
		t.Run(name, func(t *testing.T) {
			ce := waitStalled(t, stalledOperation(tc.release, tc.namespace), operationRule(t, tc.cluster))
			assert.Empty(t, ce.Hint)
			assert.Equal(t, "helm-operation-b85sw stalled:  exit code: 123; "+clierr.LeftOutNote, ce.Message)
		})
	}
}

// TestStalledOperationWithoutTypedClusterHasNoRemedy asserts the command is
// left out when no --cluster was typed, because get app requires it.
func TestStalledOperationWithoutTypedClusterHasNoRemedy(t *testing.T) {
	ce := waitStalled(t, stalledOperation("monitoring", "cattle-monitoring-system"), operationRule(t, ""))
	assert.Equal(t, "helm-operation-b85sw stalled:  exit code: 123", ce.Message)
	assert.Empty(t, ce.Hint)
}

// TestStalledOperationWithoutReleaseOrNamespaceHasNoRemedy asserts an operation
// whose status lacks either value gives the plain stalled error.
func TestStalledOperationWithoutReleaseOrNamespaceHasNoRemedy(t *testing.T) {
	for name, obj := range map[string]*unstructured.Unstructured{
		"no release":   stalledOperation("", "ns"),
		"no namespace": stalledOperation("rel", ""),
		"neither":      stalledOperation("", ""),
	} {
		t.Run(name, func(t *testing.T) {
			ce := waitStalled(t, obj, operationRule(t, "c-m-abc"))
			assert.Equal(t, "helm-operation-b85sw stalled:  exit code: 123", ce.Message)
			assert.Empty(t, ce.Hint)
		})
	}
}

// TestStalledRowWithoutRemedyKeepsItsError asserts a row with
// WaitFailsOnStalled and no remedy field gives the plain stalled error with no
// hint.
func TestStalledRowWithoutRemedyKeepsItsError(t *testing.T) {
	k := mustKind(t, "gitrepo")
	require.True(t, k.WaitFailsOnStalled)
	require.Nil(t, k.StalledRemedy)
	obj := multiConditionObj([3]string{"Ready", "True", ""}, [3]string{"Stalled", "True", "authentication required"})
	ce := waitStalled(t, obj, stalledRule{fail: k.WaitFailsOnStalled, remedy: k.StalledRemedy, cluster: "c-m-abc"})
	assert.Equal(t, "helm-operation-b85sw stalled: authentication required", ce.Message)
	assert.Empty(t, ce.Hint)
	assert.False(t, ce.Retryable)
}
