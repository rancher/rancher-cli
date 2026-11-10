package fidelity

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellLexer splits one logical line of shell text into commands. It reads
// quotes, escapes, nested groups ($(...), ${...}, <(...), backticks) and
// redirections, and ends a command at an unquoted |, ;, &&, || or lone &. Every
// command of a nested group is a command of its own. It reads text only and
// runs nothing.
type shellLexer struct {
	s        string
	pos      int
	commands [][]string
}

// lexShellLine returns every command of the line as its words, quotes kept.
// A redirection and its operand are not words.
func lexShellLine(line string) [][]string {
	l := &shellLexer{s: line}
	l.list(0)
	return l.commands
}

var shellRedirect = regexp.MustCompile(`^[0-9]*(?:&>>|&>|>>|>&|>\||<<<|<<-|<<|<&|<>|<|>)`)

func (l *shellLexer) peek(n int) byte {
	if l.pos+n < len(l.s) {
		return l.s[l.pos+n]
	}
	return 0
}

// redirectLength is the length of a redirection operator at the position, or
// zero when the text there starts a process substitution or no redirection.
func (l *shellLexer) redirectLength() int {
	m := shellRedirect.FindString(l.s[l.pos:])
	if m == "" {
		return 0
	}
	if last := m[len(m)-1]; (last == '<' || last == '>') && l.peek(len(m)) == '(' {
		return 0
	}
	return len(m)
}

func (l *shellLexer) list(closer byte) {
	var words []string
	finish := func() {
		if len(words) > 0 {
			l.commands = append(l.commands, words)
		}
		words = nil
	}
	defer finish()
	for {
		for l.pos < len(l.s) && (l.s[l.pos] == ' ' || l.s[l.pos] == '\t' || l.s[l.pos] == '\r' || l.s[l.pos] == '\n') {
			l.pos++
		}
		if l.pos >= len(l.s) {
			return
		}
		c := l.s[l.pos]
		if closer != 0 && c == closer {
			l.pos++
			return
		}
		if c == '#' {
			l.pos = len(l.s)
			return
		}
		if n := l.redirectLength(); n > 0 {
			l.pos += n
			for l.pos < len(l.s) && (l.s[l.pos] == ' ' || l.s[l.pos] == '\t') {
				l.pos++
			}
			if l.pos < len(l.s) && !strings.ContainsRune("|;&()", rune(l.s[l.pos])) {
				l.word(closer)
			}
			continue
		}
		switch c {
		case '|', ';', '&':
			finish()
			for l.pos < len(l.s) && l.s[l.pos] == c {
				l.pos++
			}
		case ')':
			finish()
			l.pos++
		case '(':
			finish()
			l.pos++
			l.list(')')
		default:
			start := l.pos
			word := l.word(closer)
			if l.pos == start {
				l.pos++
				continue
			}
			words = append(words, word)
		}
	}
}

func (l *shellLexer) word(closer byte) string {
	start := l.pos
loop:
	for l.pos < len(l.s) {
		switch c := l.s[l.pos]; c {
		case ' ', '\t', '\r', '\n':
			break loop
		case '\\':
			l.pos = min(l.pos+2, len(l.s))
		case '\'':
			l.skipSingle()
		case '"':
			l.skipDouble()
		case '`':
			if closer == '`' {
				break loop
			}
			l.pos++
			l.list('`')
		case '$':
			l.dollar(false)
		case '<', '>':
			if l.peek(1) != '(' {
				break loop
			}
			l.pos += 2
			l.list(')')
		case '(':
			if l.pos == start || l.s[l.pos-1] != '=' {
				break loop
			}
			l.pos++
			l.skipArray()
		case '|', '&', ';', ')':
			break loop
		default:
			l.pos++
		}
	}
	return l.s[start:l.pos]
}

func (l *shellLexer) skipSingle() {
	l.pos++
	if end := strings.IndexByte(l.s[l.pos:], '\''); end >= 0 {
		l.pos += end + 1
		return
	}
	l.pos = len(l.s)
}

func (l *shellLexer) skipDouble() {
	l.pos++
	for l.pos < len(l.s) {
		switch l.s[l.pos] {
		case '\\':
			l.pos = min(l.pos+2, len(l.s))
		case '"':
			l.pos++
			return
		case '$':
			l.dollar(true)
		case '`':
			l.pos++
			l.list('`')
		default:
			l.pos++
		}
	}
}

func (l *shellLexer) dollar(inDouble bool) {
	switch l.peek(1) {
	case '(':
		l.pos += 2
		l.list(')')
	case '{':
		l.pos += 2
		l.skipBraces()
	case '\'':
		if inDouble {
			l.pos++
			return
		}
		l.pos += 2
		for l.pos < len(l.s) && l.s[l.pos] != '\'' {
			if l.s[l.pos] == '\\' {
				l.pos++
			}
			l.pos++
		}
		l.pos = min(l.pos+1, len(l.s))
	default:
		l.pos++
	}
}

func (l *shellLexer) skipBraces() {
	depth := 1
	for l.pos < len(l.s) {
		switch l.s[l.pos] {
		case '\\':
			l.pos = min(l.pos+2, len(l.s))
		case '\'':
			l.skipSingle()
		case '"':
			l.skipDouble()
		case '$':
			l.dollar(false)
		case '{':
			depth++
			l.pos++
		case '}':
			depth--
			l.pos++
			if depth == 0 {
				return
			}
		default:
			l.pos++
		}
	}
}

func (l *shellLexer) skipArray() {
	depth := 1
	for l.pos < len(l.s) {
		switch l.s[l.pos] {
		case '\\':
			l.pos = min(l.pos+2, len(l.s))
		case '\'':
			l.skipSingle()
		case '"':
			l.skipDouble()
		case '$':
			l.dollar(false)
		case '`':
			l.pos++
			l.list('`')
		case '(':
			depth++
			l.pos++
		case ')':
			depth--
			l.pos++
			if depth == 0 {
				return
			}
		default:
			l.pos++
		}
	}
}

// unquoteWord removes the quotes and escapes of a word. A nested group stays as
// written.
func unquoteWord(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case '\'':
			end := strings.IndexByte(raw[i+1:], '\'')
			if end < 0 {
				b.WriteString(raw[i+1:])
				return b.String()
			}
			b.WriteString(raw[i+1 : i+1+end])
			i += end + 1
		case '"':
			i++
			for i < len(raw) && raw[i] != '"' {
				if raw[i] == '\\' && i+1 < len(raw) {
					i++
				}
				b.WriteByte(raw[i])
				i++
			}
		case '\\':
			if i+1 < len(raw) {
				i++
				b.WriteByte(raw[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

var (
	shellAssignment   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)
	curlFunctionStart = regexp.MustCompile(`^\s*(?:function\s+([A-Za-z_][A-Za-z0-9_.:-]*)(?:\s*\(\))?|([A-Za-z_][A-Za-z0-9_.:-]*)\s*\(\))\s*\{(.*)$`)
	curlFunctionHead  = regexp.MustCompile(`^\s*(?:function\s+[A-Za-z_][A-Za-z0-9_.:-]*(?:\s*\(\))?|[A-Za-z_][A-Za-z0-9_.:-]*\s*\(\))\s*$`)
	forwardedWord     = regexp.MustCompile(`^"?\$(?:[@*]|\{[@*][^}]*\})"?$`)

	approvedHeader   = regexp.MustCompile(`^@<\(printf 'Authorization: Bearer %s\\n' "\$\{[A-Z][A-Z0-9_]*\}"\)$`)
	credentialHeader = regexp.MustCompile(`(?i)^\s*(?:proxy-)?authorization\s*:|bearer|cookie\s*:`)
	contentTypeValue = regexp.MustCompile(`(?i)^\s*content-type\s*:`)
	hostHeaderPrefix = regexp.MustCompile(`(?i)^host: ?$`)
	headerOnLine     = regexp.MustCompile(`(?:^|\s)(?:-[A-Za-z]*H|--header)`)
	bareExpansion    = regexp.MustCompile(`^\$(?:\{[^}]*\}|[A-Za-z_][A-Za-z0-9_]*|[@*0-9])$`)
	commandExpansion = regexp.MustCompile(`^"?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?"?$`)
	arrayExpansion   = regexp.MustCompile(`^"?\$\{([A-Za-z_][A-Za-z0-9_]*)\[[@*]\]\}"?$`)
	arrayAssignment  = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\+?=\(([\s\S]*?)\)?$`)
	embeddedCurl     = regexp.MustCompile(`["']curl["']\s*[,\]]`)
	curlInString     = regexp.MustCompile(`\bcurl\b`)
	credentialInText = regexp.MustCompile(`(?i)(?:^|\s)(?:-[A-Za-z]*[HuUb]|(?:` + longOptionPattern() + `)(?:=|\s|$))|authorization`)
)

// longOptionPattern is the alternation of every long option of the header and
// credential lists, for the rule on a string that holds curl.
func longOptionPattern() string {
	var quoted []string
	for _, option := range slices.Concat(headerLongOptions, credentialLongOptions) {
		quoted = append(quoted, regexp.QuoteMeta(option))
	}
	return strings.Join(quoted, "|")
}

var headerLongOptions = []string{"--header", "--proxy-header", "--expand-header"}

// valueOptionLetters are the short curl options that take a value: the rest of
// the cluster is that value, not more options.
const valueOptionLetters = "AcCdDeEFKmoPQrtTwxXyYz"

var credentialLongOptions = []string{"--user", "--proxy-user", "--cookie", "--oauth2-bearer", "--aws-sigv4", "--variable"}

// isCommentLine reports whether the logical line is a comment.
func isCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

// isCommandName reports whether the word names one of names: its last path
// segment, quotes removed, is a name and it holds no expansion.
func isCommandName(raw string, names map[string]bool) bool {
	text := unquoteWord(raw)
	if strings.ContainsAny(text, "$`") {
		return false
	}
	last := text
	if _, after, found := strings.CutLast(text, "/"); found {
		last = after
	}
	return names[last]
}

// forwardsArguments reports whether the command calls one of names and passes
// on the arguments of its own function.
func forwardsArguments(command []string, names map[string]bool) bool {
	calls, forwards := false, false
	for _, word := range command {
		if isCommandName(word, names) {
			calls = true
		}
		if forwardedWord.MatchString(word) {
			forwards = true
		}
	}
	return calls && forwards
}

// derivedWrappers returns the functions of the text that run curl with the
// arguments they were called with, directly or through another such function,
// sorted by name.
func derivedWrappers(lines []string) []string {
	var folded []string
	for i := 0; i < len(lines); i++ {
		if i+1 < len(lines) && curlFunctionHead.MatchString(lines[i]) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "{") {
			folded = append(folded, lines[i]+" "+strings.TrimSpace(lines[i+1]))
			i++
			continue
		}
		folded = append(folded, lines[i])
	}
	lines = folded
	bodies := map[string][]string{}
	for i, line := range lines {
		match := curlFunctionStart.FindStringSubmatch(line)
		if match == nil || isCommentLine(line) {
			continue
		}
		name := match[1] + match[2]
		rest := strings.TrimSpace(match[3])
		if body, ok := strings.CutSuffix(rest, "}"); ok {
			bodies[name] = append(bodies[name], body)
			continue
		}
		if rest != "" {
			bodies[name] = append(bodies[name], rest)
		}
		for _, next := range lines[i+1:] {
			if next == "}" {
				break
			}
			bodies[name] = append(bodies[name], next)
		}
	}
	names := map[string]bool{"curl": true, "curl.exe": true}
	var wrappers []string
	for changed := true; changed; {
		changed = false
		for name, body := range bodies {
			if names[name] {
				continue
			}
			for _, line := range body {
				if isCommentLine(line) {
					continue
				}
				if slices.ContainsFunc(lexShellLine(line), func(c []string) bool { return forwardsArguments(c, names) }) {
					names[name] = true
					wrappers = append(wrappers, name)
					changed = true
					break
				}
			}
		}
	}
	slices.Sort(wrappers)
	return wrappers
}

// withoutGroups returns the text with every $(...), <(...) and >(...) group
// removed: a command in such a group is a command of its own and is scanned
// as one.
func withoutGroups(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if i+1 < len(text) && text[i+1] == '(' && (text[i] == '$' || text[i] == '<' || text[i] == '>') {
			depth := 0
			for i++; i < len(text); i++ {
				if text[i] == '(' {
					depth++
				} else if text[i] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			continue
		}
		b.WriteByte(text[i])
	}
	return b.String()
}

// curlInvocation is the argument list that follows one command word of curl or
// of a wrapper of curl. variable is true when the command word is an expansion.
type curlInvocation struct {
	args     []string
	variable bool
}

var shellPrefixWords = []string{"!", "if", "then", "elif", "else", "while", "until", "do", "{", "time"}

// curlInvocations returns each place the command runs curl or a wrapper. A
// command that only names curl (command -v, type, which, hash, a for list) runs
// nothing.
func curlInvocations(command []string, names map[string]bool) []curlInvocation {
	first := 0
	for first < len(command) && (shellAssignment.MatchString(command[first]) || slices.Contains(shellPrefixWords, command[first])) {
		first++
	}
	if first >= len(command) {
		return nil
	}
	switch unquoteWord(command[first]) {
	case "type", "which", "hash", "for", "select":
		return nil
	case "command":
		if first+1 < len(command) && (command[first+1] == "-v" || command[first+1] == "-V") {
			return nil
		}
	}
	var out []curlInvocation
	for k := first; k < len(command); k++ {
		if isCommandName(command[k], names) {
			out = append(out, curlInvocation{args: command[k+1:]})
		}
	}
	if commandExpansion.MatchString(command[first]) {
		out = append(out, curlInvocation{args: command[first+1:], variable: true})
	}
	return out
}

// curlScan is the result of scanning the text of one script.
type curlScan struct {
	// offenders are the logical lines (white space squashed, the name of the file
	// in front) that put a credential on a curl argument list.
	offenders []string
	reasons   map[string][]string
	wrappers  []string
	// curlLines counts the logical lines that run curl or a wrapper; headers
	// counts the header values checked; approved counts the values in the
	// approved form; contentTypes counts the literal Content-Type values.
	curlLines, headers, approved, contentTypes int
}

func squashSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// headerOption returns the value of the header option at args[i]: the word
// after -H or --header, or the rest of the word. next is true when the value is
// the following word.
func headerOption(args []string, i int) (value string, ok, next bool) {
	word := args[i]
	for _, long := range headerLongOptions {
		if word == long {
			if i+1 < len(args) {
				return args[i+1], true, true
			}
			return "", false, false
		}
		if v, found := strings.CutPrefix(word, long+"="); found {
			return v, true, false
		}
	}
	if !strings.HasPrefix(word, "-") || strings.HasPrefix(word, "--") {
		return "", false, false
	}
	for j := 1; j < len(word); j++ {
		c := word[j]
		if !isClusterLetter(c) {
			break
		}
		if strings.IndexByte(valueOptionLetters, c) >= 0 {
			break
		}
		if c == 'H' {
			if rest := word[j+1:]; rest != "" {
				return rest, true, false
			}
			if i+1 < len(args) {
				return args[i+1], true, true
			}
			return "", false, false
		}
	}
	return "", false, false
}

// isClusterLetter reports whether c can be part of a cluster of short options:
// a letter, one of the digit options (-0 to -6) or #.
func isClusterLetter(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '#'
}

// credentialOption reports whether the word is an option that carries a
// credential: a user, a cookie, a bearer token or a signature.
func credentialOption(word string) bool {
	for _, long := range credentialLongOptions {
		if word == long || strings.HasPrefix(word, long+"=") {
			return true
		}
	}
	if !strings.HasPrefix(word, "-") || strings.HasPrefix(word, "--") {
		return false
	}
	for j := 1; j < len(word); j++ {
		c := word[j]
		if !isClusterLetter(c) {
			return false
		}
		if strings.IndexByte(valueOptionLetters, c) >= 0 {
			return false
		}
		if c == 'u' || c == 'U' || c == 'b' {
			return true
		}
	}
	return false
}

// checkHeaderValue returns why a header value is refused, or nothing.
func checkHeaderValue(raw string, scan *curlScan) []string {
	scan.headers++
	if approvedHeader.MatchString(raw) {
		scan.approved++
		return nil
	}
	text := unquoteWord(raw)
	if strings.HasPrefix(text, "@") {
		return []string{"a header read from a file the scan cannot see: " + raw}
	}
	at := strings.IndexAny(text, "$`")
	if at < 0 {
		if credentialHeader.MatchString(text) {
			return []string{"a header that holds a credential: " + raw}
		}
		if contentTypeValue.MatchString(text) {
			scan.contentTypes++
		}
		return nil
	}
	if hostHeaderPrefix.MatchString(text[:at]) {
		return nil
	}
	return []string{"a header with an expansion: " + raw}
}

// checkCurlArguments returns why the arguments of one curl call are refused.
func checkCurlArguments(args []string, scan *curlScan) []string {
	var reasons []string
	for i := 0; i < len(args); i++ {
		word := args[i]
		if bareExpansion.MatchString(word) && !arrayExpansion.MatchString(word) {
			reasons = append(reasons, "options kept in a variable: "+word)
			continue
		}
		if value, ok, next := headerOption(args, i); ok {
			if next {
				i++
			}
			reasons = append(reasons, checkHeaderValue(value, scan)...)
			continue
		}
		if credentialOption(word) {
			reasons = append(reasons, "an option that carries a credential: "+word)
		}
	}
	return reasons
}

// scanCurlSource scans the text of one script. name is the file name that
// prefixes each offender.
func scanCurlSource(name, source string) curlScan {
	lines := joinContinuations(strings.Split(source, "\n"))
	scan := curlScan{reasons: map[string][]string{}, wrappers: derivedWrappers(lines)}
	names := map[string]bool{"curl": true, "curl.exe": true}
	for _, wrapper := range scan.wrappers {
		names[wrapper] = true
	}

	type parsedLine struct {
		text     string
		commands [][]string
	}
	var parsed []parsedLine
	arrays := map[string]bool{}
	for _, line := range lines {
		if isCommentLine(line) || strings.TrimSpace(line) == "" {
			continue
		}
		commands := lexShellLine(line)
		parsed = append(parsed, parsedLine{line, commands})
		for _, command := range commands {
			for _, invocation := range curlInvocations(command, names) {
				for _, word := range invocation.args {
					if m := arrayExpansion.FindStringSubmatch(word); m != nil {
						arrays[m[1]] = true
					}
				}
			}
		}
	}

	for _, line := range parsed {
		var reasons []string
		curl := false
		if embeddedCurl.MatchString(line.text) {
			reasons = append(reasons, "curl started from embedded code")
		}
		for _, command := range line.commands {
			for _, word := range command {
				text := withoutGroups(unquoteWord(word))
				if curlInString.MatchString(text) && credentialInText.MatchString(text) {
					reasons = append(reasons, "curl started from a string: "+word)
				}
			}
		}
		check := func(invocation curlInvocation) {
			reasons = append(reasons, checkCurlArguments(invocation.args, &scan)...)
			if invocation.variable && headerOnLine.MatchString(line.text) {
				reasons = append(reasons, "command kept in a variable")
			}
		}
		for _, command := range line.commands {
			for _, invocation := range curlInvocations(command, names) {
				if !invocation.variable {
					curl = true
				}
				check(invocation)
			}
			for _, word := range command {
				m := arrayAssignment.FindStringSubmatch(word)
				if m == nil || !arrays[m[1]] {
					continue
				}
				for _, inner := range lexShellLine("curl " + m[2]) {
					for _, invocation := range curlInvocations(inner, map[string]bool{"curl": true}) {
						check(invocation)
					}
				}
			}
		}
		if curl {
			scan.curlLines++
		}
		if len(reasons) > 0 {
			offender := name + ": " + squashSpace(line.text)
			scan.offenders = append(scan.offenders, offender)
			scan.reasons[offender] = reasons
		}
	}
	return scan
}

// curlAllowlistRows are the offenders the guard accepts, keyed by the file name
// and the logical line with white space squashed, each with its reason.
var curlAllowlistRows = map[string]string{}

// checkCurlAllowlist returns the offenders that have no row and the problems of
// the rows: an empty reason, and a row that no offender matches.
func checkCurlAllowlist(offenders []string, rows map[string]string) (unlisted, problems []string) {
	for _, offender := range offenders {
		if _, ok := rows[offender]; !ok {
			unlisted = append(unlisted, offender)
		}
	}
	for key, reason := range rows {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, "the row has no reason: "+key)
		}
		if !slices.Contains(offenders, key) {
			problems = append(problems, "the row matches no offending line; remove it: "+key)
		}
	}
	slices.Sort(problems)
	return unlisted, problems
}

type scannedScript struct {
	name string
	scan curlScan
}

// scriptFiles returns the scripts/ directory and the names of its regular
// files, whatever their first line. Symlinks and directories are skipped.
func scriptFiles(t *testing.T) (dir string, names []string) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts"))
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	return dir, names
}

// scanScripts scans every regular file directly under scripts/.
func scanScripts(t *testing.T) []scannedScript {
	t.Helper()
	dir, names := scriptFiles(t)
	var out []scannedScript
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		out = append(out, scannedScript{name, scanCurlSource(name, string(raw))})
	}
	return out
}

// TestCurlArgumentListsHoldNoAuthorizationHeaderOrCredentialOption asserts that
// no curl call in a script under scripts/ puts an Authorization header, a
// cookie or one of the credential options listed below on its argument list,
// which every process of every session on the machine can read. It reads the
// text of every regular file in the directory, whatever its first line, so a new
// script or a sourced library is scanned without an edit. Continuation lines are
// joined and comments skipped. A command word is curl (by its last path segment)
// or a function of the same file that runs curl with the arguments it was called
// with ("$@", "${@}", "${@:2}" or $*, defined as name() {, function name { or
// with the { on the next line), found by repeating until no new function turns
// up; a call with -H to such a function is scanned like curl. A header value
// (-H, --header, --proxy-header, --expand-header, alone, in a cluster of short
// options or after an equals sign; a cluster ends at an option that takes a
// value, so -XPUT is no header) is allowed when it is exactly @<(printf
// 'Authorization: Bearer %s\n' "${NAME}") with NAME a variable in capitals, a
// literal that is neither a credential nor a cookie, or a Host header with an
// expansion. Any other header that starts with @, a header with an expansion,
// and the options -u, -U, -b, --user, --proxy-user, --cookie, --oauth2-bearer,
// --aws-sigv4 and --variable are offences. An unquoted option kept in a variable
// is an offence, and so is a curl command word kept in a variable on a line with
// a header; an array passed as "${NAME[@]}" makes the line that assigns NAME=(
// count as a curl call. A quoted "curl" followed by a comma or a bracket
// (embedded Python) is an offence, and so is a quoted word that holds curl with
// a header or credential option or the word authorization (sh -c, bash -c, eval,
// docker exec ... sh -c): it fails closed, even for prose such as echo "curl -H
// is slow". The exact approved text is the only allowed form because the token
// stays off every argument list only while printf is the shell builtin: an
// external printf or env printf in the same place would put it on the argument
// list of the process that writes the pipe.
//
// Not covered: a wrapper whose body is a subshell (fetch() ( curl "$@" )), a
// wrapper that passes its arguments on through an array, and a wrapper with a
// blank line between the name and the {; --pass, --key-pass, -E and --cert with
// a password; a user and password in the URL (https://user:pass@host); a secret
// in -d, --data-urlencode or -F; a literal header with another secret name such
// as X-Auth-Token; a curl run from a string that holds none of the header or
// credential options above; a header that sits on another logical line than its
// curl command through a quoted newline; an array assigned over several lines
// without a backslash; a variable that holds a whole option in quotes; and a
// wrapper called from another file. An offender is one logical line, however
// many bad values it holds. A line with a reason in curlAllowlistRows is
// accepted; a row with no reason or with no offending line fails.
func TestCurlArgumentListsHoldNoAuthorizationHeaderOrCredentialOption(t *testing.T) {
	var offenders []string
	reasons := map[string][]string{}
	for _, script := range scanScripts(t) {
		offenders = append(offenders, script.scan.offenders...)
		maps.Copy(reasons, script.scan.reasons)
	}
	unlisted, problems := checkCurlAllowlist(offenders, curlAllowlistRows)
	var lines []string
	for _, offender := range unlisted {
		lines = append(lines, fmt.Sprintf("%s\n    %s", offender, strings.Join(reasons[offender], "\n    ")))
	}
	assert.Emptyf(t, lines, "a curl call puts a credential on its argument list; send the token as the comment above api() in scripts/e2e-fidelity describes")
	assert.Empty(t, problems)
}

// TestCurlCredentialScanReadsTheScripts asserts that the scan of the scripts
// has not gone blind: it found the three scripts that matter and every regular
// file of the directory, derived exactly one wrapper of curl in
// scripts/e2e-fidelity (a new wrapper is a reviewed change to this assertion),
// and saw at least one approved header and one literal Content-Type header. It
// prints the counts.
func TestCurlCredentialScanReadsTheScripts(t *testing.T) {
	scripts := scanScripts(t)
	_, files := scriptFiles(t)
	regular := len(files)
	var names []string
	for _, script := range scripts {
		names = append(names, script.name)
	}
	for _, want := range []string{"e2e-fidelity", "e2e-fidelity-downstream", "rancher-api-fields"} {
		assert.Contains(t, names, want)
	}
	assert.Len(t, scripts, regular)

	for _, script := range scripts {
		if script.name != "e2e-fidelity" {
			continue
		}
		assert.Equal(t, []string{"api"}, script.scan.wrappers)
		assert.GreaterOrEqual(t, script.scan.approved, 1, "no header in the approved form")
		assert.GreaterOrEqual(t, script.scan.contentTypes, 1, "no literal Content-Type header")
		assert.GreaterOrEqual(t, script.scan.curlLines, 1)
		assert.GreaterOrEqual(t, script.scan.headers, 1)
		t.Logf("scripts/e2e-fidelity: wrappers %v, %d curl or wrapper lines, %d header values, %d approved, %d Content-Type", script.scan.wrappers, script.scan.curlLines, script.scan.headers, script.scan.approved, script.scan.contentTypes)
	}
	t.Logf("%d regular files scanned: %v", len(scripts), names)
}

// TestCurlCredentialMatcher asserts what the scan flags and what it passes, on
// source text, including a source with Windows line ends and each row of the
// allowlist check.
func TestCurlCredentialMatcher(t *testing.T) {
	const approved = `@<(printf 'Authorization: Bearer %s\n' "${T}")`
	rows := []struct {
		name   string
		source string
		want   int
	}{
		{"the approved form", `curl -s -H ` + approved + ` url`, 0},
		{"a literal Content-Type", `curl -s -H 'Content-Type: application/json' url`, 0},
		{"a Host header with an expansion", `curl -s -H "Host: ${X}" url`, 0},
		{"a host header in lower case", `curl -s -H "host: ${h}" url`, 0},
		{"a header with the token", `curl -s -H "Authorization: Bearer ${T}" url`, 1},
		{"a literal authorization header", `curl -s -H 'authorization:x' url`, 1},
		{"a long option", `curl -s --header "Authorization: Bearer ${T}" url`, 1},
		{"a long option with an equals sign", `curl -s --header="Authorization: Bearer ${T}" url`, 1},
		{"a header glued to -H", `curl -s -H"Authorization: Bearer ${T}" url`, 1},
		{"a header glued to -H without quotes", `curl -s -HAuthorization:x url`, 1},
		{"a header kept in a variable", `curl -s -H "$hdr" url`, 1},
		{"a header kept in a braced variable", `curl -s -H "${hdr}" url`, 1},
		{"a header read from stdin", `curl -s -H @- url`, 1},
		{"a header read from a named file", `curl -s -H @"$f" url`, 1},
		{"a header from an external printf", `curl -s -H @<(/usr/bin/printf 'Authorization: Bearer %s\n' "$T") url`, 1},
		{"the approved form and a second header", `curl -s -H ` + approved + ` -H "Authorization: x" url`, 1},
		{"a header on a continuation line", "curl -s \\\n  -H \"Authorization: x\" url", 1},
		{"a header in a comment", `# curl -H "Authorization: x" url`, 0},
		{"a header with another name and an expansion", `curl -s -H "X-Api-Key: ${T}" url`, 1},
		{"a bearer option", `curl --oauth2-bearer "$T" url`, 1},
		{"a user option", `curl -u "a:$p" url`, 1},
		{"a user option in a cluster", `curl -su "a:$p" url`, 1},
		{"a proxy user option", `curl -U "a:$p" url`, 1},
		{"a cookie option", `curl -b "s=$T" url`, 1},
		{"a long cookie option", `curl --cookie "s=$T" url`, 1},
		{"a signature option", `curl --aws-sigv4 x url`, 1},
		{"a variable option", `curl --variable "t=$T" url`, 1},
		{"a header in a cluster of options", `curl -sH "Authorization: Bearer ${T}" url`, 1},
		{"a header in a longer cluster", `curl -fsSH "Authorization: x" url`, 1},
		{"a proxy header", `curl --proxy-header "Authorization: x" url`, 1},
		{"a redirection before the header", `curl -s 2>&1 -H "Authorization: Bearer ${T}" url`, 1},
		{"a here-string before the header", `curl url <<<"x" -H "Authorization: x"`, 1},
		{"a here-document before the header", `curl url -d @- <<JSON -H "Authorization: x"`, 1},
		{"a path to curl", `/usr/bin/curl -H "Authorization: x" url`, 1},
		{"a command kept in a variable", `"$CURL" -H "Authorization: x" url`, 1},
		{"a command in a pipeline", `x | curl -H "Authorization: Bearer ${T}" url`, 1},
		{"a command in a command substitution", `code="$(curl -s -H "Authorization: Bearer ${T}" url)" || true`, 1},
		{"options kept in a variable", `curl $opts url`, 1},
		{"an array with a header", "arr=(-H \"Authorization: x\")\ncurl \"${arr[@]}\" url", 1},
		{"an array of plain options", "arr=(--fail --silent)\ncurl \"${arr[@]}\" url", 0},
		{"embedded code", `["curl", "-H", "Authorization: Bearer x"]`, 1},
		{"a for list", `for tool in docker curl openssl; do`, 0},
		{"a quoted word with curl in it", `echo "error: curl failed"`, 0},
		{"command -v", `command -v curl`, 0},
		{"type", `type curl`, 0},
		{"which", `which curl`, 0},
		{"a redirection after the URL", `curl -sk "url" 2>&1`, 0},
		{"one line with two bad headers", `curl -H "Authorization: a" -H "Cookie: b" url`, 1},
		{"a wrapper", "api() {\n  curl -s \"$@\"\n}\napi -H \"Authorization: Bearer ${T}\" url", 1},
		{"a function that is no wrapper", "api() {\n  echo hi\n}\napi -H \"Authorization: x\" url", 0},
		{"a wrapper of a wrapper", "api() {\n  curl -s \"$@\"\n}\napi_json() { api \"$@\"; }\napi_json -H \"Authorization: x\" url", 1},
		{"a wrapper that forwards the arguments in braces", "fetch() {\n  curl -s \"${@}\"\n}\nfetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a wrapper that forwards a slice of the arguments", "fetch() {\n  curl -s \"${@:2}\"\n}\nfetch x -H \"Authorization: Bearer ${T}\" url", 1},
		{"a wrapper defined with the function keyword", "function fetch {\n  curl -s \"$@\"\n}\nfetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a wrapper defined with the function keyword and parentheses", "function fetch() {\n  curl -s \"$@\"\n}\nfetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a wrapper with the brace on the next line", "fetch()\n{\n  curl -s \"$@\"\n}\nfetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a wrapper with the brace on the next line and the function keyword", "function fetch\n{\n  curl -s \"$@\"\n}\nfetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a wrapper in braces called with the approved form", "fetch() {\n  curl -s \"${@}\"\n}\nfetch -H " + approved + " url", 0},
		{"curl in a string run by sh", `sh -c "curl -s -H \"Authorization: Bearer $T\" url"`, 1},
		{"curl in a string run by bash", `bash -c 'curl -u a:b url'`, 1},
		{"curl in a string run by eval", `eval "curl -H x url"`, 1},
		{"curl in a string run through docker exec", `docker exec c sh -c 'curl -H "Authorization: x" url'`, 1},
		{"curl in a string with a long credential option", `sh -c "curl --user a:b url"`, 1},
		{"prose with curl and an option fails closed", `echo "curl -H is slow"`, 1},
		{"curl in a string with no header or credential option", `sh -c "curl -s url"`, 0},
		{"a command substitution with the approved form", `code="$(curl -s -H ` + approved + ` url)"`, 0},
		{"a short option that takes a value holds no credential option", `curl -XPUT url`, 0},
		{"a short option that takes a value holds no header option", `curl -XHEAD url`, 0},
		{"the rest of a method option is no header value even when it reads like one", `curl -XHAuthorization:x url`, 0},
		{"a hyphen in the wrapper name", "my-fetch() {\n  curl -s \"$@\"\n}\nmy-fetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a hyphen in the wrapper name with the function keyword", "function my-fetch {\n  curl -s \"$@\"\n}\nmy-fetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a dot in the wrapper name", "my.fetch() {\n  curl -s \"$@\"\n}\nmy.fetch -H \"Authorization: Bearer ${T}\" url", 1},
		{"a digit option before the header", `curl -4H "Authorization: x" url`, 1},
		{"a digit option before a user", `curl -s4u a:b url`, 1},
		{"a hash option before the header", `curl -#H "Authorization: x" url`, 1},
		{"curl in a string with a proxy header", `sh -c "curl --proxy-header \"X-T: $T\" url"`, 1},
		{"curl in a string with a variable option", `sh -c "curl -s --variable t=$T url"`, 1},
		{"curl in a string with a signature option", `sh -c "curl --aws-sigv4 x url"`, 1},
		{"curl in a string with a longer option name", `sh -c "curl --user-agent x url"`, 0},
		{"a cluster that ends in an output file", `curl -sSo out url`, 0},
		{"a header after a method in a cluster", `curl -XPUT -H "Authorization: x" url`, 1},
		{"Windows line ends", "curl -s \\\r\n  -H \"Authorization: x\" url\r\n", 1},
		{"Windows line ends and the approved form", "curl -s \\\r\n  -H " + approved + " url\r\n", 0},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			scan := scanCurlSource("x", row.source)
			assert.Lenf(t, scan.offenders, row.want, "offenders %v for %s", scan.offenders, row.source)
		})
	}

	t.Run("a method option holds no header", func(t *testing.T) {
		assert.Zero(t, scanCurlSource("x", `curl -XHEAD url`).headers)
	})

	t.Run("the approved form is counted", func(t *testing.T) {
		scan := scanCurlSource("x", `curl -s -H `+approved+` -H 'Content-Type: application/json' url`)
		assert.Equal(t, 1, scan.approved)
		assert.Equal(t, 1, scan.contentTypes)
		assert.Equal(t, 2, scan.headers)
		assert.Equal(t, 1, scan.curlLines)
	})

	t.Run("the function api of the script and a caller", func(t *testing.T) {
		source := shellFunction(t, "api") + "api -o /dev/null -X POST \"${BASE_URL}/x\" -d @- <<JSON\n{}\nJSON\n"
		scan := scanCurlSource("e2e-fidelity", source)
		assert.Empty(t, scan.offenders)
		assert.Equal(t, []string{"api"}, scan.wrappers)
	})

	t.Run("an allowlisted line", func(t *testing.T) {
		scan := scanCurlSource("x", `curl -H "Authorization: x" url`)
		require.Len(t, scan.offenders, 1)
		unlisted, problems := checkCurlAllowlist(scan.offenders, map[string]string{scan.offenders[0]: "a reason"})
		assert.Empty(t, unlisted)
		assert.Empty(t, problems)
	})

	t.Run("a stale row", func(t *testing.T) {
		_, problems := checkCurlAllowlist(nil, map[string]string{"x: curl url": "a reason"})
		assert.Len(t, problems, 1)
	})

	t.Run("a row with no reason", func(t *testing.T) {
		scan := scanCurlSource("x", `curl -H "Authorization: x" url`)
		_, problems := checkCurlAllowlist(scan.offenders, map[string]string{scan.offenders[0]: " "})
		assert.Len(t, problems, 1)
	})
}
