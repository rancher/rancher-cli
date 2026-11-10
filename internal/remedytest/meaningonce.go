package remedytest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AssertClauseOnce asserts msg holds clause once, and that it comes in the
// order the wording needs relative to arg, the text of the command or clause
// that names a placeholder the clause explains: before arg when beforeArg is
// set (the command that holds the placeholder), after it otherwise (a clause
// that names the placeholder). msg must hold arg.
func AssertClauseOnce(tb testing.TB, msg, clause, arg string, beforeArg bool) {
	tb.Helper()
	assert.Equal(tb, 1, strings.Count(msg, clause), "the message holds %q once: %q", clause, msg)
	at := strings.Index(msg, arg)
	require.GreaterOrEqual(tb, at, 0, "the message holds %q: %q", arg, msg)
	clauseAt := strings.Index(msg, clause)
	if beforeArg {
		assert.Less(tb, clauseAt, at, "%q comes before %q: %q", clause, arg, msg)
		return
	}
	assert.Greater(tb, clauseAt, at, "%q follows %q: %q", clause, arg, msg)
}
