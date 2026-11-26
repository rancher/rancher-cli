package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/secret"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const secretInputText = "fake-secret-input-0001"

// secretFake is an Input with one Secret flag, "from-file", typed with the
// values in secrets.
type secretFake struct {
	mapInput
	secrets map[string][]secret.Value
}

func (f secretFake) Secrets(name string) []secret.Value { return f.secrets[name] }

// secretsOf returns the texts of the values the input returns for name.
func secretsOf(in Input, name string) []string {
	var out []string
	for _, v := range in.Secrets(name) {
		out = append(out, secret.Reveal(v))
	}
	return out
}

// TestSecretsPassThroughTheInputWrappers asserts each Input the pipeline wraps
// around the command's own, the one Plan gets and the one Execute gets, returns
// the values of Secrets and the other methods of the input it wraps.
func TestSecretsPassThroughTheInputWrappers(t *testing.T) {
	t.Parallel()
	inner := secretFake{
		s:       map[string]string{"name": "x"},
		secrets: map[string][]secret.Value{"from-file": {secret.New(secretInputText)}},
	}
	for name, wrapped := range map[string]Input{
		"the input of Plan":    stdinInput{Input: inner, terminal: true},
		"the input of Execute": textTableInput{Input: inner, textTable: true},
	} {
		assert.Equal(t, []string{secretInputText}, secretsOf(wrapped, "from-file"), name)
		assert.Nil(t, wrapped.Secrets("other"), name)
		assert.Equal(t, "x", wrapped.String("name"), name)
	}
}

// TestSecretFlagIsValidatedThroughSecrets asserts a flag marked Secret is
// checked on the values of Secrets: a value passes, a blank value is refused
// with the empty-value text, a required flag that is absent is refused with the
// missing-flag text, and an absent optional flag passes. Strings returns
// nothing for the flag, which would read as absent.
func TestSecretFlagIsValidatedThroughSecrets(t *testing.T) {
	t.Parallel()
	optional := Descriptor{Name: "write", Flags: []ArgSpec{{Name: "from-file", Type: "stringSlice", Secret: true}}}
	required := Descriptor{Name: "write", Flags: []ArgSpec{{Name: "from-file", Type: "stringSlice", Secret: true, Required: true}}}
	typed := func(values ...string) secretFake {
		var vs []secret.Value
		for _, v := range values {
			vs = append(vs, secret.New(v))
		}
		return secretFake{
			is:      map[string]bool{"from-file": true},
			secrets: map[string][]secret.Value{"from-file": vs},
		}
	}

	assert.Nil(t, validateInput(optional, typed(secretInputText), false))
	assert.Nil(t, validateInput(required, typed(secretInputText), false))
	assert.Nil(t, validateInput(optional, secretFake{}, false))

	ce := validateInput(optional, typed(" "), false)
	require.NotNil(t, ce)
	assert.Equal(t, EmptyValueError("from-file").Message, ce.Message)
	ce = validateInput(optional, typed(secretInputText, ""), false)
	require.NotNil(t, ce)
	assert.Equal(t, EmptyValueError("from-file").Message, ce.Message)
	ce = validateInput(required, typed(" "), false)
	require.NotNil(t, ce)
	assert.Equal(t, "--from-file is empty; pass a value", ce.Message)

	ce = validateInput(required, secretFake{}, false)
	require.NotNil(t, ce)
	assert.Equal(t, missingFlagError(required.Flags[0]).Message, ce.Message)

	only := typed(secretInputText)
	assert.Nil(t, only.Strings("from-file"))
	assert.Equal(t, []string{secretInputText}, secretsOf(only, "from-file"))
}
