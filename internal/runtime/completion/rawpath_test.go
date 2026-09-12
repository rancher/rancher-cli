package completion

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRawStringCannotBeRequested asserts the Path type is the only way
// into GetJSON: the compiler refuses testdata/rawpath, a program that
// passes a string literal, naming the mismatched argument.
func TestRawStringCannotBeRequested(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("go", "build", "-o", os.DevNull, "./testdata/rawpath")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "the raw-path program must not compile:\n%s", out)
	assert.Contains(t, string(out), `cannot use "/k8s/clusters/local/api/v1/namespaces" (untyped string constant) as completion.Path value`)
}
