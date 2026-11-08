package remedytest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// AssertIDShownWithEscapes asserts text, which names id, shows the id as
// clierr.IDText writes it, holds no rune the terminal hides (once the remedy
// marker is read as the word it stands for; a line break between the lines of
// a text is allowed), and does not hold the id with those runes removed, which
// would read as another id. id must hold a rune the terminal hides, so that
// its removed form differs from it.
func AssertIDShownWithEscapes(tb testing.TB, text, id string, msgAndArgs ...any) {
	tb.Helper()
	stripped := clierr.WithoutUnsafeToDisplay(id)
	if !assert.NotEqual(tb, id, stripped, "the planted id holds no rune the terminal hides") {
		return
	}
	assert.Contains(tb, text, clierr.IDText(id), msgAndArgs...)
	shown := strings.ReplaceAll(text, clierr.Rancher, "rancher")
	for line := range strings.SplitSeq(shown, "\n") {
		assert.Equal(tb, line, clierr.WithoutUnsafeToDisplay(line), "no rune the terminal hides")
	}
	assert.NotContains(tb, text, stripped, "the id with its hidden runes removed reads as another id")
}
