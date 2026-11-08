package provisioning

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/remedytest"
)

// TestCreatorIDMismatchWarningShowsHiddenRunesAsEscapes asserts the warning for
// a creatorId annotation that differs from the caller's user id shows the id
// the server returned with escapes when it holds hidden runes, never with the
// runes removed.
func TestCreatorIDMismatchWarningShowsHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	const stamped = "u-stamped"
	const wrong = "u-wrong\u202e\u200b\a\n\u2063rancher-2"
	client := &ccRecClient{probeUserID: stamped, returnCreatorID: wrong}
	cmd := NewCreateCluster(fakeFactory{c: client}, errRootFactory{})
	_, exec, err := planCluster(t, cmd, ccInput{a: []string{"imported-1"}, b: map[string]bool{"import": true}})
	require.NoError(t, err)
	result, err := exec(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, result.Warnings)
	w := result.Warnings[0]
	assert.Contains(t, w, "("+strconv.Quote(wrong)+") differs from your user ID ("+stamped+")")
	remedytest.AssertIDShownWithEscapes(t, w, wrong)
}
