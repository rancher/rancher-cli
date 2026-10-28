package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// directoryRuleEnv points the home directory, XDG_CONFIG_HOME and
// RANCHER_CLI_CONFIG_DIR at nothing but a fresh home, so the platform
// default directory is that home's and no input names another. It returns
// the home. Not for parallel tests.
func directoryRuleEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(config.EnvConfigDir, "")
	return home
}

// refuseToOpenDirectory replaces the directory open with one that fails
// with a marked hint and records each directory it was asked to open.
func refuseToOpenDirectory(t *testing.T) *[]string {
	t.Helper()
	opened := new([]string)
	prev := openDirectory
	openDirectory = func(dir string, _ bool) (config.CredStore, *config.CacheAnchor, error) {
		*opened = append(*opened, dir)
		return nil, nil, clierr.New(clierr.CodeValidation, "the directory cannot be opened").WithHint(clierr.Rancher + " config status")
	}
	t.Cleanup(func() { openDirectory = prev })
	return opened
}

// runDirectoryLine runs argv through a new root over wr and returns stdout
// and stderr.
func runDirectoryLine(t *testing.T, root *ucli.Command, argv ...string) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	CaptureArgv(argv)
	t.Cleanup(func() { CaptureArgv(nil) })
	_ = root.Run(t.Context(), append([]string{"rancher"}, argv...))
	return out.String(), errOut.String()
}

// hintOf is the text of the hint line of a text error.
func hintOf(t *testing.T, stderr string) string {
	t.Helper()
	for line := range strings.Lines(stderr) {
		if hint, ok := strings.CutPrefix(strings.TrimSpace(line), "hint: "); ok {
			return hint
		}
	}
	require.Fail(t, "no hint line", "stderr: %q", stderr)
	return ""
}

// configStatusHint is the hint refuseToOpenDirectory's error carries, as a
// run that names dir prints it; plain for dir "".
func configStatusHint(dir string) string {
	if dir == "" {
		return "rancher config status"
	}
	return "rancher --config " + clierr.ShellQuote(dir) + " config status"
}

// directoryLine is the words that run d far enough to open its config
// directory: its name, a value for every argument it declares and for every
// required flag. A flag type it has no value for is an error.
func directoryLine(d runtime.Descriptor) ([]string, error) {
	line := append(strings.Fields(d.Name), argValues(d.Args)...)
	for _, f := range d.Flags {
		if !f.Required {
			continue
		}
		switch f.Type {
		case "bool":
			line = append(line, "--"+f.Name)
		case "string", "stringSlice":
			value := "x"
			if len(f.Enum) > 0 {
				value = f.Enum[0]
			}
			line = append(line, "--"+f.Name, value)
		default:
			return nil, fmt.Errorf("%s: no value for required flag --%s of type %q", d.Name, f.Name, f.Type)
		}
	}
	return line, nil
}

// directoryCommands is the descriptor of every command the root wires
// through the non-bare branch of adaptEntry: every entry of the root's entry
// list that is not bare, the support-bundle entry buildWiredRegistry adds
// included.
func directoryCommands(t *testing.T, wr wiredRegistry) []runtime.Descriptor {
	t.Helper()
	skip := map[string]bool{"schema": true, "completion": true}
	listed := map[string]bool{}
	for _, e := range buildEntries() {
		name := e.make(runDeps{}).Describe().Name
		if e.bare {
			skip[name] = true
		} else {
			listed[name] = true
		}
	}
	var out []runtime.Descriptor
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		if skip[d.Name] {
			continue
		}
		out = append(out, d)
		delete(listed, d.Name)
	}
	require.Empty(t, listed, "entries the registry does not hold")
	require.Greater(t, len(out), 150)
	slices.SortFunc(out, func(a, b runtime.Descriptor) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// everyCommandHint runs every command of the root with prefix before its
// line and asserts the hint of its failure to open the directory is
// configStatusHint(want(d)): the directory it names, "" for none.
func everyCommandHint(t *testing.T, prefix []string, want func(runtime.Descriptor) string) {
	t.Helper()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	opened := refuseToOpenDirectory(t)
	cmds := directoryCommands(t, wr)
	ran := 0
	for _, d := range cmds {
		t.Run(d.Name, func(t *testing.T) {
			line, err := directoryLine(d)
			require.NoError(t, err, "a command whose line cannot be built is a failure")
			*opened = nil
			argv := append(slices.Clone(prefix), line...)
			_, stderr := runDirectoryLine(t, wr.root("v-test"), argv...)
			require.Len(t, *opened, 1, "%v does not reach the directory open: %s", argv, stderr)
			assert.Equal(t, configStatusHint(want(d)), hintOf(t, stderr), "%v", argv)
			assert.NotContains(t, stderr, clierr.RemedyMarker)
			ran++
		})
	}
	assert.Equal(t, len(cmds), ran, "every command is run")
}

// platformDefaultDir is the directory a run with no input of its own
// opens.
func platformDefaultDir(t *testing.T) string {
	t.Helper()
	dir, err := config.PlatformDefaultDir()
	require.NoError(t, err)
	return dir
}

// TestEveryCommandNamesANonDefaultConfigDirectory asserts a remedy of every
// command names the run's directory whenever a command pasted without
// --config would open another one: a directory other than the platform
// default, however the run chose it (a --config flag, a relative one, a
// custom XDG_CONFIG_HOME, RANCHER_CLI_CONFIG_DIR, a directory that cannot be
// compared with the default), and the default itself when
// RANCHER_CLI_CONFIG_DIR or XDG_CONFIG_HOME points elsewhere. It names none
// when the variable holds the default path or a symlink reaches the
// default. Not parallel: it sets the environment.
func TestEveryCommandNamesANonDefaultConfigDirectory(t *testing.T) {
	t.Run("config flag", func(t *testing.T) {
		directoryRuleEnv(t)
		dir := filepath.Join(t.TempDir(), "cfg")
		everyCommandHint(t, []string{"--config", dir}, func(runtime.Descriptor) string { return dir })
	})

	t.Run("relative config flag", func(t *testing.T) {
		directoryRuleEnv(t)
		wd := t.TempDir()
		t.Chdir(wd)
		realWD, err := os.Getwd()
		require.NoError(t, err)
		everyCommandHint(t, []string{"--config", "cfg"}, func(runtime.Descriptor) string { return filepath.Join(realWD, "cfg") })
	})

	t.Run("custom XDG_CONFIG_HOME", func(t *testing.T) {
		directoryRuleEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		everyCommandHint(t, nil, func(runtime.Descriptor) string { return filepath.Join(xdg, "rancher") })
	})

	t.Run("a custom XDG_CONFIG_HOME names the platform default given by --config", func(t *testing.T) {
		directoryRuleEnv(t)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		def := platformDefaultDir(t)
		everyCommandHint(t, []string{"--config", def}, func(runtime.Descriptor) string { return def })
	})

	t.Run("RANCHER_CLI_CONFIG_DIR", func(t *testing.T) {
		directoryRuleEnv(t)
		dir := filepath.Join(t.TempDir(), "cfg")
		t.Setenv(config.EnvConfigDir, dir)
		everyCommandHint(t, nil, func(runtime.Descriptor) string { return dir })
	})

	t.Run("RANCHER_CLI_CONFIG_DIR set to the platform default", func(t *testing.T) {
		directoryRuleEnv(t)
		def := platformDefaultDir(t)
		t.Setenv(config.EnvConfigDir, def)
		everyCommandHint(t, nil, func(d runtime.Descriptor) string {
			if d.RemedyConfigDir {
				return def
			}
			return ""
		})
	})

	t.Run("RANCHER_CLI_CONFIG_DIR names another directory than the platform default given by --config", func(t *testing.T) {
		directoryRuleEnv(t)
		def := platformDefaultDir(t)
		t.Setenv(config.EnvConfigDir, filepath.Join(t.TempDir(), "cfg"))
		everyCommandHint(t, []string{"--config", def}, func(runtime.Descriptor) string { return def })
	})

	t.Run("a symlink to the platform default is the default", func(t *testing.T) {
		directoryRuleEnv(t)
		def := platformDefaultDir(t)
		require.NoError(t, os.MkdirAll(def, 0o700))
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(def, link); err != nil {
			t.Skip("no symlink here: " + err.Error())
		}
		everyCommandHint(t, []string{"--config", link}, func(d runtime.Descriptor) string {
			if d.RemedyConfigDir {
				return link
			}
			return ""
		})
	})

	t.Run("XDG_CONFIG_HOME set to the default location", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("the default directory comes from APPDATA there")
		}
		home := directoryRuleEnv(t)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		def := platformDefaultDir(t)
		everyCommandHint(t, nil, func(d runtime.Descriptor) string {
			if d.RemedyConfigDir {
				return def
			}
			return ""
		})
	})

	t.Run("a directory that cannot be compared with the default is named", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("the default directory comes from APPDATA there")
		}
		directoryRuleEnv(t)
		t.Setenv("HOME", "")
		_, err := config.PlatformDefaultDir()
		require.Error(t, err, "with no home directory the default cannot be known")
		dir := filepath.Join(t.TempDir(), "cfg")
		everyCommandHint(t, []string{"--config", dir}, func(runtime.Descriptor) string { return dir })
	})
}

// TestEveryCommandNamesNoDefaultConfigDirectory asserts a run against the
// platform default directory prints its remedies without --config, except
// the commands whose directory comes from an exec entry or a flag the shell
// that pastes a remedy does not carry. Not parallel: it sets the
// environment.
func TestEveryCommandNamesNoDefaultConfigDirectory(t *testing.T) {
	directoryRuleEnv(t)
	def := platformDefaultDir(t)
	everyCommandHint(t, nil, func(d runtime.Descriptor) string {
		if d.RemedyConfigDir {
			return def
		}
		return ""
	})
}

// directoryRemedyCommand is a local command that refuses with a marked hint
// and sets RemedyConfigDir, the way a command that opens no directory itself
// but runs where one is named does.
type directoryRemedyCommand struct {
	unless func(runtime.Input) bool
	ran    *bool
}

func (c directoryRemedyCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "directory-remedy", CommandType: runtime.Local,
		RemedyConfigDir: true, RemedyConfigDirUnless: c.unless}
}

func (c directoryRemedyCommand) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	*c.ran = true
	return runtime.Result{}, clierr.New(clierr.CodeValidation, "refused").WithHint(clierr.Rancher + " config status")
}

// bareRoot is a root with the real global flags and cmd as its one command,
// wired as a bare entry.
func bareRoot(t *testing.T, cmd runtime.Command) *ucli.Command {
	t.Helper()
	reg := runtime.NewRegistry(runtime.MapResolver(provider.NewResolver()))
	leaf := adaptEntry(reg, "v-test", true, func(runDeps) runtime.Command { return cmd })
	wr := wiredRegistry{reg: reg, groups: map[string][]*ucli.Command{"": {leaf}}, order: []string{""}}
	return wr.root("v-test")
}

// TestABareCommandThatSetsRemedyConfigDirNamesItsDirectory asserts a bare
// command whose descriptor sets RemedyConfigDir resolves the directory and
// names it in its remedies, the platform default included, unless its own
// condition says the environment already carries it; and that a directory
// that cannot be resolved renders as the failure to resolve one does, with
// the command not run. Not parallel: it sets the environment.
func TestABareCommandThatSetsRemedyConfigDirNamesItsDirectory(t *testing.T) {
	t.Run("always named, the default included", func(t *testing.T) {
		directoryRuleEnv(t)
		def := platformDefaultDir(t)
		var ran bool
		_, stderr := runDirectoryLine(t, bareRoot(t, directoryRemedyCommand{ran: &ran}), "directory-remedy")
		assert.True(t, ran)
		assert.Equal(t, configStatusHint(def), hintOf(t, stderr))
		assert.NotContains(t, stderr, clierr.RemedyMarker)

		dir := filepath.Join(t.TempDir(), "cfg")
		ran = false
		_, stderr = runDirectoryLine(t, bareRoot(t, directoryRemedyCommand{ran: &ran}), "--config", dir, "directory-remedy")
		assert.True(t, ran)
		assert.Equal(t, configStatusHint(dir), hintOf(t, stderr))
	})

	t.Run("the command's own condition says the environment names it", func(t *testing.T) {
		directoryRuleEnv(t)
		dir := filepath.Join(t.TempDir(), "cfg")
		var ran bool
		cmd := directoryRemedyCommand{ran: &ran, unless: func(runtime.Input) bool { return true }}
		_, stderr := runDirectoryLine(t, bareRoot(t, cmd), "--config", dir, "directory-remedy")
		assert.True(t, ran)
		assert.Equal(t, configStatusHint(""), hintOf(t, stderr))
	})

	t.Run("a directory that cannot be resolved", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("the default directory comes from APPDATA there")
		}
		directoryRuleEnv(t)
		t.Setenv("HOME", "")
		_, resolveErr := config.ResolveDir("")
		require.Error(t, resolveErr)
		var want bytes.Buffer
		require.Error(t, renderDepsError(&ucli.Command{Name: "rancher", ErrWriter: &want}, runtime.IO{}, resolveErr))

		var ran bool
		stdout, stderr := runDirectoryLine(t, bareRoot(t, directoryRemedyCommand{ran: &ran}), "directory-remedy")
		assert.False(t, ran, "the command does not run")
		assert.Empty(t, stdout)
		assert.Equal(t, want.String(), stderr)
		assert.NotContains(t, stderr, "--config")
	})
}

// TestErrorsBeforeTheDirectoryIsResolvedStayPlain records that an error
// rendered before the run's directory is known prints as it always did: an
// unknown command, an unknown flag and a flag whose value is another flag
// each fail before the directory open, and the one command such an error
// names that reads a directory (the detail of an unknown delete kind) is
// written as plain text, which no directory changes. It asserts that detail
// is present and that no --config appears; it does not guard the directory
// rule. Not parallel: it sets the environment.
func TestErrorsBeforeTheDirectoryIsResolvedStayPlain(t *testing.T) {
	directoryRuleEnv(t)
	dir := filepath.Join(t.TempDir(), "cfg")
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	opened := refuseToOpenDirectory(t)
	for _, line := range [][]string{
		{"delete", "cluster", "c-1"},
		{"get", "pod", "--no-such-flag"},
		{"get", "pod", "--cluster", "--output"},
	} {
		argv := append([]string{"--config", dir}, line...)
		_, stderr := runDirectoryLine(t, wr.root("v-test"), argv...)
		assert.Empty(t, *opened, "%v: refused before the directory is opened", argv)
		assert.Contains(t, stderr, "error", "%v", argv)
		assert.NotContains(t, stderr, clierr.RemedyMarker, "%v", argv)
		assert.NotContains(t, stderr, "--config", "%v: %s", argv, stderr)
		if line[0] == "delete" {
			assert.Contains(t, stderr, "rancher delete provisioning-cluster", "%v: the detail names its command", argv)
		}
	}
}
