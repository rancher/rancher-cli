package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUploadCommandIsBounded asserts the longest the upload command runs is
// below the age at which another process takes over the upload lock; no
// process removes a lock the command still holds.
func TestUploadCommandIsBounded(t *testing.T) {
	assert.Less(t, UploadCommandBound, idStaleAfter)
	assert.Positive(t, UploadCommandBound)
}
