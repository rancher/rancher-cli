package rbac

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// revokeFields classifies every text field of RevokePartialFailure: the
// failed delete's message is the text of the error it failed with, so it can
// hold a remedy command; the binding names are the server's; the code is
// the CLI's own word.
var revokeFields = remedytest.FieldLists{
	Remedy: []string{"Failed.Message"},
	Server: []string{"Deleted[]", "Failed.Name", "Unattempted[]"},
	Other:  []string{"Failed.Code"},
}

// TestRevokePartialFailureTextFieldsAreAllClassified asserts every text
// field of the type is in exactly one of the three lists.
func TestRevokePartialFailureTextFieldsAreAllClassified(t *testing.T) {
	t.Parallel()
	remedytest.AssertEveryFieldClassified(t, reflect.TypeFor[RevokePartialFailure](), revokeFields)
}

// TestRevokePartialFailureMapTextMapsOnlyTheMessage asserts MapText
// rewrites the message of the failed delete, which can hold a remedy
// command, and no other field the lists name.
func TestRevokePartialFailureMapTextMapsOnlyTheMessage(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[RevokePartialFailure]()
	planted := remedytest.PlantMarked(t, typ).Interface().(RevokePartialFailure)
	const prefix = " --config '/d'"
	got, ok := planted.MapText(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(RevokePartialFailure)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, revokeFields, revokeFields.Remedy, prefix)
	assert.Contains(t, planted.Failed.Message, clierr.RemedyMarker, "the original is untouched")
}
