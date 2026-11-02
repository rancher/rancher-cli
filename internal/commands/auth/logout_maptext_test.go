package auth

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// logoutPartialFields classifies every text field of LogoutAllPartialFailure:
// the logout to run again, the error message of a certificate failure and the
// recovery commands hold remedy commands; the server names, backends, token
// names, entries, owner and certificate facts come from the server or the
// user; the rest is the CLI's own wording and holds no command.
var logoutPartialFields = remedytest.FieldLists{
	Remedy: []string{"Rerun", "NotRevoked[].Error", "NotRevoked[].Recovery[]"},
	Server: []string{
		"LoggedOut[]", "DeletedFrom{key}", "DeletedFrom{}[]", "DeletedCache[]",
		"Revoked[].Ref.Name", "Revoked[].Server",
		"NotRevoked[].Ref.Name", "NotRevoked[].Server", "NotRevoked[].Entry", "NotRevoked[].LoginAs",
		"NotRevoked[].Certificate.Fingerprint", "NotRevoked[].Certificate.Subject", "NotRevoked[].Certificate.Issuer",
		"NotRevoked[].Certificate.NotBefore", "NotRevoked[].Certificate.NotAfter",
	},
	Other: []string{
		"Revoked[].Ref.Kind", "Revoked[].Source",
		"NotRevoked[].Ref.Kind", "NotRevoked[].Source", "NotRevoked[].Reason", "NotRevoked[].Code",
		"NotRevoked[].Retry", "NotRevoked[].Certificate.Kind",
	},
}

// TestLogoutAllPartialFailureTextFieldsAreAllClassified asserts every text
// field of the payload is in exactly one of the three lists.
func TestLogoutAllPartialFailureTextFieldsAreAllClassified(t *testing.T) {
	t.Parallel()
	remedytest.AssertEveryFieldClassified(t, reflect.TypeFor[LogoutAllPartialFailure](), logoutPartialFields)
}

// TestLogoutAllPartialFailureMapTextMapsOnlyTheRemedyFields asserts MapText
// rewrites the logout to run again and the error message and recovery
// commands of every token that may still be live, and no other field, and
// leaves the original untouched.
func TestLogoutAllPartialFailureMapTextMapsOnlyTheRemedyFields(t *testing.T) {
	t.Parallel()
	planted := remedytest.PlantMarked(t, reflect.TypeFor[LogoutAllPartialFailure]()).Interface().(LogoutAllPartialFailure)
	const prefix = " --config '/d'"
	got, ok := planted.MapText(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(LogoutAllPartialFailure)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, logoutPartialFields, logoutPartialFields.Remedy, prefix)
	assert.Contains(t, planted.Rerun, clierr.RemedyMarker, "the original is untouched")
	assert.Contains(t, planted.NotRevoked[0].Recovery[0], clierr.RemedyMarker, "the original is untouched")
}

// TestLogoutAllPartialFailureMapCommandsMapsTheRerunAndTheRecovery asserts
// MapCommands rewrites the logout to run again and each recovery command, and
// leaves the error message as it is.
func TestLogoutAllPartialFailureMapCommandsMapsTheRerunAndTheRecovery(t *testing.T) {
	t.Parallel()
	planted := remedytest.PlantMarked(t, reflect.TypeFor[LogoutAllPartialFailure]()).Interface().(LogoutAllPartialFailure)
	const prefix = " --config '/d'"
	got, ok := planted.MapCommands(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(LogoutAllPartialFailure)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, logoutPartialFields, []string{"Rerun", "NotRevoked[].Recovery[]"}, prefix)
}
