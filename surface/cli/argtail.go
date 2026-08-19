package cli

import (
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// The --rancher-completion-* token namespace is reserved for the v2 completion
// protocol. Both CaptureArgv and SanitizedArgs strip these tokens so they never
// reach urfave as unknown flags, regardless of where they appear in argv.
const (
	protoSentinel = "--rancher-completion-proto=2"
	curPrefix     = "--rancher-completion-cur="
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

// CaptureArgv processes os.Args[1:] before urfave parses (round-5 encoding):
//  1. Detects the v2 proto sentinel and strips it.
//  2. Scans remaining tokens: strips --generate-shell-completion; extracts
//     rawCurrentWord from --rancher-completion-cur=<word> and drops that token;
//     all other tokens go into rawArgv. (The argv urfave actually receives is
//     produced separately by SanitizedArgs.)
//  3. When v1 (sentinel absent): rawCurrentWord = ""; rawArgv = remaining tokens.
func CaptureArgv(args []string) {
	proto2 := len(args) > 0 && args[0] == protoSentinel
	if proto2 {
		args = args[1:]
	}
	rawProtoV2 = proto2
	rawV1Completion = false
	rawCurrentWord = ""

	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--generate-shell-completion" {
			rawV1Completion = true
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

// SanitizedArgs strips ALL --rancher-completion-* tokens from os.Args so
// urfave never sees unknown flags. It strips both the proto sentinel and any
// --rancher-completion-cur=... token wherever they appear — not just at
// position 0 — as a deliberate defense-in-depth measure: if either token
// appears at an unexpected position (e.g. due to a future shell-script change
// or caller error), it is still suppressed rather than forwarded to urfave.
// Call as root.Run(ctx, SanitizedArgs(os.Args)).
func SanitizedArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == protoSentinel || strings.HasPrefix(a, curPrefix) {
			continue
		}
		out = append(out, a)
	}
	return out
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
}

// ParseArgTail determines the TAB completion site from the leaf's arg tail.
//
// When hasMarker is true (v2 protocol): currentWord was extracted from the
// --rancher-completion-cur= field by CaptureArgv; args contains only
// words-before-cursor. currentWord is classified FIRST (round-5 finding 1):
//   - contains '=' with '-' prefix  → equals-form SiteFlagValue
//   - starts with '-' without '='   → SiteNameLike (partial flag name)
//   - otherwise (non-flag or empty) → parse args to find if the last meaningful
//     token is a value-taking flag; if so → space-form SiteFlagValue with
//     Partial=currentWord; else → SitePositional with Partial=currentWord.
//
// When hasMarker is false (v1 protocol): old last-token-as-partial heuristic runs.
func ParseArgTail(args []string, currentWord string, hasMarker bool, leaf runtime.Descriptor, globals []runtime.ArgSpec) CompletionSite {
	all := append(append([]runtime.ArgSpec{}, leaf.Flags...), globals...)

	if hasMarker {
		return classifyCurrentWordV2(args, currentWord, all)
	}

	// v1 fallback: last-token-as-partial heuristic (old installed scripts).
	positional := 0
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
					// v1: if next token is the last, treat it as the partial.
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
			return CompletionSite{Kind: SitePositional, Index: positional}
		}
		positional++
	}
	return CompletionSite{Kind: SitePositional, Index: positional}
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
	// Track the last value-taking flag token in args (if any is the last meaningful token).
	positional := 0
	lastFlagSpelling := ""
	lastFlagCanonical := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if eq := strings.Index(arg, "="); eq > 0 {
				// --flag=value in argv (v1 or complete value) — not a "last flag" scenario.
				lastFlagSpelling = ""
				lastFlagCanonical = ""
				continue
			}
			canonical := resolveFlagCanonical(arg, all)
			if flagTakesValue(canonical, all) {
				if i+1 < len(args) {
					// Consume the next token as the flag's complete value.
					i++
					lastFlagSpelling = ""
					lastFlagCanonical = ""
				} else {
					// Flag is the last token in args — currentWord is the partial value.
					lastFlagSpelling = arg
					lastFlagCanonical = canonical
				}
			} else {
				lastFlagSpelling = ""
				lastFlagCanonical = ""
			}
		} else {
			positional++
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
	return CompletionSite{Kind: SitePositional, Index: positional, Partial: currentWord}
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
	{Name: "project", Type: "string"},
	{Name: "read-only", Type: "bool"},
	{Name: "debug", Type: "bool"},
	{Name: "config", Type: "string", Aliases: []string{"c"}},
}
