package main_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// errorContainsRE matches the banned spelling: a positive Contains
// assertion over err.Error()'s string, which testify expresses directly as
// ErrorContains(t, err, ...). The negative form - NotContains over
// err.Error() - is deliberately NOT matched: testify has no
// ErrorNotContains, so that spelling is the only way to assert a substring
// is absent from an error.
var errorContainsRE = regexp.MustCompile(`(assert|require)\.Contains\(\s*t,\s*[A-Za-z]*[eE]rr\.Error\(\)`)

// TestNoContainsOverErrorString fails on any test asserting a substring of
// err.Error() with Contains instead of ErrorContains. The enabled
// testifylint checkers do not catch this shape, so the suite holds the
// line itself.
func TestNoContainsOverErrorString(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	for _, path := range files {
		if !strings.HasSuffix(path, "_test.go") || path == "errorcontains_test.go" {
			continue
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for i, line := range strings.Split(string(data), "\n") {
			if errorContainsRE.MatchString(line) {
				offenders = append(offenders, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	for _, o := range offenders {
		t.Errorf("use ErrorContains(t, err, ...) instead of Contains over err.Error(): %s", o)
	}
}
