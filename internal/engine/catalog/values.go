package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// hiddenText is what every printed form of Values shows, and what the
// server-text filter puts in place of a value.
const hiddenText = "<hidden>"

// flagValues is the repeatable flag that names a values file.
const flagValues = "values"

// Values holds the chart values a command sends: the --values files merged
// in order, or those with the values app install adds (compose). Its content
// is read only by the request body builder, the value composition, the
// preview's listing and the server-text filter. Every printing and
// encoding method returns <hidden>. The methods have value receivers, so a
// Values inside another value prints <hidden> too; on a nil *Values the
// method wrappers Go generates refuse the call before any memory is read,
// and fmt prints <nil>.
type Values struct {
	tree map[string]any
}

func (Values) String() string               { return hiddenText }
func (Values) GoString() string             { return hiddenText }
func (Values) Format(f fmt.State, _ rune)   { writeHidden(f) }
func (Values) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(hiddenText)), nil }
func (Values) MarshalYAML() (any, error)    { return hiddenText, nil }
func (Values) MarshalText() ([]byte, error) { return []byte(hiddenText), nil }

// writeHidden writes <hidden> for a Format method. fmt.Formatter returns
// no error: a failed write is the caller's writer failing, which fmt
// reports to its own caller through the writer, and nothing is left to do
// here.
func writeHidden(f fmt.State) {
	if _, err := io.WriteString(f, hiddenText); err != nil {
		return
	}
}

// requestValues is the values field of a request body: its JSON is the
// values themselves, the one encoding that writes them, used only when the
// body is sent. Every other printing method returns <hidden>. A body holds
// it by pointer, and every method is safe on a nil pointer.
type requestValues struct{ v Values }

func (*requestValues) String() string             { return hiddenText }
func (*requestValues) GoString() string           { return hiddenText }
func (*requestValues) Format(f fmt.State, _ rune) { writeHidden(f) }
func (*requestValues) MarshalYAML() (any, error)  { return hiddenText, nil }
func (r *requestValues) MarshalJSON() ([]byte, error) {
	if r == nil || r.v.tree == nil {
		return []byte("null"), nil
	}
	return json.Marshal(r.v.tree)
}

// request is the values field of a request body: nil, which leaves the
// field out, when no value is sent.
func (v Values) request() *requestValues {
	if len(v.tree) == 0 {
		return nil
	}
	return &requestValues{v: v}
}

// ReadValues reads the --values files in the order typed and merges them,
// each over the ones before: maps merge key by key, any other value
// replaces, and null is kept as a value to send. "-" reads stdin, at most
// once, and is refused when stdin is a terminal. Every refusal is
// VALIDATION and names the file as typed and at most a line number: no
// error text of the YAML decoders, which can quote the file's content.
func ReadValues(src engine.FileSource, paths []string, stdinTerminal bool) (Values, error) {
	merged := map[string]any{}
	stdin := false
	for _, p := range paths {
		if p == "-" {
			if stdin {
				return Values{}, clierr.New(clierr.CodeValidation, "--values - is given more than once; stdin can be read once")
			}
			stdin = true
			if stdinTerminal {
				return Values{}, clierr.New(clierr.CodeValidation, "--values -: stdin is a terminal; pipe the values or name a file")
			}
		}
		b, err := src.Read(p)
		if err != nil {
			return Values{}, clierr.New(clierr.CodeValidation, "cannot read the --values file "+fileName(p))
		}
		m, err := decodeValuesFile(p, b)
		if err != nil {
			return Values{}, err
		}
		mergeValues(merged, m)
	}
	return Values{tree: merged}, nil
}

// fileName is the --values argument as typed, quoted for a message; "-" is
// named as stdin.
func fileName(p string) string {
	if p == "-" {
		return "- (stdin)"
	}
	return clierr.ShellQuote(p)
}

// yamlLine reads the line number a YAML decoder's syntax error starts with.
var yamlLine = regexp.MustCompile(`^yaml: line (\d+):`)

// decodeValuesFile decodes one values file: one YAML document whose top
// level is a mapping. A UTF-8 byte order mark at the start is dropped; CRLF
// line endings count as one line break.
func decodeValuesFile(p string, b []byte) (map[string]any, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	refuse := func(what string, line int) error {
		msg := "--values file " + fileName(p) + ": " + what
		if line > 0 {
			msg = "--values file " + fileName(p) + ", line " + strconv.Itoa(line) + ": " + what
		}
		return clierr.New(clierr.CodeValidation, msg)
	}
	dec := yamlv3.NewDecoder(bytes.NewReader(b))
	var docs []*yamlv3.Node
	for {
		var n yamlv3.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			line := 0
			if m := yamlLine.FindStringSubmatch(err.Error()); m != nil {
				line, _ = strconv.Atoi(m[1])
			}
			return nil, refuse("not valid YAML", line)
		}
		docs = append(docs, &n)
	}
	switch {
	case len(docs) == 0:
		return nil, refuse("holds no YAML document; it holds one document whose top level is a mapping", 0)
	case len(docs) > 1:
		return nil, refuse(fmt.Sprintf("holds %d YAML documents; it holds one document whose top level is a mapping", len(docs)), docs[1].Line)
	}
	top := docs[0]
	if top.Kind == yamlv3.DocumentNode && len(top.Content) == 1 {
		top = top.Content[0]
	}
	if top.Kind != yamlv3.MappingNode {
		return nil, refuse("the top level is not a mapping", top.Line)
	}
	// yaml.v3 refuses a repeated key and a scalar that does not fit its
	// tag; sigs.k8s.io/yaml then gives the JSON form the request sends.
	var check any
	if err := top.Decode(&check); err != nil {
		return nil, refuse("the YAML could not be decoded", 0)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, refuse("the YAML could not be decoded", 0)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// mergeValues merges src over dst in place: a key whose values are both
// maps merges recursively; any other value of src replaces dst's.
func mergeValues(dst, src map[string]any) {
	for k, sv := range src {
		sm, sIsMap := sv.(map[string]any)
		dm, dIsMap := dst[k].(map[string]any)
		if sIsMap && dIsMap {
			mergeValues(dm, sm)
			continue
		}
		dst[k] = sv
	}
}

// paths lists the path of every leaf of the values, sorted: a value that is
// not a map, or an empty map. A key is written after a dot, or in brackets
// as a JSON string when it is empty or holds a dot, a bracket or a quote:
// values.a.b, values["a.b"].c.
func (v Values) paths() []string {
	var out []string
	v.leaves(func(path string, _ any) { out = append(out, path) })
	return out
}

// leaves calls fn for every leaf of the values in the order paths lists
// them, with its path and value.
func (v Values) leaves(fn func(path string, value any)) {
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			p := prefix + keySegment(k)
			if sub, ok := m[k].(map[string]any); ok && len(sub) > 0 {
				walk(p, sub)
				continue
			}
			fn(p, m[k])
		}
	}
	walk("values", v.tree)
}

// valuePath is the path of one value in a values tree, one key per element.
type valuePath []string

// String writes the path as paths does: values.global.cattle.clusterId.
func (p valuePath) String() string {
	var b strings.Builder
	b.WriteString("values")
	for _, k := range p {
		b.WriteString(keySegment(k))
	}
	return b.String()
}

// pathState is what a values tree holds at a path.
type pathState int

const (
	// pathAbsent: the tree holds no value at the path, and every parent on
	// it that the tree holds is a mapping.
	pathAbsent pathState = iota
	// pathPresent: the tree holds a value at the path, null included.
	pathPresent
	// pathBlocked: a parent on the path holds a value that is not a
	// mapping (null, a scalar or a list), so nothing can be set below it.
	pathBlocked
)

// lookup reads tree at path: the value and the state, and for a blocked
// path the path of the parent that blocks it.
func lookup(tree map[string]any, path valuePath) (any, pathState, valuePath) {
	m := tree
	for i, k := range path {
		x, ok := m[k]
		if !ok {
			return nil, pathAbsent, nil
		}
		if i == len(path)-1 {
			return x, pathPresent, nil
		}
		sub, ok := x.(map[string]any)
		if !ok {
			return nil, pathBlocked, path[:i+1]
		}
		m = sub
	}
	return nil, pathAbsent, nil
}

// state reports what the files' values hold at path, and for a blocked
// path the parent that blocks it.
func (v Values) state(path valuePath) (pathState, valuePath) {
	_, st, parent := lookup(v.tree, path)
	return st, parent
}

// mergedString is the string at path in the files' values merged over
// defaults, the chart's default values: the files' value when they hold
// one at path, else the default's. Anything but a string is "".
func (v Values) mergedString(defaults map[string]any, path valuePath) string {
	x, st, _ := lookup(v.tree, path)
	switch st {
	case pathBlocked:
		return ""
	case pathAbsent:
		x, _, _ = lookup(defaults, path)
	}
	s, _ := x.(string)
	return s
}

// addedValue is a value the CLI adds to the files' values at path. It is
// set only where the files hold no value on its path; with replace it also
// replaces a value the files hold at path itself. Nothing is set below a
// parent the files set to anything but a mapping.
type addedValue struct {
	path    valuePath
	value   any
	replace bool
}

// compose returns the values a request sends: the files' values with each
// added value set by its rule, missing parents created as mappings, and the
// value at each path of drop removed. added lists, in the order paths lists
// them, the leaves the CLI set to a value other than the files' own;
// dropped lists the paths of drop the files held a value at. v itself is
// not changed, so the filter and paths keep covering the files' values.
func (v Values) compose(add []addedValue, drop []valuePath) (sent Values, added, dropped []string) {
	tree, _ := deepCopy(v.tree).(map[string]any)
	if tree == nil {
		tree = map[string]any{}
	}
	for _, path := range drop {
		_, st, _ := lookup(tree, path)
		if st != pathPresent {
			continue
		}
		parent, _, _ := lookup(tree, path[:len(path)-1])
		if len(path) == 1 {
			parent = tree
		}
		delete(parent.(map[string]any), path[len(path)-1])
		dropped = append(dropped, path.String())
	}
	set := map[string]bool{}
	for _, a := range add {
		old, st, _ := lookup(tree, a.path)
		if st == pathBlocked || (st == pathPresent && !a.replace) {
			continue
		}
		if st == pathPresent && reflect.DeepEqual(old, a.value) {
			continue
		}
		m := tree
		for _, k := range a.path[:len(a.path)-1] {
			sub, ok := m[k].(map[string]any)
			if !ok {
				sub = map[string]any{}
				m[k] = sub
			}
			m = sub
		}
		m[a.path[len(a.path)-1]] = a.value
		set[a.path.String()] = true
	}
	sent = Values{tree: tree}
	sent.leaves(func(path string, _ any) {
		if set[path] {
			added = append(added, path)
		}
	})
	return sent, added, dropped
}

// deepCopy copies the maps and lists of a decoded values tree.
func deepCopy(x any) any {
	switch t := x.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	}
	return x
}

// previewEntries lists the leaves of v, the values a request sends, for the
// preview: an added leaf shows its value, a leaf at one of shown shows its
// value when it is a string, and every other leaf shows <hidden>.
func (v Values) previewEntries(added []string, shown []valuePath) []safety.DiffEntry {
	show := map[string]bool{}
	for _, p := range added {
		show[p] = true
	}
	shownString := map[string]bool{}
	for _, p := range shown {
		shownString[p.String()] = true
	}
	var out []safety.DiffEntry
	v.leaves(func(path string, value any) {
		_, isString := value.(string)
		if show[path] || (shownString[path] && isString) {
			out = append(out, safety.DiffEntry{Path: path, To: value})
			return
		}
		out = append(out, safety.DiffEntry{Path: path, To: hiddenText})
	})
	return out
}

// keySegment writes one key of a values path.
func keySegment(k string) string {
	if k == "" || strings.ContainsAny(k, ".[]\"'") {
		b, _ := json.Marshal(k)
		return "[" + string(b) + "]"
	}
	return "." + k
}

// minFilteredLength is the shortest value, in characters, the server-text
// filter replaces: a shorter one would also replace ordinary words.
const minFilteredLength = 4

// Filter returns text with every value of v that could be a secret replaced
// by <hidden>: each string leaf of 4 characters or more, each line of 4
// characters or more of a multi-line string, and the JSON-escaped form of
// each. Every occurrence of each of them in text as given is found,
// occurrences that overlap included, and each run of text they cover
// (occurrences that overlap or touch form one run) becomes one <hidden>, so
// the result does not depend on the order of the values. It does not cover
// numbers, booleans, shorter strings or a value the chart transforms. Every
// path of the catalog commands that prints server text after the values
// were sent runs it.
func (v Values) Filter(text string) string { return v.filterFunc()(text) }

// filterFunc returns Filter with the list of values to hide built once, for
// a caller that filters many strings. A replacer that deletes every value
// only finds whether one occurs: it replaces the match that starts first
// and skips a value that overlaps it, so it cannot hide both. When one
// occurs, each value marks the bytes its occurrences cover in one array,
// each byte at most once (only the bytes past the end of its previous
// occurrence): marking every byte of every occurrence is too slow on
// repetitive text, where occurrences overlap many times.
func (v Values) filterFunc() func(string) string {
	needles := v.needles()
	if len(needles) == 0 {
		return func(text string) string { return text }
	}
	pairs := make([]string, 0, 2*len(needles))
	for _, n := range needles {
		pairs = append(pairs, n, "")
	}
	detect := strings.NewReplacer(pairs...)
	return func(text string) string {
		if len(detect.Replace(text)) == len(text) {
			return text
		}
		covered := make([]bool, len(text))
		for _, n := range needles {
			end := 0
			for from := 0; ; {
				i := strings.Index(text[from:], n)
				if i < 0 {
					break
				}
				start := from + i
				for j := max(start, end); j < start+len(n); j++ {
					covered[j] = true
				}
				end = start + len(n)
				from = start + 1
			}
		}
		var b strings.Builder
		for i := 0; i < len(text); {
			j := i
			for j < len(text) && covered[j] == covered[i] {
				j++
			}
			if covered[i] {
				b.WriteString(hiddenText)
			} else {
				b.WriteString(text[i:j])
			}
			i = j
		}
		return b.String()
	}
}

// needles lists the texts Filter replaces, longest first.
func (v Values) needles() []string {
	var needles []string
	add := func(s string) {
		if utf8.RuneCountInString(s) < minFilteredLength {
			return
		}
		needles = append(needles, s)
		for _, escaped := range jsonForms(s) {
			if escaped != s {
				needles = append(needles, escaped)
			}
		}
	}
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			add(t)
			if strings.ContainsAny(t, "\r\n") {
				for line := range strings.FieldsFuncSeq(t, func(r rune) bool { return r == '\n' || r == '\r' }) {
					add(line)
				}
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v.tree)
	slices.SortFunc(needles, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(needles)
}

// jsonForms returns the forms s takes inside a JSON string: with and
// without the escapes of <, > and &.
func jsonForms(s string) []string {
	plain, _ := json.Marshal(s)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return []string{trimQuotes(string(plain))}
	}
	unescapedHTML := strings.TrimSuffix(b.String(), "\n")
	return []string{trimQuotes(string(plain)), trimQuotes(unescapedHTML)}
}

func trimQuotes(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`)
}

// FilterError returns ce with the server text in its message and hint
// passed through v.Filter. Its code and retry advice are kept; the cause is
// dropped, because its text is the server's answer as it was sent.
func (v Values) FilterError(err error) error {
	ce := clierr.From(err)
	out := clierr.New(ce.Code, v.Filter(ce.Message)).WithRetryable(ce.Retryable)
	if ce.Hint != "" {
		out = out.WithHint(v.Filter(ce.Hint))
	}
	return out
}
