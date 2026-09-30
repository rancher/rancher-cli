package kubeconfig

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// defaultExecCommand is the command an exec entry runs unless
// --exec-command names another: the bare name, which kubectl looks up on
// PATH where it runs.
const defaultExecCommand = "rancher"

// userHomeDir is os.UserHomeDir behind a package var, so a test can give
// the ~ expansion a home of its own.
var userHomeDir = os.UserHomeDir

// execCommand is the exec entry's command for --exec-command's value, and
// whether it is a path. A bare name (no path separator: / everywhere, and
// \ on Windows) is written as given; kubectl looks it up on PATH where it
// runs, the convention the other exec plugins follow. A leading ~ or ~/
// (~\ on Windows) is the home directory first: a shell leaves it as typed
// in --exec-command=~/bin/x and in quotes. A path is made absolute against
// the working directory kubeconfig ran in, since kubectl runs from
// another; an absolute path is written as given. The value is not checked
// for existence: it is meaningful where kubectl runs, and may be a
// wrapper.
func execCommand(value string, set bool) (command string, path bool, err error) {
	if !set {
		return defaultExecCommand, false, nil
	}
	if value == "" {
		return "", false, clierr.New(clierr.CodeValidation, "--exec-command needs a command name or path")
	}
	if rest, ok := strings.CutPrefix(value, "~"); ok {
		if rest != "" && !os.IsPathSeparator(rest[0]) {
			return "", false, clierr.New(clierr.CodeValidation, "--exec-command: write the path out; ~user is not expanded here")
		}
		home, herr := userHomeDir()
		if herr != nil {
			return "", false, clierr.New(clierr.CodeValidation, "--exec-command: the home directory for ~ cannot be found: "+herr.Error())
		}
		return home + rest, true, nil
	}
	if !ExecCommandIsPath(value) {
		return value, false, nil
	}
	if filepath.IsAbs(value) {
		return value, true, nil
	}
	abs, aerr := filepath.Abs(value)
	if aerr != nil {
		return "", false, clierr.New(clierr.CodeValidation, "--exec-command: "+aerr.Error())
	}
	return abs, true, nil
}

// ExecCommandIsPath reports whether --exec-command reads value as a path
// rather than a name kubectl looks up on PATH: a leading ~, or a separator
// of this platform's paths (/ everywhere, and \ on Windows). Completion
// asks the same question, so what it offers means what the command reads.
func ExecCommandIsPath(value string) bool {
	return strings.HasPrefix(value, "~") || strings.ContainsFunc(value, isPathSeparator)
}

// isPathSeparator reports a separator of this platform's paths.
func isPathSeparator(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) }
