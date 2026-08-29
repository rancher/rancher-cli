// Package style applies presentation-only ANSI styling to rendered CLI
// text. Every wrap is additive: removing the escape codes it inserts
// restores the exact bytes a caller passed in, so a styled render can
// always be checked against its plain form byte-for-byte.
package style

import "strings"

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// Style wraps text in ANSI escape codes when Enabled, and returns text
// unchanged otherwise — the one flag every wrap method consults.
type Style struct {
	Enabled bool
}

// Bold, Red, Yellow, and Cyan each wrap text in the named SGR code and a
// trailing reset. Disabled or empty text passes through unchanged — an
// empty wrap would place codes around nothing visible.
func (s Style) Bold(text string) string   { return s.wrap(ansiBold, text) }
func (s Style) Red(text string) string    { return s.wrap(ansiRed, text) }
func (s Style) Yellow(text string) string { return s.wrap(ansiYellow, text) }
func (s Style) Cyan(text string) string   { return s.wrap(ansiCyan, text) }

func (s Style) wrap(code, text string) string {
	if !s.Enabled || text == "" {
		return text
	}
	return code + text + ansiReset
}

// Strip removes every ANSI SGR escape sequence from text, restoring the
// exact plain bytes a Style wrap was built from.
func Strip(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '[' {
			j := i + 2
			for j < len(text) && text[j] != 'm' {
				j++
			}
			if j < len(text) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}
