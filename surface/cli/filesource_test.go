package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileSourceReadsFileAndStdin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	require.NoError(t, os.WriteFile(p, []byte("kind: Pod"), 0o600))

	fs := newFileSource(strings.NewReader("from-stdin"))

	b, err := fs.Read(p)
	require.NoError(t, err)
	assert.Equal(t, "kind: Pod", string(b))

	b, err = fs.Read("-")
	require.NoError(t, err)
	assert.Equal(t, "from-stdin", string(b))

	_, err = fs.Read(filepath.Join(dir, "absent.yaml"))
	assert.Error(t, err)
}
