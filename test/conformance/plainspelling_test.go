package conformance

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// plainSpellingAllowlist names files that may carry a rejected character,
// each with the reviewed reason. A name that ends in "/" is a directory and
// covers every file below it. Authored text uses plain spelling ("facade",
// never an accented letter) and the ASCII hyphen for every dash; runtime data
// from users and servers is rendered verbatim and is never authored text.
var plainSpellingAllowlist = map[string]string{
	"internal/cli/testdata/fuzz/FuzzParseLine/": "Go writes the corpus files of the fuzz target with %q, which keeps " +
		"printable non-ASCII characters as they are; an input with one is the point of a corpus entry",
}

// plainSpellingAllowed reports whether allowlist names path: an entry equal
// to the path, or an entry that ends in "/" and starts the path.
func plainSpellingAllowed(allowlist map[string]string, path string) bool {
	if _, ok := allowlist[path]; ok {
		return true
	}
	for entry := range allowlist {
		if strings.HasSuffix(entry, "/") && strings.HasPrefix(path, entry) {
			return true
		}
	}
	return false
}

// rejectedRune reports the characters authored text may not contain:
// every dash-like code point other than the ASCII hyphen (figure dash,
// en-dash, em-dash, horizontal bar, minus sign) and Latin letters
// carrying a diacritic, including the combining marks that build one (the
// plain spelling always exists). Decorative characters such as the middle
// dot, arrows, the multiplication sign, and the ellipsis are allowed.
func rejectedRune(r rune) bool {
	switch {
	case r >= '\u2012' && r <= '\u2015', r == '\u2212':
		return true
	case r >= '\u00C0' && r <= '\u024F' && r != '\u00D7' && r != '\u00F7':
		return true
	case r >= '\u0300' && r <= '\u036F', r >= '\u1E00' && r <= '\u1EFF':
		return true
	}
	return false
}

// plainSpellingOffenders returns a message for each line of the files in
// paths, read with read, that carries a rejected character. A file the
// allowlist names, and a file that is not UTF-8 text, are left out.
func plainSpellingOffenders(paths []string, read func(string) ([]byte, error), allowlist map[string]string) ([]string, error) {
	var offenders []string
	for _, path := range paths {
		if plainSpellingAllowed(allowlist, path) {
			continue
		}
		data, err := read(path)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			continue // binary, not authored text
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, r := range line {
				if rejectedRune(r) {
					offenders = append(offenders, fmt.Sprintf("%s:%d: U+%04X in %q", path, i+1, r, strings.TrimSpace(line)))
					break
				}
			}
		}
	}
	return offenders, nil
}

// TestPlainSpelling fails on any text file in the repository (every file
// git lists that decodes as UTF-8 without a NUL byte, whatever its name -
// goldens, fixtures, workflows and CODEOWNERS are authored text too) whose
// authored text carries a dash other than the hyphen or an accented Latin
// letter, naming each offending line and code point.
func TestPlainSpelling(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	offenders, err := plainSpellingOffenders(files, os.ReadFile, plainSpellingAllowlist)
	require.NoError(t, err)
	for _, o := range offenders {
		t.Errorf("use the ASCII hyphen and plain spelling: %s", o)
	}
}

// TestPlainSpellingDirectoryEntryCoversOnlyItsDirectory asserts that an
// allowlist entry that ends in "/" covers the files below that directory and
// no other: a file with an en dash below it passes, and the same file in a
// sibling directory whose name starts with the same characters, in the same
// directory path under another parent, or named like the directory without
// the slash, still fails.
func TestPlainSpellingDirectoryEntryCoversOnlyItsDirectory(t *testing.T) {
	t.Parallel()
	const text = "an en dash \u2013 here\n"
	allowlist := map[string]string{"data/fuzz/Target/": "test"}
	paths := []string{
		"data/fuzz/Target/entry",
		"data/fuzz/Target/nested/entry",
		"data/fuzz/TargetTwo/entry",
		"data/fuzz/Target",
		"other/data/fuzz/Target/entry",
	}
	read := func(string) ([]byte, error) { return []byte(text), nil }
	offenders, err := plainSpellingOffenders(paths, read, allowlist)
	require.NoError(t, err)
	var names []string
	for _, o := range offenders {
		name, _, _ := strings.Cut(o, ":")
		names = append(names, name)
	}
	require.Equal(t, paths[2:], names)
}
