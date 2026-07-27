package output

import (
	"bytes"
	"encoding/json"
)

const managedFieldsKey = "managedFields"

// scrubbedTree marshals v (envelope or bare value) compactly for DETECTION
// ONLY — the bytes are never written to output. When the literal managedFields
// key is absent it reports hit=false: callers fall through to today's encoders,
// so unaffected commands change in no way at all (MF-2). When present, it
// returns the decoded tree (UseNumber, so large ints re-encode verbatim) with
// managedFields removed from every metadata map.
func scrubbedTree(v any) (any, bool, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false, err
	}
	if !bytes.Contains(b, []byte(`"`+managedFieldsKey+`"`)) {
		return nil, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, false, err
	}
	scrubManagedFields(tree)
	return tree, true, nil
}

// scrubManagedFields deletes managedFields wherever it sits inside a
// metadata map — and only there, so a same-named key in user data
// survives (MF-2, MF-6). Recurses through all maps and slices: named
// objects, bare list arrays, and objects nested in composite documents
// are all reached through the one rule.
func scrubManagedFields(v any) {
	switch t := v.(type) {
	case map[string]any:
		if meta, ok := t["metadata"].(map[string]any); ok {
			delete(meta, managedFieldsKey)
		}
		for _, child := range t {
			scrubManagedFields(child)
		}
	case []any:
		for _, item := range t {
			scrubManagedFields(item)
		}
	}
}
