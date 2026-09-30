package cli

import (
	"maps"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/kubeconfig"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// execCommandFlag is kubeconfig's flag naming the command kubectl runs
// for the exec plugin.
const execCommandFlag = "exec-command"

// execCommandSite reports an --exec-command flag-value site on a command
// whose descriptor carries the flag. The site is owned here, so the live
// layer is never consulted for it.
func execCommandSite(site CompletionSite, d runtime.Descriptor) bool {
	return site.Kind == SiteFlagValue && site.FlagName == execCommandFlag &&
		slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == execCommandFlag })
}

// execCommandCandidates completes an --exec-command value. The command
// reads a bare word as a name kubectl looks up on PATH and a word with a
// separator, or a leading ~, as a path, so every value offered means what
// it offers:
//   - a bare word completes to the executables on PATH whose names begin
//     with it, else to the word itself, and the empty word to rancher.
//     Every candidate begins with the word, so bash never falls back to
//     file names from the working directory, which would read as PATH
//     names;
//   - a path word gets nothing, so the shell's own file completion runs,
//     quoting and expanding as it does (fish, which never offers files,
//     has one rule for it in its script).
func execCommandCandidates(site CompletionSite) []string {
	word := site.Partial
	if kubeconfig.ExecCommandIsPath(word) {
		return nil
	}
	names := []string{"rancher"}
	if word != "" {
		names = pathExecutables(word, os.Getenv("PATH"), goruntime.GOOS)
		if len(names) == 0 {
			names = []string{word}
		}
	}
	if site.EqualsForm {
		for i, n := range names {
			names[i] = site.FlagSpelling + "=" + n
		}
	}
	return names
}

// pathExecutables lists, sorted and once each, the executables in the
// directories of path whose names begin with prefix: one read of each
// directory. On Windows an executable is a file whose extension PATHEXT
// lists, offered without it, since that is how a command names it. A name
// outside the shell-safe charset is not offered: a shell would split or
// expand it as inserted.
func pathExecutables(prefix, path, goos string) []string {
	var exts []string
	if goos == "windows" {
		pathext := os.Getenv("PATHEXT")
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		for e := range strings.SplitSeq(pathext, ";") {
			if e != "" {
				exts = append(exts, strings.ToLower(e))
			}
		}
	}
	seen := map[string]bool{}
	for dir := range strings.SplitSeq(path, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, prefix) || e.IsDir() {
				continue
			}
			if goos == "windows" {
				ext := strings.ToLower(filepath.Ext(name))
				if !slices.Contains(exts, ext) {
					continue
				}
				name = name[:len(name)-len(ext)]
				if !strings.HasPrefix(name, prefix) {
					continue
				}
			} else if !isExecutable(filepath.Join(dir, name)) {
				continue
			}
			if completionCharset.MatchString(name) {
				seen[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// isExecutable reports a regular file, or a link to one, with an execute
// bit set.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
