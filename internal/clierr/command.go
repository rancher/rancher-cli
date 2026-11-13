package clierr

import (
	"fmt"
	"slices"
	"strings"
)

// CommandWord is constant command text: command words ("delete token"), a
// flag spelling ("--cluster"), flag text with placeholders
// (" --cacert <file>"), a placeholder ("<entry>") or the lead-in of a step
// ("then "). An untyped string constant converts to it implicitly; a string
// variable does not compile. A conformance test refuses every other way to
// make one from a runtime value, so every CommandWord is built from constants
// or from a kinds table row.
type CommandWord string

// CommandOption adds flags to a command. A helper that builds flags for
// several commands returns one, never a string.
type CommandOption func(Command) Command

// Lead is the constant words that introduce the command right after it in a
// Text ("; revoke it with: "). Text writes it only when that command is
// printed. Facts such as who runs the command and where are prose, not part
// of a Lead.
type Lead CommandWord

// LeftOutNote is the sentence that stands in for a command or sequence left
// out because a value in it cannot be printed.
const LeftOutNote = "a command is left out because a value in it cannot be printed so that every supported shell reads it as written"

// NotQuotable reports whether v cannot be one argument of a printed command
// that POSIX sh, bash, zsh, fish, PowerShell 7 and Windows PowerShell 5.1 all
// read as written. It holds for an empty value, a value NotPrintable
// reports, a rune that ends a single-quoted string (U+0027 and the curly
// single quotes U+2018 to U+201B), U+0022, two backslashes in a row and a
// final backslash.
func NotQuotable(v string) bool {
	return v == "" || NotPrintable(v) ||
		strings.ContainsAny(v, "'\u2018\u2019\u201a\u201b\"") ||
		strings.Contains(v, `\\`) || strings.HasSuffix(v, `\`)
}

// quoteExact wraps v, which NotQuotable reports false for, in single quotes.
func quoteExact(v string) string { return "'" + v + "'" }

// cmdValue is one positional value: its text as printed, and whether it
// starts with "-" once the whitespace around it is cut off.
type cmdValue struct {
	text string
	dash bool
}

// Command is one rancher command built from exact values. Its methods return
// a new Command; the receiver never changes.
type Command struct {
	start   string
	words   []CommandWord
	values  []cmdValue
	flags   []string
	list    string // a revoke's list command, kept when the revoke is left out
	server  string // the server a revoke runs against, named in its fallback
	leftOut bool
}

// NewCommand starts a remedy command, opened by Rancher.
func NewCommand(words CommandWord) Command {
	return Command{start: Rancher, words: []CommandWord{words}}
}

// FileCommand starts a command written into file text, where no invocation
// expands it: it starts with the plain word rancher.
func FileCommand(words CommandWord) Command {
	return Command{start: "rancher", words: []CommandWord{words}}
}

// FileCommandIn starts "rancher --config '<dir>' <words>" for file text. An
// empty dir gives no --config; a dir NotQuotable reports true leaves the
// command out.
func FileCommandIn(dir string, words CommandWord) Command {
	c := FileCommand(words)
	switch {
	case dir == "":
	case NotQuotable(dir):
		c.leftOut = true
	default:
		c.start = "rancher --config " + quoteExact(dir)
	}
	return c
}

// Word adds a command word from a reviewed table, unquoted.
func (c Command) Word(w CommandWord) Command {
	c.words = slices.Concat(c.words, []CommandWord{w})
	return c
}

// Arg adds positional values, each in single quotes. A value NotQuotable
// reports true leaves the command out.
func (c Command) Arg(values ...string) Command {
	added := make([]cmdValue, 0, len(values))
	for _, v := range values {
		if NotQuotable(v) {
			c.leftOut = true
		}
		added = append(added, cmdValue{text: quoteExact(v), dash: strings.HasPrefix(strings.TrimSpace(v), "-")})
	}
	c.values = slices.Concat(c.values, added)
	return c
}

// Placeholder adds a positional placeholder such as "<entry>".
func (c Command) Placeholder(p CommandWord) Command {
	c.values = slices.Concat(c.values, []cmdValue{{text: string(p)}})
	return c
}

// FlagValue adds " <spelling> '<value>'". A value NotQuotable reports true
// leaves the command out.
func (c Command) FlagValue(spelling CommandWord, value string) Command {
	if NotQuotable(value) {
		c.leftOut = true
	}
	c.flags = slices.Concat(c.flags, []string{" " + string(spelling) + " " + quoteExact(value)})
	return c
}

// Flag adds a flag with no value, written as its spelling ("--apply").
func (c Command) Flag(spelling CommandWord) Command {
	c.flags = slices.Concat(c.flags, []string{" " + string(spelling)})
	return c
}

// FlagText adds constant flag text that starts with a space and may hold
// placeholders (" --cluster <id>").
func (c Command) FlagText(text CommandWord) Command {
	c.flags = slices.Concat(c.flags, []string{string(text)})
	return c
}

// With applies the options in order.
func (c Command) With(opts ...CommandOption) Command {
	for _, o := range opts {
		c = o(c)
	}
	return c
}

// On names the server a revoke runs against when it is not the current one;
// a left-out revoke with a server says the token stays there.
func (c Command) On(server string) Command {
	c.server = server
	return c
}

// OK reports whether the command can be printed.
func (c Command) OK() bool { return !c.leftOut }

// text is the command line. When a positional value starts with "-" the flags
// come first and "--" ends them: the command parser reads no flag after it.
func (c Command) text() string {
	var b strings.Builder
	b.WriteString(c.start)
	for _, w := range c.words {
		b.WriteString(" ")
		b.WriteString(string(w))
	}
	dashed := slices.ContainsFunc(c.values, func(v cmdValue) bool { return v.dash })
	if dashed {
		b.WriteString(strings.Join(c.flags, ""))
		b.WriteString(" --")
	}
	for _, v := range c.values {
		b.WriteString(" ")
		b.WriteString(v.text)
	}
	if !dashed {
		b.WriteString(strings.Join(c.flags, ""))
	}
	return b.String()
}

// fallback is what stands in for a left-out revoke, without the note: the
// command that lists the tokens. It is "" for any other command.
func (c Command) fallback() string {
	switch {
	case c.list == "":
		return ""
	case c.server != "":
		return "the token stays on " + c.server + "; log in there and run: " + c.list
	}
	return "find it with: " + c.list
}

// String is the command. A left-out revoke gives its fallback and
// LeftOutNote; any other left-out command gives LeftOutNote.
func (c Command) String() string {
	if !c.leftOut {
		return c.text()
	}
	return WithLeftOutNote(c.fallback())
}

// Field is the command for a field that holds only a command: the command,
// the list command of a left-out revoke, or "" for any other left-out command.
func (c Command) Field() string {
	switch {
	case !c.leftOut:
		return c.text()
	case c.list != "":
		return c.list
	}
	return ""
}

// Revoke is the delete of a token of the kind ref names, with the command
// that lists tokens of that kind. The v3 kind is deprecated, so its delete
// carries --allow-deprecated.
func Revoke(ref TokenRef) Command {
	if ref.Kind == TokenKindV3 {
		c := NewCommand("delete management-token").Arg(ref.Name).Flag("--apply").Flag("--yes").Flag("--allow-deprecated")
		c.list = NewCommand("get management-tokens").text()
		return c
	}
	c := NewCommand("delete token").Arg(ref.Name).Flag("--apply").Flag("--yes")
	c.list = NewCommand("get tokens").text()
	return c
}

// RevokeKubeconfig is the delete of a Kubeconfig, with the command that
// lists them.
func RevokeKubeconfig(name string) Command {
	c := NewCommand("delete kubeconfig").Arg(name).Flag("--apply").Flag("--yes")
	c.list = NewCommand("get kubeconfigs").text()
	return c
}

// FileRevokeKubeconfig is RevokeKubeconfig for the header of a kubeconfig
// file, which no invocation expands.
func FileRevokeKubeconfig(name string) Command {
	c := FileCommand("delete kubeconfig").Arg(name).Flag("--apply").Flag("--yes")
	c.list = FileCommand("get kubeconfigs").text()
	return c
}

type seqStep struct {
	lead CommandWord
	cmd  Command
}

// Sequence is commands that run in order, each depending on the one before.
// It is left out whole when any step is.
type Sequence struct {
	steps  []seqStep
	server string // named in the fallback of a sequence that holds a revoke
}

// NewSequence starts an empty sequence.
func NewSequence() Sequence { return Sequence{} }

// Then adds a step with its constant lead-in ("", "then ", "then if it has no
// login, run: ").
func (s Sequence) Then(lead CommandWord, c Command) Sequence {
	s.steps = slices.Concat(s.steps, []seqStep{{lead: lead, cmd: c}})
	return s
}

// On names the server the sequence runs against, for the fallback of a
// sequence that holds a revoke.
func (s Sequence) On(server string) Sequence {
	s.server = server
	return s
}

// OK reports whether every step can be printed.
func (s Sequence) OK() bool {
	return !slices.ContainsFunc(s.steps, func(st seqStep) bool { return st.cmd.leftOut })
}

// revokeList is the list command of the first revoke in the sequence.
func (s Sequence) revokeList() string {
	for _, st := range s.steps {
		if st.cmd.list != "" {
			return st.cmd.list
		}
	}
	return ""
}

// fallback is what stands in for a left-out sequence that holds a revoke,
// without the note.
func (s Sequence) fallback() string {
	switch l := s.revokeList(); {
	case l == "":
		return ""
	case s.server != "":
		return "the token stays on " + s.server + "; log in there and run: " + l
	default:
		return "find it with: " + l
	}
}

// Lines is one command per step, or nil when the sequence is left out.
func (s Sequence) Lines() []string {
	if !s.OK() {
		return nil
	}
	out := make([]string, 0, len(s.steps))
	for _, st := range s.steps {
		out = append(out, st.cmd.text())
	}
	return out
}

// Field is the json form: the commands; when the sequence is left out and
// holds a revoke, the list command of that revoke; otherwise nil.
func (s Sequence) Field() []string {
	if s.OK() {
		return s.Lines()
	}
	if l := s.revokeList(); l != "" {
		return []string{l}
	}
	return nil
}

// String joins the steps with "; ", each after its lead-in. A left-out
// sequence gives its fallback and LeftOutNote.
func (s Sequence) String() string {
	if !s.OK() {
		return WithLeftOutNote(s.fallback())
	}
	parts := make([]string, 0, len(s.steps))
	for _, st := range s.steps {
		parts = append(parts, string(st.lead)+st.cmd.text())
	}
	return strings.Join(parts, "; ")
}

// Text joins prose (string), Lead, Command and Sequence parts into one text.
// A Lead is written only when the command right after it is printed. Before a
// left-out command or sequence the Lead is dropped, the fallback of a
// left-out revoke (or of a sequence that holds one) is written in its place,
// and LeftOutNote is written once, at the end. Prose is never dropped. Text
// panics on a part of any other type.
func Text(parts ...any) string {
	var b strings.Builder
	left := false
	var lead Lead
	emit := func(ok bool, text, fallback string) {
		switch {
		case ok:
			b.WriteString(string(lead))
			b.WriteString(text)
		default:
			left = true
			if fallback != "" {
				if s := b.String(); s != "" && !strings.HasSuffix(s, "; ") {
					b.WriteString("; ")
				}
				b.WriteString(fallback)
			}
		}
		lead = ""
	}
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case Lead:
			lead = v
		case Command:
			if v.start == "" {
				// The zero Command stands for no command: no lead-in, no note.
				lead = ""
				continue
			}
			emit(v.OK(), v.text(), v.fallback())
		case Sequence:
			emit(v.OK(), v.String(), v.fallback())
		default:
			panic(fmt.Sprintf("clierr.Text: unsupported part of type %T", p))
		}
	}
	if left {
		if s := b.String(); s != "" && !strings.HasSuffix(s, "; ") {
			b.WriteString("; ")
		}
		b.WriteString(LeftOutNote)
	}
	return NoteOnce(b.String())
}

// NoteOnce is text with LeftOutNote in it once, at the end, however many of
// the texts joined into it were written with a command left out. Text that
// holds no note is returned as it is.
func NoteOnce(text string) string {
	if !strings.Contains(text, LeftOutNote) {
		return text
	}
	text = strings.ReplaceAll(text, "; "+LeftOutNote, "")
	if rest, ok := strings.CutPrefix(text, LeftOutNote); ok {
		// The note led the text: drop the separator that followed it too.
		text = strings.TrimPrefix(rest, "; ")
	}
	return WithLeftOutNote(strings.ReplaceAll(text, LeftOutNote, ""))
}

// FlagFragment is "<spelling> '<value>'" for text that asks the reader to add
// a flag; an empty spelling gives "'<value>'" for a positional value. ok is
// false when the value is refused.
func FlagFragment(spelling CommandWord, value string) (string, bool) {
	if NotQuotable(value) {
		return "", false
	}
	if spelling == "" {
		return quoteExact(value), true
	}
	return string(spelling) + " " + quoteExact(value), true
}

// FlagFragmentText is "<spelling> <text>" with constant text
// ("--set fieldname=<value>").
func FlagFragmentText(spelling, text CommandWord) string {
	return string(spelling) + " " + string(text)
}

// PlainWord reports whether v is non-empty and every rune is an ASCII letter
// or digit, ".", "_" or "-": a word every supported shell reads as written
// without quotes. A word that starts with one "-" and holds a "." is not
// plain: PowerShell passes it to a program as two arguments, split at the ".".
func PlainWord(v string) bool {
	if v == "" {
		return false
	}
	if len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.Contains(v, ".") {
		return false
	}
	for _, r := range v {
		plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !plain {
			return false
		}
	}
	return true
}

// ShellWord is v as it is when PlainWord(v), else v in single quotes; ok is
// false when NotQuotable(v). It is the form of a value printed without quotes
// before: a plain value keeps its bytes, a value that needs quoting gets
// quoted.
func ShellWord(v string) (string, bool) {
	switch {
	case NotQuotable(v):
		return "", false
	case PlainWord(v):
		return v, true
	}
	return quoteExact(v), true
}

// FlagFragmentWord is "<spelling> <ShellWord(value)>" (an empty spelling
// gives the word alone), for a flag whose value was printed without quotes
// before.
func FlagFragmentWord(spelling CommandWord, value string) (string, bool) {
	w, ok := ShellWord(value)
	switch {
	case !ok:
		return "", false
	case spelling == "":
		return w, true
	}
	return string(spelling) + " " + w, true
}

// FlagFragmentAfterWord is "<spelling> <prefix><ShellWord(value)>"
// ("--name <entry>-local", "--name <entry>-'a b'").
func FlagFragmentAfterWord(spelling, prefix CommandWord, value string) (string, bool) {
	w, ok := ShellWord(value)
	if !ok {
		return "", false
	}
	return string(spelling) + " " + string(prefix) + w, true
}

// WithLeftOutNote is text with "; " and LeftOutNote added, unless it already
// holds the note. A site whose fragment function reports false drops the
// clause that holds the fragment and calls it.
func WithLeftOutNote(text string) string {
	switch {
	case strings.Contains(text, LeftOutNote):
		return text
	case text == "":
		return LeftOutNote
	}
	return text + "; " + LeftOutNote
}

// WithHintCommand sets the hint to c.Field() and adds LeftOutNote to the
// message once whenever c is left out. The hint of a left-out revoke is its
// list command; of any other left-out command, none.
func (e *Error) WithHintCommand(c Command) *Error {
	e.Hint = c.Field()
	if !c.OK() {
		e.Message = WithLeftOutNote(e.Message)
	}
	return e
}

// WithHintCommand is the same for a PrimaryFailure.
func (p PrimaryFailure) WithHintCommand(c Command) PrimaryFailure {
	p.Hint = c.Field()
	if !c.OK() {
		p.Message = WithLeftOutNote(p.Message)
	}
	return p
}

// TypedLine is the command line the operator typed, rebuilt for the hint of a
// refused line: the plain word rancher, then each typed word. It is file text
// with no remedy marker. Any refused word or value leaves the line out.
type TypedLine struct {
	parts   []string
	leftOut bool
}

// NewTypedLine starts the line with the word rancher.
func NewTypedLine() TypedLine { return TypedLine{parts: []string{"rancher"}} }

// Bare adds a command name, a flag spelling or "--": as typed when it is a
// plain word, else quoted, which the shell reads as the same argument. A word
// NotQuotable reports true leaves the line out.
func (l TypedLine) Bare(word string) TypedLine {
	w, ok := ShellWord(word)
	if !ok {
		l.leftOut = true
	}
	l.parts = slices.Concat(l.parts, []string{w})
	return l
}

// Value adds a typed value in single quotes.
func (l TypedLine) Value(v string) TypedLine {
	if NotQuotable(v) {
		l.leftOut = true
	}
	l.parts = slices.Concat(l.parts, []string{quoteExact(v)})
	return l
}

// Joined adds "<flag>='<value>'" for a flag typed with a joined value, or a
// bool value joined to its flag.
func (l TypedLine) Joined(flag, value string) TypedLine {
	f, ok := ShellWord(flag)
	if !ok || NotQuotable(value) {
		l.leftOut = true
	}
	l.parts = slices.Concat(l.parts, []string{f + "=" + quoteExact(value)})
	return l
}

// OK reports whether the line can be printed.
func (l TypedLine) OK() bool { return !l.leftOut }

// Field is the line for a hint, or "" when it is left out.
func (l TypedLine) Field() string {
	if l.leftOut {
		return ""
	}
	return strings.Join(l.parts, " ")
}
