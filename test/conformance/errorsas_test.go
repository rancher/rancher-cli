package conformance

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") || path == "test/conformance/errorsas_test.go" {
			continue
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for i, line := range strings.Split(string(data), "\n") {
			if errorsAsRE.MatchString(line) {
				offenders = append(offenders, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	for _, o := range offenders {
		t.Errorf("use errors.AsType[T](err) instead of errors.As(err, &target): %s", o)
	}
}
