package provisioning

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/rancher/rancher-cli/internal/secret"
	"github.com/stretchr/testify/assert"
)

// TestFileWordPathRevealsOnlyAFromFilePath asserts the path of a --from-file
// argument is returned as typed, and the argument of any other flag, which may
// be a secret, gives "".
func TestFileWordPathRevealsOnlyAFromFilePath(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "./key.txt", fileWordPath(fieldWord{flag: flagFromFile, value: secret.New("./key.txt")}))
	assert.Empty(t, fileWordPath(fieldWord{flag: flagSet, value: secret.New(ccwSecret)}))
	assert.Empty(t, fileWordPath(fieldWord{value: secret.New(ccwSecret)}))
}

// TestShownValueNeverReturnsTheTextOfASecretField asserts the preview shows a
// public field's value as typed and a secret field's as engine.Redacted.
func TestShownValueNeverReturnsTheTextOfASecretField(t *testing.T) {
	t.Parallel()
	public := credentialValue{field: cloudcredfields.Field{Name: "region"}, value: secret.New("us-east-1")}
	assert.Equal(t, "us-east-1", shownValue(public))
	hidden := credentialValue{field: cloudcredfields.Field{Name: "secretKey", Secret: true}, value: secret.New(ccwSecret)}
	assert.Equal(t, engine.Redacted, shownValue(hidden))
}
