package inspect

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// TestInspectUserKeepsAServerValueThatLooksLikeACommand asserts that a user
// whose display name and last login hold U+2063 followed by the word rancher
// (text a server sends, not this process's marker) is printed with no
// directory by the whole pipeline, with a config directory in use: the
// text print removes the character from the last login line and from the
// display name, so neither is a command form, no line holds --config, and
// the json output holds the last login byte for byte.
func TestInspectUserKeepsAServerValueThatLooksLikeACommand(t *testing.T) {
	t.Parallel()
	const planted = "\u2063rancher delete x"
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	ua := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "UserAttribute",
		"metadata":   map[string]any{"name": "u-abc"},
		"lastLogin":  planted,
	}}
	newCmd := func() runtime.Command {
		c := newFakeClient().
			withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", planted, "jane", "local://u-abc")).
			withObject(rbac.UserAttrGVR, "", "u-abc", ua).
			withList(grbKind.GVR, &unstructured.UnstructuredList{}).
			withList(crtbKind.GVR, &unstructured.UnstructuredList{}).
			withList(prtbKind.GVR, &unstructured.UnstructuredList{})
		return newUserCmdFake(c)
	}
	dir := t.TempDir()

	t.Run("text", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := runtime.Run(t.Context(), newCmd(), input{args: []string{"u-abc"}}, runtime.IO{Out: &out, Err: &errBuf, RemedyDir: dir}, safety.Binding{})
		require.Nil(t, ce)
		got := out.String()
		assert.Contains(t, got, "  lastLogin: rancher delete x\n")
		assert.Contains(t, got, "  displayName: rancher delete x\n")
		assert.NotContains(t, got, "--config")
		assert.NotContains(t, got, clierr.RemedyMarker)
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := runtime.Run(t.Context(), newCmd(), input{args: []string{"u-abc"}}, runtime.IO{Out: &out, Err: &errBuf, Format: "json", RemedyDir: dir}, safety.Binding{})
		require.Nil(t, ce)
		var doc struct {
			Data struct {
				User struct {
					LastLogin string `json:"lastLogin"`
				} `json:"user"`
			} `json:"data"`
		}
		require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
		assert.Equal(t, planted, doc.Data.User.LastLogin)
		var all any
		require.NoError(t, yaml.Unmarshal(out.Bytes(), &all))
		for _, s := range decodedUserStrings(all) {
			assert.NotContains(t, s, "--config")
		}
	})
}

// decodedUserStrings returns every string of a decoded json document: values
// and keys.
func decodedUserStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, decodedUserStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range x {
			out = append(out, k)
			out = append(out, decodedUserStrings(e)...)
		}
		return out
	}
	return nil
}
