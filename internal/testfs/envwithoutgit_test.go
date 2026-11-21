package testfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func setEnviron(t *testing.T, entries []string) {
	t.Helper()
	old := environ
	environ = func() []string { return entries }
	t.Cleanup(func() { environ = old })
}

// TestEnvWithoutGitDropsOnlyGitVariables asserts which entries the helper
// removes on Unix, where names are case-sensitive, and on Windows, where they
// are not, including entries with no name, a name shorter than the prefix and
// a name that starts with an equals sign.
func TestEnvWithoutGitDropsOnlyGitVariables(t *testing.T) {
	cases := []struct {
		name string
		goos string
		in   []string
		want []string
	}{
		{
			name: "unix drops every GIT_ name",
			goos: "linux",
			in:   []string{"HOME=/home/a", "GIT_DIR=/r/.git", "GIT_INDEX_FILE=/r/.git/index", "PATH=/bin", "GIT_PREFIX="},
			want: []string{"HOME=/home/a", "PATH=/bin"},
		},
		{
			name: "unix keeps other cases and similar names",
			goos: "darwin",
			in:   []string{"git_dir=a", "Git_Dir=b", "GITHUB_TOKEN=c", "GIT=d", "MY_GIT_DIR=e", "GIT_=f"},
			want: []string{"git_dir=a", "Git_Dir=b", "GITHUB_TOKEN=c", "GIT=d", "MY_GIT_DIR=e"},
		},
		{
			name: "windows drops every case",
			goos: "windows",
			in:   []string{"Path=C:\\x", "git_dir=a", "Git_Index_File=b", "GIT_PREFIX=c", "GITHUB_TOKEN=d"},
			want: []string{"Path=C:\\x", "GITHUB_TOKEN=d"},
		},
		{
			name: "windows keeps entries that start with an equals sign",
			goos: "windows",
			in:   []string{"=C:=C:\\x", "=GIT_DIR=a", "==::=::\\", "GIT_DIR=b"},
			want: []string{"=C:=C:\\x", "=GIT_DIR=a", "==::=::\\"},
		},
		{
			name: "a short name does not panic",
			goos: "windows",
			in:   []string{"OS=Windows_NT", "A=b", "=", "", "GIT", "GI=x", "GIT_"},
			want: []string{"OS=Windows_NT", "A=b", "=", "", "GIT", "GI=x"},
		},
		{
			name: "a short name does not panic on unix",
			goos: "linux",
			in:   []string{"OS=x", "=", "", "G=1"},
			want: []string{"OS=x", "=", "", "G=1"},
		},
		{
			name: "an empty environment",
			goos: "linux",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setGOOS(t, c.goos)
			setEnviron(t, c.in)
			assert.Equal(t, c.want, EnvWithoutGit())
		})
	}
}

// TestEnvWithoutGitReadsTheProcessEnvironment asserts the helper reads the
// real environment and returns no GIT_ name from it.
func TestEnvWithoutGitReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("GIT_DIR", "/nowhere")
	t.Setenv("GIT_INDEX_FILE", "/nowhere/index")
	t.Setenv("GIT_WORK_TREE", "/nowhere")
	t.Setenv("GIT_TESTFS_MARKER", "1")
	t.Setenv("TESTFS_KEPT_VARIABLE", "kept")
	got := EnvWithoutGit()
	assert.NotEmpty(t, got)
	assert.Contains(t, got, "TESTFS_KEPT_VARIABLE=kept")
	for _, entry := range got {
		assert.False(t, isGitVariable(entry), "%s is a git variable", entry)
	}
}
