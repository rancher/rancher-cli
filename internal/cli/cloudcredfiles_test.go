package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ccFileRun runs a create that reads the secretKey of an amazonec2 credential
// from path, and returns what the run wrote and the posts the server saw.
func ccFileRun(t *testing.T, path string) (stdout, stderr string, code int, posts []string) {
	t.Helper()
	stdout, stderr, code, _ = ccRun(t, ccStub(ccNorman{status: 201, body: `{"id":"cattle-global-data:cc-abcde"}`}, &posts, false),
		[]string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=" + path, "--apply", "--wait=false"}, "")
	return stdout, stderr, code, posts
}

// TestCloudCredentialFromFileUnreadablePaths asserts that a path no read can
// open is one refusal that names the path as typed and no operating system
// text, on every operating system: a path below a plain file, a directory in
// place of a file, a missing file and a path typed with a drive letter or
// backslashes. No request is sent.
func TestCloudCredentialFromFileUnreadablePaths(t *testing.T) {
	restoreRawArgv(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(plain, []byte("fake-file-secret-0001\n"), 0o600))
	directory := filepath.Join(dir, "d")
	testfs.DirectoryInPlaceOfFile(t, directory)
	for _, tc := range []struct{ name, path string }{
		{"a path below a plain file", filepath.Join(plain, "x")},
		{"a directory in place of a file", directory},
		{"a missing file", filepath.Join(dir, "missing")},
		{"a drive letter path", `C:\Users\me\kc.yaml`},
		{"a backslash path", `.\token.txt`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, _, posts := ccFileRun(t, tc.path)
			assert.Empty(t, posts)
			assert.Contains(t, stdout+stderr, "cannot read the --from-file file "+tc.path)
			assert.Contains(t, stdout+stderr, "VALIDATION")
		})
	}
}

// TestCloudCredentialFromFileSymlink asserts a symlinked file is read through
// the link.
func TestCloudCredentialFromFileSymlink(t *testing.T) {
	restoreRawArgv(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("fake-link-secret-0001\n"), 0o600))
	link := filepath.Join(dir, "link.txt")
	testfs.Symlink(t, target, link)
	stdout, stderr, code, posts := ccFileRun(t, link)
	require.Zero(t, code, stdout+stderr)
	assert.Equal(t, "fake-link-secret-0001", ccPostedValue(t, posts, "amazonec2", "secretKey"))
}

// TestCloudCredentialFromFileSplitsAtTheFirstEquals asserts that a value of
// --from-file splits at its first equals sign, so a path may hold another.
func TestCloudCredentialFromFileSplitsAtTheFirstEquals(t *testing.T) {
	restoreRawArgv(t)
	dir := filepath.Join(t.TempDir(), "a=b")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "k.txt")
	require.NoError(t, os.WriteFile(path, []byte("fake-equals-secret-0001\n"), 0o600))
	stdout, stderr, code, posts := ccFileRun(t, path)
	require.Zero(t, code, stdout+stderr)
	assert.Equal(t, "fake-equals-secret-0001", ccPostedValue(t, posts, "amazonec2", "secretKey"))
}
