package main_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// errorsAsRE matches the pre-generics spelling of a typed error match:
// errors.As with a pointer-to-target argument. errors.AsType returns the
// typed value and a bool in one call, needs no pre-declared variable, and
// is the spelling this codebase uses.
var errorsAsRE = regexp.MustCompile(`\berrors\.As\(`)

// TestNoErrorsAsWithTarget fails on any Go file, test or not, that still
// spells a typed error match as errors.As(err, &target) instead of
// errors.AsType[T](err). No linter enforces the modern form, so the suite
// holds the line itself.
func TestNoErrorsAsWithTarget(t *testing.T) {
	t.Parallel()
	var offenders []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "docs" || name == "bin" || name == "vendor" || strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || path == "errorsas_test.go" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if errorsAsRE.MatchString(line) {
				offenders = append(offenders, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("use errors.AsType[T](err) instead of errors.As(err, &target): %s", o)
	}
}
