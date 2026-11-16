package clierr

import (
	"strconv"
	"strings"
)

// LineText is the line s with each clause that NotPrintable reports true for
// written as IDText writes it. A clause is the text before the first boundary,
// between two boundaries, or after the last boundary; a line with no boundary
// is one clause. A boundary is Rancher (the remedy marker followed by
// "rancher"), "; " or " (". keptBeforeCommand in internal/runtime reads "; "
// and " (" as clause boundaries and Rancher as the start of a command. The
// scan takes the earliest boundary each time: in "; (" the boundary is "; ",
// and the next clause starts with "(". Every boundary stays as written and
// outside any quotes: remedy expansion needs Rancher to find each command, and
// "; " and " (" to find the clause that holds the first one. No string
// LineText writes in double quotes holds "; ", " (" or a single quote; the
// boundaries of the output are those of the input. A line with nothing to
// quote comes back unchanged. A remedy marker that is not followed by
// "rancher", and another process's marker, are part of a clause and are
// written as escapes.
//
// The words after Rancher, up to the next boundary, are a clause like any
// other. A word in the same piece as a hidden rune or an invalid byte is
// quoted with it. A piece is a part of a clause cut at single quotes, as
// IDText cuts it; a clause with no single quote is one piece. Apply LineText
// once to a whole line, before remedy expansion.
func LineText(s string) string {
	var b strings.Builder
	start := 0
	for start < len(s) {
		rest := s[start:]
		end, width := len(rest), 0
		if i := strings.Index(rest, Rancher); i >= 0 {
			end, width = i, len(Rancher)
		}
		if i := strings.Index(rest, "; "); i >= 0 && i < end {
			end, width = i, len("; ")
		}
		if i := strings.Index(rest, " ("); i >= 0 && i < end {
			end, width = i, len(" (")
		}
		b.WriteString(IDText(rest[:end]))
		b.WriteString(rest[end : end+width])
		start += end + width
	}
	return b.String()
}

// ValueText is v written for a place where it may hold a message or command of
// this CLI. A value from a server never holds this process's remedy marker;
// IDText writes the whole of it. A value that holds the marker is a message or
// command of this CLI relayed as a value; LineText writes it clause by clause
// and leaves each Rancher as written.
func ValueText(v string) string {
	if strings.Contains(v, RemedyMarker) {
		return LineText(v)
	}
	return IDText(v)
}

// WholeValueText is v written for a place where the renderer knows where v
// starts and ends. When v is exactly the double-quoted form strconv.Quote
// writes for a text that NotPrintable reports true for, and holds no single
// quote, v would print the same as the escaped form of that text; it is
// written in double quotes again, with its quotes and backslashes as escapes.
// Any other value is written as ValueText writes it. A value that holds a
// single quote goes to ValueText: IDText never writes a single quote inside
// double quotes. Such a value is never one double-quoted string that IDText
// wrote, and quoting it again would put a single quote inside double quotes.
// These come back as they are: "$PASSWORD", "ops", '"a"', a command with
// nothing to escape, ShellQuote of an IDText result. A part of a line has no
// known start and end, and a command is part of a line; for those, line texts
// and previews use LineText or ValueText. A value that holds the remedy marker
// goes to ValueText before any other test.
func WholeValueText(v string) string {
	if !strings.Contains(v, RemedyMarker) && !strings.Contains(v, "'") {
		if u, err := strconv.Unquote(v); err == nil && NotPrintable(u) && strconv.Quote(u) == v {
			return strconv.Quote(v)
		}
	}
	return ValueText(v)
}
