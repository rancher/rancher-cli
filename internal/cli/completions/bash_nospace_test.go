//go:build !windows

package completions

// Deliberately plain-stdlib (no testify): these tests execute under a real
// external bash and are kept dependency-light on purpose.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nospaceBashes returns every distinct bash to exercise: the PATH one plus
// /bin/bash when it differs (macOS ships stock bash 3.2 there - the shell
// the continuation handling must work on WITHOUT compopt). The assertions
// are on COMPREPLY content, which is version-independent, so running under
// bash 3 and 4+ asserts that no 4+-only construct sneaked into the code path.
func nospaceBashes(t *testing.T) []string {
	t.Helper()
	var out []string
	p := requireShell(t, "bash")
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	out = append(out, p)
	const stock = "/bin/bash"
	if fi, err := os.Stat(stock); err == nil && fi.Mode()&0o111 != 0 {
		dup := false
		for _, p := range out {
			if p == stock {
				dup = true
			}
		}
		if !dup {
			out = append(out, stock)
		}
	}
	return out
}

// bashDriverOpts adjusts the driver environment: workDir sets the shell's
// cwd and HOME (so filename-fallback cases run against a controlled corpus
// and tilde expansion is testable); disableCompopt runs `enable -n compopt`
// first, which removes the builtin on bash 4+ and is a harmless error on
// bash 3 - the one lever that makes the bash-3-only fallback path
// exercisable under ANY harness bash (e.g. Linux CI without a bash 3).
type bashDriverOpts struct {
	workDir        string
	disableCompopt bool
}

// bashCompletionEntries drives the full __rancher_bash_autocomplete in
// insert mode (COMP_TYPE unset, the first-TAB path) under bashBin against a
// fake rancher emitting candidateLines, and returns the resulting COMPREPLY
// entries VERBATIM - trailing spaces preserved, one "R:" line per entry.
// compopt is deliberately NOT stubbed: outside a live completion call it
// fails exactly as it is absent on bash 3, so the code path exercised here
// is the compopt-free one every bash shares.
func bashCompletionEntries(t *testing.T, bashBin string, compWords []string, compCword int, candidateLines []string, opts bashDriverOpts) []string {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "bash.sh")
	if err := os.WriteFile(scriptPath, bashScript, 0o644); err != nil {
		t.Fatalf("writing bash.sh: %v", err)
	}

	binDir := t.TempDir()
	var fake strings.Builder
	fake.WriteString("#!/bin/sh\n")
	for _, line := range candidateLines {
		fmt.Fprintf(&fake, "printf '%%s\\n' '%s'\n", strings.ReplaceAll(line, "'", "'\\''"))
	}
	if err := os.WriteFile(filepath.Join(binDir, "rancher"), []byte(fake.String()), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	quoted := make([]string, len(compWords))
	for i, w := range compWords {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", "'\\''") + "'"
	}
	wordsLiteral := "(" + strings.Join(quoted, " ") + ")"

	prelude := ""
	if opts.workDir != "" {
		prelude += fmt.Sprintf("cd %q || exit 1\nexport HOME=%q\n", opts.workDir, opts.workDir)
	}
	if opts.disableCompopt {
		prelude += "enable -n compopt 2>/dev/null || true\n"
	}

	driver := fmt.Sprintf(`
set +e
%s
export PATH=%q:"$PATH"
_get_comp_words_by_ref() { cur="${COMP_WORDS[COMP_CWORD]}"; prev="${COMP_WORDS[COMP_CWORD-1]}"; words=("${COMP_WORDS[@]}"); cword="${COMP_CWORD}"; }
source %q
COMP_WORDS=%s
COMP_CWORD=%d
COMP_TYPE=""
__rancher_bash_autocomplete
if [[ ${#COMPREPLY[@]} -gt 0 ]]; then printf 'R:%%s\n' "${COMPREPLY[@]}"; fi
printf 'DONE\n'
`, prelude, binDir, scriptPath, wordsLiteral, compCword)

	out, err := exec.Command(bashBin, "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("running bash driver (%s): %v\noutput:\n%s", bashBin, err, out)
	}
	if !strings.Contains(string(out), "DONE") {
		t.Fatalf("driver did not run to completion (%s); output:\n%s", bashBin, out)
	}

	var entries []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if after, ok := strings.CutPrefix(line, "R:"); ok {
			entries = append(entries, after)
		}
	}
	return entries
}

// TestBashCompletionCandidateBoundaries asserts the bash-3-compatible
// continuation handling: the script is registered with -o nospace (readline
// never appends the argument-ending space itself), so the script marks each
// candidate's own boundary in COMPREPLY. A FINISHED candidate carries an
// explicit trailing space; a candidate ending in a continuation character -
// "amazonec2config/" expects a name after the slash, "template=" expects a
// Go template after the equals - carries none, so accepting it keeps the
// cursor in the same word for the second-stage completion. This needs no
// compopt, so bash 3.2 (macOS stock) and bash 4+ behave identically; the
// test runs under every available bash. Multiple matches only ever insert
// readline's common prefix, which never reaches a trailing space ("pod "
// vs "pods " share only "pod"), and an empty COMPREPLY still hands off to
// the -o default filename fallback.
func TestBashCompletionCandidateBoundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		words      []string
		cword      int
		candidates []string
		want       []string
	}{
		{
			name:       "unique slash-terminated kind prefix gets no trailing space",
			words:      []string{"rancher", "create", "cluster", "--machine-config", "amazo"},
			cword:      4,
			candidates: []string{"amazonec2config/", "azureconfig/"},
			want:       []string{"amazonec2config/"},
		},
		{
			name:       "unique equals-terminated template form gets no trailing space",
			words:      []string{"rancher", "get", "-o", "temp"},
			cword:      3,
			candidates: []string{"json", "template="},
			want:       []string{"template="},
		},
		{
			name:       "unique finished argument carries its trailing space",
			words:      []string{"rancher", "get", "-o", "js"},
			cword:      3,
			candidates: []string{"json", "yaml"},
			want:       []string{"json "},
		},
		{
			name:       "multiple finished matches each carry the space",
			words:      []string{"rancher", "get", "po"},
			cword:      2,
			candidates: []string{"pod:a pod", "pods:pods alias"},
			want:       []string{"pod ", "pods "},
		},
		{
			name:       "multiple continuation matches stay bare",
			words:      []string{"rancher", "create", "cluster", "--machine-config", "a"},
			cword:      4,
			candidates: []string{"amazonec2config/", "azureconfig/"},
			want:       []string{"amazonec2config/", "azureconfig/"},
		},
		{
			name:       "no match leaves COMPREPLY empty for the -o default fallback",
			words:      []string{"rancher", "get", "zzz"},
			cword:      2,
			candidates: []string{"pod", "pods"},
			want:       nil,
		},
	}

	for _, bashBin := range nospaceBashes(t) {
		for _, tc := range cases {
			t.Run(strings.TrimPrefix(strings.ReplaceAll(bashBin, "/", "_"), "_")+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				got := bashCompletionEntries(t, bashBin, tc.words, tc.cword, tc.candidates, bashDriverOpts{})
				if !sliceEqual(got, tc.want) {
					t.Errorf("COMPREPLY mismatch under %s\n got:  %q\n want: %q", bashBin, got, tc.want)
				}
			})
		}
	}
}

// TestBashCompletionRegistersNospace asserts the registration line itself: the
// candidate-boundary contract above only holds if readline's own space
// appending is off, and that must be the compspec option -o nospace -
// set at registration precisely because bash 3 has no compopt to set it
// per-invocation.
func TestBashCompletionRegistersNospace(t *testing.T) {
	t.Parallel()
	want := "complete -o bashdefault -o default -o nospace -F __rancher_bash_autocomplete rancher"
	if !strings.Contains(string(bashScript), want) {
		t.Errorf("bash.sh must register with %q", want)
	}
}

// bashHasCompopt reports whether bashBin ships the compopt builtin (bash 4+).
func bashHasCompopt(t *testing.T, bashBin string) bool {
	t.Helper()
	return exec.Command(bashBin, "-c", "type compopt >/dev/null 2>&1").Run() == nil
}

// fallbackCorpus builds the controlled directory tree the filename-fallback
// cases complete against: a regular file, a directory (with one file inside
// for the trailing-slash case), and a file with a space in its name.
func fallbackCorpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"plainfile.txt", "my file.txt", filepath.Join("subdir", "inner.txt")} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestBash3FallbackGeneratesFileCandidates asserts the bash-3-only filename
// fallback: with no compopt the script cannot re-enable readline's appended
// space for the -o default fallback, so it must generate the file/directory
// candidates itself with the same per-candidate boundaries its own
// candidates carry - "/" (no space) after directories, a trailing space
// after regular files, names escaped with printf %q so a plain name with a
// space inserts as one shell word. compopt is disabled in the driver
// (enable -n on bash 4+, already absent on bash 3), so this exercises the
// bash-3 path under every available bash, including stock /bin/bash 3.2 on
// macOS where it is the real production path.
func TestBash3FallbackGeneratesFileCandidates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cur  string
		want func(home string) []string
	}{
		{
			name: "regular file gets the trailing-space boundary",
			cur:  "plain",
			want: func(string) []string { return []string{"plainfile.txt "} },
		},
		{
			name: "directory gets a slash and stays open",
			cur:  "sub",
			want: func(string) []string { return []string{"subdir/"} },
		},
		{
			name: "name with a space is escaped into one shell word",
			cur:  "my",
			want: func(string) []string { return []string{`my\ file.txt `} },
		},
		{
			name: "trailing-slash dir lists its contents",
			cur:  "subdir/",
			want: func(string) []string { return []string{"subdir/inner.txt "} },
		},
		{
			name: "empty cur lists the working directory",
			cur:  "",
			want: func(string) []string {
				return []string{`my\ file.txt `, "plainfile.txt ", "subdir/"}
			},
		},
		{
			name: "tilde prefix expands against HOME",
			cur:  "~/plain",
			want: func(home string) []string { return []string{home + "/plainfile.txt "} },
		},
		{
			name: "no matching file leaves COMPREPLY empty",
			cur:  "zzz",
			want: func(string) []string { return nil },
		},
	}

	for _, bashBin := range nospaceBashes(t) {
		for _, tc := range cases {
			t.Run(strings.TrimPrefix(strings.ReplaceAll(bashBin, "/", "_"), "_")+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				dir := fallbackCorpus(t)
				got := bashCompletionEntries(t, bashBin,
					[]string{"rancher", "get", tc.cur}, 2, nil,
					bashDriverOpts{workDir: dir, disableCompopt: true})
				want := tc.want(dir)
				if !sliceEqual(got, want) {
					t.Errorf("COMPREPLY mismatch under %s (compopt disabled)\n got:  %q\n want: %q", bashBin, got, want)
				}
			})
		}
	}
}

// TestModernBashKeepsNativeFallback asserts the bash 4+ side: when compopt IS
// available the script must NOT take the self-generated path - COMPREPLY
// stays empty so readline's own -o default filename completion (with its
// native quoting, tilde handling, and the compopt-restored space) owns the
// fallback exactly as before. Skips when no harness bash has compopt
// (fails instead when CI is set).
func TestModernBashKeepsNativeFallback(t *testing.T) {
	t.Parallel()

	ran := false
	for _, bashBin := range nospaceBashes(t) {
		if !bashHasCompopt(t, bashBin) {
			continue
		}
		ran = true
		t.Run(strings.TrimPrefix(strings.ReplaceAll(bashBin, "/", "_"), "_"), func(t *testing.T) {
			t.Parallel()
			dir := fallbackCorpus(t)
			got := bashCompletionEntries(t, bashBin,
				[]string{"rancher", "get", "plain"}, 2, nil,
				bashDriverOpts{workDir: dir})
			if len(got) != 0 {
				t.Errorf("bash 4+ must leave COMPREPLY empty for the native fallback, got %q", got)
			}
		})
	}
	if !ran {
		shellMissing(t, "no bash with compopt available; modern-fallback side not exercisable")
	}
}
