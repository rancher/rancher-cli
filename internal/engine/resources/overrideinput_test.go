package resources

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/secret"
	"github.com/stretchr/testify/assert"
)

// secretsInput is a runtime.Input whose Secrets returns one known value.
type secretsInput struct{ runtime.Input }

func (secretsInput) Secrets(name string) []secret.Value {
	if name == "from-file" {
		return []secret.Value{secret.New("fake-override-secret-0001")}
	}
	return nil
}

func (secretsInput) String(string) string { return "typed" }

// TestOverrideInputForwardsSecrets asserts the input create wraps around the
// command's own returns the values of Secrets, and still prefers an override
// for String.
func TestOverrideInputForwardsSecrets(t *testing.T) {
	t.Parallel()
	var in runtime.Input = overrideInput{Input: secretsInput{}, overrides: map[string]string{"namespace": "ns-1"}}
	got := in.Secrets("from-file")
	if assert.Len(t, got, 1) {
		assert.Equal(t, "fake-override-secret-0001", secret.Reveal(got[0]))
	}
	assert.Nil(t, in.Secrets("other"))
	assert.Equal(t, "ns-1", in.String("namespace"))
	assert.Equal(t, "typed", in.String("cluster"))
}
