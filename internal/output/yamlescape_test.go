package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/output/style"
)

// refusedRunes are the 35 code points the YAML library refuses when they
// appear raw in the JSON text it parses (it changes U+0085 into a space): DEL,
// U+0080 to U+009F, U+FFFE and U+FFFF.
var refusedRunes = func() []rune {
	rs := []rune{0x7f}
	for r := rune(0x80); r <= 0x9f; r++ {
		rs = append(rs, r)
	}
	return append(rs, 0xfffe, 0xffff)
}()

// yamlRow is one text the yaml output must write and read back exactly. raw
// lists the characters the YAML emitter writes unescaped.
type yamlRow struct {
	name string
	text string
	raw  []rune
}

func yamlRows() []yamlRow {
	var rows []yamlRow
	for _, r := range refusedRunes {
		rows = append(rows, yamlRow{name: fmt.Sprintf("%U", r), text: string(r)})
	}
	for _, r := range []rune{0xa0, 0xa1, 0xd7ff, 0xe000, 0xfffd} {
		rows = append(rows, yamlRow{name: fmt.Sprintf("%U written raw", r), text: string(r), raw: []rune{r}})
	}
	for _, r := range []rune{0x7e, 0x2028, 0x2029, 0xfeff, 0x10000, 0x10ffff, 0x1f600} {
		rows = append(rows, yamlRow{name: fmt.Sprintf("%U", r), text: string(r)})
	}
	for r := rune(0); r <= 0x1f; r++ {
		rows = append(rows, yamlRow{name: fmt.Sprintf("%U", r), text: string(r)})
	}
	for name, text := range map[string]string{
		"DEL inside a word":                 "a\x7fb",
		"two in a row":                      "\x7f\u009b",
		"one at each end":                   "\x7fmiddle\u009b",
		"beside U+00E9":                     "\u00e9\x7f\u00e9\u0085",
		"beside U+4E16 and U+1F600":         "\u4e16\x7f\U0001f600\u009b\u4e16\U0001f600",
		"after a backslash":                 "a\\\x7f",
		"the text of an escape":             `\u007f`,
		"the text of an escape beside DEL":  `\u007f` + "\x7f" + `\u0085`,
		"beside a double quote":             "\"\x7f\"",
		"U+FFFE and U+FFFF beside text":     "x\ufffey\uffffz",
		"U+0084 U+0085 U+0086 in a row":     "\u0084\u0085\u0086",
		"a newline before U+0085":           "line one\n\u0085line two",
		"U+007E U+007F U+0080 in a row":     "~\x7f\u0080",
		"U+009F U+00A0 in a row":            "\u009f\u00a0",
		"U+FFFD beside U+FFFE and U+FFFF":   "\ufffd\ufffe\uffff",
		"U+D7FF U+E000 beside U+FFFE":       "\ud7ff\ue000\ufffe",
		"a long run of one character":       strings.Repeat("\u0085", 100),
		"a long run of letters and one DEL": strings.Repeat("abc", 100) + "\x7f",
	} {
		rows = append(rows, yamlRow{name: name, text: text})
	}
	return rows
}

func renderYAMLData(t *testing.T, data map[string]string) string {
	t.Helper()
	tree := make(map[string]any, len(data))
	for k, v := range data {
		tree[k] = v
	}
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "yaml", NewEnvelope("X", tree)))
	return buf.String()
}

func readBackSigs(t *testing.T, out string) map[string]string {
	t.Helper()
	var doc struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(out), &doc))
	return doc.Data
}

func readBackV3(t *testing.T, out string) map[string]string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, yamlv3.Unmarshal([]byte(out), &doc))
	data, ok := doc["data"].(map[string]any)
	require.True(t, ok, "data is not a mapping: %v", doc["data"])
	got := make(map[string]string, len(data))
	for k, v := range data {
		s, ok := v.(string)
		require.True(t, ok, "the value of %q is not a string: %v", k, v)
		got[k] = s
	}
	return got
}

// TestRenderYAMLWritesEveryRefusedCharacter asserts the yaml output of a text
// that holds a character the YAML library refuses, as a value and as a key, is
// written with no error, reads back as the original text through two
// YAML libraries, and holds none of the 35 characters raw. The characters
// beside the set are written the way the emitter writes them and read back
// exactly.
func TestRenderYAMLWritesEveryRefusedCharacter(t *testing.T) {
	t.Parallel()
	for _, row := range yamlRows() {
		for _, position := range []string{"value", "key"} {
			t.Run(row.name+" as a "+position, func(t *testing.T) {
				t.Parallel()
				data := map[string]string{"k": row.text}
				if position == "key" {
					data = map[string]string{row.text: "v"}
				}
				out := renderYAMLData(t, data)
				assert.Equal(t, data, readBackSigs(t, out), "sigs.k8s.io/yaml")
				assert.Equal(t, data, readBackV3(t, out), "gopkg.in/yaml.v3")
				for _, r := range refusedRunes {
					assert.False(t, strings.ContainsRune(out, r), "%U is written raw: %q", r, out)
				}
				for _, r := range row.raw {
					assert.True(t, strings.ContainsRune(out, r), "%U is not written raw: %q", r, out)
				}
			})
		}
	}
}

// TestRenderYAMLEscapesAfterTheRewrite asserts the escape runs after the
// rewrite: a rewrite that decodes the document and writes it again with HTML
// escaping off puts DEL and U+0080 to U+009F back raw, and the output still
// reads back exactly.
func TestRenderYAMLEscapesAfterTheRewrite(t *testing.T) {
	t.Parallel()
	rewrite := func(b []byte) []byte {
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return b
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(doc); err != nil {
			return b
		}
		return buf.Bytes()
	}
	text := "a\x7f\u009b\u0085b<&>"
	var buf bytes.Buffer
	require.NoError(t, RenderWith(&buf, "yaml", NewEnvelope("X", map[string]any{"k": text}), style.Style{}, rewrite))
	out := buf.String()
	want := map[string]string{"k": text}
	assert.Equal(t, want, readBackSigs(t, out))
	assert.Equal(t, want, readBackV3(t, out))
	for _, r := range refusedRunes {
		assert.False(t, strings.ContainsRune(out, r), "%U is written raw: %q", r, out)
	}
}

// TestRenderJSONWritesTheRefusedCharactersRaw asserts the json output keeps
// DEL and U+0080 to U+009F as they are: only the yaml output escapes them.
func TestRenderJSONWritesTheRefusedCharactersRaw(t *testing.T) {
	t.Parallel()
	for _, r := range refusedRunes {
		var buf bytes.Buffer
		require.NoError(t, Render(&buf, "json", NewEnvelope("X", map[string]any{"k": "a" + string(r) + "b"})))
		assert.True(t, strings.ContainsRune(buf.String(), r), "%U", r)
		var doc struct {
			Data map[string]string `json:"data"`
		}
		require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
		assert.Equal(t, "a"+string(r)+"b", doc.Data["k"], "%U", r)
	}
}

// TestEscapeRefusedRunes asserts the set of escaped characters is exactly the
// 35 code points: every valid code point from U+0000 to U+10FFFF inside a JSON
// string is returned unchanged unless it is one of the 35, and each of the 35
// is written as a six-character JSON escape with four lowercase hex digits. It
// asserts also that a text with nothing to escape is returned as the same
// slice, and that a byte that is not valid UTF-8 is copied as it is.
func TestEscapeRefusedRunes(t *testing.T) {
	t.Parallel()
	require.Len(t, refusedRunes, 35)
	in := make([]byte, 0, 16)
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		in = append(append(append(in[:0], `"a`...), string(r)...), `b"`...)
		want := string(in)
		if slices.Contains(refusedRunes, r) {
			want = fmt.Sprintf(`"a\u%04xb"`, r)
		}
		if got := string(escapeRefusedRunes(in)); got != want {
			require.Failf(t, "wrong escape", "%U: want %q, got %q", r, want, got)
		}
	}

	plain := []byte(`{"k":"plain \\u007f text"}`)
	got := escapeRefusedRunes(plain)
	require.Len(t, got, len(plain))
	assert.Same(t, &plain[0], &got[0], "a text with nothing to escape is returned without a copy")

	for name, in := range map[string][]byte{
		"a lone lead byte of a two-byte character": []byte("\"a\xc2b\""),
		"a truncated three-byte character":         []byte("\"a\xef\xbfb\""),
		"a lone continuation byte":                 []byte("\"a\x85b\""),
	} {
		assert.Equal(t, in, escapeRefusedRunes(in), name)
	}
	assert.Equal(t, []byte("\"\xc2\\u0085\""), escapeRefusedRunes([]byte("\"\xc2\xc2\x85\"")),
		"an invalid byte before a character of the set is copied and the character is escaped")
}

// firstDifference names the first rune at which two texts differ.
func firstDifference(want, got string) string {
	w, g := []rune(want), []rune(got)
	for i := range min(len(w), len(g)) {
		if w[i] != g[i] {
			return fmt.Sprintf("rune %d: want %U, got %U", i, w[i], g[i])
		}
	}
	return fmt.Sprintf("the lengths differ: want %d runes, got %d", len(w), len(g))
}

// assertReadsBackExactly fails with the first differing code point when a
// library returns something other than the original key and value.
func assertReadsBackExactly(t *testing.T, library string, chunk int, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		gotValue, ok := got[k]
		if !ok {
			for gk := range got {
				require.Failf(t, "key not read back", "%s, chunk %d: the key differs at %s", library, chunk, firstDifference(k, gk))
			}
			require.Failf(t, "key not read back", "%s, chunk %d: no key", library, chunk)
		}
		if gotValue != v {
			require.Failf(t, "value not read back", "%s, chunk %d: %s", library, chunk, firstDifference(v, gotValue))
		}
	}
	require.Len(t, got, len(want), "%s, chunk %d", library, chunk)
}

// TestRenderYAMLEveryCodePointReadsBack asserts a selection of the valid code
// points, from U+0000 to U+10FFFF, is written by the yaml output and read back
// unchanged by two YAML libraries, as a value and as a key. The selection is
// U+0000 to U+00FF, U+FFF0 to U+FFFF (the 35 refused code points and their
// neighbours) and every seventh code point of the whole range; the direct test
// of escapeRefusedRunes covers every code point. The code points go in strings
// of 1024 runes as a value and 128 runes as a key (the YAML library limits an
// implicit key to 1024 characters). Each string starts with a letter, which
// keeps a string from being only characters the library changes or refuses.
func TestRenderYAMLEveryCodePointReadsBack(t *testing.T) {
	t.Parallel()
	var all []rune
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		if r <= 0xff || r >= 0xfff0 && r <= 0xffff || r%7 == 0 {
			all = append(all, r)
		}
	}
	const shards = 4
	for _, position := range []struct {
		name  string
		chunk int
		key   bool
	}{{"value", 1023, false}, {"key", 127, true}} {
		for shard := range shards {
			t.Run(fmt.Sprintf("%s shard %d", position.name, shard), func(t *testing.T) {
				t.Parallel()
				chunk := 0
				for start := 0; start < len(all); start += position.chunk {
					chunk++
					if chunk%shards != shard {
						continue
					}
					text := "a" + string(all[start:min(start+position.chunk, len(all))])
					data := map[string]string{"k": text}
					if position.key {
						data = map[string]string{text: "v"}
					}
					out := renderYAMLData(t, data)
					assertReadsBackExactly(t, "sigs.k8s.io/yaml", chunk, data, readBackSigs(t, out))
					assertReadsBackExactly(t, "gopkg.in/yaml.v3", chunk, data, readBackV3(t, out))
				}
			})
		}
	}
}
