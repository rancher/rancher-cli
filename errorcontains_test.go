package main_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// errorContainsRE matches the banned spelling: a positive Contains
// assertion over err.Error()'s string, which testify expresses directly as
// ErrorContains(t, err, ...). The negative form — NotContains over
// err.Error() — is deliberately NOT matched: testify has no
// ErrorNotContains, so that spelling is the only way to assert a substring
// is absent from an error.
var errorContainsRE = regexp.MustCompile(`(assert|require)\.Contains\(\s*t,\s*[A-Za-z]*[eE]rr\.Error\(\)`)

// TestNoContainsOverErrorString fails on any test asserting a substring of
// err.Error() with Contains instead of ErrorContains. The enabled
// testifylint checkers do not catch this shape, so the suite holds the
// line itself.
func TestNoContainsOverErrorString(t *testing.T) {
	t.Parallel()
	var offenders []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "docs" || name == "bin" || strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || path == "errorcontains_test.go" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if errorContainsRE.MatchString(line) {
				offenders = append(offenders, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("use ErrorContains(t, err, ...) instead of Contains over err.Error(): %s", o)
	}
}
