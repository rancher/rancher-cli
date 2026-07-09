package resources

import (
	"context"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// seqClient serves a fixed sequence of Get results (objects and/or errors), one
// per call; the last entry repeats once the sequence is exhausted, so a
// timeout test can poll past the scripted objects.
type seqClient struct {
	objs  []*unstructured.Unstructured
	errs  []error
	calls int
}

func (s *seqClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	i := s.calls
	if i >= len(s.objs) {
		i = len(s.objs) - 1
	}
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return s.objs[i], nil
}
func (s *seqClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (s *seqClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (s *seqClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (s *seqClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}
func (s *seqClient) Logs(context.Context, string, string, string, int) (string, error) {
	return "", nil
}

func stateObj(state, transitioning string) *unstructured.Unstructured {
	status := map[string]any{"state": state}
	if transitioning != "" {
		status["transitioning"] = transitioning
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x"},
		"status":   status,
	}}
}

func errorObj(msg string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x"},
		"status": map[string]any{
			"transitioning":        "error",
			"transitioningMessage": msg,
		},
	}}
}

func conditionObj(t, status string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x"},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": t, "status": status},
			},
		},
	}}
}

func TestParseWaitCondition(t *testing.T) {
	t.Parallel()
	mgmt := kinds.Kind{Scope: kinds.Management}
	down := kinds.Kind{Scope: kinds.Downstream}
	tests := []struct {
		expr    string
		k       kinds.Kind
		want    waitCond
		wantErr bool
	}{
		{"", mgmt, waitCond{kind: "state", name: "active"}, false},
		{"", down, waitCond{kind: "condition", name: "Ready", status: "True"}, false},
		{"state=migrating", mgmt, waitCond{kind: "state", name: "migrating"}, false},
		{"condition=Ready", down, waitCond{kind: "condition", name: "Ready", status: "True"}, false},
		{"condition=Synced=False", down, waitCond{kind: "condition", name: "Synced", status: "False"}, false},
		{"jsonpath={.foo}", down, waitCond{}, true},
		{"garbage", down, waitCond{}, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.expr+"/"+string(tt.k.Scope), func(t *testing.T) {
			t.Parallel()
			got, err := parseWaitCondition(tt.expr, tt.k)
			if tt.wantErr {
				var ce *clierr.Error
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWaitForPollsUntilState(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		stateObj("provisioning", "yes"),
		stateObj("active", "no"),
	}}
	cond := waitCond{kind: "state", name: "active"}
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "active", mustState(got))
	assert.Equal(t, 2, client.calls)
}

func TestWaitForFailsFastOnTransitioningError(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		stateObj("provisioning", "yes"),
		errorObj("quota exceeded"),
	}}
	cond := waitCond{kind: "state", name: "active"}
	start := time.Now()
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 5*time.Second)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota exceeded")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.False(t, ce.Retryable)
	assert.Less(t, elapsed, 5*time.Second, "fail-fast must not burn the timeout")
}

func TestWaitForConditionStatus(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		conditionObj("Ready", "False"),
		conditionObj("Ready", "True"),
	}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 2, client.calls)
}

func TestWaitForTimeoutCarriesLastState(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{stateObj("provisioning", "yes")}}
	cond := waitCond{kind: "state", name: "active"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.True(t, ce.Retryable)
	assert.Contains(t, err.Error(), "provisioning")
}

func mustState(obj *unstructured.Unstructured) string {
	s, _, _ := unstructured.NestedString(obj.Object, "status", "state")
	return s
}

func TestGetWaitRequiresName(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	cmd := NewGet(k, &recFactory{}).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), input{s: map[string]string{"cluster": "prod"}, b: map[string]bool{"wait": true}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Error(), "--wait requires a resource name")
}

func TestGetForWithoutWait(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")
	cmd := NewGet(k, &recFactory{}).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "ns", "for": "state=active"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}
