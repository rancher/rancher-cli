package testfs

import (
	"os"
	"strings"
)

// environ is the one read of the whole environment; the package's own tests
// replace it.
var environ = os.Environ

// EnvWithoutGit returns the environment of the test process without the
// variables whose names start with GIT_. Every process a test starts takes its
// environment from this function, because git sets GIT_DIR, GIT_INDEX_FILE and
// others for a hook, and a git command a test runs in a temporary directory
// then acts on the repository that runs the hook. Names are compared without
// regard to case on Windows, where the environment is case-insensitive.
// Entries such as =C:=C:\dir, whose name starts with an equals sign, are kept.
func EnvWithoutGit() []string {
	var kept []string
	for _, entry := range environ() {
		if !isGitVariable(entry) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// isGitVariable reports whether the entry (name=value) is a variable whose
// name starts with GIT_. The name ends at the first equals sign after the
// first character; =C:=C:\dir has the name =C:.
func isGitVariable(entry string) bool {
	name := entry
	if i := strings.IndexByte(entry[min(1, len(entry)):], '='); i >= 0 {
		name = entry[:i+1]
	}
	if len(name) < len("GIT_") {
		return false
	}
	prefix := name[:len("GIT_")]
	if OnWindows() {
		return strings.EqualFold(prefix, "GIT_")
	}
	return prefix == "GIT_"
}
