package moduleargs

import (
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func ignores(s string) string { return "plain" }

func returns(s string) string { return s }

func quotedIgnoring(name string) string {
	return strconv.Quote(ignores(clierr.ShellQuote(name)))
}

func quotedReturning(name string) string {
	return strconv.Quote(returns(clierr.ShellQuote(name)))
}
