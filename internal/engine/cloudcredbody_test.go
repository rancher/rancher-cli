package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const bodySecret = "fake-body-secret-0001"

// TestCloudCredentialBodyPrintsHiddenInEveryFormat asserts a body holding a
// secret value prints <hidden> through every printing and encoding form, on a
// value, a pointer and inside another value, and that only RequestJSON writes
// the value.
func TestCloudCredentialBodyPrintsHiddenInEveryFormat(t *testing.T) {
	t.Parallel()
	body := NewCloudCredentialBody(map[string]any{
		"name":                      "x",
		"amazonec2credentialConfig": map[string]any{"secretKey": bodySecret},
	})
	type holder struct{ B CloudCredentialBody }
	for name, got := range map[string]string{
		"%v":                     fmt.Sprintf("%v", body),
		"%+v":                    fmt.Sprintf("%+v", body),
		"%#v":                    fmt.Sprintf("%#v", body),
		"%s":                     fmt.Sprintf("%s", body),
		"%d":                     fmt.Sprintf("%d", body),
		"Sprint":                 fmt.Sprint(body),
		"pointer":                fmt.Sprintf("%v", &body),
		"in struct":              fmt.Sprintf("%+v", holder{body}),
		"in a Go struct literal": fmt.Sprintf("%#v", holder{body}),
	} {
		assert.NotContains(t, got, bodySecret, name)
		assert.Contains(t, got, HiddenText, name)
	}
	asJSON, err := json.Marshal(body)
	require.NoError(t, err)
	var decoded string
	require.NoError(t, json.Unmarshal(asJSON, &decoded))
	assert.Equal(t, HiddenText, decoded)
	asJSON, err = json.Marshal(holder{body})
	require.NoError(t, err)
	assert.NotContains(t, string(asJSON), bodySecret)
	asYAML, err := yaml.Marshal(holder{body})
	require.NoError(t, err)
	assert.NotContains(t, string(asYAML), bodySecret)
	text, err := body.MarshalText()
	require.NoError(t, err)
	assert.Equal(t, HiddenText, string(text))

	raw, err := body.RequestJSON()
	require.NoError(t, err)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(raw, &sent))
	assert.Equal(t, bodySecret, sent["amazonec2credentialConfig"].(map[string]any)["secretKey"])
}
