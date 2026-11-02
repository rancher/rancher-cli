package token

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// revocationReportFields classifies the text fields of the report every
// revoke payload holds: the error message of a certificate failure and the
// recovery commands hold remedy commands; the names, entries, owner and
// certificate facts come from the server or the user; the rest is the CLI's
// own wording and holds no command.
var revocationReportFields = remedytest.FieldLists{
	Remedy: []string{"NotRevoked[].Error", "NotRevoked[].Recovery[]"},
	Server: []string{
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

// deleteTokenFailureFields classifies the text fields of the payload of a
// failed auth delete-token: the report's, and the keys of the cached
// credentials it deleted and kept.
var deleteTokenFailureFields = remedytest.FieldLists{
	Remedy: []string{"NotRevoked[].Error", "NotRevoked[].Recovery[]"},
	Server: []string{
		"Deleted[]", "Kept[]",
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

// TestRevocationReportTextFieldsAreAllClassified asserts every text field of
// the report is in exactly one of the three lists.
func TestRevocationReportTextFieldsAreAllClassified(t *testing.T) {
	t.Parallel()
	remedytest.AssertEveryFieldClassified(t, reflect.TypeFor[RevocationReport](), revocationReportFields)
}

// TestRevocationReportMapTextMapsOnlyTheRemedyFields asserts MapText rewrites
// the error message and each recovery command of every token that may still
// be live, and no other field, and leaves the original untouched.
func TestRevocationReportMapTextMapsOnlyTheRemedyFields(t *testing.T) {
	t.Parallel()
	planted := remedytest.PlantMarked(t, reflect.TypeFor[RevocationReport]()).Interface().(RevocationReport)
	const prefix = " --config '/d'"
	got, ok := planted.MapText(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(RevocationReport)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, revocationReportFields, revocationReportFields.Remedy, prefix)
	assert.Contains(t, planted.NotRevoked[0].Recovery[0], clierr.RemedyMarker, "the original is untouched")
}

// TestRevocationReportMapCommandsMapsOnlyTheRecovery asserts MapCommands
// rewrites each recovery command and leaves the error message, which is
// prose with commands in it, as it is.
func TestRevocationReportMapCommandsMapsOnlyTheRecovery(t *testing.T) {
	t.Parallel()
	planted := remedytest.PlantMarked(t, reflect.TypeFor[RevocationReport]()).Interface().(RevocationReport)
	const prefix = " --config '/d'"
	got, ok := planted.MapCommands(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(RevocationReport)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, revocationReportFields, []string{"NotRevoked[].Recovery[]"}, prefix)
	assert.Contains(t, planted.NotRevoked[0].Recovery[0], clierr.RemedyMarker, "the original is untouched")
}

// TestDeleteTokenFailureTextFieldsAreAllClassified asserts every text field
// of the payload is in exactly one of the three lists.
func TestDeleteTokenFailureTextFieldsAreAllClassified(t *testing.T) {
	t.Parallel()
	remedytest.AssertEveryFieldClassified(t, reflect.TypeFor[DeleteTokenFailure](), deleteTokenFailureFields)
}

// TestDeleteTokenFailureMapTextMapsOnlyTheRemedyFields asserts MapText
// rewrites the error message and each recovery command of every token that
// may still be live, and no other field, and leaves the original untouched.
func TestDeleteTokenFailureMapTextMapsOnlyTheRemedyFields(t *testing.T) {
	t.Parallel()
	planted := remedytest.PlantMarked(t, reflect.TypeFor[DeleteTokenFailure]()).Interface().(DeleteTokenFailure)
	const prefix = " --config '/d'"
	got, ok := planted.MapText(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(DeleteTokenFailure)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, deleteTokenFailureFields, deleteTokenFailureFields.Remedy, prefix)
	assert.Contains(t, planted.NotRevoked[0].Recovery[0], clierr.RemedyMarker, "the original is untouched")
}

// TestDeleteTokenFailureMapCommandsMapsOnlyTheRecovery asserts MapCommands
// rewrites each recovery command and no other field.
func TestDeleteTokenFailureMapCommandsMapsOnlyTheRecovery(t *testing.T) {
	t.Parallel()
	planted := remedytest.PlantMarked(t, reflect.TypeFor[DeleteTokenFailure]()).Interface().(DeleteTokenFailure)
	const prefix = " --config '/d'"
	got, ok := planted.MapCommands(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(DeleteTokenFailure)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, deleteTokenFailureFields, []string{"NotRevoked[].Recovery[]"}, prefix)
}

// TestMapLiveTokensKeepsAnEmptiedCommandAndSharesNothing asserts a command a
// function empties stays as an empty item and the list keeps its shape, and
// that the mapped list does not write into the slices of the original.
func TestMapLiveTokensKeepsAnEmptiedCommandAndSharesNothing(t *testing.T) {
	t.Parallel()
	list := []NotRevokedToken{{Error: "e", Recovery: []string{"a", "b"}}, {Recovery: nil}}
	got := MapLiveTokens(list, nil, func(string) string { return "" })
	assert.Equal(t, []string{"", ""}, got[0].Recovery)
	assert.Equal(t, "e", got[0].Error)
	assert.Nil(t, got[1].Recovery)
	assert.Equal(t, []string{"a", "b"}, list[0].Recovery)
	assert.Nil(t, MapLiveTokens(nil, nil, nil))
}
