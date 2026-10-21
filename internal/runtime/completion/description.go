package completion

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxDescriptionRunes is the longest description, in runes, before the cut:
// a longer text is cut to this length and "..." is appended.
const MaxDescriptionRunes = 56

// descriptionEllipsis ends a description cut to MaxDescriptionRunes.
const descriptionEllipsis = "..."

// SanitizeDescription turns text a server supplied into one line a shell
// shows safely beside a completion candidate, or "" when no description
// is left. Format runes (zero-width joiners, bidirectional controls, the
// soft hyphen) are deleted. Control, private-use and unassigned runes,
// the line and paragraph separators and the replacement character become
// a space, as does every other space rune; a run of spaces collapses to
// one and the ends are trimmed. A text over MaxDescriptionRunes is cut at
// a rune boundary, trimmed again and ends in "...". A text with no
// visible rune (only spaces and the invisible filler characters) has no
// description.
func SanitizeDescription(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	space := true
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cf, r):
			continue
		case descriptionSpace(r):
			if !space {
				b.WriteByte(' ')
				space = true
			}
		default:
			b.WriteRune(r)
			space = false
		}
	}
	out := strings.TrimRight(b.String(), " ")
	if !hasVisibleRune(out) {
		return ""
	}
	if utf8.RuneCountInString(out) <= MaxDescriptionRunes {
		return out
	}
	cut := []rune(out)[:MaxDescriptionRunes]
	out = strings.TrimRight(string(cut), " ")
	if !hasVisibleRune(out) {
		return ""
	}
	return out + descriptionEllipsis
}

// descriptionSpace reports whether r is shown as a space: a space rune, a
// line or paragraph separator, the replacement character (what decoding
// invalid UTF-8 leaves), or a control, private-use, surrogate or
// unassigned rune (the "other" class of the Go unicode tables, whose
// format runes the caller has already deleted).
func descriptionSpace(r rune) bool {
	switch r {
	case '\u2028', '\u2029', utf8.RuneError:
		return true
	}
	return unicode.IsSpace(r) || unicode.Is(unicode.C, r)
}

// hasVisibleRune reports whether s holds a graphic rune that shows
// something: the Hangul fillers and the blank braille pattern are graphic
// and draw nothing.
func hasVisibleRune(s string) bool {
	for _, r := range s {
		switch r {
		case '\u3164', '\u1160', '\u2800':
			continue
		}
		if unicode.IsGraphic(r) && !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// ItemString returns the string at path in item (a decoded JSON object),
// or "" when a step is missing or the value is not a string.
func ItemString(item map[string]any, path []string) string {
	cur := item
	for i, step := range path {
		v, ok := cur[step]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			s, _ := v.(string)
			return s
		}
		cur, _ = v.(map[string]any)
		if cur == nil {
			return ""
		}
	}
	return ""
}

// ItemDescription returns the description text of item built from the
// strings at paths, in the order given: see JoinDescription.
func ItemDescription(item map[string]any, paths [][]string) string {
	values := make([]string, len(paths))
	for i, p := range paths {
		values[i] = ItemString(item, p)
	}
	return JoinDescription(values...)
}

// JoinDescription returns the first non-empty value, then each other
// value that differs from it, once, in parentheses and comma-separated:
// "alice (Alice Smith)", "alice" when the display name is empty or the
// same. A value with no visible rune (only white space, control, format
// or filler characters) counts as empty. The text is raw: the surface
// sanitizes it.
func JoinDescription(values ...string) string {
	var first string
	var rest []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		switch {
		case !hasVisibleRune(v):
		case first == "":
			first = v
		case v != first && !slices.Contains(rest, v):
			rest = append(rest, v)
		}
	}
	if len(rest) == 0 {
		return first
	}
	return first + " (" + strings.Join(rest, ", ") + ")"
}
