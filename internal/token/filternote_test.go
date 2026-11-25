package token

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDeleteTokenPrintsNoFilterNote asserts auth delete-token, whose text
// has the lines of a certificate failure with their hidden characters
// removed, prints no line about removed characters.
func TestDeleteTokenPrintsNoFilterNote(t *testing.T) {
	t.Parallel()
	assert.True(t, deleteTokenCmd{}.Describe().NoFilterNote)
}
