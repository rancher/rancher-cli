package accepted

import (
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type advice struct{ Sequence string }

type speaker interface{ say() string }

type commandSpeaker struct{ name string }

func (c commandSpeaker) say() string { return clierr.ShellQuote(c.name) }

const suffix = " - %s"

func structField(name string) string {
	rec := advice{Sequence: clierr.ShellQuote(name)}
	return fmt.Sprintf("%q", rec.Sequence)
}

func mapLookup(name string) string {
	m := map[string]string{"k": clierr.ShellQuote(name)}
	return strconv.Quote(m["k"])
}

func functionValue(name string) string {
	f := func() string { return clierr.ShellQuote(name) }
	return strconv.Quote(f())
}

func interfaceCall(name string) string {
	var s speaker = commandSpeaker{name: name}
	return strconv.Quote(s.say())
}

func literalCalledInPlace(name string) string {
	return strconv.Quote(func() string { return clierr.ShellQuote(name) }())
}

func typeAssertion(name string) string {
	var x any = clierr.ShellQuote(name)
	return strconv.Quote(x.(string))
}

func typeSwitch(name string) string {
	var x any = clierr.ShellQuote(name)
	switch v := x.(type) {
	case string:
		return strconv.Quote(v)
	}
	return ""
}

func constantFormat(name, c string) string {
	return fmt.Sprintf("%q "+suffix, name, clierr.ShellQuote(c))
}

func plainParameter(v string) string { return fmt.Sprintf("%q", v) }

func shellQuoteOnly(name string) string { return fmt.Sprintf("%s", clierr.ShellQuote(name)) }

func unrelatedValues(name, other string) string {
	return clierr.ShellQuote(name) + strconv.Quote(other)
}

func fill(dst *string, n string) { *dst = clierr.ShellQuote(n) }

// outParameterLimit is accepted because a value written through a pointer
// parameter is not followed.
func outParameterLimit(n string) string {
	var s string
	fill(&s, n)
	return strconv.Quote(s)
}

var packageLiteral = func(s string) string { return strconv.Quote(s) }

// packageLevelLiteralLimit is accepted because the parameters of a function
// literal held by a package-level variable are not followed.
func packageLevelLiteralLimit(n string) string { return packageLiteral(clierr.ShellQuote(n)) }

func apply(f func(string) string, s string) string { return f(s) }

// literalArgumentLimit is accepted because the parameters of a function
// literal passed as an argument are not followed.
func literalArgumentLimit(n string) string {
	return apply(func(s string) string { return strconv.Quote(s) }, clierr.ShellQuote(n))
}

func fillSlice(out []string, s string) { out[0] = s }

// sliceOutParameterLimit is accepted because a value written into a slice
// parameter is not followed.
func sliceOutParameterLimit(n string) string {
	out := make([]string, 1)
	fillSlice(out, clierr.ShellQuote(n))
	return strconv.Quote(out[0])
}

func fillMap(m map[string]string, s string) { m["k"] = s }

// mapOutParameterLimit is accepted because a value written into a map
// parameter is not followed.
func mapOutParameterLimit(n string) string {
	m := map[string]string{}
	fillMap(m, clierr.ShellQuote(n))
	return strconv.Quote(m["k"])
}

func fillAppend(out *[]string, s string) { *out = append(*out, s) }

// appendOutParameterLimit is accepted because a value appended through a
// pointer to a slice parameter is not followed.
func appendOutParameterLimit(n string) string {
	var parts []string
	fillAppend(&parts, clierr.ShellQuote(n))
	return strconv.Quote(parts[0])
}

func send(ch chan string, s string) { ch <- s }

// channelOutParameterLimit is accepted because a value sent on a channel
// parameter is not followed.
func channelOutParameterLimit(n string) string {
	ch := make(chan string, 1)
	send(ch, clierr.ShellQuote(n))
	return strconv.Quote(<-ch)
}
