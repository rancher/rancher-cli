package runtime_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// dataValues are the strings a result's data holds in each shape: a value
// whose encoding a scanner can misread, the marked command, and three values
// from outside the process.
type dataValues struct {
	misread string
	marked  string
	planted string
	other   string
	plain   string
}

// dataShape is one JSON shape that can hold a string: how the data is built
// from the values, and how the values are read back from the decoded data.
type dataShape struct {
	name string
	// marked is the marked command of the shape: the default when empty.
	marked string
	// effect is true for the preview of a dry run, whose data is a
	// safety.Effect.
	effect bool
	build  func(v dataValues) any
	// path and keys locate the five values in the decoded data: the object
	// keys or array indexes of the marked command, the misread value and the
	// three outside values, in the order of dataValues, below path.
	path []any
	keys [5]any
	// read replaces path and keys for a shape whose values are not found by
	// them.
	read func(t *testing.T, data any, v dataValues) dataValues
}

// values reads the five values of the shape from its decoded data.
func (shape dataShape) values(t *testing.T, data any, v dataValues) dataValues {
	t.Helper()
	if shape.read != nil {
		return shape.read(t, data, v)
	}
	return readKeys(t, data, shape.path, shape.keys)
}

// dataStruct is a struct with json keys out of alphabetical order and a number
// above 2^53.
type dataStruct struct {
	Zeta  string `json:"zeta"`
	Alpha string `json:"alpha"`
	Mid   string `json:"mid"`
	Omega string `json:"omega"`
	Beta  string `json:"beta"`
	Big   int64  `json:"big"`
}

type dataInner struct {
	First  string `json:"first"`
	Second string `json:"second"`
	Third  string `json:"third"`
	Fourth string `json:"fourth"`
	Fifth  string `json:"fifth"`
	Sixth  string `json:"sixth"`
}

type dataOuter struct {
	Inner *dataInner `json:"inner"`
}

// dataAny holds each string in a field of type any.
type dataAny struct {
	First  any `json:"first"`
	Second any `json:"second"`
	Third  any `json:"third"`
	Fourth any `json:"fourth"`
	Fifth  any `json:"fifth"`
	Sixth  any `json:"sixth"`
}

// dataCustom writes its own JSON with json.Marshal, as the types with a
// MarshalJSON method in the production tree do.
type dataCustom struct{ values []string }

func (c dataCustom) MarshalJSON() ([]byte, error) {
	return json.Marshal(dataInner{c.values[0], c.values[1], c.values[2], c.values[3], c.values[4], c.values[5]})
}

// dataTable is a payload that describes its own table.
type dataTable struct {
	First  string `json:"first"`
	Second string `json:"second"`
	Third  string `json:"third"`
	Fourth string `json:"fourth"`
	Fifth  string `json:"fifth"`
	Sixth  string `json:"sixth"`
}

func (d dataTable) Columns() []string { return []string{"FIRST", "SECOND"} }
func (d dataTable) Rows() [][]string  { return [][]string{{d.First, d.Second}} }

// dataAt returns the string at path in decoded data: a string is an object
// key and an int an index of an array.
func dataAt(t *testing.T, data any, path ...any) string {
	t.Helper()
	cur := data
	for _, step := range path {
		switch k := step.(type) {
		case string:
			object, ok := cur.(map[string]any)
			require.True(t, ok, "%v is not an object, at key %q", cur, k)
			cur, ok = object[k]
			require.True(t, ok, "the object %v has no key %q", object, k)
		case int:
			array, ok := cur.([]any)
			require.True(t, ok && k < len(array), "%v has no index %d", cur, k)
			cur = array[k]
		}
	}
	s, ok := cur.(string)
	require.True(t, ok, "%v is not a string", cur)
	return s
}

// readKeys returns the values of the fields named by keys, in the order of
// dataValues.
func readKeys(t *testing.T, data any, prefix []any, keys [5]any) dataValues {
	t.Helper()
	at := func(i int) string { return dataAt(t, data, append(slices.Clone(prefix), keys[i])...) }
	return dataValues{misread: at(0), marked: at(1), planted: at(2), other: at(3), plain: at(4)}
}

// dataShapes are the JSON shapes a string can take in result data.
func dataShapes() []dataShape {
	return []dataShape{
		{name: "struct field", marked: clierr.Rancher + " get pods a<b&c",
			build: func(v dataValues) any {
				return dataStruct{v.misread, v.marked, v.planted, v.other, v.plain, 9007199254740993}
			},
			keys: [5]any{"zeta", "alpha", "mid", "omega", "beta"}},
		{name: "nested pointer to a struct",
			build: func(v dataValues) any {
				return dataOuter{&dataInner{v.misread, v.marked, v.planted, v.other, v.plain, "words"}}
			},
			path: []any{"inner"},
			keys: [5]any{"first", "second", "third", "fourth", "fifth"}},
		{name: "list of strings",
			build: func(v dataValues) any { return []string{v.misread, v.marked, v.planted, v.other, v.plain} },
			keys:  [5]any{0, 1, 2, 3, 4}},
		{name: "map of any",
			build: func(v dataValues) any {
				return map[string]any{"a1": v.misread, "b": v.marked, "c": v.planted, "d": v.other, "e": v.plain}
			},
			keys: [5]any{"a1", "b", "c", "d", "e"}},
		{name: "any field holding a string",
			build: func(v dataValues) any {
				return dataAny{v.misread, v.marked, v.planted, v.other, v.plain, "words"}
			},
			keys: [5]any{"first", "second", "third", "fourth", "fifth"}},
		// For the command alone the key starts with words that sort it after
		// the misread value and before the outside keys whether or not it is
		// written as a command, so the keys keep their order in the output. The
		// key of the form with server text starts with "server said: ", which
		// fixes its place the same way, before the first byte a command changes.
		{name: "map key", marked: "zzz " + clierr.Rancher + " get pods",
			build: func(v dataValues) any {
				return map[string]string{v.misread: "a", v.marked: "b", v.planted: "c", v.other: "d", v.plain: "e"}
			},
			read: func(t *testing.T, data any, v dataValues) dataValues {
				t.Helper()
				keys, ok := data.(map[string]any)
				require.True(t, ok, "%v is not an object", data)
				got := dataValues{misread: "(missing)", planted: "(missing)", other: "(missing)", plain: "(missing)"}
				var rest []string
				for k := range keys {
					switch k {
					case v.misread:
						got.misread = k
					case v.planted:
						got.planted = k
					case v.other:
						got.other = k
					case v.plain:
						got.plain = k
					default:
						rest = append(rest, k)
					}
				}
				slices.Sort(rest)
				got.marked = strings.Join(rest, " | ")
				return got
			}},
		{name: "type with its own MarshalJSON",
			build: func(v dataValues) any {
				return dataCustom{[]string{v.misread, v.marked, v.planted, v.other, v.plain, "words"}}
			},
			keys: [5]any{"first", "second", "third", "fourth", "fifth"}},
		{name: "tabular payload",
			build: func(v dataValues) any {
				return dataTable{v.misread, v.marked, v.planted, v.other, v.plain, "words"}
			},
			keys: [5]any{"first", "second", "third", "fourth", "fifth"}},
		{name: "dry-run effect", effect: true,
			build: func(v dataValues) any {
				return safety.Effect{
					PreviewSource: safety.PreviewServerDryRun,
					Action:        safety.ActionUpdate, Target: v.misread, DisplayKind: "Cluster", DisplayName: "c",
					Server: v.marked, Cluster: v.planted, ResourceVersion: v.other,
					Objects: []safety.Object{{Kind: "cluster", Name: "c", Outcome: safety.OutcomeRancherKeeps, Message: v.plain}},
				}
			},
			read: func(t *testing.T, data any, _ dataValues) dataValues {
				t.Helper()
				return dataValues{
					misread: dataAt(t, data, "target"), marked: dataAt(t, data, "server"), planted: dataAt(t, data, "cluster"),
					other: dataAt(t, data, "resourceVersion"), plain: dataAt(t, data, "objects", 0, "message"),
				}
			}},
	}
}

// misreadDocuments are the values whose encoding a scanner can misread, one
// for each document: a string that ends in one backslash, one that ends in
// two, and one with an odd number of escaped quotes. A document holds one of
// them before the marked value and no other, since two misreadings in one
// document can cancel each other.
var misreadDocuments = []struct{ name, value string }{
	{"one backslash", `x\`},
	{"two backslashes", `x\\`},
	{"escaped quote", `say "hi`},
}

// markedKinds are the two forms of the marked value of a document: the
// command of the shape alone, and a text that holds a command beside text from
// outside the process (U+2063 and the word rancher), so that a mapping of the
// selected string that also rewrote the outside text would show. want is the
// text a reader decodes for a directory prefix, written out here and never
// computed by the expansion under test; the command alone is the shape's own
// text with the marker replaced by the prefix.
var markedKinds = []struct {
	name, text string
	want       func(marked, prefix string) string
}{
	{"a command", "", func(marked, prefix string) string {
		return strings.ReplaceAll(marked, clierr.Rancher, "rancher"+prefix)
	}},
	{"a command beside server text", "server said: \u2063rancher get pods; run " + clierr.Rancher + " config status",
		func(_, prefix string) string {
			return "server said: \u2063rancher get pods; run rancher" + prefix + " config status"
		}},
}

// dataValuesFor are the values of one document of a shape; markedText, when it
// is not empty, replaces the marked value of the shape.
func dataValuesFor(shape dataShape, misread, markedText string) dataValues {
	marked := shape.marked
	if marked == "" {
		marked = clierr.Rancher + " get pods"
	}
	if markedText != "" {
		marked = markedText
	}
	return dataValues{
		misread: misread,
		marked:  marked,
		planted: "\u2063rancher get pods",
		other:   "\u2063\u2060\u2061\u2062\u2064rancher get pods",
		plain:   "rancher get nodes",
	}
}

// renderData runs the shape's data through the pipeline in format with the
// directory and returns what it wrote to standard output.
func renderData(t *testing.T, shape dataShape, format, dir string, v dataValues) []byte {
	t.Helper()
	var out, errBuf bytes.Buffer
	streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: dir, DryRun: shape.effect}
	var cmd runtime.Command = siteRead{res: runtime.Result{Kind: "Report", Data: shape.build(v)}}
	if shape.effect {
		cmd = siteMutating{d: runtime.Descriptor{Name: "clusters scale", CommandType: runtime.Mutating}, eff: shape.build(v).(safety.Effect)}
	}
	_, ce := runtime.Run(t.Context(), cmd, siteInput{}, streams, safety.Binding{})
	require.Nil(t, ce)
	return out.Bytes()
}

// decodeData decodes the data of the envelope of a json or yaml document.
func decodeData(t *testing.T, format string, b []byte) any {
	t.Helper()
	var doc struct {
		Data any `json:"data"`
	}
	if format == "yaml" {
		require.NoError(t, yaml.Unmarshal(b, &doc))
	} else {
		require.True(t, json.Valid(b), "the document is not valid JSON: %q", b)
		require.NoError(t, json.Unmarshal(b, &doc))
	}
	return doc.Data
}

// TestResultDataNamesTheDirectoryInEveryShape asserts, for each JSON shape a
// string can take in result data and each value whose encoding a scanner can
// misread, in json and yaml, with a directory that holds a backslash, a double
// quote and a single quote and with no directory: the decoded marked command
// is the command written for the invocation, every value from outside the
// process (U+2063 and the word rancher, another process's marker, plain text)
// and the misread value keep their own bytes, and the output equals, byte for
// byte, the output for the same data with the marked command written before
// the run. That equality also asserts the key order, the digits of a number
// and the escaping of the changed string. The marked value is a command alone
// in one form of each document and a text with a command beside text from
// outside the process in the other: only the second shows a mapping of the
// selected string that rewrites the outside text too, in yaml as in json.
func TestResultDataNamesTheDirectoryInEveryShape(t *testing.T) {
	t.Parallel()
	named := t.TempDir() + "/a\\b\"c'd"
	namedPrefix, printable := runtime.RemedyPrefix(named)
	require.True(t, printable)
	for _, dir := range []struct{ name, dir, prefix string }{
		{"a directory with quotes", named, namedPrefix},
		{"no directory", "", ""},
	} {
		for _, shape := range dataShapes() {
			for _, format := range []string{"json", "yaml"} {
				for _, doc := range misreadDocuments {
					for _, kind := range markedKinds {
						name := dir.name + ", " + shape.name + ", " + format + ", " + doc.name + ", " + kind.name
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							v := dataValuesFor(shape, doc.value, kind.text)
							out := renderData(t, shape, format, dir.dir, v)
							got := shape.values(t, decodeData(t, format, out), v)
							want := kind.want(v.marked, dir.prefix)
							assert.Equal(t, want, got.marked, "the marked command")
							assert.Equal(t, v.misread, got.misread, "the misread value")
							assert.Equal(t, v.planted, got.planted, "U+2063 and the word rancher")
							assert.Equal(t, v.other, got.other, "another process's marker")
							assert.Equal(t, v.plain, got.plain, "plain text")
							assert.NotContains(t, string(out), clierr.RemedyMarker)
							assert.NotContains(t, got.marked, clierr.RemedyMarker)
							if kind.text == "" {
								assert.False(t, holdsMarkerRune(got.marked), "the marked command holds a character of the marker: %q", got.marked)
							}

							expanded := v
							expanded.marked = want
							assert.Equal(t, string(renderData(t, shape, format, dir.dir, expanded)), string(out), "the output of the data written before the run")
						})
					}
				}
			}
		}
	}
}

// TestResultDataForAnUnprintableDirectoryOffersNoCommand asserts that for a
// directory a terminal line cannot show, json and yaml result data holds no
// command and no part of the directory: a command alone becomes the note, a
// text keeps its words before the clause with the command and ends with the
// note, and each of four commands becomes the note.
func TestResultDataForAnUnprintableDirectoryOffersNoCommand(t *testing.T) {
	t.Parallel()
	note := runtime.UnprintableConfigDirNote
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"a command alone", []string{clierr.Rancher + " get pods"}, []string{note}},
		{"a text whose second clause holds the command", []string{"see the list; then run " + clierr.Rancher + " config status"}, []string{"see the list; " + note}},
		{"four commands", []string{clierr.Rancher + " a", clierr.Rancher + " b", clierr.Rancher + " c", clierr.Rancher + " d"}, []string{note, note, note, note}},
		{"a command beside server text", []string{"server said: \u2063rancher get pods; run " + clierr.Rancher + " config status"}, []string{"server said: \u2063rancher get pods; " + note}},
	} {
		for _, format := range []string{"json", "yaml"} {
			t.Run(tc.name+", "+format, func(t *testing.T) {
				t.Parallel()
				var out, errBuf bytes.Buffer
				streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: unprintableDirectory}
				cmd := siteRead{res: runtime.Result{Kind: "Report", Data: map[string]any{"values": tc.in}}}
				_, ce := runtime.Run(t.Context(), cmd, siteInput{}, streams, safety.Binding{})
				require.Nil(t, ce)
				data := decodeData(t, format, out.Bytes())
				var got []string
				for i := range tc.want {
					got = append(got, dataAt(t, data, "values", i))
				}
				assert.Equal(t, tc.want, got)
				for _, s := range jsonStrings(data) {
					if !strings.Contains(s, "\u2063") {
						assert.NotContains(t, s, "rancher ")
					}
					assert.NotContains(t, s, "--config")
					assert.NotContains(t, s, "home/u/")
				}
			})
		}
	}
}

// quotedTemplate puts each command of markedRow inside double quotes, which a
// scanner of JSON strings would read as the string literals of a document.
const quotedTemplate = `template="{{.next}}" "{{.name}}"`

// TestTableWideNameAndTemplateDataPrintThePlainCommand asserts a marked command
// in the data of a result written as a table, wide, name or a template prints
// as the plain command with no character of the marker and no directory,
// also when a template puts the commands inside double quotes.
func TestTableWideNameAndTemplateDataPrintThePlainCommand(t *testing.T) {
	t.Parallel()
	wantByFormat := map[string]string{
		"table":    "rancher config status",
		"wide":     "rancher config status",
		"name":     "rancher get pods\n",
		"template": "\"rancher config status\" \"rancher get pods\"\n",
	}
	for _, df := range dataFormats {
		if df.name == "json" || df.name == "yaml" {
			continue
		}
		want, ok := wantByFormat[df.name]
		require.True(t, ok, "the format %s has no expected output", df.name)
		f := struct{ name, format, want string }{df.name, df.format, want}
		if f.name == "template" {
			f.format = quotedTemplate
		}
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			streams := runtime.IO{Out: &out, Err: &errBuf, Format: f.format, RemedyDir: t.TempDir()}
			_, ce := runtime.Run(t.Context(), dataCommand{runtime.Result{Kind: "Report", Data: newMarkedRow("a value")}}, siteInput{}, streams, safety.Binding{})
			require.Nil(t, ce)
			assert.Contains(t, out.String(), f.want)
			if f.name == "template" || f.name == "name" {
				assert.Equal(t, f.want, out.String())
			}
			assert.False(t, holdsMarkerRune(out.String()), "%q", out.String())
			assert.NotContains(t, out.String(), "--config")
		})
	}
}

// TestKeysThatHoldACommandBecomeOneKeyForAnUnprintableDirectory asserts the
// output for an object with two keys that each hold a command, for a
// directory that cannot be printed: each key becomes the note, so json holds
// the note as the key twice and yaml keeps the last value.
func TestKeysThatHoldACommandBecomeOneKeyForAnUnprintableDirectory(t *testing.T) {
	t.Parallel()
	note := runtime.UnprintableConfigDirNote
	run := func(format string) []byte {
		t.Helper()
		var out, errBuf bytes.Buffer
		streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: unprintableDirectory}
		cmd := siteRead{res: runtime.Result{Kind: "Report", Data: map[string]string{clierr.Rancher + " a": "1", clierr.Rancher + " b": "2"}}}
		_, ce := runtime.Run(t.Context(), cmd, siteInput{}, streams, safety.Binding{})
		require.Nil(t, ce)
		return out.Bytes()
	}

	var doc struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(run("json"), &doc))
	dec := json.NewDecoder(bytes.NewReader(doc.Data))
	_, err := dec.Token()
	require.NoError(t, err)
	var keys, values []string
	for dec.More() {
		key, err := dec.Token()
		require.NoError(t, err)
		keys = append(keys, key.(string))
		var value string
		require.NoError(t, dec.Decode(&value))
		values = append(values, value)
	}
	assert.Equal(t, []string{note, note}, keys)
	assert.Equal(t, []string{"1", "2"}, values)

	assert.Equal(t, map[string]any{note: "2"}, decodeData(t, "yaml", run("yaml")))
}

// TestEveryOutputFormatIsRewrittenOrWrittenAsToday asserts each format the CLI
// accepts, and a template, has a class: json and yaml name the directory in a
// marked command of result data, and the others print the plain command. A
// format added to runtime.OutputFormats fails until it is classified here.
func TestEveryOutputFormatIsRewrittenOrWrittenAsToday(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)
	classes := map[string]string{
		"table": "plain", "wide": "plain", "name": "plain", "template": "plain",
		"json": "rewritten", "yaml": "rewritten",
	}
	for _, format := range append(slices.Clone(runtime.OutputFormats), "template") {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			class, ok := classes[format]
			require.True(t, ok, "the format %s has no class: decide whether its output names the directory", format)
			run := format
			if format == "template" {
				run = quotedTemplate
			}
			var out, errBuf bytes.Buffer
			streams := runtime.IO{Out: &out, Err: &errBuf, Format: run, RemedyDir: dir}
			_, ce := runtime.Run(t.Context(), dataCommand{runtime.Result{Kind: "Report", Data: newMarkedRow("a value")}}, siteInput{}, streams, safety.Binding{})
			require.Nil(t, ce)
			assert.False(t, holdsMarkerRune(out.String()), "%q", out.String())
			if class == "rewritten" {
				data, ok := decodeData(t, format, out.Bytes()).(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "rancher"+prefix+" get pods", data["name"])
				return
			}
			assert.NotContains(t, out.String(), "--config")
			assert.Contains(t, out.String(), "rancher get pods")
			var render bytes.Buffer
			require.NoError(t, output.Render(&render, run, output.NewEnvelope("Report", newMarkedRow("a value"))))
			assert.Equal(t, strings.ReplaceAll(render.String(), clierr.Rancher, "rancher"), out.String(), "the render with no rewrite")
		})
	}
}
