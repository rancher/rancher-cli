package setters

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type holder struct{ v string }

func (h holder) with(s string) holder {
	h.v = s
	return h
}

type pointerHolder struct{ v string }

func (p *pointerHolder) set(s string) *pointerHolder {
	p.v = s
	return p
}

func ignoring(s string) holder { return holder{} }

func quotedSetter(n string) string { return fmt.Sprintf("%q", holder{}.with(clierr.ShellQuote(n))) }

func quotedPointerSetter(n string) string {
	return fmt.Sprintf("%q", (&pointerHolder{}).set(clierr.ShellQuote(n)))
}

func plainIgnoring(n string) string { return fmt.Sprintf("%q", ignoring(clierr.ShellQuote(n)).v) }
