package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// The --rancher-completion-* flag namespace is reserved for the completion
// protocol. CaptureArgv strips these arguments so they never reach the
// command-line parser as unknown flags, regardless of where they appear.
const (
	protoSentinel  = "--rancher-completion-proto=2"
	curPrefix      = "--rancher-completion-cur="
	completionFlag = "--generate-shell-completion"
)

var (
	rawArgv        []string
	rawCurrentWord string
	rawCompleting  bool
)

// completionActive reports whether the current invocation is a shell-completion
// request: a line that opens with the protocol marker.
func completionActive() bool { return rawCompleting }

// SetRawArgv replaces the package-level argv capture (test seam).
func SetRawArgv(args []string) { rawArgv = args }

// SetCurrentWord replaces the current-word capture (test seam).
func SetCurrentWord(w string) { rawCurrentWord = w }

// GetCurrentWord returns the captured current word.
func GetCurrentWord() string { return rawCurrentWord }

// completionRequest is the one place that decides an invocation is a
// completion request. args are the command-line words after the program name.
// A line is a request when it opens with the protocol marker, which every
// script that `rancher completion` writes puts first. Where a "--" sits on the
// line does not matter: the answer comes from the marker alone, and every
// later layer reads it from rawCompleting.
func completionRequest(args []string) bool {
	return len(args) > 0 && args[0] == protoSentinel
}

// OutdatedCompletionScript reports whether args, the command-line words after
// the program name, are a completion request from a script older than the
// ones this binary writes: a line that ends with --generate-shell-completion
// and does not open with the protocol marker. Such a line must never run a
// command, so main stops before building the command tree: no request, no
// file write, no keychain or credential helper call, no prompt. When it
// reports true it has written to w one line that points to the help of the
// completion command, which prints a current script. main passes stderr: a
// script reads every stdout line as a candidate, so the line would land on
// the command line.
func OutdatedCompletionScript(args []string, w io.Writer) bool {
	if len(args) == 0 || args[len(args)-1] != completionFlag || completionRequest(args) {
		return false
	}
	_, _ = fmt.Fprintln(w, "The rancher completion script in this shell is out of date. To regenerate it, see: rancher completion --help")
	return true
}

// CaptureArgv processes os.Args[1:] before the command line is parsed. A
// command line that opens with the protocol marker is a completion request:
// the marker is stripped, rawCurrentWord is read from
// --rancher-completion-cur=<word>, and --generate-shell-completion is
// stripped. On any other command line that flag is an ordinary argument,
// such as a flag's value. The marker and the current-word field are stripped
// wherever they sit. rawArgv holds the arguments the parser reads.
func CaptureArgv(args []string) {
	request := completionRequest(args)
	rawCompleting = request
	rawCurrentWord = ""

	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == protoSentinel || (a == completionFlag && request) {
			continue
		}
		if strings.HasPrefix(a, curPrefix) {
			if request {
				rawCurrentWord = a[len(curPrefix):]
			}
			continue // always stripped (never forward the protocol field)
		}
		out = append(out, a)
	}
	rawArgv = out
}

// CompletionSiteKind discriminates the three site classifications.
type CompletionSiteKind int

const (
	SiteNameLike CompletionSiteKind = iota
	SiteFlagValue
	SitePositional
)

// CompletionSite describes the TAB position relative to the leaf command.
type CompletionSite struct {
	Kind         CompletionSiteKind
	FlagName     string
	FlagSpelling string
	EqualsForm   bool
	// Partial is the value fragment being completed: the current word the user
	// typed, for SiteFlagValue and SitePositional.
	Partial string
	Index   int
	// Args holds the positional arguments typed before a SitePositional
	// site, in order (its length is Index), for a completer whose
	// candidates depend on them.
	Args []string
	// DoubleDash marks a SitePositional site typed after a "--", where every
	// word is an argument. After holds the words typed there before the one
	// being completed; Index counts them with the positionals before the "--".
	DoubleDash bool
	After      []string
}

// doubleDashAt returns the index in tail of the "--" after which the line holds
// only arguments (see tailWords), or -1.
func doubleDashAt(tail []string, flags map[string]bool) int {
	return slices.IndexFunc(scanTail(tail, flags), func(w tailWord) bool { return w.kind == endWord })
}

// doubleDashSite classifies the TAB position on a line whose flags a "--" has
// ended. tail is the leaf's arg tail and delimiter the index of that "--" (see
// doubleDashAt). Every word after it is an argument, so the site is always a
// positional one, even when the word being completed starts with a dash.
func doubleDashSite(tail []string, delimiter int, flags map[string]bool) CompletionSite {
	before, after := tail[:delimiter], tail[delimiter+1:]
	typed, _, _ := tailWords(before, flags)
	return CompletionSite{
		Kind:       SitePositional,
		Index:      len(typed) + len(after),
		Args:       append(slices.Clone(typed), after...),
		Partial:    rawCurrentWord,
		DoubleDash: true,
		After:      after,
	}
}

// noCompletionAfterDoubleDash wraps the completion of a command that has
// subcommands. After a "--" every word is an argument, so none names a command
// or a flag, and such a command offers nothing.
func noCompletionAfterDoubleDash(next func(context.Context, *Command, *Input)) func(context.Context, *Command, *Input) {
	return func(ctx context.Context, c *Command, in *Input) {
		if doubleDashAt(stripRoutingPrefixByCmd(rawArgv, c), levelFlags(c)) >= 0 {
			return
		}
		next(ctx, c, in)
	}
}

// ParseArgTail determines the TAB completion site from the leaf's arg tail.
//
// currentWord was extracted from the --rancher-completion-cur= field by
// CaptureArgv; args contains only words-before-cursor. currentWord is
// classified FIRST:
//   - contains '=' with '-' prefix  → equals-form SiteFlagValue
//   - starts with '-' without '='   → SiteNameLike (partial flag name)
//   - otherwise (non-flag or empty) → parse args to find if the last meaningful
//     word is a value-taking flag; if so → space-form SiteFlagValue with
//     Partial=currentWord; else → SitePositional with Partial=currentWord.
func ParseArgTail(args []string, currentWord string, leaf runtime.Descriptor, globals []runtime.ArgSpec) CompletionSite {
	all := append(append([]runtime.ArgSpec{}, leaf.Flags...), globals...)

	// Equals-form: e.g. "--by=n" or "--cluster=prod" in currentWord.
	if strings.HasPrefix(currentWord, "-") {
		if eq := strings.Index(currentWord, "="); eq > 0 {
			spelling := currentWord[:eq]
			canonical := resolveFlagCanonical(spelling, all)
			return CompletionSite{
				Kind:         SiteFlagValue,
				FlagName:     canonical,
				FlagSpelling: spelling,
				EqualsForm:   true,
				Partial:      currentWord[eq+1:],
			}
		}
		// Partial flag name (e.g. "--cl") → SiteNameLike.
		return CompletionSite{Kind: SiteNameLike}
	}

	// Non-flag currentWord (or empty): scan args to determine context.
	// Track the last value-taking flag word in args (if any is the last meaningful word).
	positional := 0
	var typed []string
	lastFlagSpelling := ""
	lastFlagCanonical := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if eq := strings.Index(arg, "="); eq > 0 {
				// --flag=value in argv (a complete value) - not a "last flag" scenario.
				lastFlagSpelling = ""
				lastFlagCanonical = ""
				continue
			}
			canonical := resolveFlagCanonical(arg, all)
			if flagTakesValue(canonical, all) {
				if i+1 < len(args) {
					// Consume the next word as the flag's complete value.
					i++
					lastFlagSpelling = ""
					lastFlagCanonical = ""
				} else {
					// Flag is the last word in args - currentWord is the partial value.
					lastFlagSpelling = arg
					lastFlagCanonical = canonical
				}
			} else {
				lastFlagSpelling = ""
				lastFlagCanonical = ""
			}
		} else {
			positional++
			typed = append(typed, arg)
			lastFlagSpelling = ""
			lastFlagCanonical = ""
		}
	}

	if lastFlagCanonical != "" {
		return CompletionSite{
			Kind:         SiteFlagValue,
			FlagName:     lastFlagCanonical,
			FlagSpelling: lastFlagSpelling,
			EqualsForm:   false,
			Partial:      currentWord,
		}
	}
	return CompletionSite{Kind: SitePositional, Index: positional, Partial: currentWord, Args: typed}
}

// levelFlags returns the flags cmd accepts, as name (and alias) to whether
// the flag takes a value: its own, which include the help flag (and, at the
// root, the version flag), and the ones its ancestors apply to their
// subcommands (every flag that is not local to its command). It is read from
// the command tree, not from a separate list.
func levelFlags(cmd *Command) map[string]bool {
	flags := map[string]bool{}
	for i, c := range cmd.Lineage() {
		for _, f := range c.Flags {
			if i > 0 && f.Local {
				continue
			}
			// A command's own flag wins: an ancestor's flag does not apply
			// when the command already has a flag with one of its names.
			if i > 0 && slices.ContainsFunc(f.Names(), func(name string) bool { _, ok := flags[name]; return ok }) {
				continue
			}
			for _, name := range f.Names() {
				flags[name] = f.takesValue()
			}
		}
	}
	return flags
}

// onceFlagNames maps every name and alias of a flag cmd itself declares
// with Once to the flag's name. A second instance of such a flag is refused
// while the line is parsed, before the command runs. The adapter sets Once
// only on a leaf's own flags (runtime.ArgSpec.Once).
func onceFlagNames(cmd *Command) map[string]string {
	names := map[string]string{}
	for _, f := range cmd.Flags {
		if !f.Once {
			continue
		}
		for _, name := range f.Names() {
			names[name] = f.Name
		}
	}
	return names
}

// tailWords scans tail, the arguments typed after a command path, under flags
// (see levelFlags). It returns the arguments that are neither flags nor the
// value of a flag that takes one, how many of those arguments precede the
// first flag that is not in flags (all of them when there is none), and
// whether the tail holds such a flag. A flag that is not in flags is rejected
// and counts as a flag without a value. A "--" ends the flags: the arguments
// after it are all arguments. So does an argument whose first character after
// the dash is not a letter (`-1`, or a dash before a symbol), which is read
// as a value, with the arguments after it. A bare "-" is an argument, and the
// arguments after it are not counted: a command line with one is refused.
// Each argument is classified with the whitespace around it cut off
// (` --apply=` is a flag, `--yes= ` has an empty value), and an argument that
// is not a flag is kept as typed. It is the one scan of a tail, for the check
// that a command line is rejected and for the hint built from the arguments
// typed after a mistyped `help`.
func tailWords(tail []string, flags map[string]bool) (words []string, beforeUndefined int, undefined bool) {
	beforeUndefined = -1
	for _, w := range scanTail(tail, flags) {
		switch w.kind {
		case plainWord:
			words = append(words, w.text)
		case flagWord:
			if !w.defined && beforeUndefined < 0 {
				beforeUndefined = len(words)
			}
		}
	}
	if beforeUndefined < 0 {
		return words, len(words), false
	}
	return words, beforeUndefined, true
}

// wordKind classifies one word of a tail.
type wordKind int

const (
	plainWord   wordKind = iota // neither a flag nor a flag's value
	flagWord                    // a flag, with its value when joined by "="
	valueWord                   // the separate value of the flag before it
	endWord                     // the "--" that ends the flags
	droppedWord                 // a word after a bare "-", which the line is refused for
)

// tailWord is one word of a tail as scanTail reads it.
type tailWord struct {
	// text is the word as typed, except a flag word, which has the
	// whitespace around it cut off, and a bare "-", which is "-".
	text string
	kind wordKind
	// For a flagWord: the name typed (dashes and any joined value cut),
	// whether flags holds it, whether it takes a value, and whether its
	// value is joined to it by "=".
	name                        string
	defined, takesValue, joined bool
	// For a flagWord: the value joined to it by "=", as typed.
	value string
	// For a plainWord: whether it is a bare "-". The scan reads no word
	// after it; those words are kept as droppedWord, the words the refusal
	// of a bare "-" names.
	dash bool
}

// wordClass is how one argument of a command line is read by a command that
// parses flags, before the command's own rules (how many arguments it takes,
// the commands it names) apply.
type wordClass int

const (
	wordBlank   wordClass = iota // only whitespace: an argument, as typed
	wordEnd                      // "--": every later word is an argument
	wordPlain                    // does not start with a dash: an argument, as typed
	wordDash                     // "-" on its own: an argument, and parsing goes on
	wordNotFlag                  // a dash and a character that is not a letter: this word and every later word are arguments
	wordFlag                     // a flag, with its value when joined by "="
)

// classifiedWord is one word as classifyWord reads it.
type classifiedWord struct {
	class wordClass
	// trimmed is the word with the whitespace around it cut off, the form
	// it is classified in.
	trimmed string
	// For a wordFlag: name is what follows one or two dashes, up to the
	// first "=", joined whether the word holds an "=", and value what
	// follows the first "=" of the word as typed.
	name, value string
	joined      bool
}

// classifyWord reads one argument of a command line. It is the one reading of
// a single argument for scanTail and for parseLine. The two cannot drift
// apart.
func classifyWord(word string) classifiedWord {
	trimmed := strings.TrimSpace(word)
	c := classifiedWord{trimmed: trimmed}
	switch {
	case trimmed == "":
		c.class = wordBlank
	case trimmed == "--":
		c.class = wordEnd
	case trimmed[0] != '-':
		c.class = wordPlain
	case trimmed == "-":
		c.class = wordDash
	default:
		dashes := 1
		if trimmed[1] == '-' {
			dashes = 2
		} else if r, _ := utf8.DecodeRuneInString(trimmed[1:]); !unicode.IsLetter(r) {
			c.class = wordNotFlag
			return c
		}
		c.class = wordFlag
		var found bool
		c.name, _, found = strings.Cut(trimmed[dashes:], "=")
		if found {
			_, c.value, _ = strings.Cut(word, "=")
		}
		c.joined = found
	}
	return c
}

// scanTail classifies every word of tail under flags, by the rules
// tailWords states; tailWords and scanLine read its result.
func scanTail(tail []string, flags map[string]bool) []tailWord {
	out := make([]tailWord, 0, len(tail))
	plain := func(rest []string) {
		for _, w := range rest {
			out = append(out, tailWord{text: w, kind: plainWord})
		}
	}
	for i := 0; i < len(tail); i++ {
		c := classifyWord(tail[i])
		switch c.class {
		case wordBlank, wordPlain:
			plain(tail[i : i+1])
		case wordEnd:
			out = append(out, tailWord{text: c.trimmed, kind: endWord})
			plain(tail[i+1:])
			return out
		case wordDash:
			out = append(out, tailWord{text: c.trimmed, kind: plainWord, dash: true})
			for _, w := range tail[i+1:] {
				out = append(out, tailWord{text: w, kind: droppedWord})
			}
			return out
		case wordNotFlag:
			plain(tail[i:])
			return out
		case wordFlag:
			takesValue, defined := flags[c.name]
			out = append(out, tailWord{text: c.trimmed, kind: flagWord, name: c.name, defined: defined, takesValue: takesValue, joined: c.joined, value: c.value})
			if takesValue && !c.joined {
				i++
				if i < len(tail) {
					out = append(out, tailWord{text: tail[i], kind: valueWord})
				}
			}
		}
	}
	return out
}

// scanLine reads argv, the command line after the program name, for c:
// the words ahead of c's tail (the command names and the flags typed
// between them) and the tail, each read by scanTail under the flags c
// accepts. It is the one reading of a line for the checks the command
// pipeline makes on it and for completion's decision that the line is
// rejected.
func scanLine(argv []string, c *Command) (head, tail []tailWord) {
	rest := stripRoutingPrefixByCmd(argv, c)
	levels := levelFlags(c)
	return scanTail(argv[:len(argv)-len(rest)], levels), scanTail(rest, levels)
}

// hasBareDash reports whether words, read by scanTail, hold a bare "-": a
// line with a word after one is refused by lineRefusal, so completion offers
// nothing on a line that holds one.
func hasBareDash(words []tailWord) bool {
	return slices.ContainsFunc(words, func(w tailWord) bool { return w.dash })
}

// boolValueKind reads w as a bool flag the command accepts, typed with "="
// and no value (empty) or a value of only whitespace (blank), which is
// parsed as true or rejected. lineRefusal refuses both and completion offers
// nothing on a line holding either.
func boolValueKind(w tailWord) (blank, empty bool) {
	if w.kind != flagWord || !w.defined || w.takesValue || !w.joined {
		return false, false
	}
	return w.value != "" && strings.TrimSpace(w.value) == "", w.value == ""
}

// isEmptyBool reports whether w is a bool flag the command accepts, typed
// with "=" and an empty value or one of only whitespace (`--apply=`): a
// word lineRefusal refuses, and a line holding one is one completion offers
// nothing on.
func isEmptyBool(w tailWord) bool {
	blank, empty := boolValueKind(w)
	return blank || empty
}

// isBoolValueAfter reports whether w is a positional word that reads as a
// bool, typed as its own word directly after prev, a bool flag the command
// accepts typed without a value: a bool flag takes its value only joined
// with "=", so the word is an argument.
func isBoolValueAfter(prev, w tailWord) bool {
	if w.kind != plainWord || !isSeparateBoolFlag(prev) {
		return false
	}
	_, err := strconv.ParseBool(strings.TrimSpace(w.text))
	return err == nil
}

// isBoolValue reports whether typed[i] is a word isBoolValueAfter reads
// after typed[i-1]: a word lineRefusal refuses, and a line holding one is
// one completion offers nothing on.
func isBoolValue(typed []tailWord, i int) bool {
	return i > 0 && isBoolValueAfter(typed[i-1], typed[i])
}

// isSeparateBoolFlag reports whether w is a bool flag the command accepts,
// typed without a joined value.
func isSeparateBoolFlag(w tailWord) bool {
	return w.kind == flagWord && w.defined && !w.takesValue && !w.joined
}

// scanOutputFormat scans the captured command line for an -o/--output value
// when no parsed flag can be trusted yet - an unknown command or a flag error
// can fire before this invocation's own
// --output. It recognizes all four accepted spellings (-o v, -o=v, --output
// v, --output=v) via the same canonical-name/arity primitives ParseArgTail
// and checkFlagValues already use - no second flag grammar - stops at the
// first "--" (positionals after the delimiter never select an error's
// rendering), and a trailing output flag with no following argument selects no
// format (an empty result, same as never having typed one). A later
// occurrence overrides an earlier one, as the last value wins for a repeated
// flag.
func scanOutputFormat(argv []string) string {
	return scanGlobalFlag(argv, "output")
}

// scanColorFlag is scanOutputFormat's twin for --color, feeding the styled
// rendering of errors raised before this invocation's flags have parsed.
func scanColorFlag(argv []string) string {
	return scanGlobalFlag(argv, "color")
}

// scanGlobalFlag is the scan of the captured command line behind
// scanOutputFormat and scanColorFlag: it resolves each flag word against the global specs'
// canonical names, honors the separate and equals value spellings for every
// alias the flag's spec defines (four spellings for --output with its -o
// short form, two for --color, which has none) with last-flag-wins, stops
// at the first "--", and yields "" for a trailing flag with no value.
func scanGlobalFlag(argv []string, canonical string) string {
	value := ""
	for i := 0; i < len(argv); i++ {
		word := argv[i]
		if word == "--" {
			break
		}
		if !strings.HasPrefix(word, "-") {
			continue
		}
		if eq := strings.Index(word, "="); eq > 0 {
			if resolveFlagCanonical(word[:eq], globalArgSpecs) == canonical {
				value = word[eq+1:]
			}
			continue
		}
		if resolveFlagCanonical(word, globalArgSpecs) != canonical {
			continue
		}
		if i+1 < len(argv) && argv[i+1] != "--" {
			value = argv[i+1]
			i++
		} else {
			value = ""
		}
	}
	return value
}

func resolveFlagCanonical(s string, flags []runtime.ArgSpec) string {
	name := strings.TrimLeft(s, "-")
	if eq := strings.Index(name, "="); eq >= 0 {
		name = name[:eq]
	}
	for _, f := range flags {
		if f.Name == name {
			return f.Name
		}
		if slices.Contains(f.Aliases, name) {
			return f.Name
		}
	}
	return name
}

func flagTakesValue(canonical string, flags []runtime.ArgSpec) bool {
	for _, f := range flags {
		if f.Name == canonical {
			return f.Type != "bool"
		}
	}
	return false
}

func flagIsKnown(s string, flags []runtime.ArgSpec) bool {
	name := strings.TrimLeft(s, "-")
	for _, f := range flags {
		if f.Name == name {
			return true
		}
		if slices.Contains(f.Aliases, name) {
			return true
		}
	}
	return false
}

var globalArgSpecs = []runtime.ArgSpec{
	{Name: "dry-run", Type: "bool"},
	{Name: "apply", Type: "bool"},
	{Name: "yes", Type: "bool"},
	{Name: "allow-deprecated", Type: "bool"},
	{Name: "no-input", Type: "bool"},
	{Name: "output", Type: "enum", Enum: runtime.OutputFormatCompletions, Aliases: []string{"o"}},
	{Name: "color", Type: "enum", Enum: runtime.ColorModes},
	{Name: "project", Type: "string"},
	{Name: "read-only", Type: "bool"},
	{Name: "debug", Type: "bool"},
	{Name: "config", Type: "string", Aliases: []string{"c"}},
}
