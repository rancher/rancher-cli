package engine

import "strings"

// escapePointerToken escapes one JSON object key for use as an RFC 6901
// pointer segment: "~" becomes "~0" and "/" becomes "~1", in that order, so
// a key that already contains "~1" round-trips instead of colliding with an
// escaped "/".
func escapePointerToken(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

// unescapePointerToken reverses escapePointerToken: "~1" back to "/" before
// "~0" back to "~", the opposite order from escaping, so the two escapes
// don't unescape each other.
func unescapePointerToken(s string) string {
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

// PointerFor renders keys as the RFC 6901 pointer they address, escaping
// each segment so a key containing "/" or "~" cannot be mistaken for a path
// through it. No keys is the empty pointer, the whole document.
func PointerFor(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("/")
		b.WriteString(escapePointerToken(k))
	}
	return b.String()
}

// SplitPointer splits an RFC 6901 JSON pointer into its unescaped segments.
// The empty pointer (the whole document) yields nil; "/" yields
// []string{""}, the top-level key that is the empty string - a different
// pointer from the whole document.
func SplitPointer(p string) []string {
	if p == "" {
		return nil
	}
	var segments []string
	for s := range strings.SplitSeq(strings.TrimPrefix(p, "/"), "/") {
		segments = append(segments, unescapePointerToken(s))
	}
	return segments
}
