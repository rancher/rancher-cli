package cli

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// The --rancher-completion-* flag namespace is reserved for the v2 completion
// protocol. Both CaptureArgv and SanitizedArgs strip these words so they never
// reach urfave as unknown flags, regardless of where they appear in argv.
const (
	protoSentinel  = "--rancher-completion-proto=2"
	curPrefix      = "--rancher-completion-cur="
	completionFlag = "--generate-shell-completion"
)

var (
	rawArgv         []string
	rawCurrentWord  string
	rawProtoV2      bool
	rawV1Completion bool // --generate-shell-completion seen in CaptureArgv (v1 path)
)

// completionActive reports whether the current invocation is a shell-completion
// request on either the v2 protocol (rawProtoV2) or urfave's built-in v1
// --generate-shell-completion flag (rawV1Completion). The guard must not fire
// on either path.
func completionActive() bool { return rawProtoV2 || rawV1Completion }

// SetRawArgv replaces the package-level argv capture (test seam).
func SetRawArgv(args []string) { rawArgv = args }

// SetCurrentWord replaces the current-word capture (test seam).
func SetCurrentWord(w string) { rawCurrentWord = w }

// GetCurrentWord returns the captured current word (v2 protocol).
func GetCurrentWord() string { return rawCurrentWord }

// completionRequest is the one place that decides an invocation is a
// completion request. args are the command-line words after the program name.
// A line is a request when it opens with the v2 protocol marker, or ends with
// the --generate-shell-completion flag that every installed script, in every
// version, puts last. Where a "--" sits on the line does not matter: the
// answer comes from the markers alone, and every later layer reads it from
// rawProtoV2 and rawV1Completion or from the argv SanitizedArgs hands urfave.
func completionRequest(args []string) (proto2, request bool) {
	proto2 = len(args) > 0 && args[0] == protoSentinel
	return proto2, proto2 || (len(args) > 0 && args[len(args)-1] == completionFlag)
}

// CaptureArgv processes os.Args[1:] before urfave parses:
//  1. Detects the v2 proto sentinel and strips it.
//  2. Scans remaining words: strips the proto sentinel wherever else it sits,
//     as SanitizedArgs does; strips --generate-shell-completion when the line
//     is a completion request (anywhere else it is an ordinary word, such as a
//     flag's value); extracts rawCurrentWord from --rancher-completion-cur=<word>
//     and drops that word; all other words go into rawArgv. (The argv urfave
//     actually receives is produced separately by SanitizedArgs.)
//  3. When v1 (sentinel absent): rawCurrentWord = ""; rawArgv = remaining words.
func CaptureArgv(args []string) {
	proto2, request := completionRequest(args)
	if proto2 {
		args = args[1:]
	}
	rawProtoV2 = proto2
	rawV1Completion = request && !proto2
	rawCurrentWord = ""

	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == completionFlag && request {
			continue
		}
		// SanitizedArgs drops the marker wherever it sits, so the line read
		// here holds the words urfave parses.
		if a == protoSentinel {
			continue
		}
		if strings.HasPrefix(a, curPrefix) {
			if proto2 {
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
// A completion request must reach urfave as one it completes, never one it
// runs. urfave completes only when the flag is last and no "--" precedes the
// last word; after one it reads the flag as one more argument and runs the
// command. So for a request the words from the first "--" on are dropped
// (rawArgv keeps them, and the completion callbacks read the line from there)
// and the flag is put last. The exception is a "--" that is the last word of a
// v1 request: an older script sends the word being completed last, and urfave
// completes `--` itself as it always has.
//
// The cut is at the first literal "--", as urfave's own check is, even when
// that "--" is the value of a flag typed before the command name
// (`rancher -c -- completion <TAB>`): urfave then stops at the root, the
// callbacks read the line with that "--" as a value, and nothing is offered.
// Nothing runs.
//
// Call as root.Run(ctx, SanitizedArgs(os.Args)).
func SanitizedArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == protoSentinel || strings.HasPrefix(a, curPrefix) {
			continue
		}
		out = append(out, a)
	}
	if len(args) == 0 {
		return out
	}
	proto2, request := completionRequest(args[1:])
	if !request {
		return out
	}
	out = slices.DeleteFunc(out, func(a string) bool { return a == completionFlag })
	if len(out) > 0 {
		for i, a := range out[1:] {
			if a == "--" && (proto2 || i < len(out)-2) {
				out = out[:i+1]
				break
			}
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
	// Partial is the value fragment being completed. Set for SiteFlagValue (always)
	// and for SitePositional when hasMarker is true (the currentWord the user typed).
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

// ParseArgTail determines the TAB completion site from the leaf's arg tail.
//
// When hasMarker is true (v2 protocol): currentWord was extracted from the
// --rancher-completion-cur= field by CaptureArgv; args contains only
// words-before-cursor. currentWord is classified FIRST:
//   - contains '=' with '-' prefix  → equals-form SiteFlagValue
//   - starts with '-' without '='   → SiteNameLike (partial flag name)
//   - otherwise (non-flag or empty) → parse args to find if the last meaningful
//     word is a value-taking flag; if so → space-form SiteFlagValue with
//     Partial=currentWord; else → SitePositional with Partial=currentWord.
//
// When hasMarker is false (v1 protocol): old last-word-as-partial heuristic runs.
func ParseArgTail(args []string, currentWord string, hasMarker bool, leaf runtime.Descriptor, globals []runtime.ArgSpec) CompletionSite {
	all := append(append([]runtime.ArgSpec{}, leaf.Flags...), globals...)

	if hasMarker {
		return classifyCurrentWordV2(args, currentWord, all)
	}

	// v1 fallback: last-word-as-partial heuristic (old installed scripts).
	positional := 0
	var typed []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if eq := strings.Index(arg, "="); strings.HasPrefix(arg, "-") && eq > 0 {
			spelling := arg[:eq]
			canonical := resolveFlagCanonical(spelling, all)
			if i == len(args)-1 {
				return CompletionSite{
					Kind:         SiteFlagValue,
					FlagName:     canonical,
					FlagSpelling: spelling,
					EqualsForm:   true,
					Partial:      arg[eq+1:],
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			canonical := resolveFlagCanonical(arg, all)
			if flagTakesValue(canonical, all) {
				if i+1 < len(args) {
					// v1: if next word is the last, treat it as the partial.
					if i+1 == len(args)-1 {
						return CompletionSite{
							Kind:         SiteFlagValue,
							FlagName:     canonical,
							FlagSpelling: arg,
							EqualsForm:   false,
							Partial:      args[i+1],
						}
					}
					i++
				} else {
					return CompletionSite{Kind: SiteFlagValue, FlagName: canonical, FlagSpelling: arg}
				}
			} else if i == len(args)-1 && !flagIsKnown(arg, all) {
				return CompletionSite{Kind: SiteNameLike}
			}
			continue
		}
		if i == len(args)-1 {
			return CompletionSite{Kind: SitePositional, Index: positional, Args: typed}
		}
		positional++
		typed = append(typed, arg)
	}
	return CompletionSite{Kind: SitePositional, Index: positional, Args: typed}
}

// doubleDashAt returns the index in tail of the "--" after which the line holds
// only arguments (see tailWords), or -1. Under v1 a "--" that is the last word is
// the word being completed, not the end of the flags: an older script sends that
// word last, and `--<TAB>` has always offered flag names.
func doubleDashAt(tail []string, flags map[string]bool) int {
	delimiter := slices.IndexFunc(scanTail(tail, flags), func(w tailWord) bool { return w.kind == endWord })
	if delimiter == len(tail)-1 && !rawProtoV2 {
		return -1
	}
	return delimiter
}

// doubleDashSite classifies the TAB position on a line whose flags a "--" has
// ended. tail is the leaf's arg tail and delimiter the index of that "--" (see
// doubleDashAt). Every word after it is an argument, so the site is always a
// positional one, even when the word being completed starts with a dash.
func doubleDashSite(tail []string, delimiter int, flags map[string]bool) CompletionSite {
	before, after := tail[:delimiter], tail[delimiter+1:]
	partial := rawCurrentWord
	if !rawProtoV2 {
		// An older script sends the word being completed last.
		partial, after = after[len(after)-1], after[:len(after)-1]
	}
	typed, _, _ := tailWords(before, flags)
	return CompletionSite{
		Kind:       SitePositional,
		Index:      len(typed) + len(after),
		Args:       append(slices.Clone(typed), after...),
		Partial:    partial,
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

// classifyCurrentWordV2 implements the v2 current-word-first classification.
// args contains words-before-cursor (currentWord already stripped by CaptureArgv).
func classifyCurrentWordV2(args []string, currentWord string, all []runtime.ArgSpec) CompletionSite {
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
				// --flag=value in argv (v1 or complete value) - not a "last flag" scenario.
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
// words after it are all words. So does a word whose byte after the dash
// is not a letter (`-1`, or a dash before a multi-byte character), which
// urfave reads as a value, with the words after it; the test is on that
// one byte, as urfave makes it. Each word is classified with the
// whitespace around it trimmed, as urfave reads it (` --apply=` is a
// flag, `--yes= ` has an empty value). It is the one scan of a tail, for the
// check that a line is rejected and for the hint built from the words typed
// after a mistyped `help`.
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
	plainWord wordKind = iota // neither a flag nor a flag's value
	flagWord                  // a flag, with its value when joined by "="
	valueWord                 // the separate value of the flag before it
	endWord                   // the "--" that ends the flags
)

// tailWord is one word of a tail as scanTail reads it.
type tailWord struct {
	// text is the word as urfave reads it: trimmed of the whitespace
	// around it, except a word that is only whitespace, the separate value
	// of a flag, and the words after the end of the flags, which urfave
	// takes as typed.
	text string
	kind wordKind
	// For a flagWord: the name typed (dashes and any joined value cut),
	// whether flags holds it, whether it takes a value, and whether its
	// value is joined to it by "=".
	name                        string
	defined, takesValue, joined bool
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
		// urfave reads every word with the whitespace around it trimmed,
		// and keeps a word that is only whitespace as typed.
		word := strings.TrimSpace(tail[i])
		if word == "" {
			plain(tail[i : i+1])
			continue
		}
		if word == "--" {
			out = append(out, tailWord{text: word, kind: endWord})
			plain(tail[i+1:])
			break
		}
		if len(word) < 2 || word[0] != '-' {
			out = append(out, tailWord{text: word, kind: plainWord})
			continue
		}
		if b := word[1]; b != '-' && !unicode.IsLetter(rune(b)) {
			plain(tail[i:])
			break
		}
		name, _, joined := strings.Cut(strings.TrimLeft(word, "-"), "=")
		takesValue, defined := flags[name]
		out = append(out, tailWord{text: word, kind: flagWord, name: name, defined: defined, takesValue: takesValue, joined: joined})
		if takesValue && !joined {
			i++
			if i < len(tail) {
				out = append(out, tailWord{text: tail[i], kind: valueWord})
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

// boolValueArgs is runtime.BoolValueArgs for leaf c run as argv (the
// command line after the program name). It reads argv with the flags c
// accepts and offers nothing unless the positional arguments it finds are
// the ones urfave parsed, so the line it returns is the command typed.
func boolValueArgs(argv []string, c *ucli.Command) (vals []runtime.BoolValue, line string, ok bool) {
	head, typed := scanLine(argv, c)
	var args []string
	parts := []string{"rancher"}
	// The routing words and global flags ahead of the tail: command names
	// as typed, flag values quoted.
	for _, w := range head {
		parts = append(parts, quoteWord(w, w.kind == plainWord))
	}
	for i, w := range typed {
		if w.kind != plainWord {
			parts = append(parts, quoteWord(w, false))
			continue
		}
		args = append(args, w.text)
		if !isBoolValue(typed, i) {
			parts = append(parts, quoteWord(w, false))
			continue
		}
		vals = append(vals, runtime.BoolValue{Index: len(args) - 1, Flag: typed[i-1].text})
		parts[len(parts)-1] += "=" + clierr.ShellQuote(w.text)
	}
	if len(vals) == 0 || !slices.Equal(args, c.Args().Slice()) {
		return nil, "", false
	}
	return vals, strings.Join(parts, " "), true
}

// emptyBoolFlags is runtime.EmptyBoolFlags for leaf c run as argv (the
// command line after the program name): the bool flags typed with "=" and
// an empty value, before or after the command name, each as typed without
// the "=". It offers nothing unless the positional arguments it finds are
// the ones urfave parsed and urfave parsed each flag it finds as set, so
// a flag it reports is one typed on the command that runs.
func emptyBoolFlags(argv []string, c *ucli.Command) (flags []string, ok bool) {
	head, typed := scanLine(argv, c)
	for _, w := range slices.Concat(head, typed) {
		if !isEmptyBool(w) {
			continue
		}
		spelling, _, _ := strings.Cut(w.text, "=")
		if !c.IsSet(strings.TrimLeft(spelling, "-")) {
			return nil, false
		}
		flags = append(flags, spelling)
	}
	var args []string
	for _, w := range typed {
		if w.kind == plainWord {
			args = append(args, w.text)
		}
	}
	if len(flags) == 0 || !slices.Equal(args, c.Args().Slice()) {
		return nil, false
	}
	return flags, true
}

// isEmptyBool reports whether w is a bool flag the command accepts, typed
// with "=" and an empty value (`--apply=`), which urfave parses as true:
// the word the command pipeline refuses, and a line holding one is one
// completion offers nothing on.
func isEmptyBool(w tailWord) bool {
	if w.kind != flagWord || !w.defined || w.takesValue || !w.joined {
		return false
	}
	_, value, _ := strings.Cut(w.text, "=")
	return value == ""
}

// isBoolValue reports whether typed[i] is a positional word typed as its
// own word directly after a bool flag that accepts it as its value: the
// word the command pipeline refuses, and a line holding one is one
// completion offers nothing on.
func isBoolValue(typed []tailWord, i int) bool {
	if typed[i].kind != plainWord || i == 0 || !isSeparateBoolFlag(typed[i-1]) {
		return false
	}
	_, err := strconv.ParseBool(typed[i].text)
	return err == nil
}

// isSeparateBoolFlag reports whether w is a bool flag the command accepts,
// typed without a joined value.
func isSeparateBoolFlag(w tailWord) bool {
	return w.kind == flagWord && w.defined && !w.takesValue && !w.joined
}

// quoteWord writes w for a command line: a flag as typed with a joined
// value quoted, "--" as typed, a word bare when bare is set (a command
// name) and quoted otherwise, like a flag's value.
func quoteWord(w tailWord, bare bool) string {
	switch {
	case w.kind == flagWord && w.joined:
		name, value, _ := strings.Cut(w.text, "=")
		return name + "=" + clierr.ShellQuote(value)
	case w.kind == flagWord, w.kind == endWord, bare:
		return w.text
	default:
		return clierr.ShellQuote(w.text)
	}
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
