package main_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// plainSpellingAllowlist names files that may carry a rejected character,
// each with the reviewed reason. Authored text uses plain spelling
// ("facade", never an accented letter) and the ASCII hyphen for every
// dash; runtime data from users and servers is rendered verbatim and is
// never authored text, so nothing needs an entry today.
var plainSpellingAllowlist = map[string]string{}

// plainSpellingSkipsDir reuses skipDirNames (tooling, VCS metadata, build
// output) with two deliberate differences: testdata IS scanned, because
// goldens and fixtures are authored text and must read plainly too, and
// .github IS scanned, because workflow files and CODEOWNERS are authored
// text as well. docs and graphify-out are gitignored and per-checkout
// (working notes, a generated knowledge graph), so they are skipped.
func plainSpellingSkipsDir(name string) bool {
	switch name {
	case "testdata", ".github":
		return false
	case "docs", "graphify-out":
		return true
	}
	return skipDirNames[name]
}

// TestPlainSpellingSkipsDir asserts the walk's directory policy: tooling
// and build output are skipped, authored-text directories are not.
func TestPlainSpellingSkipsDir(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".git", ".githooks", ".claude", "bin", "node_modules", "vendor", "docs", "graphify-out"} {
		assert.True(t, plainSpellingSkipsDir(name), name)
	}
	for _, name := range []string{"testdata", ".github", "internal", "scripts", "e2e"} {
		assert.False(t, plainSpellingSkipsDir(name), name)
	}
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

// TestPlainSpelling fails on any text file in the tree (every regular
// file that decodes as UTF-8 without a NUL byte, whatever its name) whose
// authored text carries a dash other than the hyphen or an accented Latin
// letter, naming each offending line and code point.
func TestPlainSpelling(t *testing.T) {
	t.Parallel()
	var offenders []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if plainSpellingSkipsDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, ok := plainSpellingAllowlist[path]; ok {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return nil // binary, not authored text
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, r := range line {
				if rejectedRune(r) {
					offenders = append(offenders, fmt.Sprintf("%s:%d: U+%04X in %q", path, i+1, r, strings.TrimSpace(line)))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("use the ASCII hyphen and plain spelling: %s", o)
	}
}
