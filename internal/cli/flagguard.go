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
// user is mid-typing (v2 proto) or urfave's own --generate-shell-completion
// (v1 proto) is present and dangling flags are expected, so the guard never
// fires on either path.
//
// False-positive discipline:
//   - equals-form tokens (--flag=value) are a single argv token; never examined.
//   - arbitrary strings starting with "-" (e.g. "-1.5") pass when they do not
//     match any registered flag name or alias.
//   - tokens after "--" are positional; scanning stops there.
//   - urfave reads no word after a bare "-" (only "-" once trimmed), so
//     scanning stops there too, and the command pipeline refuses the words
//     after it.
func checkFlagValues(descriptorFlags []runtime.ArgSpec, argv []string, inCompletion bool) *clierr.Error {
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
		if !strings.HasPrefix(next, "-") || strings.Contains(next, "=") {
			i++
			continue
		}
		if flagIsKnown(next, all) {
			return clierr.New(clierr.CodeValidation,
				fmt.Sprintf(
					"flag %s got flag %s as its value - empty shell variable? check that the value for %s is set, or use %s=<value> to pass a literal flag-like string",
					token, next, token, token))
		}
		// next starts with "-" but is not a registered flag (e.g. "-1.5"): valid value.
		i++
	}
	return nil
}
