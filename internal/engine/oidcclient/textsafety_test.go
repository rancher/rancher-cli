package oidcclient

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// hostileTail holds an escape sequence introducer, the one-rune control
// sequence introducer U+009B, a tab, U+202E RIGHT-TO-LEFT OVERRIDE, U+0085
// NEXT LINE, U+2028 LINE SEPARATOR, a carriage return and a line feed, then
// a line that reads as the CLI's own.
const hostileTail = "\x1b[2J\u009b2J\t\u202e\u0085\u2028\r\nforged line"

func hostileValue(p string) string { return "<" + p + ">" + hostileTail }

// assertLineRule asserts that out holds the angle-bracketed path of every
// entry of present, holds nothing the writer filter removes, and has no line
// that starts with the forged line after its leading spaces.
func assertLineRule(t *testing.T, out string, present []string) {
	t.Helper()
	for _, p := range present {
		assert.Contains(t, out, "<"+p+">", "the text prints %s", p)
	}
	assert.Equal(t, output.PrintableLines(out), out, "the text holds no character the writer filter removes")
	for line := range strings.Lines(out) {
		assert.False(t, strings.HasPrefix(strings.TrimLeft(line, " "), "forged line"), "a line starts with the forged line: %q", line)
	}
}

func TestOIDCClientTextKeepsServerValuesOnTheirLines(t *testing.T) {
	clientID, key, secret := hostileValue("clientID"), hostileValue("secretName"), hostileValue("secret")

	assertSecretLine := func(t *testing.T, text string) {
		t.Helper()
		assert.Contains(t, text, strconv.Quote(secret), "the secret is given in its quoted form")
		assert.Contains(t, text, runtime.QuotedValueNote)
	}

	t.Run("create", func(t *testing.T) {
		withFastPoll(t)
		obj1 := oidcClientObj("my-app", "", nil)
		obj2 := oidcClientObj("my-app", clientID, map[string]any{key: map[string]any{"createdAt": "1", "lastFiveCharacters": "abcde"}})
		client := &fakeOIDCClient{
			pollResponses: []*unstructured.Unstructured{obj1, obj2},
			secretResp: &unstructured.Unstructured{Object: map[string]any{
				"data": map[string]any{key: b64(secret)},
			}},
		}
		cmd := NewCreate(stubOIDCFactory{c: client}, stubRootFactory{c: fakeRootClient{body: []byte(discoveryOK)}}, stubServerBase{base: "https://rancher.example"})
		_, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}})
		require.NoError(t, err)
		res, err := exec(t.Context())
		require.NoError(t, err)

		assertLineRule(t, res.Text, []string{"clientID", "secretName"})
		assertSecretLine(t, res.Text)
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, clientID, data["clientID"])
		assert.Equal(t, key, data["secretName"])
		assert.Equal(t, secret, data["clientSecret"])
	})

	t.Run("add-secret", func(t *testing.T) {
		withFastPoll(t)
		afterAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
		_ = unstructured.SetNestedField(afterAnnotated.Object, "true", "metadata", "annotations", annotationSecretCreate)
		afterConsumed := oidcClientObj("my-app", "client-abc", map[string]any{
			"client-secret-1": map[string]any{}, key: map[string]any{},
		})
		client := &fakeOIDCClient{
			pollResponses: []*unstructured.Unstructured{afterAnnotated, afterConsumed},
			secretResp: &unstructured.Unstructured{Object: map[string]any{
				"data": map[string]any{key: b64(secret)},
			}},
		}
		_, exec, err := planAddSecret(t, NewAddSecret(stubOIDCFactory{c: client}), oidcInput{args: []string{"my-app"}})
		require.NoError(t, err)
		res, err := exec(t.Context())
		require.NoError(t, err)

		assertLineRule(t, res.Text, []string{"secretName"})
		assertSecretLine(t, res.Text)
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, key, data["secretName"])
		assert.Equal(t, secret, data["clientSecret"])
	})

	t.Run("rotate-secret", func(t *testing.T) {
		withFastPoll(t)
		planObj := oidcClientObj("my-app", "client-abc", map[string]any{key: map[string]any{}})
		stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{key: map[string]any{}})
		_ = unstructured.SetNestedField(stillAnnotated.Object, key, "metadata", "annotations", annotationSecretRegenerate)
		consumed := oidcClientObj("my-app", "client-abc", map[string]any{key: map[string]any{}})
		client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{planObj, stillAnnotated, consumed}}
		reads := 0
		client.secretRespFunc = func() *unstructured.Unstructured {
			reads++
			value := "old-value"
			if reads > 1 {
				value = secret
			}
			return &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{key: b64(value)}}}
		}
		_, exec, err := planRotateSecret(t, NewRotateSecret(stubOIDCFactory{c: client}), oidcInput{args: []string{"my-app"}})
		require.NoError(t, err)
		res, err := exec(t.Context())
		require.NoError(t, err)

		assertLineRule(t, res.Text, []string{"secretName"})
		assertSecretLine(t, res.Text)
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, key, data["secretName"])
		assert.Equal(t, secret, data["clientSecret"])
	})
}
