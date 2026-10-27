package resources

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// The values of patch's --type.
const (
	patchTypeJSON  = "json"
	patchTypeMerge = "merge"
)

// utf8BOM is the byte-order mark some editors and PowerShell write at the
// start of a UTF-8 file; it is not JSON, and a patch file may carry one.
var utf8BOM = []byte("\xef\xbb\xbf")

// patchDoc is a checked patch: the RFC 6902 ops of a json patch, or the
// RFC 7386 object of a merge patch.
type patchDoc struct {
	ops   jsonpatch.Patch
	merge []byte
}

// isMerge reports whether the patch is an RFC 7386 merge patch.
func (p patchDoc) isMerge() bool { return p.merge != nil }

// apply returns doc with the patch applied.
func (p patchDoc) apply(doc []byte) ([]byte, error) {
	if p.isMerge() {
		return jsonpatch.MergePatch(doc, p.merge)
	}
	return p.ops.Apply(doc)
}

// parsePatch checks raw as a patch of type typ and decodes it. Every
// failure is VALIDATION with a reason that names what was given; none
// quotes a decoder's text.
func parsePatch(raw []byte, typ string) (patchDoc, error) {
	raw = bytes.TrimPrefix(raw, utf8BOM)
	if len(bytes.TrimSpace(raw)) == 0 {
		return patchDoc{}, invalidPatch("the patch is empty")
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		if se, ok := errors.AsType[*json.SyntaxError](err); ok {
			return patchDoc{}, invalidPatch(fmt.Sprintf("not valid JSON at byte %d; the patch must be JSON (YAML is not accepted)", se.Offset))
		}
		return patchDoc{}, invalidPatch("not valid JSON; the patch must be JSON (YAML is not accepted)")
	}
	if key, ok := repeatedKey(raw); ok {
		return patchDoc{}, invalidPatch(fmt.Sprintf("the key %q appears twice in one JSON object; name each key once", key))
	}
	if typ == patchTypeMerge {
		return parseMergePatch(raw)
	}
	return parseJSONPatch(raw)
}

// parseJSONPatch checks the shape of an RFC 6902 patch before the library
// decodes it, so the common mistakes get a plain reason.
func parseJSONPatch(raw []byte) (patchDoc, error) {
	switch class := jsonClass(raw); class {
	case "object":
		var members map[string]json.RawMessage
		_ = json.Unmarshal(raw, &members)
		if jsonClass(members["op"]) == "string" && jsonClass(members["path"]) == "string" {
			return patchDoc{}, invalidPatch("the patch is one RFC 6902 operation; put it inside a JSON array")
		}
		return patchDoc{}, invalidPatch("a JSON object is a merge patch; pass --type merge, or give RFC 6902 operations as a JSON array")
	case "string", "number", "boolean":
		return patchDoc{}, invalidPatch(fmt.Sprintf("a JSON %s is not a patch; give RFC 6902 operations as a JSON array", class))
	case "array":
		var elems []json.RawMessage
		_ = json.Unmarshal(raw, &elems)
		for i, el := range elems {
			if class := jsonClass(el); class != "object" {
				return patchDoc{}, invalidPatch(fmt.Sprintf("patch operation %d is a JSON %s, not an object with \"op\" and \"path\"", i, class))
			}
			var members map[string]json.RawMessage
			_ = json.Unmarshal(el, &members)
			for _, field := range []string{"op", "path", "from"} {
				if v, ok := members[field]; ok && jsonClass(v) != "string" {
					article := "a"
					if field == "op" {
						article = "an"
					}
					return patchDoc{}, invalidPatch(fmt.Sprintf("patch operation %d has %s %q that is not a string", i, article, field))
				}
			}
			if err := checkOpFields(i, members); err != nil {
				return patchDoc{}, err
			}
		}
	}
	// An array of op objects, or null (an empty patch).
	ops, err := jsonpatch.DecodePatch(raw)
	if err != nil {
		return patchDoc{}, invalidPatch(err.Error() + "; expected an RFC 6902 JSON patch array")
	}
	if err := refuseServerOwnedOps(ops); err != nil {
		return patchDoc{}, err
	}
	return patchDoc{ops: ops}, nil
}

// opNeeds lists, for each RFC 6902 op, the member it needs besides op and
// path.
var opNeeds = map[string]string{
	"add":     "value",
	"remove":  "",
	"replace": "value",
	"move":    "from",
	"copy":    "from",
	"test":    "value",
}

// checkOpFields refuses op i when it lacks a member its op needs or names
// an op RFC 6902 does not define. Member types are checked already.
func checkOpFields(i int, members map[string]json.RawMessage) error {
	opRaw, ok := members["op"]
	if !ok {
		return invalidPatch(fmt.Sprintf("patch operation %d has no \"op\"", i))
	}
	var op string
	_ = json.Unmarshal(opRaw, &op)
	need, known := opNeeds[op]
	if !known {
		return invalidPatch(fmt.Sprintf("patch operation %d has the unknown operation %q; use add, remove, replace, move, copy or test", i, op))
	}
	if _, ok := members["path"]; !ok {
		return invalidPatch(fmt.Sprintf("patch operation %d has no \"path\"", i))
	}
	if _, ok := members[need]; need != "" && !ok {
		return invalidPatch(fmt.Sprintf("patch operation %d (%s) has no %q", i, op, need))
	}
	return nil
}

// parseMergePatch checks an RFC 7386 patch: only an object merges into an
// object (the library would make any other value the whole new document),
// and it may not name a field the server owns, by the same rule as the
// op refusal.
func parseMergePatch(raw []byte) (patchDoc, error) {
	switch class := jsonClass(raw); class {
	case "object":
	case "array":
		return patchDoc{}, invalidPatch("a merge patch is a JSON object, not a JSON array; for RFC 6902 operations in a JSON array leave out --type or pass --type json")
	default:
		return patchDoc{}, invalidPatch("a merge patch is a JSON object, not a JSON " + class)
	}
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	for _, keys := range engine.ServerOwnedPaths {
		if mergeNames(obj, keys) {
			return patchDoc{}, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"the merge patch targets %s; the server sets this field and a patch cannot change it", engine.PointerFor(keys)))
		}
	}
	return patchDoc{merge: raw}, nil
}

// mergeNames reports whether the merge patch obj sets or removes the field
// at keys: every key but the last names an object, and the last is present
// (null included, which removes it). A non-object on the way (metadata:
// null) is not this refusal's: it removes metadata.name too, which the
// patched-object check refuses.
func mergeNames(obj map[string]any, keys []string) bool {
	node := obj
	for i, k := range keys {
		v, ok := node[k]
		if !ok {
			return false
		}
		if i == len(keys)-1 {
			return true
		}
		if node, ok = v.(map[string]any); !ok {
			return false
		}
	}
	return false
}

// invalidPatch is the VALIDATION error for a patch that cannot be used.
func invalidPatch(reason string) error {
	return clierr.New(clierr.CodeValidation, "invalid patch: "+reason)
}

// jsonClass names the type of a JSON value by its first byte: object,
// array, string, boolean, null or number. raw is valid JSON.
func jsonClass(raw []byte) string {
	b := bytes.TrimLeft(raw, " \t\r\n")
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

// repeatedKey returns the first key that appears twice in one JSON object
// of raw, which is valid JSON. Decoders keep the last value of a repeated
// key; a patch that repeats one would lose the earlier value silently.
func repeatedKey(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() (string, bool)
	walk = func() (string, bool) {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				keyTok, _ := dec.Token()
				key, _ := keyTok.(string)
				if seen[key] {
					return key, true
				}
				seen[key] = true
				if k, ok := walk(); ok {
					return k, true
				}
			}
			_, _ = dec.Token()
		case json.Delim('['):
			for dec.More() {
				if k, ok := walk(); ok {
					return k, true
				}
			}
			_, _ = dec.Token()
		}
		return "", false
	}
	return walk()
}

// decodePatched reads the patched document as an object with a kind and a
// name. No failure quotes the decoder: the missing-kind error carries the
// whole document, Secret data included.
func decodePatched(patchedJSON []byte) (*unstructured.Unstructured, error) {
	if class := jsonClass(patchedJSON); class != "object" {
		return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"patched object is not valid: the patch replaces the object with a JSON %s; the object must stay a JSON object", class))
	}
	patched := &unstructured.Unstructured{}
	if err := patched.UnmarshalJSON(patchedJSON); err != nil {
		if runtime.IsMissingKind(err) {
			return nil, clierr.New(clierr.CodeValidation, "patched object is not valid: the patched object has no kind; a patch must keep kind")
		}
		return nil, clierr.New(clierr.CodeValidation, "patched object is not valid")
	}
	if name, _, err := unstructured.NestedString(patched.Object, "metadata", "name"); err != nil || name == "" {
		return nil, clierr.New(clierr.CodeValidation, "patched object is not valid: the patched object has no name; a patch must keep metadata.name")
	}
	return patched, nil
}

// mergeListRule ends the dropped-entry warning.
const mergeListRule = "a merge patch replaces a whole list and does not merge its entries"

// droppedListWarnings names each list that a merge patch replaces and that
// loses an entry the fetched object had: an entry of the fetched list
// with no equal entry in the list at the same path of after, the object
// the update would store.
// A list replaced by an object or a single value loses every entry; a
// null at a list's path removes it, which the diff shows. It walks the
// patch through objects only and in key order; an identical list, a pure
// append and a list the object did not have give no warning.
func droppedListWarnings(merge []byte, orig, after map[string]any) []string {
	var patch map[string]any
	if err := json.Unmarshal(merge, &patch); err != nil {
		return nil
	}
	var out []string
	var walk func(node map[string]any, path []string)
	walk = func(node map[string]any, path []string) {
		for _, k := range slices.Sorted(maps.Keys(node)) {
			at := append(slices.Clone(path), k)
			value := node[k]
			before, _, _ := unstructured.NestedFieldNoCopy(orig, at...)
			if beforeList, ok := before.([]any); ok && len(beforeList) > 0 && value != nil {
				stored, _, _ := unstructured.NestedFieldNoCopy(after, at...)
				afterList, _ := stored.([]any)
				dropped := droppedEntries(beforeList, afterList)
				if w := droppedListWarning(dropped, len(beforeList), at); w != "" {
					out = append(out, w)
				}
				continue
			}
			if child, ok := value.(map[string]any); ok {
				walk(child, at)
			}
		}
	}
	walk(patch, nil)
	return out
}

// droppedListWarning is the warning for dropped of total entries of the
// list at path, or "" when none is dropped.
func droppedListWarning(dropped, total int, path []string) string {
	switch {
	case dropped == 0:
		return ""
	case total == 1:
		return fmt.Sprintf("the merge patch removes the only entry of the list at %s; %s", engine.PointerFor(path), mergeListRule)
	}
	return fmt.Sprintf("the merge patch removes %d of %d entries of the list at %s; %s", dropped, total, engine.PointerFor(path), mergeListRule)
}

// droppedEntries counts the entries of before with no equal entry left in
// after, each entry of after matching at most one of before. Entries
// compare by their JSON encoding, so a number read as an integer and the
// same number read as a float are equal; each entry is encoded once.
func droppedEntries(before, after []any) int {
	left := make(map[string]int, len(after))
	for _, a := range after {
		if b, err := json.Marshal(a); err == nil {
			left[string(b)]++
		}
	}
	dropped := 0
	for _, e := range before {
		b, err := json.Marshal(e)
		if err == nil && left[string(b)] > 0 {
			left[string(b)]--
			continue
		}
		dropped++
	}
	return dropped
}
