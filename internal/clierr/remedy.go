package clierr

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
)

// RemedyMarker opens every remedy command a builder writes, just ahead of
// the word rancher, so the invocation that prints the command decides what
// follows that word: the config directory it runs against unless a command
// pasted without --config would open the same one (kubectl's exec plugin and
// the kubeconfig command always name it, because their directory comes from
// the exec entry or a flag the operator's shell does not carry), or nothing.
//
// The marker is U+2063 (INVISIBLE SEPARATOR) followed by 32 characters
// chosen for each process, so text that came from anywhere else - a server,
// a file, another process - cannot hold it, and only this process's marker
// opens a remedy. U+2063 appears only as the first character. All of its
// characters are Unicode format characters, which output.Printable removes:
// a marker that reaches a terminal without being expanded leaves the plain
// command, and the command's own text is unchanged.
var RemedyMarker = markerFromBytes(markerBytes())

// Rancher begins every remedy command: Rancher + " auth login ...". It is
// a variable, since the marker is chosen when the process starts.
var Rancher = RemedyMarker + "rancher"

// markerRunes are the characters a marker holds after its first one,
// indexed by a two-bit value: U+2060 (WORD JOINER) for 0, U+2061 (FUNCTION
// APPLICATION) for 1, U+2062 (INVISIBLE TIMES) for 2 and U+2064 (INVISIBLE
// PLUS) for 3.
var markerRunes = [4]rune{'\u2060', '\u2061', '\u2062', '\u2064'}

// markerBytes is 8 random bytes from one call of the top-level Uint64 of
// math/rand/v2, which the Go runtime seeds for each process and which
// never fails. crypto/rand is not used: its Read and its default reader
// end the process when the system random source fails, and the exec plugin
// must never end that way.
func markerBytes() [8]byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], rand.Uint64())
	return b
}

// markerFromBytes is U+2063 followed by one character for every two bits of
// b: the bytes in order, the two lowest bits of each byte first, each pair
// read as a number from 0 to 3 that picks a character from markerRunes. All
// 64 bits of b change the result.
func markerFromBytes(b [8]byte) string {
	var sb strings.Builder
	sb.WriteString("\u2063")
	for _, v := range b {
		for shift := 0; shift < 8; shift += 2 {
			sb.WriteRune(markerRunes[(v>>shift)&3])
		}
	}
	return sb.String()
}

// withoutSeparator is s with every U+2063 removed, which any text can hold,
// whatever process wrote it.
func withoutSeparator(s string) string { return strings.ReplaceAll(s, "\u2063", "") }

// ExpandRemedies writes prefix after the word rancher in every remedy
// command s holds: " --config '<dir>'" for an invocation whose remedies
// name its config directory, "" for every other. A marker anywhere else is
// left as it is; only a marker ahead of rancher opens a remedy.
func ExpandRemedies(s, prefix string) string {
	if !strings.Contains(s, RemedyMarker) {
		return s
	}
	return strings.ReplaceAll(s, Rancher, "rancher"+prefix)
}

// DirectoryFreeCommands are the top-level commands that read no config
// directory, so a remedy naming one carries no --config: the manifest, the
// completion scripts and the skill installer. (rancher --version is a flag
// and a command path followed by --help reads no directory either; the
// closed set DirectoryFree accepts is these, those two forms and --help
// alone.)
var DirectoryFreeCommands = map[string]bool{"schema": true, "completion": true, "skill": true}

// directoryFreeHelpRE matches a command path followed by --help.
var directoryFreeHelpRE = regexp.MustCompile(`^[a-z][a-z0-9-]*( [a-z][a-z0-9-]*)* --help$`)

// DirectoryFree is the command "rancher " + rest for a command that reads
// no config directory, so it is written without the remedy marker and
// prints the same under every invocation. rest is --help alone, a command
// path followed by " --help", --version alone, or one of
// DirectoryFreeCommands followed by nothing or a space and its arguments.
// It panics for anything else: the check is on syntax only, and a command
// path comes from the command tree, so a panic is a defect in the caller.
func DirectoryFree(rest string) string {
	if rest == "--help" || rest == "--version" || directoryFreeHelpRE.MatchString(rest) {
		return "rancher " + rest
	}
	word, _, _ := strings.Cut(rest, " ")
	if DirectoryFreeCommands[word] {
		return "rancher " + rest
	}
	panic(fmt.Sprintf("clierr.DirectoryFree: %q is not a command that reads no config directory", rest))
}

// HasRemedy reports whether s holds a remedy command.
func HasRemedy(s string) bool { return strings.Contains(s, Rancher) }

// MarkRemedy is cmd, a command built as "rancher <rest>" by code whose
// output also reaches result data, written as a remedy command. Anything
// else is returned as it is.
func MarkRemedy(cmd string) string {
	if strings.HasPrefix(cmd, "rancher ") {
		return RemedyMarker + cmd
	}
	return cmd
}

// RemedyText is a details payload whose text can hold remedy commands; the
// renderer maps each of its strings through the invocation's expansion.
type RemedyText interface {
	MapText(f func(string) string) TextDetails
}

// RemedyCommands is a details payload with fields that hold a command and
// nothing else, which a renderer that can offer no command empties.
type RemedyCommands interface {
	MapCommands(f func(string) string) TextDetails
}

// MapText returns e with f applied to its message, hint, warnings and, for
// a details payload that implements RemedyText, its details. e itself is
// returned when none of them holds a marker, so the common error pays one
// scan. The copy keeps e's code, cause, retryability and candidates.
func (e *Error) MapText(f func(string) string) *Error {
	if !e.holdsMarker() {
		return e
	}
	c := *e
	c.Message = f(e.Message)
	c.Hint = f(e.Hint)
	c.warnings = make([]string, len(e.warnings))
	for i, w := range e.warnings {
		c.warnings[i] = f(w)
	}
	if rt, ok := e.Details.(RemedyText); ok {
		c.Details = rt.MapText(f)
	}
	return &c
}

// holdsMarker reports whether any text MapText maps holds a marker.
func (e *Error) holdsMarker() bool {
	if strings.Contains(e.Message, RemedyMarker) || strings.Contains(e.Hint, RemedyMarker) {
		return true
	}
	for _, w := range e.warnings {
		if strings.Contains(w, RemedyMarker) {
			return true
		}
	}
	if rt, ok := e.Details.(RemedyText); ok {
		held := false
		rt.MapText(func(s string) string {
			held = held || strings.Contains(s, RemedyMarker)
			return s
		})
		return held
	}
	return false
}

// MapCommands maps the payload's two command fields, the recovery
// commands, and the command fields of the primary's own details when they
// implement RemedyCommands.
func (d LoginFailureDetails) MapCommands(f func(string) string) TextDetails {
	if rc, ok := d.Primary.Details.(RemedyCommands); ok {
		d.Primary.Details = rc.MapCommands(f)
	}
	if d.CredentialRecovery != nil {
		cr := *d.CredentialRecovery
		cr.Command = f(cr.Command)
		d.CredentialRecovery = &cr
	}
	if d.ConfigRecovery != nil {
		cr := *d.ConfigRecovery
		cr.Command = f(cr.Command)
		d.ConfigRecovery = &cr
	}
	return d
}

// MapText maps every string of the payload that can carry a command: the
// primary failure's message and hint, the primary's own details when they
// implement RemedyText, the two recovery commands and the recovery text.
func (d LoginFailureDetails) MapText(f func(string) string) TextDetails {
	d.Primary.Message = f(d.Primary.Message)
	d.Primary.Hint = f(d.Primary.Hint)
	if rt, ok := d.Primary.Details.(RemedyText); ok {
		d.Primary.Details = rt.MapText(f)
	}
	if d.CredentialRecovery != nil {
		cr := *d.CredentialRecovery
		cr.Command = f(cr.Command)
		d.CredentialRecovery = &cr
	}
	if d.ConfigRecovery != nil {
		cr := *d.ConfigRecovery
		cr.Command = f(cr.Command)
		d.ConfigRecovery = &cr
	}
	d.Recovery = f(d.Recovery)
	return d
}
