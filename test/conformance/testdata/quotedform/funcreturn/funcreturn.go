package funcreturn

import (
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type holder struct{ name string }

func (t holder) line() string { return clierr.ShellQuote(t.name) }

func line(n string) string { return "x " + clierr.ShellQuote(n) }

func named(n string) (out string) {
	out = clierr.ShellQuote(n)
	return
}

func quotedFunction(name string) string { return strconv.Quote(line(name)) }

func quotedNamedResult(name string) string { return strconv.Quote(named(name)) }

func quotedMethod() string { return strconv.Quote(holder{name: "x"}.line()) }
