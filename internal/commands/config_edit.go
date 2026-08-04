package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// configEditSkeleton seeds the temp buffer when no config file exists yet —
// commented-out so an unedited save still parses (as an empty config) rather
// than erroring.
const configEditSkeleton = `# No rancher CLI config file exists yet at this path.
# Save this file as-is (or empty) to leave nothing configured, or uncomment
# and fill in a server/context to create one.
#
# currentContext: my-context
# servers:
#   my-server:
#     url: https://rancher.example.com
# contexts:
#   my-context:
#     server: my-server
`

type configEditCmd struct {
	dir string
	// launchEditor invokes the resolved editor (bin+args from $VISUAL/
	// $EDITOR) against tempPath, injectable for tests; defaults to
	// execEditor. A non-nil return (a launch failure or a non-zero exit
	// alike — exec.Cmd.Run reports both as an error) means abort: the real
	// config file is untouched, and there is no meaningful edit to
	// preserve, so the temp buffer is removed.
	launchEditor func(bin string, args []string, tempPath string) error
}

// NewConfigEdit builds `config edit` (local): a kubectl-style edit loop on a
// TEMP COPY, never in place — copy, launch $VISUAL/$EDITOR, parse+validate
// on exit, write back atomically only once the buffer validates.
func NewConfigEdit(dir string) runtime.Command {
	return &configEditCmd{dir: dir, launchEditor: execEditor}
}

func (configEditCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config edit",
		Usage:       "Edit the config file in $VISUAL or $EDITOR",
		CommandType: runtime.Local,
		Examples:    []string{"rancher config edit"},
	}
}

func (c configEditCmd) Execute(ctx context.Context, _ runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	realPath := config.FilePath(c.dir)

	// Hard gate, checked once before any editing starts (not re-checked per
	// loop iteration — env/TTY state cannot change mid-run). CanPrompt()
	// already ORs "no TTY" and "--no-input"/NoInput, so one check covers
	// both.
	if !p.CanPrompt() {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("config edit needs a TTY — edit %s directly", realPath))
	}

	bin, args, ok := resolveEditor()
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("config edit needs $VISUAL or $EDITOR set to launch an editor — edit %s directly", realPath))
	}

	tempPath, baseline, existed, err := newTempCopy(realPath)
	if err != nil {
		return runtime.Result{}, err
	}

	for {
		if lerr := c.launchEditor(bin, args, tempPath); lerr != nil {
			// A plain launch/non-zero-exit abort has no "rejected but
			// meaningful edit" to preserve (the buffer is still whatever it
			// was before this failed attempt) — clean it up rather than
			// leaving stray temp files behind.
			_ = os.Remove(tempPath)
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("editor exited with an error, config left untouched: %v", lerr))
		}

		raw, rerr := os.ReadFile(tempPath)
		if rerr != nil {
			return runtime.Result{}, rerr
		}

		cfg, lerr := config.Load(tempPath)
		verr := lerr
		if verr == nil {
			verr = config.Validate(cfg)
		}

		if verr != nil {
			p.Notice(fmt.Sprintf("invalid config: %v", verr))
			again, perr := promptReEdit(ctx, p)
			if perr != nil || !again {
				return runtime.Result{}, clierr.New(clierr.CodeValidation,
					fmt.Sprintf("config edit aborted: invalid config; your edits are preserved at %s (%v)", tempPath, verr))
			}
			continue
		}

		// Write back under the config lock with a compare-and-write check: if
		// the real file changed (bytes differ or existence changed) while the
		// editor was open, conflict — the user's edits are preserved at tempPath.
		writeErr := config.WithLock(c.dir, func() error {
			current, readErr := os.ReadFile(realPath)
			currentExisted := true
			if os.IsNotExist(readErr) {
				current = nil
				currentExisted = false
				readErr = nil
			}
			if readErr != nil {
				return readErr
			}
			if currentExisted != existed || !bytes.Equal(current, baseline) {
				return clierr.New(clierr.CodeValidation,
					fmt.Sprintf("the configuration changed while you were editing; your edits are preserved at %s", tempPath))
			}
			return config.WriteRawConfig(realPath, raw)
		})
		if writeErr != nil {
			// Both the conflict clierr and I/O errors preserve tempPath — in
			// the conflict case the brief requires it; for I/O failures the
			// user's work must not be lost.
			var ce *clierr.Error
			if errors.As(writeErr, &ce) {
				return runtime.Result{}, writeErr
			}
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("writing config failed; your edits are preserved at %s: %v", tempPath, writeErr))
		}
		_ = os.Remove(tempPath)
		return runtime.Result{Kind: "ConfigEdit", Data: map[string]any{"path": realPath}}, nil
	}
}

// promptReEdit asks whether to re-edit after an invalid buffer, defaulting
// to yes. Anything not recognized as an explicit "no" (including a Prompter
// error) is treated as "no" — abort-safe, never loops on garbage input.
func promptReEdit(ctx context.Context, p runtime.Prompter) (bool, error) {
	ans, err := p.Prompt(ctx, "re-edit? [Y/n]: ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "", "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// resolveEditor reads $VISUAL, falling back to $EDITOR, then to the
// per-OS defaultEditor (notepad on Windows, none elsewhere), and splits
// the result into a binary + leading args on whitespace (e.g. "code --wait").
func resolveEditor() (bin string, args []string, ok bool) {
	v := os.Getenv("VISUAL")
	if v == "" {
		v = os.Getenv("EDITOR")
	}
	if strings.TrimSpace(v) == "" {
		v = defaultEditor()
	}
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return "", nil, false
	}
	return fields[0], fields[1:], true
}

// execEditor is launchEditor's real implementation: it execs bin with args
// plus tempPath appended, wiring stdin/stdout/stderr straight through to the
// parent process so the editor gets a real terminal.
func execEditor(bin string, args []string, tempPath string) error {
	cmd := exec.Command(bin, append(append([]string{}, args...), tempPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// newTempCopy writes the editing copy and returns the temp path plus the
// exact baseline it was created from: the real file's bytes and whether
// the file existed (an absent file is baseline (nil, false), distinct
// from an empty file). The write-back compares against this baseline
// under the config lock.
func newTempCopy(realPath string) (tmpPath string, baseline []byte, existed bool, err error) {
	dir := filepath.Dir(realPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, false, err
	}

	content, readErr := os.ReadFile(realPath)
	switch {
	case readErr == nil:
		baseline = content
		existed = true
	case os.IsNotExist(readErr):
		content = []byte(configEditSkeleton)
		// baseline = nil, existed = false
	default:
		return "", nil, false, readErr
	}

	tmp, err := os.CreateTemp(dir, ".rancher-config-edit-*.yaml")
	if err != nil {
		return "", nil, false, err
	}
	tmpPath = tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(0600); err != nil {
		cleanup()
		return "", nil, false, err
	}
	if err := config.HardenTempFileACL(tmpPath); err != nil {
		cleanup()
		return "", nil, false, err
	}
	if _, err := tmp.Write(content); err != nil {
		cleanup()
		return "", nil, false, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", nil, false, err
	}
	return tmpPath, baseline, existed, nil
}
