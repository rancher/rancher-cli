package extres

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// pcrInput is extres's shared `in` test double plus a bools map - `in`
// itself always returns false from Bool, which cannot express
// --password-stdin.
type pcrInput struct {
	args  []string
	s     map[string]string
	bools map[string]bool
}

func (i pcrInput) String(k string) string  { return i.s[k] }
func (i pcrInput) Bool(k string) bool      { return i.bools[k] }
func (i pcrInput) Strings(string) []string { return nil }
func (i pcrInput) IsSet(string) bool       { return false }
func (i pcrInput) Args() []string          { return i.args }

func passwordUpdatedStatus() map[string]any {
	return map[string]any{
		"conditions": []any{
			map[string]any{"type": "PasswordUpdated", "status": "True"},
		},
	}
}

func planPCR(t *testing.T, cmd runtime.Command, input runtime.Input) (runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "create password-change-request must be a MutatingCommand")
	_, exec, err := mc.Plan(t.Context(), input)
	return exec, err
}

// TestCreatePasswordChangeRequestPlanDisplayIdentity asserts the dry-run
// identity conformance requirement for this bespoke Effect-building
// command: DisplayKind/DisplayName are non-empty at Plan time, naming the
// target user (the request resource itself carries no name at all).
func TestCreatePasswordChangeRequestPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	client := &fakeClient{status: passwordUpdatedStatus()}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	mc := cmd.(runtime.MutatingCommand)
	eff, exec, err := mc.Plan(t.Context(), pcrInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, "password-change-request", eff.DisplayKind)
	assert.Equal(t, "u-abc123", eff.DisplayName)
}

// TestCreatePasswordChangeRequestBuildsSpec proves the request carries
// exactly userID/newPassword, and that the plaintext password never lands
// anywhere the fake didn't put it itself (i.e. only in spec.newPassword).
func TestCreatePasswordChangeRequestBuildsSpec(t *testing.T) {
	t.Parallel()
	client := &fakeClient{status: passwordUpdatedStatus()}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	exec, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)

	require.NotNil(t, client.created)
	assert.Equal(t, "ext.cattle.io/v1", client.created.GetAPIVersion())
	assert.Equal(t, "PasswordChangeRequest", client.created.GetKind())
	userID, _, _ := unstructured.NestedString(client.created.Object, "spec", "userID")
	assert.Equal(t, "u-abc123", userID)
	newPassword, _, _ := unstructured.NestedString(client.created.Object, "spec", "newPassword")
	assert.Equal(t, "aVeryLongPassword123", newPassword, "trailing newline from stdin must be trimmed")

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "PasswordChangeRequestCompleted", res.Kind)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"userID": "u-abc123"}, data,
		"Data must never carry the raw created object - its spec.newPassword echoes the plaintext")
}

// TestCreatePasswordChangeRequestPasswordStdinRequired proves the flag is
// mandatory: there is no --password flag, ever.
func TestCreatePasswordChangeRequestPasswordStdinRequired(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("whatever\n")})
	_, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}})
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Nil(t, client.created, "no network call before the password-stdin gate")
}

// TestCreatePasswordChangeRequestUnconfirmedIsCommittedFailure proves the
// server's own synchronous confirmation is checked, not inferred from a
// bare successful create - an object that comes back without a True
// PasswordUpdated condition is a committed-context failure naming the
// target user, not a silent success.
func TestCreatePasswordChangeRequestUnconfirmedIsCommittedFailure(t *testing.T) {
	t.Parallel()
	client := &fakeClient{status: map[string]any{"conditions": []any{}}}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	exec, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)

	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "u-abc123", details.ResourceName)
}

// fakeFileSource is extres's own minimal engine.FileSource test double.
type fakeFileSource struct {
	body []byte
	err  error
}

func (f fakeFileSource) Read(string) ([]byte, error) { return f.body, f.err }

// TestCreatePasswordChangeRequestOldServerDryRunNotFoundRefusesFriendly
// proves the Plan dry-run's NOT_FOUND on a server without the workflow
// reclassifies into the same CAPABILITY_UNSUPPORTED refusal the discovery
// gate produces - never a raw NOT_FOUND about an unrelated resource.
func TestCreatePasswordChangeRequestOldServerDryRunNotFoundRefusesFriendly(t *testing.T) {
	t.Parallel()
	client := &fakeClient{createErr: clierr.New(clierr.CodeNotFound, "the server could not find the requested resource")}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	_, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Contains(t, ce.Message, "password-change-request resource is absent")
}

// TestCreatePasswordChangeRequestDryRunOtherErrorsPassThrough proves only
// NOT_FOUND reclassifies: a real, actionable failure keeps its own
// classification.
func TestCreatePasswordChangeRequestDryRunOtherErrorsPassThrough(t *testing.T) {
	t.Parallel()
	client := &fakeClient{createErr: clierr.New(clierr.CodeRBAC, "denied")}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	_, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
}
