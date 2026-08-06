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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func conditionObjMsg(condType, status, message string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x"},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": condType, "status": status, "message": message},
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
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, false)
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
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Pending", "timeout message shows the real phase, not <no state>")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "retry with a longer --timeout or inspect the resource for what is holding it back", ce.Hint)
}

func TestWaitSatisfiedRancherStatePathUnchanged(t *testing.T) {
	t.Parallel()
	cond := waitCond{kind: "state", name: "active"}
	assert.True(t, waitSatisfied(stateObj("active", "no"), cond))
	assert.False(t, waitSatisfied(stateObj("active", "yes"), cond), "status.transitioning=yes still blocks satisfaction on the status.state path")
	assert.False(t, waitSatisfied(stateObj("provisioning", ""), cond))
}

// TestWaitSatisfiedStatusStateIsCaseSensitive is the round-2 repro for
// finding 4: the case-insensitive compare belongs only to the status.phase
// fallback. status.state is Rancher's own vocabulary (lowercase, e.g.
// "active") and must stay an exact match — a case-insensitive compare
// there would let a "state=Active" wait spuriously match "active" today,
// then wrongly match "aCtIvE" tomorrow.
func TestWaitSatisfiedStatusStateIsCaseSensitive(t *testing.T) {
	t.Parallel()
	cond := waitCond{kind: "state", name: "Active"}
	assert.False(t, waitSatisfied(stateObj("active", "no"), cond), "status.state must be an exact match, not case-insensitive")
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
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 5*time.Second, false)
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
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, false)
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
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond, false)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.True(t, ce.Retryable)
	assert.Contains(t, err.Error(), "provisioning")
	assert.Equal(t, "retry with a longer --timeout or inspect the resource for what is holding it back", ce.Hint)
}

func TestWaitForNotFoundFailsImmediately(t *testing.T) {
	client := &seqClient{objs: []*unstructured.Unstructured{nil}, errs: []error{clierr.New(clierr.CodeNotFound, "x not found")}}
	cond := waitCond{kind: "state", name: "active"}
	start := time.Now()
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 5*time.Second, false)
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
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, false)
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
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 20*time.Millisecond, false)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.True(t, ce.Retryable)
	assert.Contains(t, err.Error(), "no state observed")
	assert.Equal(t, "retry with a longer --timeout or inspect the resource for what is holding it back", ce.Hint)
}

// TestGetClusterBareWaitAppliesRowDefault is IC-11's command-boundary pin:
// a bare `get cluster <name> --wait` (no --for) applies the row's
// WaitDefault end to end — the command resolves the expression from the
// kind row, parses it, and polls until the Ready condition flips. The
// polling fake satisfies the condition only on the THIRD Get, so a
// non-polling shortcut or a wrong default expression fails loudly.
func TestGetClusterBareWaitAppliesRowDefault(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	k := mustKind(t, "cluster")
	require.Equal(t, "condition=Ready", k.WaitDefault, "the row default this test proves is applied")

	client := &seqClient{objs: []*unstructured.Unstructured{
		conditionObj("Ready", "False"),
		conditionObj("Ready", "False"),
		conditionObj("Ready", "True"),
	}}
	cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
	res, err := cmd.Execute(context.Background(), input{
		args: []string{"c-abc"},
		b:    map[string]bool{"wait": true},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, 3, client.calls, "the wait must POLL to the flip, not read once")
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

// TestWaitTimeoutConditionWithMessageParenthesized asserts that when the
// awaited condition carries a non-empty message, the timeout error shows it
// parenthesized: Ready=Unknown (waiting for probes: calico, kube-apiserver).
func TestWaitTimeoutConditionWithMessageParenthesized(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		conditionObjMsg("Ready", "Unknown", "waiting for probes: calico, kube-apiserver"),
	}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ready=Unknown (waiting for probes: calico, kube-apiserver)")
}

// TestWaitTimeoutConditionMessageSanitized pins defense-in-depth: a condition
// message bearing control characters (e.g. an ANSI escape or a Unicode bidi
// override) is stripped before it enters the timeout error. The sink is the
// JSON error envelope (which escapes control chars anyway), so this is
// consistency with the text-rendering sanitizer, not an exploitable hole.
func TestWaitTimeoutConditionMessageSanitized(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	// \x1b[2J is an ANSI clear-screen sequence: only the ESC byte (0x1b) is a
	// C0 control character and is stripped; the printable "[2J" suffix remains.
	// \u202e (U+202E) is a Unicode bidi override (Cf category) and is stripped.
	client := &seqClient{objs: []*unstructured.Unstructured{
		conditionObjMsg("Ready", "Unknown", "bad\x1b[2Jmsg with \u202ebidi"),
	}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad[2Jmsg with bidi", "printable chars around the escape are preserved")
	assert.NotContains(t, err.Error(), "\x1b", "ANSI escape byte must be stripped")
	assert.NotContains(t, err.Error(), "\u202e", "bidi override must be stripped")
}

// TestWaitTimeoutConditionWithoutMessageUnchanged asserts that when the
// awaited condition has no message, the timeout error keeps the current shape.
func TestWaitTimeoutConditionWithoutMessageUnchanged(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		conditionObj("Ready", "Unknown"),
	}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ready=Unknown")
	assert.NotContains(t, err.Error(), "Ready=Unknown (",
		"an empty condition message must not open a parenthesized suffix")
}

func TestConditionStatusFirstMatchWins(t *testing.T) {
	t.Parallel()
	obj := map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "False", "message": "first"},
		map[string]any{"type": "Ready", "status": "True", "message": "second"},
	}}}

	status, message, ok := conditionStatus(obj, "Ready")
	require.True(t, ok)
	assert.Equal(t, "False", status, "first matching condition wins, not the last")
	assert.Equal(t, "first", message)
}

// multiConditionObj builds an object carrying several status.conditions
// entries, each given as [type, status, message].
func multiConditionObj(conds ...[3]string) *unstructured.Unstructured {
	list := make([]any, 0, len(conds))
	for _, c := range conds {
		list = append(list, map[string]any{"type": c[0], "status": c[1], "message": c[2]})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x"},
		"status":   map[string]any{"conditions": list},
	}}
}

// TestWaitForFailsFastOnStalled pins the Fleet GitRepo shape from live
// exercise 8: a failed clone leaves Ready=True AND Stalled=True, and the
// wait must fail with the Stalled message instead of declaring the Ready
// condition satisfied.
func TestWaitForFailsFastOnStalled(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		multiConditionObj(
			[3]string{"Ready", "True", ""},
			[3]string{"Stalled", "True", "authentication required: Repository not found."},
		),
	}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 5*time.Second, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "x stalled: authentication required")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.False(t, ce.Retryable)
}

func TestWaitForStalledFalseDoesNotTrip(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		multiConditionObj(
			[3]string{"Ready", "True", ""},
			[3]string{"Stalled", "False", ""},
		),
	}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, true)
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestWaitForExplicitStalledTargetStillWaitable(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		multiConditionObj([3]string{"Stalled", "True", "boom"}),
	}}
	cond := waitCond{kind: "condition", name: "Stalled", status: "True"}
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, true)
	require.NoError(t, err, "waiting FOR Stalled must not be aborted by the Stalled fail-fast")
	require.NotNil(t, got)
}

func TestWaitForStalledEmptyMessageRendersConditionShape(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	client := &seqClient{objs: []*unstructured.Unstructured{
		multiConditionObj(
			[3]string{"Ready", "True", ""},
			[3]string{"Stalled", "True", ""},
		),
	}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "x stalled: condition Stalled=True")
}

// TestWaitForSkipsUnsettledGenerations pins the exercise-11 settle gate: an
// object whose status.observedGeneration lags metadata.generation carries
// conditions from an older spec — the wait must keep polling instead of
// trusting a satisfied condition or a Stalled abort on it.
func TestWaitForSkipsUnsettledGenerations(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	stale := multiConditionObj([3]string{"Ready", "True", ""})
	stale.SetGeneration(2)
	require.NoError(t, unstructured.SetNestedField(stale.Object, int64(1), "status", "observedGeneration"))

	settledObj := multiConditionObj([3]string{"Ready", "True", ""})
	settledObj.SetGeneration(2)
	require.NoError(t, unstructured.SetNestedField(settledObj.Object, int64(2), "status", "observedGeneration"))

	client := &seqClient{objs: []*unstructured.Unstructured{stale, settledObj}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	got, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 2, client.calls, "the unsettled first observation must not satisfy the wait")

	// The stale generation must not trigger the Stalled abort either.
	staleStalled := multiConditionObj([3]string{"Stalled", "True", "old spec"})
	staleStalled.SetGeneration(3)
	require.NoError(t, unstructured.SetNestedField(staleStalled.Object, int64(2), "status", "observedGeneration"))
	ready := multiConditionObj([3]string{"Ready", "True", ""})
	client2 := &seqClient{objs: []*unstructured.Unstructured{staleStalled, ready}}
	got, err = waitFor(context.Background(), client2, schema.GroupVersionResource{}, "", "x", cond, time.Second, true)
	require.NoError(t, err, "a stale Stalled=True must not abort the wait")
	require.NotNil(t, got)
}

// TestWaitTimeoutWithUnsettledObservationShowsCaveat pins the round-2 High-1
// caveat: when the only observation seen before timeout had a lagging
// observedGeneration, the timeout message must include the generation caveat
// so operators know the reported state is stale.
func TestWaitTimeoutWithUnsettledObservationShowsCaveat(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = orig }()

	unsettled := multiConditionObj([3]string{"Ready", "False", ""})
	unsettled.SetGeneration(2)
	require.NoError(t, unstructured.SetNestedField(unsettled.Object, int64(1), "status", "observedGeneration"))

	client := &seqClient{objs: []*unstructured.Unstructured{unsettled}}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	_, err := waitFor(context.Background(), client, schema.GroupVersionResource{}, "", "x", cond, 30*time.Millisecond, false)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.True(t, ce.Retryable)
	assert.Contains(t, err.Error(), "not yet reconciled",
		"timeout with only an unsettled observation must include the generation caveat")
}

// TestSettledAbsentFieldIsSettled documents the deliberate choice: a
// generation-1 object with no status.observedGeneration field (the omitempty
// zero case for a controller that never ran) passes straight through settled as
// true. The freshSuccessWarning advisory covers this birth window instead.
func TestSettledAbsentFieldIsSettled(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x", "generation": int64(1)},
		"status":   map[string]any{},
	}}
	assert.True(t, settled(obj), "absent observedGeneration (omitempty zero) must be treated as settled")
}

// TestFreshSuccessWarningElapsedFormatting pins the round-2 Low-4 edge cases:
// a sub-second-old object and a future creationTimestamp (server/client clock
// skew) both render "immediately after creation" instead of "0s after creation"
// or a negative duration.
func TestFreshSuccessWarningElapsedFormatting(t *testing.T) {
	t.Parallel()

	subSecond := multiConditionObj([3]string{"Ready", "True", ""})
	subSecond.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-100 * time.Millisecond)))
	w := freshSuccessWarning(subSecond, "gitrepo", "repo-1", "")
	assert.Contains(t, w, "immediately after creation",
		"sub-second elapsed must render as 'immediately after creation'")
	assert.NotContains(t, w, "0s after creation")

	skewed := multiConditionObj([3]string{"Ready", "True", ""})
	skewed.SetCreationTimestamp(metav1.NewTime(time.Now().Add(5 * time.Second)))
	w2 := freshSuccessWarning(skewed, "gitrepo", "repo-1", "")
	assert.Contains(t, w2, "immediately after creation",
		"future creationTimestamp (clock skew) must render as 'immediately after creation'")
}

// TestFreshSuccessWarning pins the advisory covering the window generation
// tracking cannot close (exercise-11 S-09.3): a kstatus controller may
// flip Stalled only after retry backoff, so a success on a seconds-old
// object warns; an aged object does not.
func TestFreshSuccessWarning(t *testing.T) {
	t.Parallel()
	young := multiConditionObj([3]string{"Ready", "True", ""})
	young.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-2 * time.Second)))
	w := freshSuccessWarning(young, "gitrepo", "repo-1", "team-a")
	assert.Contains(t, w, "re-check shortly")
	assert.Contains(t, w, "rancher get gitrepo repo-1 --namespace team-a",
		"the re-check must target the namespace the wait resolved")
	assert.NotContains(t, w, "conditions", "the advisory is condition-agnostic — state= waits satisfy it too")

	clusterScoped := freshSuccessWarning(young, "gitrepo", "repo-1", "")
	assert.Contains(t, clusterScoped, "rancher get gitrepo repo-1")
	assert.NotContains(t, clusterScoped, "--namespace")

	old := multiConditionObj([3]string{"Ready", "True", ""})
	old.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-5 * time.Minute)))
	assert.Empty(t, freshSuccessWarning(old, "gitrepo", "repo-1", "team-a"))

	noTimestamp := multiConditionObj([3]string{"Ready", "True", ""})
	assert.Empty(t, freshSuccessWarning(noTimestamp, "gitrepo", "repo-1", "team-a"))
}
