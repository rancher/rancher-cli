package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestDroppedWordsErrorNamesTheWordsAfterABareDash asserts the refusal of
// words after a "-" on its own: VALIDATION at exit 2, a message naming the
// words as typed, shell-quoted, after the words that are not used, at most
// five (six when the fifth is a flag and the next its value) and a count of
// the rest, and a hint saying where a "-" goes.
func TestDroppedWordsErrorNamesTheWordsAfterABareDash(t *testing.T) {
	t.Parallel()
	const (
		prefix = "the words after a - on its own are not used: "
		hint   = "put - last, or after -- when it is meant as an argument"
	)
	for _, tc := range []struct {
		name    string
		words   []string
		message string
	}{
		{"one word", []string{"--dry-run"}, prefix + "'--dry-run'"},
		{"words with a value to quote", []string{"--cluster", "c 1", "-o", "json"}, prefix + `'--cluster' 'c 1' '-o' 'json'`},
		{"more than five words", []string{"a", "b", "c", "d", "e", "f", "g"}, prefix + "'a' 'b' 'c' 'd' 'e' and 2 more"},
		{"a flag named last is named with its value", []string{"a", "b", "c", "d", "-o", "json", "x"}, prefix + "'a' 'b' 'c' 'd' '-o' 'json' and 1 more"},
		{"a flag named last and its value are the last words", []string{"a", "b", "c", "d", " --cluster", "c1"}, prefix + "'a' 'b' 'c' 'd' ' --cluster' 'c1'"},
		{"a flag named last before another flag is named alone", []string{"a", "b", "c", "d", "--dry-run", "--apply", "x"}, prefix + "'a' 'b' 'c' 'd' '--dry-run' and 2 more"},
		{"a word as typed with whitespace around it", []string{" x "}, prefix + "' x '"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := DroppedWordsError(tc.words)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, hint, ce.Hint)
			assert.Equal(t, 2, ce.ExitCode())
		})
	}
}
