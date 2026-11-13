package sinkforms

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func mapAll(in []string, f func(string) string) []string {
	var out []string
	for _, s := range in {
		out = append(out, f(s))
	}
	return out
}

func toASCII(name string) string { return strconv.QuoteToASCII(clierr.ShellQuote(name)) }

func toGraphic(name string) string { return strconv.QuoteToGraphic(clierr.ShellQuote(name)) }

func appendQuote(name string) []byte { return strconv.AppendQuote(nil, clierr.ShellQuote(name)) }

func appendASCII(name string) []byte { return strconv.AppendQuoteToASCII(nil, clierr.ShellQuote(name)) }

func appendGraphic(name string) []byte {
	return strconv.AppendQuoteToGraphic(nil, clierr.ShellQuote(name))
}

func sharpV(name string) string { return fmt.Sprintf("%#v", clierr.ShellQuote(name)) }

func plusSharpV(name string) string { return fmt.Sprintf("%+#v", clierr.ShellQuote(name)) }

func indexedSharpV(name string) string {
	return fmt.Sprintf("%#[2]v %[1]s", "x", clierr.ShellQuote(name))
}

func asValue() string {
	parts := []string{clierr.Rancher + " get users"}
	return strings.Join(mapAll(parts, strconv.Quote), " ")
}

func assignedValue(name string) string {
	q := strconv.QuoteToASCII
	return q(clierr.ShellQuote(name))
}

func appendsToCommand(name string) []byte {
	return strconv.AppendQuote([]byte(clierr.ShellQuote(name)), name)
}

func plainSharpV(name string) string { return fmt.Sprintf("%#v", name) }

func commandToPlainVerbs(name string) string {
	return fmt.Sprintf("%v %s %#v", clierr.ShellQuote(name), clierr.Rancher, name)
}

func runes(r rune) string { return strconv.QuoteRune(r) + strconv.QuoteRuneToASCII(r) }

func plainASCII(name string) string { return strconv.QuoteToASCII(name) }

var packageQuote = strconv.Quote

func viaPackageVar(name string) string { return packageQuote(clierr.ShellQuote(name)) }

func quoter() func(string) string { return strconv.Quote }

func apply(f func(string) string, s string) string { return f(s) }

func passed(name string) string { return apply(strconv.Quote, clierr.ShellQuote(name)) }

func applyToCommand(f func(string) string, n string) string { return f(clierr.ShellQuote(n)) }

func passedToHelper(name string) string { return applyToCommand(strconv.Quote, name) }
