package telemetrycmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// commandWith builds a setting command around the fake.
func commandWith(name, dir string, f *fakeChanges) rt.Command {
	switch name {
	case "on":
		return &onCmd{dir: dir, changes: f}
	case "off":
		return &offCmd{dir: dir, changes: f}
	case "reset":
		return &resetCmd{dir: dir, changes: f}
	}
	panic("unknown command " + name)
}

// configMap decodes config.yaml into a map.
func configMap(t *testing.T, dir string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(readConfig(t, dir)), &m))
	return m
}

const fullConfig = `currentServer: prod
tokenReuse: true
readOnly: true
completion:
  network: true
servers:
  prod:
    url: https://rancher.example.com
`

// TestOnAndOffWriteTheKeyAndKeepTheRest asserts on and off write the exact
// key telemetry as the text true or false, keep every other key, and create
// a missing config directory.
func TestOnAndOffWriteTheKeyAndKeepTheRest(t *testing.T) {
	controlsEnv(t)
	for _, tc := range []struct {
		name string
		make func(dir string) rt.Command
		want string
	}{
		{"on", NewOn, "true"},
		{"off", NewOff, "false"},
	} {
		t.Run(tc.name+" over a full config", func(t *testing.T) {
			dir := newDir(t)
			writeConfig(t, dir, fullConfig)
			res, err := execute(t, tc.make(dir))
			require.NoError(t, err)
			assert.Equal(t, "TelemetrySetting", res.Kind)
			m := configMap(t, dir)
			assert.Equal(t, tc.want, m["telemetry"])
			assert.Equal(t, "prod", m["currentServer"])
			assert.Equal(t, true, m["tokenReuse"])
			assert.Equal(t, true, m["readOnly"])
			assert.Contains(t, m, "completion")
			assert.Contains(t, m, "servers")
			assert.Equal(t, tc.want, dataMap(t, res)["configKey"])
		})
		t.Run(tc.name+" creates the directory", func(t *testing.T) {
			dir := newDir(t)
			_, err := execute(t, tc.make(dir))
			require.NoError(t, err)
			assert.DirExists(t, dir)
			assert.Equal(t, tc.want, configMap(t, dir)["telemetry"])
		})
	}
}

// TestAnUnrelatedWriteKeepsAnAbsentKeyAbsent asserts a config write that
// does not touch the telemetry key neither adds nor drops it.
func TestAnUnrelatedWriteKeepsAnAbsentKeyAbsent(t *testing.T) {
	dir := newDir(t)
	writeConfig(t, dir, fullConfig)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CurrentServer = "prod"
	require.NoError(t, cfg.Write())
	assert.NotContains(t, configMap(t, dir), "telemetry")

	writeConfig(t, dir, fullConfig+"telemetry: \"false\"\n")
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	require.NoError(t, cfg.Write())
	assert.Equal(t, "false", configMap(t, dir)["telemetry"])
}

// TestOnReportsAHigherControl asserts on warns, naming the control, when the
// variable keeps recording off, and not when nothing overrides it.
func TestOnReportsAHigherControl(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	res, err := execute(t, NewOn(dir))
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
	assert.Equal(t, "on", dataMap(t, res)["state"])

	t.Setenv(telemetry.EnvTelemetry, "false")
	res, err = execute(t, NewOn(dir))
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "recording stays off: decided by RANCHER_CLI_TELEMETRY", res.Warnings[0])
	d := dataMap(t, res)
	assert.Equal(t, "off", d["state"])
	assert.Equal(t, "RANCHER_CLI_TELEMETRY", d["decidedBy"])
	assert.Equal(t, "true", d["configKey"])
}

// TestOffDeletesWhatIsRecordedAfterWritingTheKey asserts off writes the key
// first, then deletes events, facts and the id (each separately), and
// reports what is now empty.
func TestOffDeletesWhatIsRecordedAfterWritingTheKey(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	seedStore(t, dir)
	f := &fakeChanges{}
	res, err := execute(t, commandWith("off", dir, f))
	require.NoError(t, err)
	assert.Equal(t, []string{"writeKey", "deleteEvents", "deleteFacts", "deleteDirectoryID"}, f.taken())
	assert.Equal(t, "false", configMap(t, dir)["telemetry"])
	assert.Equal(t, []string{"pruned"}, names(t, filepath.Join(dir, "telemetry")), "only the prune marker is left")
	d := dataMap(t, res)
	assert.Equal(t, []string{"events", "serverFacts", "configDirectoryId"}, d["cleared"])
	assert.Equal(t, "false", d["configKey"])
	assert.Equal(t, "off", d["state"])
	assert.Equal(t, "config", d["decidedBy"])
	assert.Empty(t, res.Warnings)
}

// TestOffWithAFailedKeyWriteDeletesNothing asserts a failed key write
// returns its error and runs no delete.
func TestOffWithAFailedKeyWriteDeletesNothing(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	seedStore(t, dir)
	before := snapshot(t, dir)
	f := &fakeChanges{keyErr: errors.New("disk full")}
	_, err := execute(t, commandWith("off", dir, f))
	require.ErrorContains(t, err, "disk full")
	assert.Equal(t, []string{"writeKey"}, f.taken())
	assert.Equal(t, before, snapshot(t, dir))
}

// TestOffAfterAFailedDeleteReportsWhatIsLeft asserts a failing delete leaves
// the key false, returns a typed error naming what is left and the telemetry
// path with a non-zero exit code, and the other deletes still run.
func TestOffAfterAFailedDeleteReportsWhatIsLeft(t *testing.T) {
	controlsEnv(t)
	boom := errors.New("access denied")
	for _, tc := range []struct {
		name       string
		fake       *fakeChanges
		left       string
		gone       []string
		stillThere []string
	}{
		{"events", &fakeChanges{eventsErr: boom}, "events", []string{"telemetry/servers", "telemetry/config-directory-id"}, []string{"telemetry/events"}},
		{"facts", &fakeChanges{factsErr: boom}, "server facts", []string{"telemetry/events", "telemetry/config-directory-id"}, []string{"telemetry/servers"}},
		{"id", &fakeChanges{idErr: boom}, "config directory id", []string{"telemetry/events", "telemetry/servers"}, []string{"telemetry/config-directory-id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDir(t)
			seedStore(t, dir)
			_, err := execute(t, commandWith("off", dir, tc.fake))
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.NotZero(t, ce.ExitCode())
			assert.Contains(t, ce.Message, `the telemetry key is "false"`)
			assert.Contains(t, ce.Message, filepath.Join(dir, "telemetry"))
			assert.Contains(t, ce.Message, tc.left)
			assert.Contains(t, ce.Message, "access denied")
			assert.Equal(t, "false", configMap(t, dir)["telemetry"])
			assert.Equal(t, []string{"writeKey", "deleteEvents", "deleteFacts", "deleteDirectoryID"}, tc.fake.taken(), "every delete ran")
			for _, p := range tc.gone {
				assert.NoFileExists(t, filepath.Join(dir, filepath.FromSlash(p)))
				assert.NoDirExists(t, filepath.Join(dir, filepath.FromSlash(p)))
			}
			for _, p := range tc.stillThere {
				_, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
				assert.NoError(t, statErr, p)
			}
		})
	}
}

// The failure message when the telemetry path cannot be opened names every
// item, then the path, then the one reason, once.
const (
	unsafeItems  = "events, server facts, config directory id"
	unsafeReason = "telemetry path is a symlink or not a directory"
)

// TestOffAndResetOverAnUnsafeTelemetryPath asserts a telemetry symlink and a
// telemetry regular file each give a typed error that names the items left,
// the path once and the reason once, leave the symlink target untouched, and
// for off leave the key false.
func TestOffAndResetOverAnUnsafeTelemetryPath(t *testing.T) {
	controlsEnv(t)
	for _, kind := range []string{"symlink", "regular file"} {
		for _, name := range []string{"off", "reset"} {
			t.Run(kind+" "+name, func(t *testing.T) {
				dir := newDir(t)
				require.NoError(t, os.MkdirAll(dir, 0o700))
				outside := t.TempDir()
				seedStore(t, outside)
				if kind == "symlink" {
					skipOnWindows(t, "symlinks differ")
					if err := os.Symlink(filepath.Join(outside, "telemetry"), filepath.Join(dir, "telemetry")); err != nil {
						t.Skipf("symlinks are not available: %v", err)
					}
				} else {
					putFile(t, dir, "telemetry", "not a directory")
				}
				before := snapshot(t, outside)
				_, err := execute(t, commandWith(name, dir, &fakeChanges{}))
				var ce *clierr.Error
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				head := `the telemetry key is "false"`
				if name == "reset" {
					head = "telemetry reset is incomplete"
				}
				require.ErrorContains(t, err, head+": could not delete "+unsafeItems+" from "+filepath.Join(dir, "telemetry")+" ("+unsafeReason+")")
				assert.Equal(t, 1, strings.Count(ce.Message, "a symlink or not a directory"), ce.Message)
				assert.Equal(t, 1, strings.Count(ce.Message, filepath.Join(dir, "telemetry")), ce.Message)
				assert.Equal(t, before, snapshot(t, outside), "nothing is followed through the symlink")
				if name == "off" {
					assert.Equal(t, "false", configMap(t, dir)["telemetry"])
				}
			})
		}
	}
}

// TestFailedDeletesEachNameTheirOwnError asserts that once the store is open,
// each delete that fails is named with its own error, in order, joined by
// "; ", whatever the other errors say.
func TestFailedDeletesEachNameTheirOwnError(t *testing.T) {
	controlsEnv(t)
	for _, name := range []string{"off", "reset"} {
		t.Run(name, func(t *testing.T) {
			dir := newDir(t)
			seedStore(t, dir)
			f := &fakeChanges{eventsErr: errors.New("busy"), idErr: errors.New("access denied")}
			_, err := execute(t, commandWith(name, dir, f))
			require.ErrorContains(t, err, ": events (busy); config directory id (access denied)")
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			head := `the telemetry key is "false"`
			if name == "reset" {
				head = "telemetry reset is incomplete"
			}
			assert.Equal(t, head+": could not delete from "+filepath.Join(dir, "telemetry")+": events (busy); config directory id (access denied)", ce.Message)
			f = &fakeChanges{eventsErr: errors.New("busy"), factsErr: errors.New("busy"), idErr: errors.New("access denied")}
			_, err = execute(t, commandWith(name, dir, f))
			require.ErrorContains(t, err, ": events (busy); server facts (busy); config directory id (access denied)")
			f = &fakeChanges{eventsErr: errors.New("busy"), factsErr: &openError{err: errors.New("telemetry path is a symlink or not a directory")}}
			_, err = execute(t, commandWith(name, dir, f))
			require.ErrorContains(t, err, ": events (busy); server facts (telemetry path is a symlink or not a directory)")
		})
	}
}

// TestADeleteOfAPathAlreadyRemovedIsNotAFailure asserts an error wrapping
// fs.ErrNotExist from any delete counts as success, and that off over a
// store with no id file succeeds.
func TestADeleteOfAPathAlreadyRemovedIsNotAFailure(t *testing.T) {
	controlsEnv(t)
	gone := fmt.Errorf("remove x: %w", fs.ErrNotExist)
	for _, name := range []string{"off", "reset"} {
		t.Run(name, func(t *testing.T) {
			dir := newDir(t)
			seedStore(t, dir)
			f := &fakeChanges{eventsErr: gone, factsErr: gone, idErr: gone}
			res, err := execute(t, commandWith(name, dir, f))
			require.NoError(t, err)
			assert.Equal(t, []string{"events", "serverFacts", "configDirectoryId"}, dataMap(t, res)["cleared"])
		})
	}
	t.Run("no id file", func(t *testing.T) {
		dir := newDir(t)
		seedStore(t, dir)
		require.NoError(t, os.Remove(filepath.Join(dir, "telemetry", "config-directory-id")))
		_, err := execute(t, NewOff(dir))
		require.NoError(t, err)
		assert.Equal(t, []string{"pruned"}, names(t, filepath.Join(dir, "telemetry")))
	})
}

// TestOffAndResetDeleteWhateverTheVariableSays asserts both commands delete
// under RANCHER_CLI_TELEMETRY=false and under =true, and off under true
// warns that recording stays on.
func TestOffAndResetDeleteWhateverTheVariableSays(t *testing.T) {
	for _, value := range []string{"false", "true"} {
		for _, name := range []string{"off", "reset"} {
			t.Run(name+" under "+value, func(t *testing.T) {
				controlsEnv(t)
				t.Setenv(telemetry.EnvTelemetry, value)
				dir := newDir(t)
				seedStore(t, dir)
				res, err := execute(t, commandWith(name, dir, &fakeChanges{}))
				require.NoError(t, err)
				assert.Equal(t, []string{"pruned"}, names(t, filepath.Join(dir, "telemetry")))
				if name == "off" && value == "true" {
					require.Len(t, res.Warnings, 1)
					assert.Equal(t, "recording stays on: decided by RANCHER_CLI_TELEMETRY", res.Warnings[0])
					assert.Equal(t, "on", dataMap(t, res)["state"])
				} else {
					assert.Empty(t, res.Warnings)
				}
			})
		}
	}
}

// TestOffFailureCarriesTheVariableWarning asserts a failed off keeps the
// warnings that belong to the run.
func TestOffFailureCarriesTheVariableWarning(t *testing.T) {
	controlsEnv(t)
	t.Setenv(telemetry.EnvTelemetry, "true")
	dir := newDir(t)
	seedStore(t, dir)
	_, err := execute(t, commandWith("off", dir, &fakeChanges{factsErr: errors.New("busy")}))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Warnings(), 1)
	assert.Contains(t, ce.Warnings()[0], "recording stays on")
}

// TestResetDeletesExactlyTheRecordedData asserts reset removes the events,
// facts and id and nothing else: config.yaml (bytes and modification time)
// and the prune marker are unchanged, the key keeps its text, and reset
// never calls the key writer.
func TestResetDeletesExactlyTheRecordedData(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	seedStore(t, dir)
	writeConfig(t, dir, fullConfig+"telemetry: \"false\"\n")
	hourAgo := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "config.yaml"), hourAgo, hourAgo))
	putFile(t, dir, "other-file", "keep")
	cfgBefore := snapshotOne(t, filepath.Join(dir, "config.yaml"))
	markerBefore := snapshotOne(t, filepath.Join(dir, "telemetry", "pruned"))
	otherBefore := snapshotOne(t, filepath.Join(dir, "other-file"))

	f := &fakeChanges{}
	res, err := execute(t, commandWith("reset", dir, f))
	require.NoError(t, err)
	assert.Equal(t, []string{"deleteEvents", "deleteFacts", "deleteDirectoryID"}, f.taken(), "reset never calls the key writer")
	assert.Equal(t, []string{"pruned"}, names(t, filepath.Join(dir, "telemetry")))
	assert.Equal(t, cfgBefore, snapshotOne(t, filepath.Join(dir, "config.yaml")))
	assert.Equal(t, markerBefore, snapshotOne(t, filepath.Join(dir, "telemetry", "pruned")))
	assert.Equal(t, otherBefore, snapshotOne(t, filepath.Join(dir, "other-file")))
	assert.Equal(t, "false", configMap(t, dir)["telemetry"])
	assert.Equal(t, "TelemetryReset", res.Kind)
	assert.Equal(t, []string{"events", "serverFacts", "configDirectoryId"}, dataMap(t, res)["cleared"])
}

func snapshotOne(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return fmt.Sprintf("%d %d %s", info.Size(), info.ModTime().UnixNano(), data)
}

// TestResetCreatesNothingWithoutADirectory asserts reset over a missing and
// an empty config directory succeeds and creates nothing, and runs no
// delete for a missing one.
func TestResetCreatesNothingWithoutADirectory(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	f := &fakeChanges{}
	res, err := execute(t, commandWith("reset", dir, f))
	require.NoError(t, err)
	assert.Empty(t, f.taken(), "a missing directory calls nothing in the store")
	assert.NoDirExists(t, dir)
	assert.Contains(t, res.Text, "nothing to delete")

	require.NoError(t, os.MkdirAll(dir, 0o700))
	before := snapshot(t, dir)
	_, err = execute(t, NewReset(dir))
	require.NoError(t, err)
	assert.Equal(t, before, snapshot(t, dir))
}

// TestTheVariableDeletesNothing asserts status, show and on leave every
// event, facts file and the id in place under RANCHER_CLI_TELEMETRY=false
// and under an ignored value.
func TestTheVariableDeletesNothing(t *testing.T) {
	for _, value := range []string{"false", "off", "no"} {
		t.Run(value, func(t *testing.T) {
			controlsEnv(t)
			t.Setenv(telemetry.EnvTelemetry, value)
			dir := newDir(t)
			seedStore(t, dir)
			before := snapshot(t, filepath.Join(dir, "telemetry"))
			for _, cmd := range []rt.Command{NewStatus(dir, telemetry.ExperimentView{}), NewShow(dir), NewOn(dir)} {
				_, err := execute(t, cmd)
				require.NoError(t, err)
				assert.Equal(t, before, snapshot(t, filepath.Join(dir, "telemetry")), cmd.Describe().Name)
			}
		})
	}
}

// TestTelemetryOnSerializedUnderConfigLock asserts the lock boundary of on.
func TestTelemetryOnSerializedUnderConfigLock(t *testing.T) {
	controlsEnv(t)
	dir := cmdtest.SeedTwoServerConfig(t)
	cmdtest.AssertSerializedWithLoadUnderLock(t, dir,
		func() { _, err := execute(t, NewOn(dir)); assert.NoError(t, err) },
		func() { cmdtest.WriteSentinelServer(t, dir) },
		func() { _, err := execute(t, NewOn(dir)); assert.NoError(t, err) },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok)
	require.NotNil(t, cfg.Telemetry)
	assert.Equal(t, "true", *cfg.Telemetry)
}

// TestTelemetryOffSerializedUnderConfigLock asserts the lock boundary of off.
func TestTelemetryOffSerializedUnderConfigLock(t *testing.T) {
	controlsEnv(t)
	dir := cmdtest.SeedTwoServerConfig(t)
	seedStore(t, dir)
	cmdtest.AssertSerializedWithLoadUnderLock(t, dir,
		func() { _, err := execute(t, NewOff(dir)); assert.NoError(t, err) },
		func() { cmdtest.WriteSentinelServer(t, dir) },
		func() { _, err := execute(t, NewOff(dir)); assert.NoError(t, err) },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok)
	require.NotNil(t, cfg.Telemetry)
	assert.Equal(t, "false", *cfg.Telemetry)
}

// TestTwoParallelOffsBothSucceedAndEmptyTheStore asserts two off commands run
// at once over a seeded store both exit 0 and leave nothing recorded.
func TestTwoParallelOffsBothSucceedAndEmptyTheStore(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	seedStore(t, dir)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = NewOff(dir).(rt.ReadCommand).Execute(t.Context(), cmdtest.Input{}, nil)
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, []string{"pruned"}, names(t, filepath.Join(dir, "telemetry")))
	assert.Equal(t, "false", configMap(t, dir)["telemetry"])
}

// TestWarningsPerCommand asserts the warnings of all five commands: an
// ignored variable value warns in every command with the value quoted; an
// invalid key warns in status, show and reset with the value quoted, while on
// and off replace it, carry the old text in replacedValue and say so in
// their text, with no invalid-key warning.
func TestWarningsPerCommand(t *testing.T) {
	commands := []string{"status", "on", "off", "reset", "show"}
	build := func(name, dir string) rt.Command {
		switch name {
		case "status":
			return NewStatus(dir, telemetry.ExperimentView{})
		case "on":
			return NewOn(dir)
		case "off":
			return NewOff(dir)
		case "reset":
			return NewReset(dir)
		}
		return NewShow(dir)
	}
	for _, value := range []string{"off", "a b\nc"} {
		for _, name := range commands {
			t.Run("variable "+fmt.Sprintf("%q", value)+" "+name, func(t *testing.T) {
				controlsEnv(t)
				t.Setenv(telemetry.EnvTelemetry, value)
				dir := newDir(t)
				writeConfig(t, dir, "currentServer: \"\"\n")
				res, err := execute(t, build(name, dir))
				require.NoError(t, err)
				want := "RANCHER_CLI_TELEMETRY is set to " + quoted(value) + " and is ignored; only true or false are valid"
				assert.Contains(t, res.Warnings, want)
				for _, w := range res.Warnings {
					assert.NotContains(t, w, "\n")
				}
			})
		}
	}
	for _, key := range []string{"maybe", "a b\nc"} {
		for _, name := range commands {
			t.Run("key "+fmt.Sprintf("%q", key)+" "+name, func(t *testing.T) {
				controlsEnv(t)
				dir := newDir(t)
				writeConfig(t, dir, "telemetry: "+quoted(key)+"\n")
				res, err := execute(t, build(name, dir))
				require.NoError(t, err)
				keyWarning := "the telemetry key in " + filepath.Join(dir, "config.yaml") + " is " + quoted(key) + " and is ignored; only true or false are valid"
				switch name {
				case "on", "off":
					assert.NotContains(t, res.Warnings, keyWarning)
					for _, w := range res.Warnings {
						assert.NotContains(t, w, "invalid")
					}
					assert.Equal(t, key, dataMap(t, res)["replacedValue"])
					assert.Contains(t, res.Text, "replaced the invalid value "+quoted(key)+" of the telemetry key")
				default:
					assert.Contains(t, res.Warnings, keyWarning)
				}
			})
		}
	}
}

// TestOnAndOffOmitReplacedValueWhenNothingWasReplaced asserts replacedValue is
// absent for an absent, true or false key.
func TestOnAndOffOmitReplacedValueWhenNothingWasReplaced(t *testing.T) {
	controlsEnv(t)
	for _, cfg := range []string{"", "telemetry: \"true\"\n", "telemetry: \"false\"\n"} {
		dir := newDir(t)
		writeConfig(t, dir, cfg+"currentServer: \"\"\n")
		res, err := execute(t, NewOn(dir))
		require.NoError(t, err)
		assert.NotContains(t, dataMap(t, res), "replacedValue", cfg)
		assert.NotContains(t, res.Text, "replaced")
	}
}

// TestSettingResultsEncodeAsJSON asserts the Data of on, off and reset
// encodes with the documented keys.
func TestSettingResultsEncodeAsJSON(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	res, err := execute(t, NewOff(dir))
	require.NoError(t, err)
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.ElementsMatch(t, []string{"configKey", "state", "decidedBy", "cleared"}, keys(got))
	assert.True(t, strings.HasPrefix(res.Text, `telemetry key set to "false"`), res.Text)
	assert.Contains(t, res.Text, "deleted the recorded events, the server facts and the config directory id")
	require.NotNil(t, res.StyledText)
	assert.Equal(t, res.Text, res.StyledText(style.Style{}), "the text a terminal shows is the text checked here")
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
