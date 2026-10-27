package resources

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// deleteProjectFields classifies every text field of
// DeleteProjectPartialFailure: an object's message is the text of the error
// its delete failed with, so it can hold a remedy command; the names are
// the server's; the kind, outcome and code are the CLI's own words.
var deleteProjectFields = remedytest.FieldLists{
	Remedy: []string{"Objects[].Message"},
	Server: []string{"Objects[].Name"},
	Other:  []string{"Objects[].Kind", "Objects[].Outcome", "Objects[].Code"},
}

// TestDeleteProjectPartialFailureTextFieldsAreAllClassified asserts every
// text field of the type is in exactly one of the three lists.
func TestDeleteProjectPartialFailureTextFieldsAreAllClassified(t *testing.T) {
	t.Parallel()
	remedytest.AssertEveryFieldClassified(t, reflect.TypeFor[DeleteProjectPartialFailure](), deleteProjectFields)
}

// TestDeleteProjectPartialFailureMapTextMapsOnlyTheMessages asserts MapText
// rewrites the message of every object, which can hold a remedy command, and
// no other field the lists name.
func TestDeleteProjectPartialFailureMapTextMapsOnlyTheMessages(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[DeleteProjectPartialFailure]()
	planted := remedytest.PlantMarked(t, typ).Interface().(DeleteProjectPartialFailure)
	const prefix = " --config '/d'"
	got, ok := planted.MapText(func(s string) string { return clierr.ExpandRemedies(s, prefix) }).(DeleteProjectPartialFailure)
	require.True(t, ok)
	remedytest.AssertMappedAsListed(t, planted, got, deleteProjectFields, deleteProjectFields.Remedy, prefix)
	assert.Contains(t, planted.Objects[0].Message, clierr.RemedyMarker, "the original is untouched")
}
