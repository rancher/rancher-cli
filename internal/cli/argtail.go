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
	ucli "github.com/urfave/cli/v3"
)

// The --rancher-completion-* flag namespace is reserved for the completion
// protocol. Both CaptureArgv and SanitizedArgs strip these words so they never
// reach urfave as unknown flags, regardless of where they appear in argv.
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
// later layer reads it from rawCompleting or from the argv SanitizedArgs hands
// urfave.
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

// CaptureArgv processes os.Args[1:] before urfave parses. A line that opens
// with the protocol marker is a completion request: the marker is stripped,
// rawCurrentWord is read from --rancher-completion-cur=<word>, and
// --generate-shell-completion is stripped. On any other line that flag is an
// ordinary word, such as a flag's value. The marker and the current-word field
// are stripped wherever they sit, as SanitizedArgs does, so rawArgv holds the
// words urfave parses. (The argv urfave actually receives is produced
// separately by SanitizedArgs.)
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
			continue // always stripped from urfave argv (never forward the protocol field)
		}
		out = append(out, a)
	}
	rawArgv = out
}

// SanitizedArgs strips ALL --rancher-completion-* words from os.Args so
// urfave never sees unknown flags. It strips both the proto sentinel and any
// --rancher-completion-cur=... word wherever they appear - not just at
// position 0 - as a deliberate defense-in-depth measure: if either word
// appears at an unexpected position (e.g. due to a future shell-script change
// or caller error), it is still suppressed rather than forwarded to urfave.
//
// A completion request (a line that opens with the protocol marker) must
// reach urfave as one it completes, never one it runs. urfave offers nothing
// for a word typed after a "--", and the CLI's own completion answers there
// (doubleDashSite). So for a request the words from the first "--" on are
// dropped (rawArgv keeps them, and the completion callbacks read the line
// from there) and the flag is put last.
//
// The cut is at the first literal "--", as urfave's own check is, even when
// that "--" is the value of a flag typed before the command name
// (`rancher -c -- completion <TAB>`): urfave then stops at the root, the
// callbacks read the line with that "--" as a value, and nothing is offered.
// Nothing runs.
//
// Run hands root.Run the result of SanitizedArgs(os.Args).
func SanitizedArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == protoSentinel || strings.HasPrefix(a, curPrefix) {
			continue
		}
		out = append(out, a)
	}
	if len(args) == 0 || !completionRequest(args[1:]) {
		return out
	}
	out = slices.DeleteFunc(out, func(a string) bool { return a == completionFlag })
	if len(out) > 0 {
		if i := slices.Index(out[1:], "--"); i >= 0 {
			out = out[:i+1]
		}
	}
	return append(out, completionFlag)
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
func noCompletionAfterDoubleDash(next ucli.ShellCompleteFunc) ucli.ShellCompleteFunc {
	return func(ctx context.Context, c *ucli.Command) {
		if doubleDashAt(stripRoutingPrefixByCmd(rawArgv, c), levelFlags(c)) >= 0 {
			return
		}
		next(ctx, c)
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
// the flag takes a value: its own, which include the help flag urfave adds
// (and, at the root, the version flag), and the ones its ancestors apply to
// their subcommands (every flag that is not local to its command). It is
// read from the command tree, so it follows what urfave parses, not a
// separate list.
func levelFlags(cmd *ucli.Command) map[string]bool {
	flags := map[string]bool{}
	for i, c := range cmd.Lineage() {
		for _, f := range c.Flags {
			if lf, ok := f.(ucli.LocalFlag); i > 0 && (!ok || lf.IsLocal()) {
				continue
			}
			// A command's own flag wins: urfave does not apply an ancestor's
			// flag when the command already has a flag with one of its names.
			if i > 0 && slices.ContainsFunc(f.Names(), func(name string) bool { _, ok := flags[name]; return ok }) {
				continue
			}
			df, ok := f.(ucli.DocGenerationFlag)
			takesValue := ok && df.TakesValue()
			for _, name := range f.Names() {
				flags[name] = takesValue
			}
		}
	}
	return flags
}

// onceFlagNames maps every name and alias of a flag cmd itself declares
// with urfave's OnlyOnce to the flag's name. urfave refuses a second
// instance of such a flag while parsing, before the command runs. The
// adapter sets OnlyOnce only on a leaf's own flags (runtime.ArgSpec.Once),
// from the three flag types it builds.
func onceFlagNames(cmd *ucli.Command) map[string]string {
	names := map[string]string{}
	for _, f := range cmd.Flags {
		var once bool
		switch f := f.(type) {
		case *ucli.StringFlag:
			once = f.OnlyOnce
		case *ucli.BoolFlag:
			once = f.OnlyOnce
		case *ucli.StringSliceFlag:
			once = f.OnlyOnce
		}
		if !once {
			continue
		}
		for _, name := range f.Names() {
			names[name] = f.Names()[0]
		}
	}
	return names
}

// tailWords scans tail, the words typed after a command path, under flags
// (see levelFlags). It returns the words that are neither flags nor the
// value of a flag that takes one, how many of those words precede the first
// flag that is not in flags (all of them when there is none), and whether
// the tail holds such a flag. A flag that is not in flags is rejected by
// urfave and counts as a flag without a value. A "--" ends the flags: the
// words after it are all words. So does a word whose first character after
// the dash is not a letter (`-1`, or a dash before a symbol), which urfave
// reads as a value, with the words after it. A bare "-" is a word, and the
// words after it are not counted: a line with one is refused. Each word is
// classified with the whitespace around it cut off, as urfave classifies it
// (` --apply=` is a flag, `--yes= ` has an empty value), and a word that is
// not a flag is kept as typed. It is the one scan of a tail, for the check
// that a line is rejected and for the hint built from the words typed after
// a mistyped `help`.
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

// wordClass is how urfave reads one word of a command line at a level that
// parses flags, before the level's own rules (how many words it takes, the
// commands it names) apply.
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
	// urfave classifies it in.
	trimmed string
	// For a wordFlag: name is what follows one or two dashes, up to the
	// first "=", joined whether the word holds an "=", and value what
	// follows the first "=" of the word as typed.
	name, value string
	joined      bool
}

// classifyWord reads word the way urfave classifies it. It is the one
// reading of a single word for scanTail and for parseLine, so the two
// cannot drift apart.
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
func scanLine(argv []string, c *ucli.Command) (head, tail []tailWord) {
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
// and no value (empty) or a value of only whitespace (blank), which urfave
// parses as true or rejects. lineRefusal refuses both and completion offers
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

// scanOutputFormat performs a raw-argv scan for an -o/--output value when no
// parsed flag can be trusted yet - an unmatched command or a flag-parse
// failure can fire before urfave finishes parsing this invocation's own
// --output. It recognizes all four accepted spellings (-o v, -o=v, --output
// v, --output=v) via the same canonical-name/arity primitives ParseArgTail
// and checkFlagValues already use - no second flag grammar - stops at the
// first "--" (positionals after the delimiter never select an error's
// rendering), and a trailing output flag with no following word selects no
// format (an empty result, same as never having typed one). A later
// occurrence overrides an earlier one, matching urfave's own last-flag-wins
// semantics for a repeated flag.
func scanOutputFormat(argv []string) string {
	return scanGlobalFlag(argv, "output")
}

// scanColorFlag is scanOutputFormat's twin for --color, feeding the styled
// rendering of errors raised before this invocation's flags have parsed.
func scanColorFlag(argv []string) string {
	return scanGlobalFlag(argv, "color")
}

// scanGlobalFlag is the shared raw-argv scan behind scanOutputFormat and
// scanColorFlag: it resolves each flag word against the global specs'
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
