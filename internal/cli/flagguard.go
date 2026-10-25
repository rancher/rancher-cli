package cli

import (
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// checkFlagValues scans argv for the space-form "--flag next-flag" pattern
// where a value-taking flag's next token is itself a registered flag spelling -
// the hallmark of an unset shell variable between two flags (e.g.
// --cloud-credential $DEMO_CC --k8s-version with DEMO_CC unset). Returns a
// VALIDATION error naming both flags on the first match; nil when clean.
//
// inCompletion must be completionActive() at the call site: when true, the
// user is mid-typing and dangling flags are expected, so the guard never
// fires.
//
// secretInput is whether the command takes a secret input (Descriptor.
// SecretInput). On such a command a next word written with "=" whose part
// before the first "=" is a registered flag spelling
// (--flag next-flag=value) is refused too: it may carry a secret, and the
// message names the two flag spellings only, never what follows the "=".
//
// False-positive discipline:
//   - equals-form tokens (--flag=value) are a single argv token; never examined,
//     so --flag=--other=value passes on every command.
//   - arbitrary strings starting with "-" (e.g. "-1.5", "-x=1") pass when they
//     do not match any registered flag name or alias; a next word with "="
//     passes on a command that takes no secret input, whatever it starts with.
//   - the cost on a command that takes a secret input: a literal value that
//     starts with a registered flag spelling and "=" (--from-literal -c=v)
//     is refused, and the message names the way out, the equals form
//     (--from-literal=-c=v), which passes.
//   - tokens after "--" are positional; scanning stops there.
//   - a bare "-" (only "-" once trimmed) ends the command line: a line with
//     words after it is refused before the command runs, so scanning stops
//     there too.
func checkFlagValues(descriptorFlags []runtime.ArgSpec, argv []string, inCompletion, secretInput bool) *clierr.Error {
	if inCompletion {
		return nil
	}
	all := append(append([]runtime.ArgSpec{}, descriptorFlags...), globalArgSpecs...)
	for i := 0; i+1 < len(argv); i++ {
		token := argv[i]
		if token == "--" || strings.TrimSpace(token) == "-" {
			break
		}
		// Skip non-flag tokens and equals-form flags (already value-bound).
		if !strings.HasPrefix(token, "-") || strings.Contains(token, "=") {
			continue
		}
		canonical := resolveFlagCanonical(token, all)
		if !flagTakesValue(canonical, all) {
			continue
		}
		next := argv[i+1]
		// Normal value or equals-form literal: advance past the consumed value.
		if !strings.HasPrefix(next, "-") {
			i++
			continue
		}
		if spelling, _, hasEquals := strings.Cut(next, "="); hasEquals {
			if secretInput && flagIsKnown(spelling, all) {
				return flagAsValueError(token, spelling)
			}
			i++
			continue
		}
		if flagIsKnown(next, all) {
			return flagAsValueError(token, next)
		}
		// next starts with "-" but is not a registered flag (e.g. "-1.5"): valid value.
		i++
	}
	return nil
}

// flagAsValueError is the refusal of the flag token that got the flag
// spelling next as its value.
func flagAsValueError(token, next string) *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf(
			"flag %s got flag %s as its value - empty shell variable? check that the value for %s is set, or use %s=<value> to pass a literal flag-like string",
			token, next, token, token))
}
