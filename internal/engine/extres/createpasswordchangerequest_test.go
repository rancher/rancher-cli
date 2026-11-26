package extres

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/secret"
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

func (i pcrInput) String(k string) string        { return i.s[k] }
func (i pcrInput) Bool(k string) bool            { return i.bools[k] }
func (i pcrInput) Strings(string) []string       { return nil }
func (i pcrInput) IsSet(string) bool             { return false }
func (i pcrInput) Secrets(string) []secret.Value { return nil }
func (i pcrInput) Args() []string                { return i.args }

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
	assert.Contains(t, ce.Message, "PasswordChangeRequest resource is absent")
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

// countingSource is a FileSource that serves body and counts the reads.
type countingSource struct {
	body  string
	reads *int
}

func (s countingSource) Read(string) ([]byte, error) {
	*s.reads++
	return []byte(s.body), nil
}

const currentPasswordWarning = "the dry-run does not check the current password. When the change is applied, Rancher checks it only if you lack " +
	"permission to change every user's password; with that permission, Rancher ignores line 1 and sets line 2 as the new password"

func bothPasswordFlags() map[string]bool {
	return map[string]bool{"current-password-stdin": true, "password-stdin": true}
}

func planPCREffect(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	return mc.Plan(t.Context(), input)
}

// TestCreatePasswordChangeRequestSendsTheCurrentPasswordWhenAsked asserts the
// request holds currentPassword, with userID and newPassword, only when
// --current-password-stdin is set, and that stdin is read once either way.
func TestCreatePasswordChangeRequestSendsTheCurrentPasswordWhenAsked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		bools map[string]bool
		stdin string
		want  map[string]any
	}{
		{"both flags", bothPasswordFlags(), "currentHorse1\naVeryLongPassword123\n",
			map[string]any{"userID": "u-abc123", "currentPassword": "currentHorse1", "newPassword": "aVeryLongPassword123"}},
		{"both flags with crlf", bothPasswordFlags(), "currentHorse1\r\naVeryLongPassword123\r\n",
			map[string]any{"userID": "u-abc123", "currentPassword": "currentHorse1", "newPassword": "aVeryLongPassword123"}},
		{"the new password only", map[string]bool{"password-stdin": true}, "aVeryLongPassword123\n",
			map[string]any{"userID": "u-abc123", "newPassword": "aVeryLongPassword123"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{status: passwordUpdatedStatus()}
			reads := 0
			cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, countingSource{body: tc.stdin, reads: &reads})
			_, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: tc.bools})
			require.NoError(t, err)
			spec, _, err := unstructured.NestedMap(client.created.Object, "spec")
			require.NoError(t, err)
			assert.Equal(t, tc.want, spec)
			assert.Equal(t, 1, reads, "stdin is read once")
		})
	}
}

// TestCreatePasswordChangeRequestWarnsOnlyWithTheCurrentPassword asserts the
// preview warns that the dry-run does not check the current password only
// when --current-password-stdin is set, and that the diff hides the value.
func TestCreatePasswordChangeRequestWarnsOnlyWithTheCurrentPassword(t *testing.T) {
	t.Parallel()
	client := &fakeClient{status: passwordUpdatedStatus()}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("currentHorse1\naVeryLongPassword123\n")})
	eff, _, err := planPCREffect(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: bothPasswordFlags()})
	require.NoError(t, err)
	assert.Equal(t, []string{currentPasswordWarning}, eff.Warnings)
	hidden := map[string]any{}
	for _, entry := range eff.Diff {
		hidden[entry.Path] = entry.To
	}
	assert.Equal(t, engine.Redacted, hidden["/spec/currentPassword"])
	assert.Equal(t, engine.Redacted, hidden["/spec/newPassword"])
	assert.Equal(t, "u-abc123", hidden["/spec/userID"])
	assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)

	cmd = NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
	eff, _, err = planPCREffect(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}})
	require.NoError(t, err)
	assert.Empty(t, eff.Warnings)
	for _, entry := range eff.Diff {
		assert.NotEqual(t, "/spec/currentPassword", entry.Path)
	}
}

// TestCreatePasswordChangeRequestRefusesBeforeAnyRequest asserts the refusals
// of the two flags and of the input: VALIDATION, no hint, no request, and a
// newline inside the password names the flag that sends the current
// password.
func TestCreatePasswordChangeRequestRefusesBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stdin, want string
		bools             map[string]bool
	}{
		{"the current password without the new one", "a\nb\n", "--current-password-stdin needs --password-stdin", map[string]bool{"current-password-stdin": true}},
		{"one line", "aVeryLongPassword123\n", "lines found: 1", bothPasswordFlags()},
		{"two lines for the new password only", "currentHorse1\naVeryLongPassword123\n",
			"the password read from stdin has more than one line; --password-stdin takes the password on one line. To send the current password too, pass --current-password-stdin and pipe two lines",
			map[string]bool{"password-stdin": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{}
			cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte(tc.stdin)})
			_, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: tc.bools})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Empty(t, ce.Hint)
			assert.Contains(t, ce.Message, tc.want)
			assert.Nil(t, client.created, "no request is sent")
		})
	}
}

// TestCreatePasswordChangeRequestMapsRancherRefusalsOnApply asserts the apply
// maps the error of the real create request, split on what the command sent,
// and that the dry-run's error is not mapped: Rancher's dry-run returns before
// both checks; the same text from it is not one of the two refusals.
func TestCreatePasswordChangeRequestMapsRancherRefusalsOnApply(t *testing.T) {
	t.Parallel()
	refused := classified(t, invalidCurrentPasswordStatus(""))
	notAuthorized := classified(t, notAuthorizedStatus(""))
	for _, tc := range []struct {
		name     string
		bools    map[string]bool
		stdin    string
		realErr  error
		wantCode string
		message  string
	}{
		{"a refused current password, one sent", bothPasswordFlags(), "currentHorse1\naVeryLongPassword123\n", refused, clierr.CodeValidation, refusedCurrentPassword},
		{"a refused current password, none sent", map[string]bool{"password-stdin": true}, "aVeryLongPassword123\n", refused, clierr.CodeValidation, requiresCurrentPassword},
		{"another user's password", bothPasswordFlags(), "currentHorse1\naVeryLongPassword123\n", notAuthorized, clierr.CodeRBAC, onlyYourOwnPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{realCreateErr: tc.realErr}
			cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte(tc.stdin)})
			exec, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: tc.bools})
			require.NoError(t, err, "the dry-run succeeds")
			_, err = exec(t.Context())
			ce, ok := err.(*clierr.Error)
			require.True(t, ok)
			assert.Equal(t, tc.wantCode, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
		})
	}

	t.Run("the dry-run error is not mapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{createErr: refused}
		cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("currentHorse1\naVeryLongPassword123\n")})
		_, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: bothPasswordFlags()})
		assert.Same(t, refused, err)
	})

	t.Run("another response to the real create request is returned as it is", func(t *testing.T) {
		t.Parallel()
		other := clierr.New(clierr.CodeGeneric, "the server is busy")
		client := &fakeClient{realCreateErr: other}
		cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("aVeryLongPassword123\n")})
		exec, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}})
		require.NoError(t, err)
		_, err = exec(t.Context())
		assert.Same(t, other, err)
	})
}

// TestCreatePasswordChangeRequestResultNeverHoldsThePasswords asserts the
// result of the apply holds the user id only, though the server echoes both
// passwords in the created object.
func TestCreatePasswordChangeRequestResultNeverHoldsThePasswords(t *testing.T) {
	t.Parallel()
	client := &fakeClient{status: passwordUpdatedStatus()}
	cmd := NewCreatePasswordChangeRequest(stubFactory{c: client}, fakeFileSource{body: []byte("currentHorse1\naVeryLongPassword123\n")})
	exec, err := planPCR(t, cmd, pcrInput{args: []string{"u-abc123"}, bools: bothPasswordFlags()})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"userID": "u-abc123"}, res.Data)
}
