package configcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// fakePrompter is a scripted runtime.Prompter for tests that need to control
// CanPrompt/NoInput and the re-edit loop's answers deterministically, without
// a real TTY or a piped io.Reader.
type fakePrompter struct {
	canPrompt bool
	answers   []string
	calls     int
	notices   []string
}

func (f *fakePrompter) Prompt(_ context.Context, _ string) (string, error) {
	if f.calls >= len(f.answers) {
		return "", errors.New("fakePrompter: no more scripted answers")
	}
	a := f.answers[f.calls]
	f.calls++
	return a, nil
}
func (f *fakePrompter) PromptSecret(ctx context.Context, msg string) (string, error) {
	return f.Prompt(ctx, msg)
}
func (f *fakePrompter) ReadLine(ctx context.Context) (string, error) { return f.Prompt(ctx, "") }
func (f *fakePrompter) Notice(msg string)                            { f.notices = append(f.notices, msg) }
func (f *fakePrompter) CanPrompt() bool                              { return f.canPrompt }
func (f *fakePrompter) NoInput() bool                                { return !f.canPrompt }

func TestConfigEditNoTTYIsValidationNamingPath(t *testing.T) {
	dir := t.TempDir()
	_, err := NewConfigEdit(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, config.FilePath(dir))
	assert.Contains(t, ce.Message, "TTY")
}

// TestConfigEditEditorNonZeroExitAbortsRealFileUntouched drives the REAL
// (unfaked) execEditor default via /usr/bin/false, proving the exec.Command
// wiring itself - not just the injectable seam - treats a non-zero exit as
// abort.
func TestConfigEditEditorNonZeroExitAbortsRealFileUntouched(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "/usr/bin/false")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)
	before, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)

	_, err = NewConfigEdit(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
	require.Error(t, err)

	after, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, before, after, "real config file must be untouched after an editor abort")

	leftover, _ := filepath.Glob(filepath.Join(dir, ".rancher-config-edit-*"))
	assert.Empty(t, leftover, "no meaningful edit exists yet on a plain editor abort - the temp buffer is cleaned up")
}

func TestConfigEditLenientParseAcceptsUnknownFieldAndPreservesItOnWrite(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	withUnknownField := "readOnly: true\nfutureField: hello\n"

	// Prove Load itself accepts it - the same leniency edit must mirror.
	tmp := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(tmp, []byte(withUnknownField), 0600))
	_, loadErr := config.Load(tmp)
	require.NoError(t, loadErr, "Load must accept an unrecognized field")

	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(withUnknownField)}
	res, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
	require.NoError(t, err)
	assert.Equal(t, "ConfigEdit", res.Kind)
	assert.Equal(t, "config updated ("+config.FilePath(dir)+")\n", res.Text)

	got, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Contains(t, string(got), "futureField: hello", "the unknown field must survive the write-back verbatim (raw bytes, not a re-marshaled struct that would silently drop it)")
}

func TestConfigEditValidateCatchesDanglingCurrentServer(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	dangling := "CurrentServer: gone\nservers: {}\n"

	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(dangling)}
	p := &fakePrompter{canPrompt: true, answers: []string{"n"}}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, p)
	require.Error(t, err)
	assert.ErrorContains(t, err, `currentServer "gone" is not a defined server`)
}

func TestConfigEditReEditLoopRecoversOnSecondAttempt(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	dangling := "CurrentServer: gone\nservers: {}\n"
	fixed := "readOnly: true\n"

	calls := 0
	launch := func(bin string, args []string, tempPath string) error {
		calls++
		if calls == 1 {
			return os.WriteFile(tempPath, []byte(dangling), 0600)
		}
		return os.WriteFile(tempPath, []byte(fixed), 0600)
	}

	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	p := &fakePrompter{canPrompt: true, answers: []string{"y"}}
	res, err := cmd.Execute(t.Context(), cmdtest.Input{}, p)
	require.NoError(t, err)
	assert.Equal(t, "ConfigEdit", res.Kind)
	assert.Equal(t, 2, calls, "the editor must be re-invoked on the same temp file after an accepted re-edit")
	assert.Equal(t, 1, p.calls, "prompted exactly once, after the first invalid attempt")

	got, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, fixed, string(got))
}

func TestConfigEditReEditDeclinedAbortsPreservingRejectedBuffer(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	dangling := "CurrentServer: gone\nservers: {}\n"

	var capturedTemp string
	launch := func(bin string, args []string, tempPath string) error {
		capturedTemp = tempPath
		return os.WriteFile(tempPath, []byte(dangling), 0600)
	}

	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	p := &fakePrompter{canPrompt: true, answers: []string{"n"}}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, p)
	require.Error(t, err)
	require.NotEmpty(t, capturedTemp)
	require.ErrorContains(t, err, capturedTemp, "the error must name the preserved buffer's path")

	got, err := os.ReadFile(capturedTemp)
	require.NoError(t, err, "the rejected buffer must still exist at the temp path")
	assert.Equal(t, dangling, string(got), "the preserved buffer is the user's rejected edit, not a reset to the original file")

	_, err = os.Stat(config.FilePath(dir))
	assert.True(t, os.IsNotExist(err), "the real config file was never touched (it never existed to begin with)")
}

func TestConfigEditFinalWriteFailurePreservesBufferOnSymlinkedConfig(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real-config.yaml")
	require.NoError(t, os.WriteFile(target, []byte(validConfigYAML), 0600))

	realPath := config.FilePath(dir)
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.Symlink(target, realPath))

	launch := writesContent(validConfigYAML)
	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	p := &fakePrompter{canPrompt: true}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, p)
	require.Error(t, err)
	require.ErrorContains(t, err, "symlink")

	matches, _ := filepath.Glob(filepath.Join(dir, ".rancher-config-edit-*"))
	require.Len(t, matches, 1, "the finished (valid) edit buffer must be preserved at its temp path")
	require.ErrorContains(t, err, matches[0], "the error must name the preserved buffer's path")

	got, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.YAMLEq(t, validConfigYAML, string(got))

	// The symlink itself, and its target, must be untouched - atomicWriteFile
	// refuses BEFORE any rename.
	targetAfter, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.YAMLEq(t, validConfigYAML, string(targetAfter))
	link, err := os.Readlink(realPath)
	require.NoError(t, err)
	assert.Equal(t, target, link)
}

const validConfigYAML = "CurrentServer: prod\nservers:\n  prod:\n    url: https://acme.ex\n"

func seedConfigFile(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte(content), 0600))
}

// writesContent returns a launchEditor fake that overwrites tempPath with
// content and reports success, standing in for an editor session that
// replaces the buffer wholesale.
func writesContent(content string) func(bin string, args []string, tempPath string) error {
	return func(bin string, args []string, tempPath string) error {
		return os.WriteFile(tempPath, []byte(content), 0600)
	}
}

// TestConfigEditCleanWriteRemovesTemp proves that a successful write-back
// removes the temp file.
func TestConfigEditCleanWriteRemovesTemp(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	var capturedTemp string
	launch := func(bin string, launchArgs []string, tempPath string) error {
		capturedTemp = tempPath
		return os.WriteFile(tempPath, []byte(validConfigYAML), 0600)
	}

	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
	require.NoError(t, err)

	_, statErr := os.Stat(capturedTemp)
	assert.True(t, os.IsNotExist(statErr), "temp file must be removed on clean write-back")
}

// TestConfigEditConflictPreservesTemp proves that when the real config
// changes while the editor is open the write-back errors, preserves the
// temp file, and leaves the concurrent content in the real file.
func TestConfigEditConflictPreservesTemp(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	const concurrentContent = "CurrentServer: concurrent\n" +
		"servers:\n  concurrent:\n    url: https://concurrent.ex\n"

	var capturedTemp string
	launch := func(bin string, launchArgs []string, tempPath string) error {
		capturedTemp = tempPath
		// Simulate a concurrent write while the editor runs.
		require.NoError(t, os.WriteFile(config.FilePath(dir), []byte(concurrentContent), 0600))
		return os.WriteFile(tempPath, []byte(validConfigYAML), 0600)
	}

	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "changed while you were editing")
	assert.Contains(t, ce.Message, capturedTemp, "error message must name the preserved temp path")

	_, statErr := os.Stat(capturedTemp)
	require.NoError(t, statErr, "temp file must be preserved when conflict is detected")

	got, readErr := os.ReadFile(config.FilePath(dir))
	require.NoError(t, readErr)
	assert.Equal(t, concurrentContent, string(got), "concurrent write must survive in the real file")
}

// TestConfigEditAbsentFileBaseline covers both absent-file baseline
// directions: still-absent is a clean write; created-meanwhile is a conflict.
func TestConfigEditAbsentFileBaseline(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")

	const noServersContent = "readOnly: true\n"

	t.Run("still absent is clean write", func(t *testing.T) {
		dir := t.TempDir()
		cmd := &configEditCmd{dir: dir, launchEditor: writesContent(noServersContent)}
		_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
		require.NoError(t, err)
		got, readErr := os.ReadFile(config.FilePath(dir))
		require.NoError(t, readErr)
		assert.YAMLEq(t, noServersContent, string(got))
	})

	t.Run("created meanwhile is conflict", func(t *testing.T) {
		dir := t.TempDir()
		const concurrentContent = "CurrentServer: created\n" +
			"servers:\n  created:\n    url: https://created.ex\n"

		var capturedTemp string
		launch := func(bin string, launchArgs []string, tempPath string) error {
			capturedTemp = tempPath
			// Concurrently create the config file.
			require.NoError(t, os.WriteFile(config.FilePath(dir), []byte(concurrentContent), 0600))
			return os.WriteFile(tempPath, []byte(noServersContent), 0600)
		}

		cmd := &configEditCmd{dir: dir, launchEditor: launch}
		_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		assert.Contains(t, ce.Message, "changed while you were editing")

		_, statErr := os.Stat(capturedTemp)
		require.NoError(t, statErr, "temp file must be preserved on conflict")

		got, readErr := os.ReadFile(config.FilePath(dir))
		require.NoError(t, readErr)
		assert.Equal(t, concurrentContent, string(got), "concurrent content must survive")
	})
}

// TestConfigEditCanceledLockWaitPassesThrough asserts the re-review P2 finding:
// a cancellation hitting the write-lock wait must surface as the caller's
// own context.Canceled - never dressed as "writing config failed" - while
// the edits stay preserved in the temp file.
func TestConfigEditCanceledLockWaitPassesThrough(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	var capturedTemp string
	launch := func(bin string, launchArgs []string, tempPath string) error {
		capturedTemp = tempPath
		return os.WriteFile(tempPath, []byte(validConfigYAML), 0600)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	_, err := cmd.Execute(ctx, cmdtest.Input{}, &fakePrompter{canPrompt: true})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "cancellation must pass through the write-back path")
	assert.NotContains(t, err.Error(), "writing config failed")

	_, statErr := os.Stat(capturedTemp)
	require.NoError(t, statErr, "edits must stay preserved on cancellation")
}

// TestConfigEditReadFailureNamedAsReadFailure asserts the third-review P2: a
// failed comparison read during write-back must say the CHECK failed, not
// claim "writing config failed" for a write that was never attempted.
func TestConfigEditReadFailureNamedAsReadFailure(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	realPath := config.FilePath(dir)
	launch := func(bin string, launchArgs []string, tempPath string) error {
		// Break the comparison read: replace the real file with a directory.
		if err := os.Remove(realPath); err != nil {
			return err
		}
		if err := os.Mkdir(realPath, 0o700); err != nil {
			return err
		}
		return os.WriteFile(tempPath, []byte(validConfigYAML), 0600)
	}

	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
	require.Error(t, err)
	require.ErrorContains(t, err, "check for concurrent changes", "the failing step must be named")
	assert.NotContains(t, err.Error(), "writing config failed")
}

// TestConfigEditLockTimeoutNamesPreservedEdits asserts the fourth-review P2:
// when lock acquisition itself times out - before the write-back closure
// ever runs - the error must still tell the user their edits are preserved
// at the temp path; that branch is the one guaranteed-safe case and was the
// only one not saying so.
// No t.Parallel(): package-level lock hook and timeout seam.
func TestConfigEditLockTimeoutNamesPreservedEdits(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	restoreTimeout := config.SetLockWaitTimeoutForTest(300 * time.Millisecond)
	defer restoreTimeout()

	// Park a holder on the lock so the edit's write-back times out.
	release := make(chan struct{})
	parked := make(chan struct{})
	var once sync.Once
	restoreHook := config.SetLockHookForTest(func() {
		once.Do(func() { close(parked) })
		<-release
	})
	defer restoreHook()
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- config.WithLock(t.Context(), dir, func() error { return nil })
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("holder never acquired the lock")
	}
	restoreHook() // the edit's write-back must not park in the hook

	var capturedTemp string
	launch := func(bin string, launchArgs []string, tempPath string) error {
		capturedTemp = tempPath
		return os.WriteFile(tempPath, []byte(validConfigYAML), 0600)
	}
	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, capturedTemp, "the lock-timeout branch must name the preserved temp path")
	assert.Contains(t, ce.Message, "held by this same process", "the in-process holder gets the stage-accurate message")

	close(release)
	require.NoError(t, <-holderDone)
	_, statErr := os.Stat(capturedTemp)
	require.NoError(t, statErr, "edits preserved")
}

// TestConfigEditRejectsAddingAServer asserts that config edit no longer
// bypasses the registration path: writing raw YAML would let an added
// entry skip canonicalization, trust, and update-server's credential
// purge. Adding a server via edit must be VALIDATION naming the owning
// command.
func TestConfigEditRejectsAddingAServer(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()

	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(validConfigYAML)}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true, answers: []string{"n"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "servers.prod added")
	assert.Contains(t, ce.Message, "add-server")

	_, statErr := os.Stat(config.FilePath(dir))
	assert.True(t, os.IsNotExist(statErr), "the real config file must never be created by this edit")
}

// TestConfigEditRejectsChangingAServersURL asserts the exact credential-boundary
// violation config edit's restriction closes: editing an existing entry's
// URL from server A to server B would skip update-server's credential
// purge, after which A's name-keyed token is served for B.
func TestConfigEditRejectsChangingAServersURL(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	changed := "CurrentServer: prod\nservers:\n  prod:\n    url: https://different.example.com\n"
	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(changed)}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true, answers: []string{"n"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "servers.prod changed")
	assert.Contains(t, ce.Message, "update-server")

	got, readErr := os.ReadFile(config.FilePath(dir))
	require.NoError(t, readErr)
	assert.YAMLEq(t, validConfigYAML, string(got), "the real config file must be untouched")
}

// TestConfigEditRejectsRemovingAServer asserts the delete-server half of the
// same invariant: removing an entry via edit would skip delete-server's
// credential purge.
func TestConfigEditRejectsRemovingAServer(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	cmd := &configEditCmd{dir: dir, launchEditor: writesContent("servers: {}\n")}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true, answers: []string{"n"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "servers.prod removed")
	assert.Contains(t, ce.Message, "delete-server")
	assert.Contains(t, ce.Message, "currentServer changed")
	assert.Contains(t, ce.Message, "use-server")
}

// TestConfigEditRejectsChangingCurrentServer asserts the pointer half: use-server
// is the only switch verb.
func TestConfigEditRejectsChangingCurrentServer(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	twoServers := "CurrentServer: prod\nservers:\n  prod:\n    url: https://acme.ex\n  qa:\n    url: https://qa.ex\n"
	seedConfigFile(t, dir, twoServers)

	switched := "CurrentServer: qa\nservers:\n  prod:\n    url: https://acme.ex\n  qa:\n    url: https://qa.ex\n"
	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(switched)}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true, answers: []string{"n"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "currentServer changed")
	assert.Contains(t, ce.Message, "use-server")

	got, readErr := os.ReadFile(config.FilePath(dir))
	require.NoError(t, readErr)
	assert.YAMLEq(t, twoServers, string(got))
}

// TestConfigEditAllowsNonServerFieldsUnchanged proves the restriction is
// scoped to servers/currentServer only: completion, credentialStore, and
// readOnly stay editable.
func TestConfigEditAllowsNonServerFieldsUnchanged(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)

	edited := "CurrentServer: prod\nservers:\n  prod:\n    url: https://acme.ex\ncredentialStore: file\nreadOnly: true\n"
	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(edited)}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true})
	require.NoError(t, err)

	got, readErr := os.ReadFile(config.FilePath(dir))
	require.NoError(t, readErr)
	assert.YAMLEq(t, edited, string(got))
}

// TestConfigEditRejectsChangingTheBindingStamp asserts the stamp cannot
// be edited by hand. It is how a cache reader tells a config load that
// was overtaken by a login from one that was not; an operator who
// "repairs" a crash-residue mismatch by copying the file's value into
// config.yaml makes every record from before that login validate again,
// which is the stale-credential case the stamp exists to close. It
// advances on its own with the next login or logout.
func TestConfigEditRejectsChangingTheBindingStamp(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	stamped := "currentServer: prod\nbindingStamp: 0123456789abcdef0123456789abcdef\nservers:\n  prod:\n    url: https://rancher.example.com\n"
	seedConfigFile(t, dir, stamped)

	changed := "currentServer: prod\nbindingStamp: ffffffffffffffffffffffffffffffff\nservers:\n  prod:\n    url: https://rancher.example.com\n"
	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(changed)}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true, answers: []string{"n"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "bindingStamp changed")

	got, readErr := os.ReadFile(config.FilePath(dir))
	require.NoError(t, readErr)
	assert.YAMLEq(t, stamped, string(got), "the real config file must be untouched")
}
