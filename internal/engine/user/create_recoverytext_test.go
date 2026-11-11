package user

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// TestTheSecretRecoveryOfAnObjectNameReachesTheTextAsWritten asserts the
// default-format text of a failed Secret create for a user whose id is an
// object name holds the recovery command exactly as the command builds it.
// The command holds single and double quotes and no hidden character, so the
// renderer neither quotes nor splits it.
func TestTheSecretRecoveryOfAnObjectNameReachesTheTextAsWritten(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2",
		secretCreateErr: clierr.New(clierr.CodeGeneric, "boom")}
	_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
	require.NoError(t, err)
	_, err = exec(t.Context())
	ce, _ := committedDetails(t, err)

	var text bytes.Buffer
	require.NoError(t, runtime.RenderError(&text, "", ce))
	want := "error (GENERIC): boom\n" +
		"  committed: User u-real2 - the change may already be on the server\n" +
		"  recover: the user u-real2 was created without a password; to set one without putting it on the command line or in shell history, set PASSWORD in your shell (a password containing a double quote or backslash must be JSON-escaped in the manifest) and add --apply after reading the preview of: " +
		`echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"u-real2","namespace":"cattle-local-user-passwords"},"stringData":{"password":"'"$PASSWORD"'"}}' | ` +
		"rancher create resource -f - --cluster local --dry-run\n"
	assert.Equal(t, want, text.String())
}
