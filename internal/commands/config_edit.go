package commands

import (
	"context"
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

	tempPath, err := newTempCopy(realPath)
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

		if werr := config.WriteRawConfig(realPath, raw); werr != nil {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("writing config failed; your edits are preserved at %s: %v", tempPath, werr))
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

// newTempCopy copies the current config file (or a commented empty skeleton
// when none exists) into a new 0600 temp file in the SAME DIRECTORY as
// realPath — required for atomic rename semantics (a cross-device rename
// fails), and it also means a preserved buffer sits right next to the file
// it would have replaced.
func newTempCopy(realPath string) (string, error) {
	dir := filepath.Dir(realPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	content, err := os.ReadFile(realPath)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		content = []byte(configEditSkeleton)
	default:
		return "", err
	}

	tmp, err := os.CreateTemp(dir, ".rancher-config-edit-*.yaml")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(0600); err != nil {
		cleanup()
		return "", err
	}
	if err := config.HardenTempFileACL(tmpPath); err != nil {
		cleanup()
		return "", err
	}
	if _, err := tmp.Write(content); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}
