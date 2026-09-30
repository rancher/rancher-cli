package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kubeconfig"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// noLiveSource fails the test when the live layer is asked for a source.
type noLiveSource struct{ t *testing.T }

func (n noLiveSource) LiveSource() completion.Source {
	n.t.Helper()
	n.t.Error("the live layer was consulted for an owned site")
	return completion.Source{}
}

// writeExecutables fills dir with the given files, each executable unless
// named in plain.
func writeExecutables(t *testing.T, dir string, names []string, plain ...string) {
	t.Helper()
	for _, n := range names {
		mode := os.FileMode(0o755)
		if slices.Contains(plain, n) {
			mode = 0o644
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), mode))
	}
}

// TestExecCommandCompletionOffersOnlyWhatTheWordMeans asserts
// kubeconfig --exec-command completes a bare word to the executables on
// PATH that begin with it, else to the word itself, and the empty word to
// rancher; a path word, or one with a leading ~, gets nothing, so the
// shell completes files. Every bare candidate begins with the word. The
// site is owned: with live completion on, the live layer is not asked.
func TestExecCommandCompletionOffersOnlyWhatTheWordMeans(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("executables are marked by their mode here; the Windows extension rule has its own rows")
	}
	bin := t.TempDir()
	other := t.TempDir()
	writeExecutables(t, bin, []string{"rancher", "rancher-dev", "rancher-notes", "rancher tool", "kubectl"}, "rancher-notes")
	writeExecutables(t, other, []string{"rancher-dev", "rancher2"})
	require.NoError(t, os.Mkdir(filepath.Join(bin, "rancher-dir"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+other)
	setStoredConfigForTest(t, &config.Config{Completion: config.CompletionConfig{Network: true}}, t.TempDir())
	d := kubeconfig.NewCommand("", nil, nil).Describe()

	for _, tc := range []struct {
		word string
		want []string
	}{
		{"", []string{"rancher"}},
		{"ran", []string{"rancher", "rancher-dev", "rancher2"}},
		{"rancher-d", []string{"rancher-dev"}},
		{"zzz", []string{"zzz"}},
		{"./ra", nil},
		{"bin/ra", nil},
		{"/usr/b", nil},
		{"~", nil},
		{"~/b", nil},
	} {
		t.Run(tc.word, func(t *testing.T) {
			site := CompletionSite{Kind: SiteFlagValue, FlagName: "exec-command", FlagSpelling: "--exec-command", Partial: tc.word}
			got := dispatchSite(t.Context(), site, d, nil, noLiveSource{t}, []string{"--exec-command"})
			assert.Equal(t, tc.want, got)
			for _, c := range got {
				assert.True(t, strings.HasPrefix(c, tc.word), "%q does not begin with %q", c, tc.word)
			}
			site.EqualsForm = true
			eq := dispatchSite(t.Context(), site, d, nil, noLiveSource{t}, []string{"--exec-command=" + tc.word})
			require.Len(t, eq, len(tc.want))
			for i, c := range eq {
				assert.Equal(t, "--exec-command="+tc.want[i], c)
			}
		})
	}

	t.Run("through the command tree", func(t *testing.T) {
		origArgv, origWord, origProto, origV1 := rawArgv, rawCurrentWord, rawProtoV2, rawV1Completion
		t.Cleanup(func() {
			rawArgv, rawCurrentWord, rawProtoV2, rawV1Completion = origArgv, origWord, origProto, origV1
		})
		t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
		t.Setenv(config.EnvConfigDir, t.TempDir())
		for word, want := range map[string]string{"ranc": "rancher\nrancher-dev\nrancher2\n", "./ra": ""} {
			scriptArgs := []string{"--rancher-completion-proto=2", "kubeconfig", "--cluster", "c-1", "--exec-command",
				"--rancher-completion-cur=" + word, "--generate-shell-completion"}
			CaptureArgv(scriptArgs)
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			var out bytes.Buffer
			root.Writer = &out
			require.NoError(t, root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...))))
			assert.Equal(t, want, out.String(), word)
		}
	})
}

// TestPathExecutablesWindowsExtensions asserts the Windows rule: an
// executable is a file whose extension PATHEXT lists, offered without it.
func TestPathExecutablesWindowsExtensions(t *testing.T) {
	bin := t.TempDir()
	writeExecutables(t, bin, []string{"rancher.exe", "rancher-dev.CMD", "rancher.txt", "rancherx"})
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	assert.Equal(t, []string{"rancher", "rancher-dev"}, pathExecutables("ranc", bin, "windows"))
}
