package config

import (
	"fmt"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// NameProblem says why a name prints as another name.
type NameProblem struct {
	// Rune is the first rune the printer removes; zero when InvalidUTF8.
	Rune rune
	// InvalidUTF8 reports that the first problem is a byte that is not valid
	// UTF-8.
	InvalidUTF8 bool
}

// EntryNameProblem returns the first problem a name has, and false when it
// has none. A name has a problem when a terminal would print it as another
// name: it holds a control character (Unicode category Cc), a format
// character (Cf, which includes the invisible separator, the zero width
// characters and the direction controls), the line or paragraph separator,
// or a byte that is not valid UTF-8.
//
// The rule is clierr.IsUnsafeToDisplay, the one output.Printable applies to
// every text it prints.
func EntryNameProblem(name string) (NameProblem, bool) {
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		if r == utf8.RuneError && size == 1 {
			return NameProblem{InvalidUTF8: true}, true
		}
		if clierr.IsUnsafeToDisplay(r) {
			return NameProblem{Rune: r}, true
		}
		i += size
	}
	return NameProblem{}, false
}

// CheckNewEntryName refuses a name for a server entry that does not exist yet
// when the name prints as another name (see EntryNameProblem): a command that
// names the entry would then name another one. It does not look at the
// entries: the caller decides the name is new, because an entry already
// stored under such a name keeps working.
func CheckNewEntryName(name string) *clierr.Error {
	p, bad := EntryNameProblem(name)
	switch {
	case !bad:
		return nil
	case p.InvalidUTF8:
		return clierr.New(clierr.CodeValidation, "a new server name must be valid UTF-8; use a different name")
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("a new server name cannot hold U+%04X, which is removed when a name is printed; use a different name", p.Rune))
}
