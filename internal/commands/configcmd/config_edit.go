package configcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// configEditSkeleton seeds the temp buffer when no config file exists yet -
// commented-out so an unedited save still parses (as an empty config) rather
// than erroring. Carries no example server block: config edit can no longer
// create one (servers/currentServer are rejected at commit), so seeding one
// would only teach a workflow this command immediately refuses.
const configEditSkeleton = `# No rancher CLI config file exists yet at this path.
# Save this file as-is (or empty) to leave nothing configured.
# config edit cannot add or change servers or the current server - use
# rancher config add-server / update-server / delete-server / use-server.
`

type configEditCmd struct {
	dir string
	// launchEditor invokes the resolved editor (bin+args from $VISUAL/
	// $EDITOR) against tempPath, injectable for tests; defaults to
	// execEditor. A non-nil return (a launch failure or a non-zero exit
	// alike - exec.Cmd.Run reports both as an error) means abort: the real
	// config file is untouched, and there is no meaningful edit to
	// preserve, so the temp buffer is removed.
	launchEditor func(bin string, args []string, tempPath string) error
}

// NewConfigEdit builds `config edit` (local): a kubectl-style edit loop on a
// TEMP COPY, never in place - copy, launch $VISUAL/$EDITOR, parse+validate
// on exit, write back atomically only once the buffer validates.
func NewConfigEdit(dir string) runtime.Command {
	return &configEditCmd{dir: dir, launchEditor: execEditor}
}

func (configEditCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "config edit",
		Usage:       "Edit the config file in $VISUAL or $EDITOR (needs a TTY)",
		CommandType: runtime.Local,
		Examples:    []string{"rancher config edit"},
		Shape:       runtime.ShapeText,
	}
}

func (c configEditCmd) Execute(ctx context.Context, _ runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	realPath := config.FilePath(c.dir)

	// Hard gate, checked once before any editing starts (not re-checked per
	// loop iteration - env/TTY state cannot change mid-run). CanPrompt()
	// already ORs "no TTY" and "--no-input"/NoInput, so one check covers
	// both.
	if !p.CanPrompt() {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("config edit needs a TTY - edit %s directly", realPath))
	}

	bin, args, ok := resolveEditor()
	if !ok {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("config edit needs $VISUAL or $EDITOR set to launch an editor - edit %s directly", realPath))
	}

	tempPath, baseline, existed, err := newTempCopy(realPath)
	if err != nil {
		return runtime.Result{}, err
	}

	for {
		if lerr := c.launchEditor(bin, args, tempPath); lerr != nil {
			// A plain launch/non-zero-exit abort has no "rejected but
			// meaningful edit" to preserve (the buffer is still whatever it
			// was before this failed attempt) - clean it up rather than
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
		if verr == nil {
			verr = checkEditRestrictions(baseline, existed, cfg)
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
		// editor was open, conflict - the user's edits are preserved at tempPath.
		fnRan := false
		writeErr := config.WithLock(ctx, c.dir, func() error {
			fnRan = true
			current, readErr := os.ReadFile(realPath)
			currentExisted := true
			if os.IsNotExist(readErr) {
				current = nil
				currentExisted = false
				readErr = nil
			}
			if readErr != nil {
				// A failed comparison read is not a write failure - the
				// write was never attempted. Say which step broke.
				return clierr.New(clierr.CodeGeneric,
					fmt.Sprintf("reading the config to check for concurrent changes failed; your edits are preserved at %s: %v", tempPath, readErr))
			}
			if currentExisted != existed || !bytes.Equal(current, baseline) {
				return clierr.New(clierr.CodeConflict,
					fmt.Sprintf("the configuration changed while you were editing; your edits are preserved at %s", tempPath))
			}
			return config.WriteRawConfig(realPath, raw)
		})
		if writeErr != nil {
			// Cancellation while waiting for the write lock passes through
			// unchanged, like every other locked command - dressing it as a
			// write failure would send the user diagnosing the wrong thing.
			// The edits stay preserved at tempPath either way.
			if errors.Is(writeErr, context.Canceled) {
				return runtime.Result{}, writeErr
			}
			// Both the conflict clierr and I/O errors preserve tempPath - in
			// the conflict case the user needs it to recover their edits; for
			// I/O failures the user's work must not be lost.
			if ce, ok := errors.AsType[*clierr.Error](writeErr); ok {
				// A clierr produced inside fn (conflict, read failure)
				// already names the preserved temp path at its source; a
				// failure BEFORE fn ran can only be lock acquisition, which
				// cannot know the path - say the edits are safe for it. The
				// distinction is the fnRan fact, not message sniffing.
				if !fnRan {
					return runtime.Result{}, ce.WithMessage(
						fmt.Sprintf("%s; your edits are preserved at %s", ce.Message, tempPath))
				}
				return runtime.Result{}, writeErr
			}
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("writing config failed; your edits are preserved at %s: %v", tempPath, writeErr))
		}
		_ = os.Remove(tempPath)
		return runtime.Result{
			Kind: "ConfigEdit",
			Data: map[string]any{"path": realPath},
			Text: "config updated (" + realPath + ")\n",
		}, nil
	}
}

// promptReEdit asks whether to re-edit after an invalid buffer, defaulting
// to yes. Anything not recognized as an explicit "no" (including a Prompter
// error) is treated as "no" - abort-safe, never loops on garbage input.
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

	content, readErr := config.ReadRawConfig(realPath)
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

// credentialStoreOffense is the restriction for the declaration, with the
// remedy each kind of change has: a set or a change is a login that names
// the store, once the other entries are logged out of the one being left;
// nothing removes a declaration, and a different --config starts fresh.
func credentialStoreOffense(before, after string) string {
	switch {
	case before == after:
		return ""
	case after == "":
		return "credentialStore removed (no command removes a declaration; a different --config starts a fresh directory)"
	case before == "":
		return "credentialStore set; <name> is the entry to log in; log out the other entries first when there are any; use: " +
			clierr.Rancher + " auth login <name> --credential-store " + clierr.ShellQuote(after)
	default:
		return "credentialStore changed; <name> is the entry to log in; log out the other entries first when there are any; use: " +
			clierr.Rancher + " auth login <name> --credential-store " + clierr.ShellQuote(after)
	}
}

// checkEditRestrictions diffs the edited buffer against the pre-edit
// baseline and rejects any change to servers or currentServer - the
// registration/update/delete/use verbs are the single-write-path invariant's
// only drivers of entry and pointer state; config edit's own compare-and-
// write check (a raw byte comparison against a possibly-changed concurrent
// file) is a DIFFERENT, unrelated guard and runs separately, after this one
// passes. baselineExisted false means there was no prior file at all (an
// empty Config, not one parsed from zero bytes).
func checkEditRestrictions(baseline []byte, baselineExisted bool, edited *config.Config) error {
	before := &config.Config{Servers: map[string]config.Server{}}
	if baselineExisted {
		parsed, err := config.ParseConfig(baseline)
		if err != nil {
			return err
		}
		before = parsed
	}

	names := make(map[string]bool, len(before.Servers)+len(edited.Servers))
	for n := range before.Servers {
		names[n] = true
	}
	for n := range edited.Servers {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var offenses []string
	caFileSaid := false
	for _, n := range sorted {
		b, bOK := before.Servers[n]
		e, eOK := edited.Servers[n]
		switch {
		case !bOK && eOK:
			offenses = append(offenses, fmt.Sprintf("servers.%s added; <url> is the server's address; add it with: ", n)+
				clierr.Rancher+" config add-server"+clierr.ShellQuoteArgs(" --url <url>", n))
		case bOK && !eOK:
			offenses = append(offenses, fmt.Sprintf("servers.%s removed; use: ", n)+deleteServerCommand(n))
		case bOK && eOK && !reflect.DeepEqual(b, e):
			caFlags := config.CAFileArg("--cacert") + " or --clear-cacert changes the CA instead"
			if !caFileSaid {
				// The message names <file> once; the first server that
				// changed says what it stands for.
				caFlags += "; " + config.CAFileMeaning
				caFileSaid = true
			}
			offenses = append(offenses, fmt.Sprintf("servers.%s changed; <url> is the server's address; ", n)+caFlags+"; use: "+
				clierr.Rancher+" config update-server"+clierr.ShellQuoteArgs(" --url <url>", n))
		}
	}
	if before.CurrentServer != edited.CurrentServer {
		offenses = append(offenses, "currentServer changed; <name> is the entry to make current; use: "+clierr.Rancher+" config use-server <name>")
	}
	// The setting decides whether the exec plugin reads its own login
	// entries, so a cached kubectl credential is judged against it, and
	// only a config write that moves the binding stamp can change it
	// without a reader serving a record against the old value. This
	// command writes the file as edited and moves no stamp.
	if before.TokenReuse != edited.TokenReuse {
		state := "off"
		if edited.TokenReuse {
			state = "on"
		}
		offenses = append(offenses, "tokenReuse changed; use: "+clierr.Rancher+" config set-token-reuse "+state)
	}
	// Whether recording is on is judged by the effective value: only the
	// exact text "false" turns it off, and an absent or invalid key leaves it
	// on. An edit that changes it would skip the deletion that
	// `telemetry off` performs, so it is refused.
	if wasOff, nowOff := telemetryOff(before), telemetryOff(edited); wasOff != nowOff {
		state := "on"
		if nowOff {
			state = "off"
		}
		offenses = append(offenses, "telemetry changed; use: "+clierr.Rancher+" telemetry "+state)
	}
	// The binding stamp is how a cache reader tells a load that was
	// overtaken from one that was not. Editing it by hand makes records
	// from before a login look current again, which is the stale-credential
	// case the stamp exists to close. It advances on its own.
	if before.BindingStamp != edited.BindingStamp {
		offenses = append(offenses, "bindingStamp changed (it advances on its own with any login or logout)")
	}
	// The transition id is what makes a recorded store change active.
	// Clearing it by hand abandons an old copy the change still has to
	// delete; setting it names a change that was never prepared.
	if before.CredentialTransitionID != edited.CredentialTransitionID {
		offenses = append(offenses, "credentialTransitionId changed (a credential store change clears it when it finishes)")
	}
	// The declaration is the store every reader of this directory opens.
	// Setting, changing or removing it by hand moves them to a store the
	// edit put nothing in: a stale token there is served, and a removed
	// declaration falls back to `auto` and a pin that may name the file
	// store. Only a login changes it, after its own credential is stored.
	if offense := credentialStoreOffense(before.CredentialStore, edited.CredentialStore); offense != "" {
		offenses = append(offenses, offense)
	}
	if len(offenses) == 0 {
		return nil
	}
	return clierr.New(clierr.CodeValidation, "config edit cannot change: "+strings.Join(offenses, "; "))
}

// telemetryOff reports whether the config turns recording off.
func telemetryOff(c *config.Config) bool {
	return telemetry.ConfigSettingOf(c.Telemetry) == telemetry.ConfigOff
}
