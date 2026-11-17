package extres

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// invalidCurrentPasswordStatus is the body Rancher responds with when it
// refuses the current password: a 500 Status whose message and first cause
// hold the text. tail is appended to both texts.
func invalidCurrentPasswordStatus(tail string) string {
	return `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure",` +
		`"message":"Internal error occurred: error updating password: invalid current password` + tail + `",` +
		`"reason":"InternalError","details":{"causes":[{"message":"error updating password: invalid current password` + tail + `"}]},"code":500}`
}

// notAuthorizedStatus is the body Rancher responds with when a user changes
// another user's password without permission to change every user's password:
// a 401 Status with no details.
func notAuthorizedStatus(tail string) string {
	return `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not authorized to update password` + tail + `","reason":"Unauthorized","code":401}`
}

// classified turns a Status body into the error the client returns for it, as
// the command sees it.
func classified(t *testing.T, body string) error {
	t.Helper()
	var status metav1.Status
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	return clierr.FromStatus(&apierrors.StatusError{ErrStatus: status}, &clierr.Request{Cluster: "local"})
}

const (
	requiresCurrentPassword = "Rancher requires the current password to change your own password; " +
		"pipe the current password and the new password, one per line, and pass --current-password-stdin with --password-stdin. " +
		"The password was not changed."
	refusedCurrentPassword = "Rancher refused the current password (invalid current password); the password was not changed."
	onlyYourOwnPassword    = "you can change only your own password; to change the password of user u-abc123 you need permission to update " +
		"users.management.cattle.io and secrets in the cattle-local-user-passwords namespace"
)

// TestClassifiedResponsesOfAPasswordChangeRequest asserts what the client's
// classification gives for Rancher's two responses before the command maps
// them: the 500 is GENERIC and not retryable with Rancher's text, the 401 is
// AUTH with Rancher's text and no hint, and the status is readable from the
// error.
func TestClassifiedResponsesOfAPasswordChangeRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, code, message string
		status                    int
	}{
		{"a refused current password", invalidCurrentPasswordStatus(""), clierr.CodeGeneric, "Internal error occurred: error updating password: invalid current password", http.StatusInternalServerError},
		{"another user's password", notAuthorizedStatus(""), clierr.CodeAuth, "not authorized to update password", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classified(t, tc.body)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.False(t, ce.Retryable)
			assert.Empty(t, ce.Hint)
			status, ok := clierr.HTTPStatus(err)
			require.True(t, ok)
			assert.Equal(t, tc.status, status)
		})
	}
}

// TestRefusePasswordChangeMapsOnlyTheTwoTexts asserts the mapping table:
// each of Rancher's two refusals becomes the code that fits it, with the
// remedy and no hint, split on what the command sent and never on Rancher's
// response; every other response is returned as the same error, with the code
// it had.
func TestRefusePasswordChangeMapsOnlyTheTwoTexts(t *testing.T) {
	t.Parallel()
	passwordSecretMissing := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure",` +
		`"message":"Internal error occurred: error updating password: failed to get password secret: secrets \"u-abc123\" not found",` +
		`"reason":"InternalError","details":{"causes":[{"message":"error updating password: failed to get password secret: secrets \"u-abc123\" not found"}]},"code":500}`
	otherCause := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure",` +
		`"message":"Internal error occurred: error updating password: some other cause","reason":"InternalError","code":500}`
	permissions := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure",` +
		`"message":"error checking permissions: the check failed","reason":"Unauthorized","code":401}`
	textAlone := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"Internal error occurred: invalid current password","reason":"InternalError","code":500}`
	badRequest := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"error updating password: invalid current password","reason":"BadRequest","code":400}`
	forbidden := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not authorized to update password","reason":"Forbidden","code":403}`

	for _, tc := range []struct {
		name        string
		body        string
		sentCurrent bool
		// mappedCode and message are the code and text of a changed response; no
		// mappedCode means the same error comes back, with the code wantCode.
		mappedCode string
		message    string
		wantCode   string
		wantExit   int
	}{
		{name: "a refused current password, none sent", body: invalidCurrentPasswordStatus(""), mappedCode: clierr.CodeValidation, message: requiresCurrentPassword, wantExit: 2},
		{name: "a refused current password, one sent", body: invalidCurrentPasswordStatus(""), sentCurrent: true, mappedCode: clierr.CodeValidation, message: refusedCurrentPassword, wantExit: 2},
		{name: "another user's password", body: notAuthorizedStatus(""), mappedCode: clierr.CodeRBAC, message: onlyYourOwnPassword, wantExit: 6},
		{name: "another user's password, a current password sent", body: notAuthorizedStatus(""), sentCurrent: true, mappedCode: clierr.CodeRBAC, message: onlyYourOwnPassword, wantExit: 6},
		{name: "a missing password secret", body: passwordSecretMissing, wantCode: clierr.CodeGeneric},
		{name: "another cause of a failed update", body: otherCause, wantCode: clierr.CodeGeneric},
		{name: "a failed permission check", body: permissions, wantCode: clierr.CodeAuth},
		{name: "a failed permission check, a current password sent", body: permissions, sentCurrent: true, wantCode: clierr.CodeAuth},
		{name: "a 500 whose message has no 'error updating password: ' prefix", body: textAlone, wantCode: clierr.CodeGeneric},
		{name: "a 400 with the text of a refused current password", body: badRequest, wantCode: clierr.CodeValidation},
		{name: "a 403 with the text of another user's password refusal", body: forbidden, wantCode: clierr.CodeRBAC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := classified(t, tc.body)
			got := refusePasswordChange(in, tc.sentCurrent, "u-abc123")
			if tc.mappedCode == "" {
				assert.Same(t, in, got, "a response the command does not map comes back as it is")
				ce, ok := errors.AsType[*clierr.Error](got)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, ce.Code)
				return
			}
			ce, ok := got.(*clierr.Error)
			require.True(t, ok)
			assert.Equal(t, tc.mappedCode, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.wantExit, ce.ExitCode())
			assert.Empty(t, ce.Hint)
			assert.False(t, ce.Retryable)
			assert.Nil(t, ce.Details)
			assert.Same(t, in, errors.Unwrap(got), "Rancher's error stays in the cause")
		})
	}
}

// TestRefusePasswordChangeLeavesOtherErrorsAlone asserts that an error with no
// status, a plain error and nil come back as they are.
func TestRefusePasswordChangeLeavesOtherErrorsAlone(t *testing.T) {
	t.Parallel()
	for _, in := range []error{
		clierr.New(clierr.CodeGeneric, "Internal error occurred: "+invalidCurrentPasswordText),
		clierr.New(clierr.CodeAuth, notAuthorizedText),
		errors.New(invalidCurrentPasswordText),
		fmt.Errorf("wrapped: %w", errors.New(notAuthorizedText)),
	} {
		assert.Same(t, in, refusePasswordChange(in, true, "u-abc123"))
	}
	assert.NoError(t, refusePasswordChange(nil, false, "u-abc123"))
}

// TestRefusePasswordChangeNeverHoldsRancherText asserts that a value planted
// in Rancher's response, in the message and in the first cause, is in no part of
// the error the command returns: not its message, hint, details or any
// serialized field.
func TestRefusePasswordChangeNeverHoldsRancherText(t *testing.T) {
	t.Parallel()
	const planted = " plantedValue9"
	for _, tc := range []struct {
		name, body  string
		sentCurrent bool
	}{
		{"a refused current password, none sent", invalidCurrentPasswordStatus(planted), false},
		{"a refused current password, one sent", invalidCurrentPasswordStatus(planted), true},
		{"another user's password", notAuthorizedStatus(planted), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := refusePasswordChange(classified(t, tc.body), tc.sentCurrent, "u-abc123")
			ce, ok := got.(*clierr.Error)
			require.True(t, ok)
			assert.NotEqual(t, tc.body, ce.Message)
			assert.NotContains(t, ce.Message, "plantedValue9")
			raw, err := json.Marshal(ce)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "plantedValue9")
		})
	}
}
