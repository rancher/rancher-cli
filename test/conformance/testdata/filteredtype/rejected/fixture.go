// Package rejected is a fixture of the filtered-type scan: each line marked
// with a rule has a finding of that rule, and no other line has one.
package rejected

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"strings"
	"sync"
	_ "unsafe" // want 3

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

type row struct{ name clierr.Filtered }

// Row has an exported Filtered field.
type Row struct{ Name clierr.Filtered }

type nested struct{ rows []*row }

type holder struct{ b clierr.Builder }

type embeds struct{ clierr.Filtered }

// field has the shape of config status's field.
type field struct {
	key clierr.Filtered
	val any
}

func sink(any) {}

func sinks(...any) {}

func gen[T any](x T) T { return x }

func takesString(string) {}

// sinkRule asserts rule 1: Marked only at a sink.
func sinkRule(v string) runtime.Result {
	t := output.FilterValue(v)
	x := t.Marked()             // want 1
	takesString(t.Marked())     // want 1
	m := t.Marked               // want 1
	e := clierr.Filtered.Marked // want 1
	_, _ = m, e
	return runtime.Result{Text: x[:3]}
}

// exitRule asserts rule 2: WithoutMark only in a listed function, and
// NewFiltered only in the two filters.
func exitRule(v string) runtime.Result {
	t := output.FilterValue(v)
	_ = clierr.NewFiltered(v, true) // want 2
	return runtime.Result{
		Text:       clierr.Plain(t.WithoutMark()).Marked(),              // want 2
		StyledText: func(style.Style) string { return t.WithoutMark() }, // want 2
	}
}

// conversionRule asserts rule 3 in each position.
func conversionRule(v string, ch chan any, m map[any]int, b *clierr.Builder) any {
	f := output.FilterValue(v)
	_ = fmt.Sprintf("%v", f)           // want 3
	_ = fmt.Sprintf("%.3v", row{f})    // want 3
	_ = clierr.Sprintf("%.3v", row{f}) // want 3
	_ = fmt.Sprintf("%#[1]v", f)       // want 3
	fmt.Fprintf(os.Stdout, "%s", f)    // want 3
	_ = fmt.Sprintf("%v", b)           // want 3
	b.Printf("%v\n", &row{f})          // want 3
	_, _ = json.Marshal(Row{f})        // want 3
	_ = reflect.ValueOf(nested{})      // want 3
	sink(holder{})                     // want 3
	sinks("x", embeds{f})              // want 3
	args := []any{"x", f}              // want 3
	_ = fmt.Sprintf("%s %s", args...)
	var x any = []clierr.Filtered{f} // want 3
	_ = x
	ch <- row{f} // want 3
	m[row{f}]++  // want 3
	var p sync.Map
	p.Store("k", f)                          // want 3
	_ = gen(f)                               // want 3
	_ = maps.Keys(map[clierr.Filtered]int{}) // want 3
	var e any
	_ = e == any(f)    // want 3
	if e == (row{f}) { // want 3
		return nil
	}
	var r struct{ v any }
	r.v = map[string]clierr.Filtered{"k": f} // want 3
	_ = field{clierr.Plain("ok"), f}
	_ = field{clierr.Plain("k"), row{f}} // want 3
	var xs []any
	xs = append(xs, f) // want 3
	switch e {
	case f: // want 3
	}
	var k any
	for k = range map[clierr.Filtered]int{} { // want 3
	}
	_, _ = xs, k
	_ = clierr.TextOf(f, clierr.Lead("; "), f) // want 3
	go sink(row{f})                            // want 3
	defer sink(row{f})                         // want 3
	get := func() any { return f }             // want 3
	_ = get
	return row{f} // want 3
}

// readRule asserts rule 4: a read of Result.Text outside internal/runtime
// copies it whole or reads it through clierr.Plain.
func readRule(res runtime.Result) runtime.Result {
	n := len(res.Text)              // want 4
	local := res.WideText           // want 4
	_ = strings.TrimSpace(res.Text) // want 4
	_, _ = n, local
	return runtime.Result{Text: res.Text[:3]} // want 4
}

// stderrPrompter has every method of runtime.Prompter, and its Notice writes
// a notice to stderr.
type stderrPrompter struct{} // want 5

func (stderrPrompter) Prompt(context.Context, string) (string, error)       { return "", nil }
func (stderrPrompter) PromptSecret(context.Context, string) (string, error) { return "", nil }
func (stderrPrompter) ReadLine(context.Context) (string, error)             { return "", nil }
func (stderrPrompter) Notice(msg string)                                    { fmt.Fprintln(os.Stderr, msg) }
func (stderrPrompter) CanPrompt() bool                                      { return false }
func (stderrPrompter) NoInput() bool                                        { return true }

var (
	_ = sinkRule
	_ = exitRule
	_ = conversionRule
	_ = readRule
	_ = stderrPrompter{}
)

// label is a defined type over clierr.Filtered: it has none of its methods.
type label clierr.Filtered

// named has no Filtered, and its exported method returns one, which a
// template calls by name.
type named struct{}

// Name returns a Filtered.
func (named) Name() clierr.Filtered { return clierr.Plain("abc") }

// shadow has the fields of runtime.Result.
type shadow struct {
	Kind             string
	Data             any
	Warnings         []string
	OnceOnlyNotice   string
	Raw              bool
	RawBytes         []byte
	Text             string
	StyledText       func(style.Style) string
	WideText         string
	Summary          string
	ExitStatus       int
	CommittedContext *runtime.CommittedContext
}

func pair(v string) (row, error) { return row{output.FilterValue(v)}, nil }

// moreConversions asserts the shapes rule 3 refuses beside the first
// set: a defined type, a function value, a method that returns a Filtered,
// a runtime.Result, a tuple, a redeclared variable and a verb other than
// %s or %v.
func moreConversions(v string) {
	f := output.FilterValue(v)
	_ = fmt.Sprintf("%.3v", label(f))                // want 3
	var lx any = label(f)                            // want 3
	var fv any = func() clierr.Filtered { return f } // want 3
	_ = fmt.Sprint(named{})                          // want 3
	res := runtime.Result{Text: f.Marked()}
	_ = fmt.Sprint(res)        // want 3
	_, _ = json.Marshal(res)   // want 3
	_ = reflect.ValueOf(res)   // want 3
	_ = fmt.Sprint(pair(v))    // want 3
	var tx, terr any = pair(v) // want 3
	var rx any
	rx, ry := row{f}, 1                                              // want 3
	_ = clierr.Sprintf("%q", f)                                      // want 3
	_ = clierr.Sprintf("%T", f)                                      // want 3
	_, _, _, _, _, _, _ = lx, fv, tx, terr, rx, ry, shadow(res).Text // want 4
}

// projectAndWrite is a listed WithoutMark function that also writes text.
func projectAndWrite(fs field) string {
	var b clierr.Builder // want 2
	b.Write(fs.key)      // want 2
	exitText = "x"       // want 2
	return fs.key.WithoutMark()
}

// moreReads asserts a read of the any field outside its functions, a second
// caller of WithNoticeRewrite, and Marked into a local then cut.
func moreReads(fs field) {
	_ = fs.val                                                                  // want 3
	_ = ui.New(os.Stderr, nil, false, false).WithNoticeRewrite(strings.ToUpper) // want 5
	var r runtime.Result
	s := clierr.Plain("abc").Marked() // want 1
	r.Text = s
	r.Text = r.Text[:1] // want 4
	_ = r
}

var (
	_ = moreConversions
	_ = projectAndWrite
	_ = moreReads
)

// cmdLike has a method with parameters that returns a Result, as every
// command type does.
type cmdLike struct{}

// Execute returns a Result.
func (cmdLike) Execute(int) runtime.Result { return runtime.Result{} }

// runner has an exported method with no parameters that returns a Result,
// which a template calls by name.
type runner struct{}

// Run returns a Result.
func (runner) Run() runtime.Result { return runtime.Result{} }

type outer struct{ field }

type fieldCopy field

// moreHolders asserts more shapes: a type that reaches a Result only
// through a method, a function value and a method with no
// parameters that return a Result, a pointer conversion to a struct with
// the Result's fields, the any field through an embedding struct and a
// defined type and as a tag of a type switch with two types in one case,
// and a call of a WithoutMark function outside its callers.
func moreHolders(fs field) {
	var res runtime.Result
	_ = fmt.Sprintf("%.3v", struct { // want 3
		c cmdLike
		r runtime.Result
	}{})
	var fr any = func() runtime.Result { return res } // want 3
	_ = fmt.Sprint(runner{})                          // want 3
	_ = (*shadow)(&res)                               // want 4
	_ = outer{}.val                                   // want 3
	_ = fieldCopy{}.val                               // want 3
	switch v := fs.val.(type) {                       // want 3
	case clierr.Filtered, string:
		_ = v
	}
	_ = projectAndWrite(fs) // want 2
	_ = fr
}

var _ = moreHolders

var exitText string

// moreExits asserts the other ways a text becomes a string without Marked: a
// function value of a listed function, a read of StyledText, a conversion of
// field to another struct, and switches on the any field with an interface
// clause and with no clause for its slice.
func moreExits(fs field, res runtime.Result) {
	pf := projectAndWrite // want 2
	_ = pf
	_ = res.StyledText // want 4
	_ = struct {       // want 3
		key clierr.Filtered
		val any
	}(fs)
	switch v := fs.val.(type) { // want 3
	case clierr.Filtered:
	case []field:
	case interface{ WithoutMark() string }:
		_ = v
	}
	switch v := fs.val.(type) { // want 3
	case clierr.Filtered:
		_ = v
	}
}

var _ = moreExits
