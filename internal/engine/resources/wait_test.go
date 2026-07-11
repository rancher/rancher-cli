package resources

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
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
func (s *seqClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
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

func phaseObj(phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x"},
		"status":   map[string]any{"phase": phase},
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
	tests := []struct {
		expr    string
		want    waitCond
		wantErr bool
	}{
		{"state=migrating", waitCond{kind: "state", name: "migrating"}, false},
		{"condition=Ready", waitCond{kind: "condition", name: "Ready", status: "True"}, false},
		{"condition=Synced=False", waitCond{kind: "condition", name: "Synced", status: "False"}, false},
		{"condition=Ready=", waitCond{}, true},
		{"jsonpath={.foo}", waitCond{}, true},
		{"garbage", waitCond{}, true},
		{"", waitCond{}, true}, // empty is the caller's job to default via kinds.Kind.WaitDefault
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.expr, func(t *testing.T) {
			t.Parallel()
			got, err := parseWaitCondition(tt.expr)
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

func TestWaitSatisfiedFallsBackToPodPhase(t *testing.T) {
	t.Parallel()
	cond := waitCond{kind: "state", name: "running"}
	assert.True(t, waitSatisfied(phaseObj("Running"), cond), "state= matches status.phase case-insensitively when status.state is absent")
	assert.False(t, waitSatisfied(phaseObj("Pending"), cond))
}

func TestWaitTimeoutSummarizesPodPhase(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{phaseObj("Pending")}}
	cond := waitCond{kind: "state", name: "running"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Pending", "timeout message shows the real phase, not <no state>")
}

func TestWaitSatisfiedRancherStatePathUnchanged(t *testing.T) {
	t.Parallel()
	cond := waitCond{kind: "state", name: "active"}
	assert.True(t, waitSatisfied(stateObj("active", "no"), cond))
	assert.False(t, waitSatisfied(stateObj("active", "yes"), cond), "status.transitioning=yes still blocks satisfaction on the status.state path")
	assert.False(t, waitSatisfied(stateObj("provisioning", ""), cond))
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

func TestWaitForNotFoundFailsImmediately(t *testing.T) {
	client := &seqClient{objs: []*unstructured.Unstructured{nil}, errs: []error{clierr.New(clierr.CodeNotFound, "x not found")}}
	cond := waitCond{kind: "state", name: "active"}
	start := time.Now()
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 5*time.Second)
	elapsed := time.Since(start)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Less(t, elapsed, 5*time.Second, "NOT_FOUND must fail fast, not burn the timeout")
}

func TestWaitForRetriesTransientGetError(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{
		objs: []*unstructured.Unstructured{stateObj("provisioning", "yes"), nil, stateObj("active", "no")},
		errs: []error{nil, errors.New("transient network blip"), nil},
	}
	cond := waitCond{kind: "state", name: "active"}
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "active", mustState(got))
	assert.Equal(t, 3, client.calls)
}

func TestWaitForTimeoutWithNoSuccessfulGetCarriesNoStateObserved(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{nil}, errs: []error{errors.New("down")}}
	cond := waitCond{kind: "state", name: "active"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 20*time.Millisecond)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.True(t, ce.Retryable)
	assert.Contains(t, err.Error(), "no state observed")
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

func TestGetWaitRejectsNonWaitableKindWithoutFor(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "secret")
	require.Empty(t, k.WaitDefault, "secret must stay defaultless for this test to mean anything")
	cmd := NewGet(k, &recFactory{}).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "ns"}, b: map[string]bool{"wait": true}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Error(), "no default wait condition")
}

func TestGetWaitOnNonWaitableKindHonorsExplicitFor(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "secret")
	client := &memClient{items: []unstructured.Unstructured{*stateObj("active", "")}}
	cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns", "for": "state=active",
	}, b: map[string]bool{"wait": true}}, nil)
	require.NoError(t, err)
}

func TestGetWaitJobBareUsesConditionComplete(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "job")
	require.Equal(t, "condition=Complete", k.WaitDefault)
	client := &memClient{items: []unstructured.Unstructured{*conditionObj("Complete", "True")}}
	cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns",
	}, b: map[string]bool{"wait": true}}, nil)
	require.NoError(t, err)
}

func TestGetWaitStatefulSetBareRejectsWithoutFor(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "statefulset")
	require.Empty(t, k.WaitDefault, "statefulset has no established condition/state convention")
	cmd := NewGet(k, &recFactory{}).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), input{args: []string{"web"}, s: map[string]string{"cluster": "prod", "namespace": "ns"}, b: map[string]bool{"wait": true}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestGetWaitStatefulSetExplicitForProceeds(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "statefulset")
	client := &memClient{items: []unstructured.Unstructured{*stateObj("active", "")}}
	cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(), input{args: []string{"web"}, s: map[string]string{
		"cluster": "prod", "namespace": "ns", "for": "state=active",
	}, b: map[string]bool{"wait": true}}, nil)
	require.NoError(t, err)
}
