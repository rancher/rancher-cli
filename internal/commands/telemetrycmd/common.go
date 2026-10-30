package telemetrycmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// Names of the three kinds of recorded data, as the commands report them.
const (
	clearedEvents     = "events"
	clearedFacts      = "serverFacts"
	clearedDirectory  = "configDirectoryId"
	telemetryDirName  = "telemetry"
	valueRecordingOn  = "on"
	valueRecordingOff = "off"
)

// state is what the controls say once the key is loaded.
type state struct {
	// key is the telemetry key as loaded; nil when absent.
	key      *string
	decision telemetry.Decision
}

func (s state) stateText() string {
	if s.decision.On {
		return valueRecordingOn
	}
	return valueRecordingOff
}

// resolve resolves the controls for a loaded config.
func resolve(cfg *config.Config) state {
	return resolveKey(cfg.Telemetry)
}

func resolveKey(key *string) state {
	return state{
		key: key,
		decision: telemetry.Resolve(telemetry.ControlInput{
			Getenv: os.Getenv,
			Config: telemetry.ConfigSettingOf(key),
		}),
	}
}

// loadState loads config.yaml of dir and resolves the controls.
func loadState(dir string) (state, error) {
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return state{}, err
	}
	return resolve(cfg), nil
}

// variableWarning is the warning for an ignored RANCHER_CLI_TELEMETRY.
func variableWarning(s state) []string {
	if s.decision.IgnoredVariable == "" {
		return nil
	}
	return []string{fmt.Sprintf("%s is set to %q and is ignored; only true or false are valid",
		telemetry.EnvTelemetry, s.decision.IgnoredVariable)}
}

// keyWarning is the warning for an invalid telemetry key, for the commands
// that leave the key as it is.
func keyWarning(dir string, s state) []string {
	if telemetry.ConfigSettingOf(s.key) != telemetry.ConfigInvalid {
		return nil
	}
	return []string{fmt.Sprintf("the telemetry key in %s is %q and is ignored; only true or false are valid",
		config.FilePath(dir), *s.key)}
}

// keepWarnings is the warnings of a command that leaves the key as it is:
// the ignored variable and the invalid key.
func keepWarnings(dir string, s state) []string {
	return append(variableWarning(s), keyWarning(dir, s)...)
}

// overrideWarning names what keeps recording in a state other
// than the one a command just set.
func overrideWarning(s state, want bool) []string {
	if s.decision.On == want {
		return nil
	}
	now := valueRecordingOn
	if !s.decision.On {
		now = valueRecordingOff
	}
	return []string{fmt.Sprintf("recording stays %s: decided by %s", now, s.decision.By)}
}

// keyText is the telemetry key as the file holds it.
func keyText(key string) string { return fmt.Sprintf("telemetry: %q", key) }

// configDirectoryExists reports whether dir exists. Another stat error is a
// warning and counts as not existing.
func configDirectoryExists(dir string) (bool, []string) {
	_, err := os.Stat(dir)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, []string{fmt.Sprintf("cannot read the config directory %s: %v", dir, err)}
}

// telemetryPath is the path of the telemetry directory under dir.
func telemetryPath(dir string) string { return filepath.Join(dir, telemetryDirName) }

// storeWarning is the warning for a telemetry path that cannot be opened or
// read.
func storeWarning(dir string, err error) string {
	if errors.Is(err, telemetry.ErrUnsafeDir) {
		return fmt.Sprintf("%s is a symlink or not a directory and is not read", telemetryPath(dir))
	}
	return fmt.Sprintf("cannot read %s: %v", telemetryPath(dir), err)
}

// deleteStep is one of the three deletes.
type deleteStep struct {
	name string
	run  func(dir string) error
}

// deleteRecorded runs the three deletes, each one whatever the others did.
// A delete of a path another process already removed is not a failure. It
// returns the names of what could not be deleted and their errors.
func deleteRecorded(c changes, dir string) (left []string, errs []error) {
	steps := []deleteStep{
		{"events", c.deleteEvents},
		{"server facts", c.deleteFacts},
		{"config directory id", c.deleteDirectoryID},
	}
	for _, st := range steps {
		if err := st.run(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			left = append(left, st.name)
			errs = append(errs, err)
		}
	}
	return left, errs
}

// clearedList is what an empty store holds, as Data reports it.
func clearedList() []string { return []string{clearedEvents, clearedFacts, clearedDirectory} }

// deleteFailure is the typed error for deletes that left data behind. head
// says what is true after the command ran; the message then names what could
// not be deleted, where and why. When the store could not be opened, every
// item failed for that one reason, which is said once after the path.
// Otherwise each item is followed by its own error, separated by "; ".
func deleteFailure(head, dir string, left []string, errs []error, warnings []string) error {
	path := telemetryPath(dir)
	var msg string
	if open, ok := sameOpenFailure(errs); ok {
		msg = fmt.Sprintf("%s: could not delete %s from %s (%v)", head, strings.Join(left, ", "), path, open)
	} else {
		parts := make([]string, len(left))
		for i, name := range left {
			parts[i] = fmt.Sprintf("%s (%v)", name, errs[i])
		}
		msg = fmt.Sprintf("%s: could not delete from %s: %s", head, path, strings.Join(parts, "; "))
	}
	return clierr.New(clierr.CodeGeneric, msg).WithWarnings(warnings...)
}

// sameOpenFailure reports the open failure when every error is one: the
// store could not be opened, and the reason is said once. A mix of an open
// failure and another error names each item with its own error.
func sameOpenFailure(errs []error) (*openError, bool) {
	var first *openError
	for _, err := range errs {
		open, ok := errors.AsType[*openError](err)
		if !ok {
			return nil, false
		}
		if first == nil {
			first = open
		}
	}
	return first, first != nil
}
