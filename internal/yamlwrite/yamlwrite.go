// Package yamlwrite writes a value as YAML so that every string in it reads
// back unchanged. The library behind sigs.k8s.io/yaml refuses DEL (U+007F), the
// characters U+0080 to U+009F, U+FFFE and U+FFFF when they appear raw in the
// text it parses, and changes U+0085 into a space. This package writes each of
// them as an escape first: a file written here holds none of them raw and
// every reader that decodes YAML escapes returns the original text. It imports
// the standard library and sigs.k8s.io/yaml.
package yamlwrite

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

// Marshal returns the YAML text of v. It converts v through its JSON form, as
// sigs.k8s.io/yaml does, and returns the same bytes that library returns for a
// value that holds none of the characters the package comment names.
func Marshal(v any) ([]byte, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("error marshaling into JSON: %w", err)
	}
	return JSONToYAML(j)
}

// JSONToYAML converts the JSON text j to YAML, writing each character the
// package comment names as an escape first.
func JSONToYAML(j []byte) ([]byte, error) {
	return yaml.JSONToYAML(escapeRefusedRunes(j))
}

// escapeRefusedRunes returns the JSON text b with DEL, U+0080 to U+009F,
// U+FFFE and U+FFFF each written as a \uXXXX escape, and b itself when none
// occurs. The YAML library that parses the text refuses these characters (it
// changes U+0085 into a space). They occur only inside JSON strings; an escape
// at any position of the text is valid, the library then decodes each one
// exactly and its emitter writes it back as a YAML escape. Every other rune is
// copied as its original bytes, an invalid byte included.
func escapeRefusedRunes(b []byte) []byte {
	var out []byte
	copied := 0
	for i := 0; i < len(b); {
		switch b[i] {
		case 0x7f, 0xc2, 0xef:
		default:
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r != 0x7f && (r < 0x80 || r > 0x9f) && r != 0xfffe && r != 0xffff {
			i += size
			continue
		}
		out = append(out, b[copied:i]...)
		out = fmt.Appendf(out, `\u%04x`, r)
		i += size
		copied = i
	}
	if out == nil {
		return b
	}
	return append(out, b[copied:]...)
}
