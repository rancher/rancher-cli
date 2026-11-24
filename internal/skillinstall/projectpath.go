package skillinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

const forceDoesNotPrevent = "; --force does not prevent this error"

// refuseLinkedPath refuses a target that would send a write, a delete or a read
// to a place the command did not name. verb is "install", "uninstall" or
// "status" and selects the wording only.
//
// The skill directory itself must not be a symbolic link or a Windows
// junction, in either scope. A project target must also have a real directory
// at every component between its root (the working directory, which may itself
// be reached through a link) and the directory that holds the skill
// directory: the content of a checkout can hold a link there, and following it
// would replace or remove a directory outside the checkout. A user scope target
// gets no such walk, because the links above it belong to the user.
func refuseLinkedPath(t Target, verb string) error {
	if t.Scope == ScopeProject {
		if err := refuseLinkedParents(t, verb); err != nil {
			return err
		}
	}
	return refuseLinkedFinal(t, verb)
}

// relativePath is filepath.Rel; a test replaces it to reach the error of a
// path on another volume, which Rel returns only on Windows. A test that
// replaces it must not run in parallel.
var relativePath = filepath.Rel

// lstat is os.Lstat; a test replaces it to reach the refusal for a component
// that cannot be checked on every OS, as root too. A test that replaces it must
// not run in parallel.
var lstat = os.Lstat

// refuseLinkedParents Lstats every component of the project target's path
// below its root, except the last, top-down. Top-down matters: a plain file
// at .claude makes .claude/skills read as missing on Windows and as ENOTDIR
// elsewhere, so the file is the component that must be named.
func refuseLinkedParents(t Target, verb string) error {
	// A .. element in either path is refused as written: the walk cleans it
	// away and every operation follows the link before it. Other spellings
	// (a doubled separator, a . element, a trailing separator) name the same
	// components after cleaning and are checked as cleaned.
	if t.Root == "" {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cannot check the skill directory %s: the working directory is not known", t.Dir))
	}
	if hasDotDot(t.Root) {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("the working directory %s has a .. element", t.Root)).
			WithHint("change to the working directory by a path with no .. element, then run this command again")
	}
	if hasDotDot(t.Dir) {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("the skill directory %s has a .. element", t.Dir))
	}
	rel, err := relativePath(t.Root, t.Dir)
	sep := string(filepath.Separator)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+sep) {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("the skill directory %s is not inside the working directory %s", t.Dir, t.Root))
	}
	segments := strings.Split(rel, sep)
	for i := range len(segments) - 1 {
		component := filepath.Join(append([]string{t.Root}, segments[:i+1]...)...)
		info, err := lstat(component)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return checkFailed(t, component, err)
		}
		if !info.Mode().IsDir() {
			return notADirectory(t, verb, component, info.Mode())
		}
	}
	return nil
}

// hasDotDot reports whether an element of path is "..", split on both
// separators.
func hasDotDot(path string) bool {
	isSeparator := func(r rune) bool { return r == '/' || r == filepath.Separator }
	return slices.Contains(strings.FieldsFunc(path, isSeparator), "..")
}

// refuseLinkedFinal applies the rule for the skill directory itself. A real
// directory or a plain file there goes on to the ownership and --force checks.
func refuseLinkedFinal(t Target, verb string) error {
	// Cleaned, because Lstat of a path with a trailing separator follows a link
	// at the last element.
	info, err := lstat(filepath.Clean(t.Dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return checkFailed(t, t.Dir, err)
	}
	mode := info.Mode()
	if mode&fs.ModeSymlink != 0 || (mode&fs.ModeIrregular != 0 && !mode.IsDir()) {
		return notADirectory(t, verb, t.Dir, mode)
	}
	return nil
}

// notADirectory is the refusal for a component that is a symbolic link, a
// junction or another special entry, or a plain file.
func notADirectory(t Target, verb, component string, mode fs.FileMode) error {
	var what string
	switch {
	case mode&fs.ModeSymlink != 0:
		if destination, found := linkTarget(component); found {
			what = "is a symbolic link to " + destination
		} else {
			what = "is a symbolic link whose destination could not be read"
		}
	case mode&fs.ModeIrregular != 0:
		what = "is a Windows directory link (NTFS junction) or another entry that is not a directory"
	default:
		what = "is not a directory"
	}
	msg := fmt.Sprintf("cannot use the skill directory %s: %s, on the path to it, %s", t.Dir, component, what)
	if component == t.Dir {
		msg = fmt.Sprintf("the skill directory %s %s", t.Dir, what)
	}
	if verb == "install" && mode&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		msg += forceDoesNotPrevent
	}
	return clierr.New(clierr.CodeValidation, msg).WithHint(refusalHint(component, mode))
}

// checkFailed is the refusal for a component whose check failed with an error
// other than not-exist. The message carries the operating system's own error
// (the Err of an *fs.PathError), which does not repeat the path.
func checkFailed(t Target, component string, err error) error {
	reason := err.Error()
	if pathError, ok := errors.AsType[*fs.PathError](err); ok {
		reason = pathError.Err.Error()
	}
	msg := fmt.Sprintf("could not check the skill directory %s: %s", t.Dir, reason)
	if component != t.Dir {
		msg = fmt.Sprintf("could not check %s on the path to the skill directory %s: %s", component, t.Dir, reason)
	}
	return clierr.New(clierr.CodeValidation, msg).WithHint(fmt.Sprintf(
		"check %s and the directories above it for the problem the error names, then run this command again", component))
}

// refusalHint is the remedy for a symbolic link, for a junction or other entry
// that is not a directory or a link, and for a plain file.
func refusalHint(component string, mode fs.FileMode) string {
	if mode&fs.ModeSymlink != 0 {
		return fmt.Sprintf("remove the symbolic link at %s, then run this command again", component)
	}
	if mode&fs.ModeIrregular != 0 {
		return fmt.Sprintf("remove the Windows directory link (NTFS junction) or other entry at %s, then run this command again", component)
	}
	return fmt.Sprintf("move %s away or replace it with a directory, then run this command again", component)
}

// linkTarget names where the link at path leads: the fully resolved path, or
// the text of the link when it is dangling. found is false when neither can be
// read.
func linkTarget(path string) (destination string, found bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, true
	}
	if text, err := os.Readlink(path); err == nil {
		return text, true
	}
	return "", false
}
