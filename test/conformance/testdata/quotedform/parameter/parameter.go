package parameter

import (
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func show(v string) string { return strconv.Quote(v) }

func caller(name string) string { return show(clierr.ShellQuote(name)) }

func closure(n string) string {
	quote := func(s string) string { return strconv.Quote(s) }
	return quote(clierr.ShellQuote(n))
}
