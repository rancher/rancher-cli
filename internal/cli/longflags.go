package cli

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// longFlagSpellings returns tail with every flag word that names one of
// flags, by an alias or in a single-dash form, spelled in its long form
// (--name), with a value joined by "=" kept. The completion sources read a
// sibling flag by its long spelling only, so a line typed with -n gives
// them the same words as one typed with --namespace.
//
// Only the flag word itself changes, at its own index, and only when it
// was not already typed as --name: a flag's separate value, a positional,
// a word after "--" or after a bare "-", a flag no spec names and a word
// padded with whitespace that needs no change stay byte-for-byte. A
// rewritten word is trimmed, as the parser reads it. The result is a copy;
// tail is not modified.
func longFlagSpellings(tail []string, flags []runtime.ArgSpec) []string {
	known := make(map[string]bool, len(flags))
	for _, f := range flags {
		takesValue := f.Type != "bool"
		known[f.Name] = takesValue
		for _, a := range f.Aliases {
			known[a] = takesValue
		}
	}
	words := scanTail(tail, known)
	out := make([]string, len(tail))
	copy(out, tail)
	// scanTail gives one word per tail word; the rewrite indexes tail by
	// the word's position, so it changes nothing when the counts differ.
	if len(words) != len(tail) {
		return out
	}
	for i, w := range words {
		if w.kind != flagWord || !w.defined {
			continue
		}
		canonical := resolveFlagCanonical(w.name, flags)
		if strings.HasPrefix(w.text, "--") && w.name == canonical {
			continue
		}
		out[i] = "--" + canonical
		if w.joined {
			_, value, _ := strings.Cut(w.text, "=")
			out[i] += "=" + value
		}
	}
	return out
}
