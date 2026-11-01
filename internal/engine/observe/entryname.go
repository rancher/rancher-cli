package observe

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxComponentBytes is the longest file name the common file systems accept.
const maxComponentBytes = 255

// entryName is the name of one entry in the support-bundle archive. Its text
// is set only by entryPath and staticEntryPath, which accept nothing but
// plain file names, so no other value reaches the tar writer.
type entryName struct{ entryText string }

// windowsReservedNames are the device names Windows reads as a device in any
// directory, with any extension, in any letter case.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// entryPath builds the name of an archive entry from path segments. Each
// component is one whole segment, built by the caller with any value it holds
// already inside it ("events-" + namespace + ".json"); a "/" in a value then
// shows as a "/" in the segment and is refused.
func entryPath(components ...string) (entryName, error) {
	if len(components) == 0 {
		return entryName{}, errors.New("an archive entry name needs at least one path segment")
	}
	for _, c := range components {
		if err := checkComponent(c); err != nil {
			return entryName{}, err
		}
	}
	return entryName{entryText: strings.Join(components, "/")}, nil
}

// staticEntryPath builds the name of an entry that holds no value from the
// server. It panics on a refused name, so a typo in one fails every run of
// the command and its tests, never one user's bundle. Callers pass a string
// literal.
func staticEntryPath(path string) entryName {
	for part := range strings.SplitSeq(path, "/") {
		if err := checkComponent(part); err != nil {
			panic(fmt.Sprintf("static archive entry name %q: %v", path, err))
		}
	}
	return entryName{entryText: path}
}

// check reports why a name is not a sequence of plain path segments.
func (n entryName) check() error {
	for part := range strings.SplitSeq(n.entryText, "/") {
		if err := checkComponent(part); err != nil {
			return err
		}
	}
	return nil
}

// checkComponent is the one rule for a path segment of an archive entry: it
// accepts only ASCII letters, digits, ".", "_" and "-", so an extractor on any
// platform writes the entry where it belongs and under the name it has. It
// refuses an empty segment, "." and "..", a segment over 255 bytes or with a
// trailing ".", and one that starts with a device name Windows reserves. The
// rule applies to the whole segment: "events-" + ".." + ".json" is a plain
// name, so a value is never checked on its own.
func checkComponent(s string) error {
	switch {
	case s == "":
		return refusal(s, "it is empty")
	case s == "." || s == "..":
		return refusal(s, `it is "." or ".."`)
	case len(s) > maxComponentBytes:
		return refusal(s, "it is longer than 255 bytes")
	}
	for i := range len(s) {
		c := s[i]
		if c >= 0x80 {
			return refusal(s, "it has a non-ASCII character")
		}
		if !isPlainNameByte(c) {
			return refusal(s, "it has a "+strconv.QuoteRune(rune(c)))
		}
	}
	if strings.HasSuffix(s, ".") {
		return refusal(s, "it ends with a '.'")
	}
	base, _, _ := strings.Cut(s, ".")
	if windowsReservedNames[strings.ToUpper(base)] {
		return refusal(s, "it starts with a name reserved on Windows")
	}
	return nil
}

func isPlainNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

// refusal is the error for a refused segment. The value is quoted so a
// control character or a bidirectional mark prints as an escape.
func refusal(s, reason string) error {
	return fmt.Errorf("%s is not a plain file name: %s", quoteValue(s), reason)
}

// quoteValue quotes s for a message, cutting a value over 64 bytes short.
func quoteValue(s string) string {
	const limit = 64
	if len(s) <= limit {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:limit]) + "..."
}
