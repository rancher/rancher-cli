package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// The fixed sentences of a failure the mapping has no reason for. None holds
// text of the JSON patch library.
const (
	patchApplyFallbackFormat  = "patch operation %d (%s) does not apply to the object; check the operation against the object"
	patchApplyFallbackNoIndex = "the patch does not apply to the object; check its operations against the object"
	patchApplyFallbackMerge   = "the merge patch does not apply to the object; check it against the object"
)

// errLibraryPanic stands for a panic of the JSON patch library. It is never
// shown to a user.
var errLibraryPanic = errors.New("the JSON patch library failed")

// guarded runs a call of the JSON patch library and returns a panic of the
// library as errLibraryPanic. The library dereferences a nil pointer when a
// patch of two operations stores a JSON null where a later operation reads a
// list. The panic value is dropped.
func guarded(call func() ([]byte, error)) (out []byte, err error) {
	defer func() {
		if recover() != nil {
			out, err = nil, errLibraryPanic
		}
	}()
	return call()
}

// runOps applies the first k operations of the patch to doc.
func (p patchDoc) runOps(doc []byte, k int) ([]byte, error) {
	return guarded(func() ([]byte, error) { return p.ops[:k].Apply(doc) })
}

// explainApply returns the VALIDATION error for a json patch whose whole run
// on doc failed with wholeErr (or with errLibraryPanic). The library does not
// say which operation failed. The first operation whose prefix of the patch
// fails is found by running prefixes of the patch on doc.
//
// The whole run and a prefix run differ in one place: the result of a prefix
// is marshaled, and a prefix whose result cannot be marshaled fails there.
// An add of null at the whole object is such a prefix. When a later
// operation replaces the whole object again, the whole run passes that point
// and fails at another operation, and the search can name the add of null
// although it is not the failure of the whole patch.
//
// Some texts name a path of the object before an earlier operation of the
// same patch changed it: a path under a list names the entry as it was
// before an earlier operation shifted the indexes, and the texts about a
// list name the list at "" after an earlier replace of the whole object
// with a list or null.
func (p patchDoc) explainApply(doc []byte, wholeErr error) error {
	return explainFirstFailure(p.ops, func(k int) ([]byte, error) { return p.runOps(doc, k) }, wholeErr)
}

// explainFirstFailure finds the failing operation of ops with run, which
// applies the first k operations to the object, and explains it.
func explainFirstFailure(ops jsonpatch.Patch, run func(k int) ([]byte, error), wholeErr error) error {
	lo, cur, ok, err := firstFailingPrefix(len(ops), run, wholeErr)
	if !ok {
		return clierr.New(clierr.CodeValidation, patchApplyFallbackNoIndex)
	}
	return explainOperation(lo, ops[lo], cur, err)
}

// firstFailingPrefix searches for the first prefix of a patch of n operations
// that fails, given that the whole patch failed with wholeErr. It returns lo,
// the number of operations of the longest prefix it saw pass, cur, the output
// of that prefix, and the error of the prefix of lo+1 operations. The failing
// operation is the one at index lo. The search ends with an adjacent
// pair that passes then fails, both run, whatever the runs in between do. ok
// is false when the empty prefix fails or n is 0. It makes about log2(n) + 1
// runs.
func firstFailingPrefix(n int, run func(k int) ([]byte, error), wholeErr error) (lo int, cur []byte, ok bool, err error) {
	if n < 1 {
		return 0, nil, false, wholeErr
	}
	cur, err = run(0)
	if err != nil {
		return 0, nil, false, err
	}
	hi, hiErr := n, wholeErr
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		out, runErr := run(mid)
		if runErr != nil {
			hi, hiErr = mid, runErr
			continue
		}
		lo, cur = mid, out
	}
	return lo, cur, true, hiErr
}

// explainMergeApply returns the VALIDATION error for a merge patch the
// library could not apply. A checked merge patch of an object into a fetched
// object does not fail. The sentence exists for a change of the library.
func explainMergeApply(_ error) error {
	return clierr.New(clierr.CodeValidation, patchApplyFallbackMerge)
}

// explainOperation returns the VALIDATION error for operation i, which failed
// with err on cur, the object as it was before the operation. It decides from
// the operation itself and from the sentinels and types of the library, never
// from the text of its errors. A failure it has no reason for gets the fixed
// sentence. Every path and from it reads is a JSON Pointer: the patch is
// refused when it is read otherwise.
func explainOperation(i int, op jsonpatch.Operation, cur []byte, err error) *clierr.Error {
	kind := op.Kind()
	fallback := clierr.New(clierr.CodeValidation, fmt.Sprintf(patchApplyFallbackFormat, i, kind))
	if errors.Is(err, errLibraryPanic) {
		return fallback
	}
	path, pathErr := op.Path()
	if pathErr != nil {
		return fallback
	}
	moveOrCopy := kind == "move" || kind == "copy"
	from := ""
	if moveOrCopy {
		var fromErr error
		if from, fromErr = op.From(); fromErr != nil {
			return fallback
		}
	}
	reason := func(text string) *clierr.Error {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf("patch operation %d (%s) does not apply: %s", i, kind, text))
	}

	switch {
	case kind == "remove" && path == "":
		return reason("a patch cannot delete the whole object; give a path to a field of it to remove")
	case kind == "move" && from == "" && errors.Is(err, jsonpatch.ErrInvalid):
		return reason(`the whole object cannot be moved; give "from" a path below it`)
	case (kind == "add" || kind == "replace") && path == "":
		// The value is read from its raw bytes: ValueInterface returns a
		// number as a type of the library's own json package.
		raw := []byte("null")
		if v := op["value"]; v != nil {
			raw = []byte(*v)
		}
		if class := jsonClass(raw); class == "number" || class == "string" || class == "boolean" || class == "null" {
			return patchedNotObject(class)
		}
	case kind == "move" && from != "" && strings.HasPrefix(path, from+"/"):
		return reason(fmt.Sprintf("%q cannot move into one of its own children; give \"path\" a path outside %q", from, from))
	case kind == "move" && strings.HasSuffix(from, "/"):
		// The library reads the container before the slash and then removes
		// its key "". The source failed when that container is missing or an
		// object with no such key. When the object has the key, the source
		// passed and the failure is the destination's. For a list, the
		// error does not say which side failed.
		container, found := resolveValue(cur, parentOf(from))
		holder, isObject := container.(map[string]any)
		_, hasKey := holder[""]
		_, isList := container.([]any)
		switch {
		case found && isObject && hasKey:
		case errors.Is(err, jsonpatch.ErrMissing) && (!found || !isList):
			return reason(noValueText(kind, from))
		default:
			return fallback
		}
	case errors.Is(err, jsonpatch.ErrTestFailed):
		if path == "" {
			return reason(`the whole object differs from the value in the "value" member`)
		}
		return reason(fmt.Sprintf(`the value at %q differs from the value in the "value" member, or there is no value there`, path))
	}

	sourceFailed := moveOrCopy && !hasValue(cur, from)
	numErr, isNumErr := errors.AsType[*strconv.NumError](err)
	switch {
	case errors.Is(err, jsonpatch.ErrInvalidIndex), isNumErr && errors.Is(numErr.Err, strconv.ErrRange):
		return indexRangeReason(reason, kind, from, path, sourceFailed, fallback)
	case isNumErr:
		failing, putsValue := indexFailure(kind, from, path, sourceFailed)
		if failing == "" {
			return fallback
		}
		list, index := parentOf(failing), lastSegment(failing)
		tail := "use a whole number"
		if putsValue {
			tail += ", or - to append"
		}
		return reason(fmt.Sprintf("the list at %q takes a whole number as index, not %q; %s", list, index, tail))
	case errors.Is(err, jsonpatch.ErrMissing):
		switch {
		case sourceFailed:
			return reason(noValueText(kind, from))
		case kind == "replace" || kind == "remove" || kind == "test":
			return reason(noValueText(kind, path))
		case hasEmptyMiddle(path):
			// The library reads nothing at an empty segment in the middle
			// of a path. No parent of the path is a container for it.
			return fallback
		default:
			return reason(noContainerText(kind, path))
		}
	}
	return fallback
}

// parentOf returns the pointer as the user typed it, up to its last slash:
// the path of the list or object that holds the last segment.
func parentOf(pointer string) string {
	parent, _, found := strings.CutLast(pointer, "/")
	if !found {
		return ""
	}
	return parent
}

// lastSegment returns the unescaped key after the last slash of the pointer.
func lastSegment(pointer string) string {
	segments := engine.SplitPointer(pointer)
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

// indexFailure returns the path whose last segment is the index that failed,
// and whether the operation puts a value there.
func indexFailure(kind, from, path string, sourceFailed bool) (failing string, putsValue bool) {
	if sourceFailed {
		return from, false
	}
	return path, kind == "add" || kind == "move" || kind == "copy"
}

// indexRangeReason explains an index of a list that is out of range.
func indexRangeReason(reason func(string) *clierr.Error, kind, from, path string, sourceFailed bool, fallback *clierr.Error) *clierr.Error {
	failing, putsValue := indexFailure(kind, from, path, sourceFailed)
	if failing == "" {
		return fallback
	}
	list, index := parentOf(failing), lastSegment(failing)
	if !putsValue || strings.HasPrefix(index, "-") {
		return reason(fmt.Sprintf("the list at %q has no entry at index %s; check the index against the length of the list", list, index))
	}
	if kind == "move" {
		if parentOf(from) == list {
			return reason(fmt.Sprintf("index %s is past the end of the list at %q after the entry at %q is taken out of it; move then accepts an index up to the length of the list, or - to append", index, list, from))
		}
	}
	return reason(fmt.Sprintf("index %s is past the end of the list at %q; %s accepts an index up to the length of the list, or - to append", index, list, kind))
}

// noValueText is the text for an operation that needs a value at path and
// finds none.
func noValueText(kind, path string) string {
	var remedy string
	switch kind {
	case "replace":
		remedy = "replace changes only a value that exists"
	case "move", "copy":
		remedy = `check the "from" path against the object`
	default:
		remedy = "check the path against the object"
	}
	return fmt.Sprintf("the object has no value at %q to %s; %s", path, kind, remedy)
}

// hasEmptyMiddle reports whether a segment of the pointer that is neither the
// first nor the last is empty. The library reads the container itself for an
// empty first or last segment, and nothing for any other empty segment.
func hasEmptyMiddle(pointer string) bool {
	segments := engine.SplitPointer(pointer)
	for i, segment := range segments {
		if segment == "" && i > 0 && i < len(segments)-1 {
			return true
		}
	}
	return false
}

// noContainerText is the text for an operation that puts a value at path
// when the parent of path is no object or list.
func noContainerText(kind, path string) string {
	return fmt.Sprintf("there is no object or list at %q to put a value in; %s puts a value only inside an existing object or list", parentOf(path), kind)
}

// hasValue reports whether the JSON Pointer resolves to a value in doc, as
// the library reads a source: each segment names a key of an object, or an
// index of a list where a negative index counts from the end. The segment -
// never resolves. An empty first or last segment names the object or list it
// is read in; an empty segment in the middle resolves to nothing. The library
// does the same, because only the top-level container knows itself.
func hasValue(doc []byte, pointer string) bool {
	_, ok := resolveValue(doc, pointer)
	return ok
}

// resolveValue returns the value hasValue finds at the pointer in doc.
func resolveValue(doc []byte, pointer string) (any, bool) {
	var node any
	if json.Unmarshal(doc, &node) != nil {
		return nil, false
	}
	segments := engine.SplitPointer(pointer)
	for i, segment := range segments {
		inMiddle := i > 0 && i < len(segments)-1
		switch v := node.(type) {
		case map[string]any:
			if segment == "" {
				if inMiddle {
					return nil, false
				}
				continue
			}
			next, ok := v[segment]
			if !ok {
				return nil, false
			}
			node = next
		case []any:
			if segment == "" {
				if inMiddle {
					return nil, false
				}
				continue
			}
			index, err := strconv.Atoi(segment)
			if err != nil {
				return nil, false
			}
			if index < 0 {
				index += len(v)
			}
			if index < 0 || index >= len(v) {
				return nil, false
			}
			node = v[index]
		default:
			return nil, false
		}
	}
	return node, true
}
