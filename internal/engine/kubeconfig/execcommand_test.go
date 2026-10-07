package kubeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// execInput is a runtime.Input whose IsSet reports the flags given, so an
// explicitly empty value is told from an absent one.
type execInput struct{ s map[string]string }

func (e execInput) String(k string) string  { return e.s[k] }
func (e execInput) Bool(string) bool        { return false }
func (e execInput) Strings(string) []string { return nil }
func (e execInput) IsSet(k string) bool     { _, ok := e.s[k]; return ok }
func (e execInput) Args() []string          { return nil }

// TestExecCommandForms asserts what each form of --exec-command records:
// the default and a bare name as given (kubectl looks them up on PATH), an
// absolute path as given, a relative one made absolute from the working
// directory, a leading ~ as the home directory however the shell passed
// it, and the refusals of ~user and of an empty value.
func TestExecCommandForms(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("the POSIX forms; TestExecCommandWindowsForms has Windows' own")
	}
	wd := t.TempDir()
	t.Chdir(wd)
	realWD, err := os.Getwd()
	require.NoError(t, err)
	home := filepath.Join(t.TempDir(), "home")
	prev := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = prev })

	for _, tc := range []struct {
		name, value string
		set         bool
		want        string
		path        bool
		refusal     string
	}{
		{name: "the default", want: "rancher"},
		{name: "a bare name", value: "rancher-dev", set: true, want: "rancher-dev"},
		{name: "an absolute path, as given", value: "/opt//bin/rancher", set: true, want: "/opt//bin/rancher", path: true},
		{name: "a relative path", value: "bin/rancher", set: true, want: filepath.Join(realWD, "bin", "rancher"), path: true},
		{name: "a ./ path", value: "./rancher", set: true, want: filepath.Join(realWD, "rancher"), path: true},
		{name: "a path through ..", value: "x/../bin/rancher", set: true, want: filepath.Join(realWD, "bin", "rancher"), path: true},
		{name: "~", value: "~", set: true, want: home, path: true},
		{name: "~/ as a shell leaves it after =", value: "~/bin/rancher", set: true, want: home + "/bin/rancher", path: true},
		{name: "~user", value: "~alice/bin/rancher", set: true, refusal: "~user is not expanded here"},
		{name: "empty", value: "", set: true, refusal: "--exec-command needs a command name or path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, path, err := execCommand(tc.value, tc.set)
			if tc.refusal != "" {
				require.ErrorContains(t, err, tc.refusal)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.path, path)
		})
	}
}

// TestKubeconfigWritesTheExecCommandExactly asserts the exec entry's
// command is the resolved value, byte for byte, and that a refused value
// writes nothing and makes no request.
func TestKubeconfigWritesTheExecCommandExactly(t *testing.T) {
	dir := writeTestConfig(t, "")
	wd := t.TempDir()
	t.Chdir(wd)
	realWD, err := os.Getwd()
	require.NoError(t, err)
	cmd := newTestCommand(dir, config.NewFileStore(dir), stubFactory{}).(runtime.ReadCommand)

	res, err := cmd.Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1", "exec-command": "tools/my rancher"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(realWD, "tools", "my rancher"), loadKubeconfig(t, res).AuthInfos["prod-c-1"].Exec.Command)

	res, err = cmd.Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "rancher", loadKubeconfig(t, res).AuthInfos["prod-c-1"].Exec.Command)

	refusing := newTestCommand(dir, config.NewFileStore(dir), stubFactory{resolveErr: errors.New("no request may be made")}).(runtime.ReadCommand)
	res, err = refusing.Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1", "exec-command": ""}}, nil)
	require.ErrorContains(t, err, "--exec-command needs a command name or path")
	assert.Nil(t, res.RawBytes)
}

// TestExecCommandIsPath asserts the one rule the command and its
// completion share: a leading ~ or a path separator makes a path, anything
// else is a name kubectl looks up on PATH.
func TestExecCommandIsPath(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"~": true, "~/bin/rancher": true, "~alice": true, "bin/rancher": true, "/opt/rancher": true, "./rancher": true,
		"rancher": false, "rancher-dev": false, "": false,
	} {
		assert.Equal(t, want, ExecCommandIsPath(value), value)
	}
}
