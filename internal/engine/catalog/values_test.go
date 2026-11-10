package catalog

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// canary is a value no output may show.
const canary = "canary-s3cr3t-value"

func canaryValues(t *testing.T) Values {
	t.Helper()
	v, err := ReadValues(files{"v.yaml": "auth:\n  password: " + canary + "\nlist:\n- " + canary + "\n"}, []string{"v.yaml"}, false)
	require.NoError(t, err)
	return v
}

// TestValuesPrintHidden asserts every printing and encoding form of Values
// shows <hidden> and no value: fmt's verbs, the JSON and both YAML
// encoders, on a value and a pointer, alone and inside a struct field, and
// on a nil pointer, where fmt prints <nil> and no fault is recovered.
func TestValuesPrintHidden(t *testing.T) {
	t.Parallel()
	v := canaryValues(t)
	type holder struct {
		Values  Values
		Pointer *Values
	}
	subjects := map[string]any{
		"value":           v,
		"pointer":         &v,
		"nested value":    holder{Values: v, Pointer: &v},
		"nested pointer":  &holder{Values: v, Pointer: &v},
		"request field":   v.request(),
		"body":            installBody{Charts: []installChart{{Values: v.request()}}},
		"nil request ptr": (*requestValues)(nil),
	}
	printers := map[string]func(any) (string, error){
		"%v":          func(x any) (string, error) { return fmt.Sprintf("%v", x), nil },
		"%+v":         func(x any) (string, error) { return fmt.Sprintf("%+v", x), nil },
		"%#v":         func(x any) (string, error) { return fmt.Sprintf("%#v", x), nil },
		"%s":          func(x any) (string, error) { return fmt.Sprintf("%s", x), nil },
		"%q":          func(x any) (string, error) { return fmt.Sprintf("%q", x), nil },
		"fmt.Sprint":  func(x any) (string, error) { return fmt.Sprint(x), nil },
		"yaml.v3":     func(x any) (string, error) { b, err := yamlv3.Marshal(x); return string(b), err },
		"sigs yaml":   func(x any) (string, error) { b, err := yaml.Marshal(x); return string(b), err },
		"json":        func(x any) (string, error) { b, err := json.Marshal(x); return string(b), err },
		"json indent": func(x any) (string, error) { b, err := json.MarshalIndent(x, "", " "); return string(b), err },
		"String":      func(x any) (string, error) { return methodText(x, func(s fmt.Stringer) string { return s.String() }) },
		"GoString":    func(x any) (string, error) { return goStringText(x), nil },
	}
	for sname, subject := range subjects {
		for pname, print := range printers {
			if (strings.HasPrefix(pname, "json") || pname == "sigs yaml") && (sname == "request field" || sname == "body") {
				continue // the request body's JSON, which sigs.k8s.io/yaml writes too, is the one encoding that carries the values
			}
			out, err := print(subject)
			require.NoError(t, err, "%s %s", sname, pname)
			assert.NotContains(t, out, canary, "%s %s", sname, pname)
			assert.NotContains(t, out, "PANIC", "%s %s", sname, pname)
			if sname == "value" || sname == "pointer" {
				assert.Contains(t, out, "hidden", "%s %s", sname, pname)
			}
		}
	}
	var nilValues *Values
	for pname, print := range printers {
		out, err := print(nilValues)
		require.NoError(t, err, pname)
		assert.NotContains(t, out, "PANIC", pname)
		assert.NotContains(t, out, canary, pname)
	}
}

// TestRequestValuesJSONWritesTheValues asserts the request body's JSON is
// the one encoding that carries the values, and a body with no values
// leaves the field out.
func TestRequestValuesJSONWritesTheValues(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(installChart{Values: canaryValues(t).request()})
	require.NoError(t, err)
	assert.Contains(t, string(b), canary)
	b, err = json.Marshal(installChart{Values: Values{}.request()})
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"values"`)
}

// TestReadValuesMerges asserts files merge left to right: maps key by key,
// other values replaced, null kept as a value to send.
func TestReadValuesMerges(t *testing.T) {
	t.Parallel()
	src := files{
		"a.yaml": "image:\n  repository: nginx\n  tag: \"1\"\nlist: [a, b]\nkeep: x\ngone: y\n",
		"b.yaml": "image:\n  tag: \"2\"\nlist: [c]\ngone: null\n",
	}
	v, err := ReadValues(src, []string{"a.yaml", "b.yaml"}, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"image": map[string]any{"repository": "nginx", "tag": "2"},
		"list":  []any{"c"},
		"keep":  "x",
		"gone":  nil,
	}, v.tree)
}

// TestReadValuesStdin asserts "-" reads stdin once, a second "-" is
// refused, and a terminal on stdin is refused before anything is read.
func TestReadValuesStdin(t *testing.T) {
	t.Parallel()
	src := files{"-": "a: 1\n"}
	v, err := ReadValues(src, []string{"-"}, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": float64(1)}, v.tree)

	_, err = ReadValues(src, []string{"-", "-"}, false)
	require.ErrorContains(t, err, "--values - is given more than once")
	assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)

	_, err = ReadValues(files{}, []string{"-"}, true)
	require.ErrorContains(t, err, "--values -: stdin is a terminal; pipe the values or name a file")
	assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
}

// TestReadValuesRefusals asserts each refused file names the file as typed
// and at most a line, never the decoder's text or the file's content.
func TestReadValuesRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, content, want string
	}{
		{"scalar top level", canary + "\n", "--values file 'v.yaml', line 1: the top level is not a mapping"},
		{"list top level", "- " + canary + "\n", "--values file 'v.yaml', line 1: the top level is not a mapping"},
		{"syntax error on a canary line", "a: 1\nb: [" + canary + "\n", "--values file 'v.yaml', line 1: not valid YAML"},
		{"tab indentation", "a:\n\tb: " + canary + "\n", "--values file 'v.yaml'"},
		{"two documents", "a: 1\n---\nb: " + canary + "\n", "--values file 'v.yaml', line 2: holds 2 YAML documents"},
		{"empty", "", "--values file 'v.yaml': holds no YAML document"},
		{"tag mismatch", "a: !!int " + canary + "\n", "--values file 'v.yaml': the YAML could not be decoded"},
		{"duplicate key", "a: 1\na: " + canary + "\n", "--values file 'v.yaml': the YAML could not be decoded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadValues(files{"v.yaml": tc.content}, []string{"v.yaml"}, false)
			require.ErrorContains(t, err, tc.want)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.NotContains(t, ce.Message, canary)
			assert.NotContains(t, ce.Message, "yaml:")
		})
	}
}

// TestReadValuesUnreadableFile asserts an unreadable file is VALIDATION
// naming the path as typed, a Windows path included, whatever the OS says.
func TestReadValuesUnreadableFile(t *testing.T) {
	t.Parallel()
	for _, p := range []string{`C:\work\v.yaml`, "missing.yaml"} {
		_, err := ReadValues(files{}, []string{p}, false)
		require.ErrorContains(t, err, "cannot read the --values file "+clierr.ShellQuote(p))
		assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
	}
}

// TestReadValuesBOMAndCRLF asserts a UTF-8 byte order mark and CRLF line
// endings are read, and a line number counts CRLF as one line break.
func TestReadValuesBOMAndCRLF(t *testing.T) {
	t.Parallel()
	v, err := ReadValues(files{"v.yaml": "\xef\xbb\xbfa: 1\r\nb:\r\n  c: two\r\n"}, []string{"v.yaml"}, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": float64(1), "b": map[string]any{"c": "two"}}, v.tree)

	// The line the decoder reports for the same error is the same with LF
	// and with CRLF line endings.
	_, lf := ReadValues(files{"v.yaml": "a: 1\nb: 2\nc: x: y\n"}, []string{"v.yaml"}, false)
	_, crlf := ReadValues(files{"v.yaml": "\xef\xbb\xbfa: 1\r\nb: 2\r\nc: x: y\r\n"}, []string{"v.yaml"}, false)
	require.ErrorContains(t, lf, "line 3: not valid YAML")
	require.ErrorContains(t, crlf, "line 3: not valid YAML")
}

// TestValuesPaths asserts the paths list every leaf in order, with a key
// that holds a dot, a bracket or a quote in brackets.
func TestValuesPaths(t *testing.T) {
	t.Parallel()
	v := Values{tree: map[string]any{
		"auth":  map[string]any{"password": "x", "a.b": map[string]any{"c": 1}},
		"list":  []any{"a"},
		"empty": map[string]any{},
		`q"k`:   "v",
		"br[0]": nil,
	}}
	assert.Equal(t, []string{
		`values.auth["a.b"].c`,
		"values.auth.password",
		`values["br[0]"]`,
		"values.empty",
		"values.list",
		`values["q\"k"]`,
	}, v.paths())
}

// TestValuesFilter asserts the filter replaces every string value of 4
// characters or more, each line of a multi-line value and the JSON-escaped
// forms, a value and a longer value that holds it with one <hidden>, and
// leaves shorter strings, numbers and booleans.
func TestValuesFilter(t *testing.T) {
	t.Parallel()
	v := Values{tree: map[string]any{
		"password": canary,
		"short":    "abc",
		"n":        12345,
		"b":        true,
		"multi":    "line-one-x\nline-two-y",
		"html":     "<tag>&more",
		"quote":    `say "hi" now`,
		"nested":   []any{map[string]any{"k": canary + "-longer"}},
	}}
	text := strings.Join([]string{
		canary + "-longer", canary, "abc", "12345", "true",
		"line-one-x", "line-two-y",
		`\u003ctag\u003e\u0026more`, "<tag>&more", `say \"hi\" now`,
	}, " | ")
	got := v.Filter(text)
	assert.Equal(t, strings.Join([]string{
		"<hidden>", "<hidden>", "abc", "12345", "true",
		"<hidden>", "<hidden>",
		"<hidden>", "<hidden>", "<hidden>",
	}, " | "), got)
	assert.Equal(t, "nothing", Values{}.Filter("nothing"))
}

// TestValuesFilterOverlappingValues asserts Filter and filterFunc replace
// overlapping values with one <hidden> for the text both cover, found in
// the text as given: a value that starts first is hidden with the one it
// runs into, and a value equal to <hidden> is hidden with the value that
// overlaps it, the marker never searched.
func TestValuesFilterOverlappingValues(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		values []string
		text   string
		want   string
	}{
		"shorter value starts first": {[]string{"bcdefgh", "abcd"}, "xx abcdefgh yy", "xx <hidden> yy"},
		"value equal to <hidden>":    {[]string{"<hidden>", "den>xyz"}, "a <hidden>xyz b", "a <hidden> b"},
	} {
		tree := map[string]any{}
		for i, v := range tc.values {
			tree[strconv.Itoa(i)] = v
		}
		v := Values{tree: tree}
		assert.Equal(t, tc.want, v.Filter(tc.text), name)
		assert.Equal(t, tc.want, v.filterFunc()(tc.text), name)
	}
}

// TestValuesFilterError asserts the filtered error keeps its code and
// retry advice and hides values in its message and hint.
func TestValuesFilterError(t *testing.T) {
	t.Parallel()
	v := canaryValues(t)
	err := v.FilterError(clierr.New(clierr.CodeConflict, "server said "+canary).WithHint("try " + canary).WithRetryable(true))
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, "server said <hidden>", ce.Message)
	assert.Equal(t, "try <hidden>", ce.Hint)
}

// methodText calls String on x when x has the method and x is not a nil
// pointer, and is "" otherwise.
func methodText(x any, call func(fmt.Stringer) string) (string, error) {
	s, ok := x.(fmt.Stringer)
	if !ok || isNilPointer(x) {
		return "", nil
	}
	return call(s), nil
}

// goStringText calls GoString on x when x has the method and is not a nil
// pointer.
func goStringText(x any) string {
	s, ok := x.(fmt.GoStringer)
	if !ok || isNilPointer(x) {
		return ""
	}
	return s.GoString()
}

func isNilPointer(x any) bool {
	switch p := x.(type) {
	case *Values:
		return p == nil
	case *requestValues:
		return p == nil
	}
	return false
}
