package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	_, err := NewConfigEdit(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, config.FilePath(dir))
	assert.Contains(t, ce.Message, "TTY")
}

// This pins non-Windows behavior only. On Windows, defaultEditor()
// intentionally makes this validation path unreachable (XP-2's contract:
// an always-available notepad fallback) — see
// TestResolveEditorFallsBackToNotepadOnWindows in editor_windows_test.go.
func TestConfigEditMissingEditorIsValidationNamingBothVars(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	dir := t.TempDir()

	_, err := NewConfigEdit(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, &fakePrompter{canPrompt: true})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "VISUAL")
	assert.Contains(t, ce.Message, "EDITOR")
	assert.Contains(t, ce.Message, config.FilePath(dir))
}

// TestConfigEditEditorNonZeroExitAbortsRealFileUntouched drives the REAL
// (unfaked) execEditor default via /usr/bin/false, proving the exec.Command
// wiring itself — not just the injectable seam — treats a non-zero exit as
// abort.
func TestConfigEditEditorNonZeroExitAbortsRealFileUntouched(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "/usr/bin/false")
	dir := t.TempDir()
	seedConfigFile(t, dir, validConfigYAML)
	before, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)

	_, err = NewConfigEdit(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, &fakePrompter{canPrompt: true})
	require.Error(t, err)

	after, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, before, after, "real config file must be untouched after an editor abort")

	leftover, _ := filepath.Glob(filepath.Join(dir, ".rancher-config-edit-*"))
	assert.Empty(t, leftover, "no meaningful edit exists yet on a plain editor abort — the temp buffer is cleaned up")
}

func TestConfigEditLenientParseAcceptsUnknownFieldAndPreservesItOnWrite(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	withUnknownField := validConfigYAML + "futureField: hello\n"

	// Prove Load itself accepts it — the same leniency edit must mirror.
	tmp := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(tmp, []byte(withUnknownField), 0600))
	_, loadErr := config.Load(tmp)
	require.NoError(t, loadErr, "Load must accept an unrecognized field")

	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(withUnknownField)}
	res, err := cmd.Execute(context.Background(), args{}, &fakePrompter{canPrompt: true})
	require.NoError(t, err)
	assert.Equal(t, "ConfigEdit", res.Kind)

	got, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Contains(t, string(got), "futureField: hello", "the unknown field must survive the write-back verbatim (raw bytes, not a re-marshaled struct that would silently drop it)")
}

func TestConfigEditValidateCatchesDanglingContextServer(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	dangling := "contexts:\n  prod:\n    server: gone\nservers: {}\n"

	cmd := &configEditCmd{dir: dir, launchEditor: writesContent(dangling)}
	p := &fakePrompter{canPrompt: true, answers: []string{"n"}}
	_, err := cmd.Execute(context.Background(), args{}, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `references unknown server "gone"`)
}

func TestConfigEditReEditLoopRecoversOnSecondAttempt(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	dangling := "contexts:\n  prod:\n    server: gone\nservers: {}\n"
	fixed := validConfigYAML

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
	res, err := cmd.Execute(context.Background(), args{}, p)
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
	dangling := "contexts:\n  prod:\n    server: gone\nservers: {}\n"

	var capturedTemp string
	launch := func(bin string, args []string, tempPath string) error {
		capturedTemp = tempPath
		return os.WriteFile(tempPath, []byte(dangling), 0600)
	}

	cmd := &configEditCmd{dir: dir, launchEditor: launch}
	p := &fakePrompter{canPrompt: true, answers: []string{"n"}}
	_, err := cmd.Execute(context.Background(), args{}, p)
	require.Error(t, err)
	require.NotEmpty(t, capturedTemp)
	assert.Contains(t, err.Error(), capturedTemp, "the error must name the preserved buffer's path")

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
	_, err := cmd.Execute(context.Background(), args{}, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	matches, _ := filepath.Glob(filepath.Join(dir, ".rancher-config-edit-*"))
	require.Len(t, matches, 1, "the finished (valid) edit buffer must be preserved at its temp path")
	assert.Contains(t, err.Error(), matches[0], "the error must name the preserved buffer's path")

	got, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Equal(t, validConfigYAML, string(got))

	// The symlink itself, and its target, must be untouched — atomicWriteFile
	// refuses BEFORE any rename.
	targetAfter, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, validConfigYAML, string(targetAfter))
	link, err := os.Readlink(realPath)
	require.NoError(t, err)
	assert.Equal(t, target, link)
}

// TestDefaultEditorEmptyOffWindows pins the non-Windows contract: no
// baked-in editor default, so resolveEditor's VISUAL/EDITOR validation
// path (tested above) stays reachable. The "notepad" value is the
// Windows build's concern (editor_windows.go), exercised by the Windows
// acceptance pass — it cannot execute here.
func TestDefaultEditorEmptyOffWindows(t *testing.T) {
	assert.Equal(t, "", defaultEditor())
}

const validConfigYAML = "current-context: prod\nservers:\n  acme:\n    url: https://acme.ex\ncontexts:\n  prod:\n    server: acme\n"

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
