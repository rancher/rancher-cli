package main_test

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
// each with the reviewed reason. Authored text uses plain spelling
// ("facade", never an accented letter) and the ASCII hyphen for every
// dash; runtime data from users and servers is rendered verbatim and is
// never authored text, so nothing needs an entry today.
var plainSpellingAllowlist = map[string]string{}

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

// TestPlainSpelling fails on any text file in the repository (every file
// git lists that decodes as UTF-8 without a NUL byte, whatever its name -
// goldens, fixtures, workflows and CODEOWNERS are authored text too) whose
// authored text carries a dash other than the hyphen or an accented Latin
// letter, naming each offending line and code point.
func TestPlainSpelling(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	for _, path := range files {
		if _, ok := plainSpellingAllowlist[path]; ok {
			continue
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
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
	for _, o := range offenders {
		t.Errorf("use the ASCII hyphen and plain spelling: %s", o)
	}
}
