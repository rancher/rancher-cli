package completions

import (
	"sort"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// TestScriptsCoverExactlyTheCanonicalShellList asserts the "one shared shell
// list" contract: scripts.go has no reference to
// runtime.ShellCompletionShells(), so adding a shell to one without the other
// would otherwise compile fine and only fail at test/runtime time, not compile
// time. This test is that failure point.
func TestScriptsCoverExactlyTheCanonicalShellList(t *testing.T) {
	got := make([]string, 0, len(scripts))
	for shell := range scripts {
		got = append(got, shell)
	}
	sort.Strings(got)

	want := runtime.ShellCompletionShells()
	sort.Strings(want)

	assert.Equal(t, want, got)
}
