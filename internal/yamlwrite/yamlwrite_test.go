package yamlwrite

import (
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

// neighbours are code points beside the set that the library accepts.
var neighbours = []rune{0x7e, 0xa0, 0xfffd, 0x4e16, 0x1f600}

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

// readBackSigs decodes YAML with sigs.k8s.io/yaml into the shape of want.
func readBackSigs[T any](t *testing.T, out []byte, want T) T {
	t.Helper()
	var got T
	require.NoError(t, yaml.Unmarshal(out, &got))
	return got
}

// assertReadsBack asserts the YAML of v decodes, through sigs.k8s.io/yaml and
// through gopkg.in/yaml.v3, to a value equal to v, and holds none of the 35
// characters raw.
func assertReadsBack(t *testing.T, v any) {
	t.Helper()
	out, err := Marshal(v)
	require.NoError(t, err)
	want, err := json.Marshal(v)
	require.NoError(t, err)

	var viaSigs any
	require.NoError(t, yaml.Unmarshal(out, &viaSigs))
	got, err := json.Marshal(viaSigs)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got), "sigs.k8s.io/yaml")

	var viaV3 any
	require.NoError(t, yamlv3.Unmarshal(out, &viaV3))
	got, err = json.Marshal(viaV3)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got), "gopkg.in/yaml.v3")

	for _, r := range refusedRunes {
		assert.False(t, strings.ContainsRune(string(out), r), "%U is written raw: %q", r, out)
	}
}

// TestMarshalReadsBackEveryRefusedCharacter asserts each of the 35 characters,
// as a map value and as a map key, is written with no error and reads back as
// the original text through two YAML libraries, with no raw occurrence in the
// output.
func TestMarshalReadsBackEveryRefusedCharacter(t *testing.T) {
	t.Parallel()
	for _, r := range append(slices.Clone(refusedRunes), neighbours...) {
		text := "a" + string(r) + "b"
		t.Run(fmt.Sprintf("%U as a value", r), func(t *testing.T) {
			t.Parallel()
			assertReadsBack(t, map[string]string{"k": text})
		})
		t.Run(fmt.Sprintf("%U as a key", r), func(t *testing.T) {
			t.Parallel()
			assertReadsBack(t, map[string]string{text: "v"})
		})
	}
}

// TestMarshalReadsBackTheWholeSetInOneValue asserts all 35 characters and their
// neighbours in one value, and in one key, read back exactly.
func TestMarshalReadsBackTheWholeSetInOneValue(t *testing.T) {
	t.Parallel()
	text := string(refusedRunes) + string(neighbours)
	assertReadsBack(t, map[string]string{"k": text})
	assertReadsBack(t, map[string]string{text: "v"})
	typed := readBackSigs(t, mustMarshal(t, map[string]string{"k": text}), map[string]string{})
	assert.Equal(t, map[string]string{"k": text}, typed)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := Marshal(v)
	require.NoError(t, err)
	return out
}

type inner struct {
	Label string  `json:"label"`
	Note  *string `json:"note,omitempty"`
}

type outer struct {
	Name   string            `json:"name"`
	Count  int               `json:"count"`
	Items  []inner           `json:"items"`
	Tags   map[string]string `json:"tags"`
	Inner  *inner            `json:"inner"`
	Hidden string            `json:"-"`
}

// TestMarshalStructsAndNestedValues asserts a struct with json tags, a nested
// slice and a pointer field read back exactly when they hold the set.
func TestMarshalStructsAndNestedValues(t *testing.T) {
	t.Parallel()
	note := "n" + string(refusedRunes)
	v := outer{
		Name:   "a\u007fb",
		Count:  3,
		Items:  []inner{{Label: "\u0085", Note: &note}, {Label: "\uffff"}},
		Tags:   map[string]string{"\u009b": "\ufffe", "plain": "x"},
		Inner:  &inner{Label: string(refusedRunes) + string(neighbours)},
		Hidden: "\u0085",
	}
	assertReadsBack(t, v)
	got := readBackSigs(t, mustMarshal(t, v), outer{})
	v.Hidden = ""
	assert.Equal(t, v, got)
}

// TestMarshalMatchesTheLibraryForOrdinaryValues asserts a value that holds
// none of the 35 characters is written with the same bytes as
// sigs.k8s.io/yaml writes.
func TestMarshalMatchesTheLibraryForOrdinaryValues(t *testing.T) {
	t.Parallel()
	note := "a note: with \"quotes\"\nand a newline"
	for name, v := range map[string]any{
		"a map":    map[string]string{"b": "2", "a": "1", "c": "x y", "d": "\u00a0\ufffd\u4e16\U0001f600"},
		"a struct": outer{Name: "n", Count: 2, Items: []inner{{Label: "l", Note: &note}}, Tags: map[string]string{"k": "v"}, Inner: &inner{Label: "i"}},
		"a slice":  []any{"a", 1, true, nil, map[string]any{"k": []string{"x"}}},
		"a string": "plain",
	} {
		want, err := yaml.Marshal(v)
		require.NoError(t, err, name)
		got, err := Marshal(v)
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)
	}
}

// TestJSONToYAMLEscapesTheSet asserts JSONToYAML on a JSON text that holds the
// raw characters writes none of them raw and reads back the original text.
func TestJSONToYAMLEscapesTheSet(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]string{"k": string(refusedRunes)})
	require.NoError(t, err)
	out, err := JSONToYAML(raw)
	require.NoError(t, err)
	got := readBackSigs(t, out, map[string]string{})
	assert.Equal(t, map[string]string{"k": string(refusedRunes)}, got)
}

// TestMarshalWritesNoRawRefusedCharacter asserts the output for every one of
// the 35 holds none of them as a raw character.
func TestMarshalWritesNoRawRefusedCharacter(t *testing.T) {
	t.Parallel()
	for _, r := range refusedRunes {
		out := mustMarshal(t, map[string]string{"k" + string(r): "v" + string(r)})
		assert.False(t, strings.ContainsRune(string(out), r), "%U is written raw: %q", r, out)
	}
}

// TestMarshalReportsAJSONFailure asserts a value JSON cannot encode returns the
// library's error text.
func TestMarshalReportsAJSONFailure(t *testing.T) {
	t.Parallel()
	_, err := Marshal(make(chan int))
	require.ErrorContains(t, err, "error marshaling into JSON")
}
