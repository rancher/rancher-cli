//go:build windows

package kubeconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestExecCommandWindowsForms asserts the Windows spellings of
// --exec-command: a volume path as given with either separator, a
// backslash marks a path, a relative one is made absolute from the working
// directory, and ~\ is the home directory.
func TestExecCommandWindowsForms(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)
	realWD, err := os.Getwd()
	require.NoError(t, err)
	home := `C:\Users\u`
	prev := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = prev })

	for value, want := range map[string]string{
		`C:\tools\rancher.exe`: `C:\tools\rancher.exe`,
		`C:/tools/rancher.exe`: `C:/tools/rancher.exe`,
		`tools\rancher.exe`:    filepath.Join(realWD, "tools", "rancher.exe"),
		`.\rancher.exe`:        filepath.Join(realWD, "rancher.exe"),
		`~\bin\rancher.exe`:    home + `\bin\rancher.exe`,
		`~/bin/rancher.exe`:    home + `/bin/rancher.exe`,
		`rancher`:              `rancher`,
	} {
		got, _, err := execCommand(value, true)
		require.NoError(t, err, value)
		assert.Equal(t, want, got, value)
	}
}

// TestKubeconfigRecordsAWindowsDirectoryAsSpelled asserts a directory
// named in another casing than the file system stores it is recorded as
// spelled, and the remedies name it the same way: two spellings of one
// directory are two directories to a cache record, a miss and never a
// wrong hit, so nothing folds the case.
func TestKubeconfigRecordsAWindowsDirectoryAsSpelled(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCredentialStore, "")
	dir := strings.ToUpper(t.TempDir())
	cfg := prodCurrent(nil)
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	var notices bytes.Buffer
	io := runtime.IO{RemedyDir: dir}
	p := ui.New(&notices, nil, true, false).WithNoticeRewrite(io.ExpandText)
	res, err := NewCommand(dir, config.NewFileStore(dir), stubFactory{}).(runtime.ReadCommand).
		Execute(t.Context(), execInput{s: map[string]string{"cluster": "c-1"}}, p)
	require.NoError(t, err)
	v, _ := envValue(loadKubeconfig(t, res).AuthInfos["prod-c-1"].Exec, config.EnvConfigDir)
	assert.Equal(t, dir, v)
	assert.Contains(t, notices.String(), "rancher --config "+clierr.ShellQuote(dir)+" auth login")
}
