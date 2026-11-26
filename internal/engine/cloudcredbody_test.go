package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/rancher/rancher-cli/internal/secret"
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
		"amazonec2credentialConfig": map[string]any{"secretKey": secret.New(bodySecret)},
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

// TestRequestJSONRevealsNestedObjectsAndLists asserts the request body has
// the text of a secret.Value in a nested object and in a list, and that the
// document of the body still has the Value.
func TestRequestJSONRevealsNestedObjectsAndLists(t *testing.T) {
	t.Parallel()
	body := NewCloudCredentialBody(map[string]any{
		"config": map[string]any{
			"key":    secret.New(bodySecret),
			"list":   []any{secret.New(bodySecret), map[string]any{"deep": secret.New(bodySecret)}, "plain"},
			"nested": map[string]any{"again": map[string]any{"leaf": secret.New(bodySecret)}},
		},
	})
	raw, err := body.RequestJSON()
	require.NoError(t, err)
	var sent map[string]map[string]any
	require.NoError(t, json.Unmarshal(raw, &sent))
	assert.Equal(t, bodySecret, sent["config"]["key"])
	assert.Equal(t, []any{bodySecret, map[string]any{"deep": bodySecret}, "plain"}, sent["config"]["list"])
	assert.Equal(t, map[string]any{"again": map[string]any{"leaf": bodySecret}}, sent["config"]["nested"])
	assert.NotContains(t, fmt.Sprintf("%v", body), bodySecret)
	again, err := body.RequestJSON()
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(again), "the document still has the Values")
}

// TestRequestJSONRefusesADocumentNestedTooDeep asserts a document nested deeper
// than the check looks is an error and no body, though it has no Value: the
// check refuses what it cannot inspect.
func TestRequestJSONRefusesADocumentNestedTooDeep(t *testing.T) {
	t.Parallel()
	deep := func(levels int) map[string]any {
		doc := map[string]any{"leaf": "x"}
		for range levels {
			doc = map[string]any{"in": doc}
		}
		return doc
	}
	raw, err := NewCloudCredentialBody(deep(maxBodyDepth/2 - 4)).RequestJSON()
	require.NoError(t, err)
	assert.NotEmpty(t, raw)
	raw, err = NewCloudCredentialBody(deep(maxBodyDepth * 2)).RequestJSON()
	require.ErrorContains(t, err, "nested too deep")
	assert.Nil(t, raw)
	listed := NewCloudCredentialBody(map[string]any{"config": deep(maxBodyDepth * 2)})
	_, err = listed.RequestJSON()
	require.ErrorContains(t, err, "nested too deep")
}

// TestRequestJSONRefusesAValueItDoesNotReveal asserts a secret.Value at a
// position the body does not reveal is an error and no body: a typed list or
// map, a struct, a pointer; the placeholder is never sent in its place.
func TestRequestJSONRefusesAValueItDoesNotReveal(t *testing.T) {
	t.Parallel()
	type holder struct{ V secret.Value }
	v := secret.New(bodySecret)
	for name, leaf := range map[string]any{
		"a typed list":       []secret.Value{v},
		"a typed map":        map[string]secret.Value{"k": v},
		"a struct":           holder{v},
		"a pointer":          &v,
		"a list of pointers": []*secret.Value{&v},
		"a nested typed map": map[string]any{"deep": map[string]secret.Value{"k": v}},
		"an array":           [1]secret.Value{v},
	} {
		raw, err := NewCloudCredentialBody(map[string]any{"config": map[string]any{"leaf": leaf}}).RequestJSON()
		require.ErrorContains(t, err, "does not reveal", name)
		assert.Nil(t, raw, name)
		assert.NotContains(t, err.Error(), bodySecret, name)
	}
}
