package rbac

import (
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// hiddenRunesID holds a direction override, a zero-width space, a bell, a
// newline and the start of a remedy marker followed by the word the marker
// ends with.
const hiddenRunesID = "okta_user://u-\u202e\u200b\a\n\u2063rancher-x"

// TestDeleteUserSelfRefusalShowsAnIdWithHiddenRunesAsEscapes asserts the
// refusal to delete the caller's own user shows a typed id holding hidden
// runes with escapes, never with the runes removed.
func TestDeleteUserSelfRefusalShowsAnIdWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	k := userKind(t)
	c := &fakeClient{
		createFn:    selfUserCreate(hiddenRunesID),
		gets:        map[string]*unstructured.Unstructured{"/" + hiddenRunesID: delUserObj(hiddenRunesID)},
		allowDelete: true,
	}
	_, err := planDeleteUser(t, NewDeleteUser(k, fakeFactory{c: c}), stubInput{args: []string{hiddenRunesID}})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, "refusing to delete user "+strconv.Quote(hiddenRunesID)+": it is your own user, and deleting it locks this CLI out; log in as a different admin and delete it from there", ce.Message)
	remedytest.AssertIDShownWithEscapes(t, ce.Message, hiddenRunesID)
}

// TestMemberDisplayNameWarningShowsAPrincipalWithHiddenRunesAsEscapes asserts
// the warning for a principal several users own shows the principal, which an
// identity provider names, with escapes, never with the runes removed.
func TestMemberDisplayNameWarningShowsAPrincipalWithHiddenRunesAsEscapes(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{
		users: []map[string]any{
			userObj("u-1", "Same Name", "", hiddenRunesID),
			userObj("u-2", "Same Name", "", hiddenRunesID),
		},
	}
	_, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-1", map[string]any{"userPrincipalName": hiddenRunesID, "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "display name for "+strconv.Quote(hiddenRunesID)+" not shown")
	remedytest.AssertIDShownWithEscapes(t, warns[0], hiddenRunesID)
}
