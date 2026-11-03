package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// patchApplyObject is the ConfigMap of the patch tests with a top-level list,
// a key whose name is the empty string, and a key with a tilde that is not an
// escape.
var patchApplyObject = strings.Replace(configMapJSON, `"data":`, `"items":[1,2,3],"e":{"":[1],"k":2},"l~2":[1],"data":`, 1)

func quoted(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func opAdd(path, value string) string {
	return `{"op":"add","path":` + quoted(path) + `,"value":` + value + `}`
}

func opReplace(path, value string) string {
	return `{"op":"replace","path":` + quoted(path) + `,"value":` + value + `}`
}

func opTest(path, value string) string {
	return `{"op":"test","path":` + quoted(path) + `,"value":` + value + `}`
}

func opRemove(path string) string { return `{"op":"remove","path":` + quoted(path) + `}` }

func opMove(from, path string) string {
	return `{"op":"move","from":` + quoted(from) + `,"path":` + quoted(path) + `}`
}

func opCopy(from, path string) string {
	return `{"op":"copy","from":` + quoted(from) + `,"path":` + quoted(path) + `}`
}

func patchOf(ops ...string) string { return "[" + strings.Join(ops, ",") + "]" }

// quotedText matches a double-quoted string with Go escapes.
var quotedText = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// assertPlain asserts that message holds no text of the JSON patch library,
// no format artifact and no panic text. A path, an index and a member name
// are the user's own text and may hold any characters. Every quoted string
// is removed first.
func assertPlain(t *testing.T, message string) {
	t.Helper()
	rest := quotedText.ReplaceAllString(message, "")
	for _, bad := range []string{
		"%!", "<nil>", "jsonpatch", "Go value", "lazyNode", "strconv", "invalid index referenced",
		"missing value", "test failed", "invalid state", "nonexistent key", "doc is missing", "Unable to",
		"unable to", "json:", "panic", "runtime error", "goroutine",
	} {
		assert.NotContains(t, rest, bad, message)
	}
}

func applyMessage(i int, op, text string) string {
	return fmt.Sprintf("patch operation %d (%s) does not apply: %s", i, op, text)
}

func noValueAt(path, verb, remedy string) string {
	return fmt.Sprintf("the object has no value at %q to %s; %s", path, verb, remedy)
}

func noContainerAt(path, op string) string {
	return fmt.Sprintf("there is no object or list at %q to put a value in; %s puts a value only inside an existing object or list", path, op)
}

func notAnObject(class string) string {
	return "patched object is not valid: the patch replaces the object with a JSON " + class + "; the object must stay a JSON object"
}

func pastTheEnd(index, list, op string) string {
	return fmt.Sprintf("index %s is past the end of the list at %q; %s accepts an index up to the length of the list, or - to append", index, list, op)
}

func noEntryAt(list, index string) string {
	return fmt.Sprintf("the list at %q has no entry at index %s; check the index against the length of the list", list, index)
}

func notAWholeNumber(list, index, tail string) string {
	return fmt.Sprintf("the list at %q takes a whole number as index, not %q; %s", list, index, tail)
}

const (
	replaceRemedy  = "replace changes only a value that exists"
	pathRemedy     = "check the path against the object"
	fromRemedy     = `check the "from" path against the object`
	appendRemedy   = "use a whole number, or - to append"
	wholeNumberFix = "use a whole number"
	testDiffers    = `the value in the "value" member`
)

var (
	libMissing      = func(err error) bool { return errors.Is(err, jsonpatch.ErrMissing) }
	libTestFailed   = func(err error) bool { return errors.Is(err, jsonpatch.ErrTestFailed) }
	libInvalid      = func(err error) bool { return errors.Is(err, jsonpatch.ErrInvalid) }
	libInvalidIndex = func(err error) bool { return errors.Is(err, jsonpatch.ErrInvalidIndex) }
	libRange        = func(err error) bool {
		ne, ok := errors.AsType[*strconv.NumError](err)
		return ok && errors.Is(ne.Err, strconv.ErrRange)
	}
	libNotANumber = func(err error) bool {
		ne, ok := errors.AsType[*strconv.NumError](err)
		return ok && !errors.Is(ne.Err, strconv.ErrRange)
	}
	libPanic = func(err error) bool { return errors.Is(err, errLibraryPanic) }
	// libNoReason matches the errors of a replace of the whole object: the
	// library gives no sentinel for a value that is not an object, except the
	// one an add of null reaches through its marshaler.
	libNoReason = func(err error) bool {
		if errors.Is(err, jsonpatch.ErrExpectedObject) {
			return true
		}
		for _, s := range []error{jsonpatch.ErrMissing, jsonpatch.ErrTestFailed, jsonpatch.ErrInvalid, jsonpatch.ErrInvalidIndex} {
			if errors.Is(err, s) {
				return false
			}
		}
		_, isNumber := errors.AsType[*strconv.NumError](err)
		return !isNumber && !errors.Is(err, errLibraryPanic)
	}
)

// libraryResult runs patch on the fixture object through guarded, as the
// code under test calls the library.
func libraryResult(t *testing.T, patch string) error {
	t.Helper()
	ops, err := jsonpatch.DecodePatch([]byte(patch))
	require.NoError(t, err)
	doc, err := objFromJSON(t, patchApplyObject).MarshalJSON()
	require.NoError(t, err)
	_, err = guarded(func() ([]byte, error) { return ops.Apply(doc) })
	return err
}

type applyRow struct {
	name  string
	patch string
	want  string
	lib   func(error) bool
}

func applyRows() []applyRow {
	var rows []applyRow
	add := func(lib func(error) bool, name, patch, want string) {
		rows = append(rows, applyRow{name, patch, want, lib})
	}
	one := func(op string) string { return patchOf(op) }

	add(libMissing, "replace of a missing key", one(opReplace("/data/zz", `"1"`)),
		applyMessage(0, "replace", noValueAt("/data/zz", "replace", replaceRemedy)))
	add(libMissing, "replace below a missing parent", one(opReplace("/nope/zz", `"1"`)),
		applyMessage(0, "replace", noValueAt("/nope/zz", "replace", replaceRemedy)))
	add(libMissing, "replace of an index past the end", one(opReplace("/items/9", `"1"`)),
		applyMessage(0, "replace", noValueAt("/items/9", "replace", replaceRemedy)))
	add(libMissing, "remove of a missing key", one(opRemove("/data/zz")),
		applyMessage(0, "remove", noValueAt("/data/zz", "remove", pathRemedy)))
	add(libMissing, "remove below a missing parent", one(opRemove("/nope/zz")),
		applyMessage(0, "remove", noValueAt("/nope/zz", "remove", pathRemedy)))
	add(libMissing, "test below a missing parent", one(opTest("/nope/zz", `1`)),
		applyMessage(0, "test", noValueAt("/nope/zz", "test", pathRemedy)))
	add(libMissing, "move from a missing key", one(opMove("/data/zz", "/data/y")),
		applyMessage(0, "move", noValueAt("/data/zz", "move", fromRemedy)))
	add(libMissing, "copy from a missing parent", one(opCopy("/nope/zz", "/data/y")),
		applyMessage(0, "copy", noValueAt("/nope/zz", "copy", fromRemedy)))

	add(libMissing, "add below a missing parent", one(opAdd("/nope/x", `1`)),
		applyMessage(0, "add", noContainerAt("/nope", "add")))
	add(libMissing, "add below a string", one(opAdd("/data/a/x", `1`)),
		applyMessage(0, "add", noContainerAt("/data/a", "add")))
	add(libMissing, "move to a missing parent", one(opMove("/data/a", "/nope/y")),
		applyMessage(0, "move", noContainerAt("/nope", "move")))
	add(libMissing, "copy to a missing parent", one(opCopy("/data/a", "/nope/y")),
		applyMessage(0, "copy", noContainerAt("/nope", "copy")))

	ownChild := func(from string) string {
		return fmt.Sprintf("%q cannot move into one of its own children; give \"path\" a path outside %q", from, from)
	}
	add(libMissing, "move into the own child", one(opMove("/data", "/data/zz")), applyMessage(0, "move", ownChild("/data")))
	add(libMissing, "move of a string into itself", one(opMove("/data/a", "/data/a/x")), applyMessage(0, "move", ownChild("/data/a")))

	whole := `the whole object differs from ` + testDiffers
	value := func(path string) string {
		return fmt.Sprintf("the value at %q differs from %s, or there is no value there", path, testDiffers)
	}
	add(libTestFailed, "test of another value", one(opTest("/data/a", `"zzz"`)), applyMessage(0, "test", value("/data/a")))
	add(libTestFailed, "test of a missing key", one(opTest("/data/zz", `"zzz"`)), applyMessage(0, "test", value("/data/zz")))
	add(libTestFailed, "test of the whole object", one(opTest("", `{"a":1}`)), applyMessage(0, "test", whole))

	add(libInvalidIndex, "add past the end", one(opAdd("/items/9", `1`)), applyMessage(0, "add", pastTheEnd("9", "/items", "add")))
	add(libInvalidIndex, "copy to past the end", one(opCopy("/data/a", "/items/9")), applyMessage(0, "copy", pastTheEnd("9", "/items", "copy")))
	add(libInvalidIndex, "move to past the end", one(opMove("/data/a", "/items/9")), applyMessage(0, "move", pastTheEnd("9", "/items", "move")))

	afterTaken := func(index, from string) string {
		return fmt.Sprintf("index %s is past the end of the list at \"/items\" after the entry at %q is taken out of it; move then accepts an index up to the length of the list, or - to append", index, from)
	}
	add(libInvalidIndex, "move to the end of its own list", one(opMove("/items/2", "/items/3")), applyMessage(0, "move", afterTaken("3", "/items/2")))
	add(libInvalidIndex, "move from the start to past its own list", one(opMove("/items/0", "/items/3")), applyMessage(0, "move", afterTaken("3", "/items/0")))

	add(libRange, "add at a huge index", one(opAdd("/items/99999999999999999999", `1`)),
		applyMessage(0, "add", pastTheEnd("99999999999999999999", "/items", "add")))
	add(libRange, "test at a huge index", one(opTest("/items/99999999999999999999", `1`)),
		applyMessage(0, "test", noEntryAt("/items", "99999999999999999999")))
	add(libRange, "remove at a huge negative index", one(opRemove("/items/-99999999999999999999")),
		applyMessage(0, "remove", noEntryAt("/items", "-99999999999999999999")))
	add(libInvalidIndex, "add at a negative index", one(opAdd("/items/-9", `1`)), applyMessage(0, "add", noEntryAt("/items", "-9")))
	add(libInvalidIndex, "remove past the end", one(opRemove("/items/9")), applyMessage(0, "remove", noEntryAt("/items", "9")))
	add(libInvalidIndex, "remove at a negative index", one(opRemove("/items/-9")), applyMessage(0, "remove", noEntryAt("/items", "-9")))
	add(libInvalidIndex, "test past the end", one(opTest("/items/9", `1`)), applyMessage(0, "test", noEntryAt("/items", "9")))
	add(libInvalidIndex, "copy from past the end", one(opCopy("/items/9", "/data/y")), applyMessage(0, "copy", noEntryAt("/items", "9")))
	add(libInvalidIndex, "move from past the end", one(opMove("/items/9", "/data/y")), applyMessage(0, "move", noEntryAt("/items", "9")))

	add(libNotANumber, "add at a word", one(opAdd("/items/abc", `1`)), applyMessage(0, "add", notAWholeNumber("/items", "abc", appendRemedy)))
	add(libNotANumber, "move to a word", one(opMove("/data/a", "/items/abc")), applyMessage(0, "move", notAWholeNumber("/items", "abc", appendRemedy)))
	add(libNotANumber, "remove at a word", one(opRemove("/items/abc")), applyMessage(0, "remove", notAWholeNumber("/items", "abc", wholeNumberFix)))
	add(libNotANumber, "remove at a dash", one(opRemove("/items/-")), applyMessage(0, "remove", notAWholeNumber("/items", "-", wholeNumberFix)))
	add(libNotANumber, "test at a word", one(opTest("/items/abc", `1`)), applyMessage(0, "test", notAWholeNumber("/items", "abc", wholeNumberFix)))

	for _, tc := range []struct{ op, value, class string }{
		{"replace", `5`, "number"}, {"replace", `"s"`, "string"}, {"replace", `true`, "boolean"},
		{"add", `5`, "number"}, {"add", `"s"`, "string"}, {"add", `null`, "null"},
	} {
		op := opReplace("", tc.value)
		if tc.op == "add" {
			op = opAdd("", tc.value)
		}
		add(libNoReason, tc.op+" of the whole object with "+tc.class, one(op), notAnObject(tc.class))
	}

	add(libMissing, "remove of the whole object", one(opRemove("")),
		applyMessage(0, "remove", "a patch cannot delete the whole object; give a path to a field of it to remove"))
	add(libInvalid, "move of the whole object", one(opMove("", "/data/y")),
		applyMessage(0, "move", `the whole object cannot be moved; give "from" a path below it`))

	add(libMissing, "the second operation fails", patchOf(opAdd("/data/c", `"3"`), opRemove("/data/zz")),
		applyMessage(1, "remove", noValueAt("/data/zz", "remove", pathRemedy)))
	add(libMissing, "a path with control characters and a percent", one(opRemove("/data/\u001b[2J%s%d")),
		applyMessage(0, "remove", noValueAt("/data/\u001b[2J%s%d", "remove", pathRemedy)))

	add(libMissing, "copy from a path with an empty segment before the last", one(opCopy("/e//0", "/data/y")),
		applyMessage(0, "copy", noValueAt("/e//0", "copy", fromRemedy)))
	add(libInvalidIndex, "copy from the whole object by a slash to past the end", one(opCopy("/", "/items/9")),
		applyMessage(0, "copy", pastTheEnd("9", "/items", "copy")))
	add(libMissing, "move from a container by a trailing slash", one(opMove("/data/", "/items/9")),
		applyMessage(0, "move", noValueAt("/data/", "move", fromRemedy)))
	// The container has a key "". The library removes it, and the failure is
	// at the destination.
	add(libMissing, "move from a string by a trailing slash", one(opMove("/data/a/", "/data/y")),
		applyMessage(0, "move", noValueAt("/data/a/", "move", fromRemedy)))
	add(libMissing, "move from a container with an empty key by a trailing slash", one(opMove("/e/", "/nope/y")),
		applyMessage(0, "move", noContainerAt("/nope", "move")))
	add(libInvalidIndex, "move from a container with an empty key to past the end", one(opMove("/e/", "/items/9")),
		applyMessage(0, "move", pastTheEnd("9", "/items", "move")))
	add(libNotANumber, "move from a list by a trailing slash", one(opMove("/items/", "/data/y")),
		fmt.Sprintf(patchApplyFallbackFormat, 0, "move"))
	// An empty segment that is not the first or the last reads nothing in
	// the library. The source is the side that fails.
	for _, from := range []string{"/e//k", "/data//a", "/items//0", "///data"} {
		add(libMissing, "copy from "+from+" with an empty segment in the middle", one(opCopy(from, "/data/y")),
			applyMessage(0, "copy", noValueAt(from, "copy", fromRemedy)))
	}
	add(libMissing, "add below a path with an empty segment in the middle", one(opAdd("/e//0", `1`)),
		fmt.Sprintf(patchApplyFallbackFormat, 0, "add"))
	add(libNotANumber, "add at a word in a list whose name holds a tilde", one(opAdd("/l~2/abc", `1`)),
		applyMessage(0, "add", notAWholeNumber("/l~2", "abc", appendRemedy)))
	add(libInvalidIndex, "remove past the end of a list whose name holds a tilde", one(opRemove("/l~2/9")),
		applyMessage(0, "remove", noEntryAt("/l~2", "9")))
	add(libMissing, "add below a missing parent whose name ends in a tilde", one(opAdd("/nope~/x", `1`)),
		applyMessage(0, "add", noContainerAt("/nope~", "add")))
	// The search names the first prefix that fails. A whole object replaced
	// by null fails as a prefix, and a later operation that replaces it again
	// hides that. The message names the null although the whole run fails at
	// the third operation.
	add(libMissing, "an add of null at the whole object that a later add replaces",
		patchOf(opAdd("", `null`), opAdd("", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app","namespace":"default"},"data":{"a":"1"}}`), opRemove("/data/zz")),
		notAnObject("null"))
	add(libTestFailed, "a test fails after a null is added", patchOf(opAdd("/data", `null`), opTest("/data", `null`)),
		applyMessage(1, "test", value("/data")))

	fallback := func(i int, op string) string {
		return fmt.Sprintf(patchApplyFallbackFormat, i, op)
	}
	add(libPanic, "a list at the whole object, then a test against null", patchOf(opAdd("", `[]`), opTest("", `null`)), fallback(1, "test"))
	add(libPanic, "null at a path, then a test against a list", patchOf(opAdd("/data", `null`), opTest("/data", `[]`)), fallback(1, "test"))
	add(libPanic, "the object replaced by null, then a copy", patchOf(opReplace("", `null`), opCopy("/", "/data")), fallback(1, "copy"))
	return rows
}

// TestPatchApplyErrorsHavePlainReasons asserts, row by row through the whole
// command, the exact message of every kind of operation that does not apply
// to the fetched object, the VALIDATION code and exit code 2, that nothing is
// sent to the server, and that the library fails on the row in the way the
// row's reason depends on: a library change that moves an error to another
// sentinel fails the row.
func TestPatchApplyErrorsHavePlainReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range applyRows() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			libErr := libraryResult(t, tc.patch)
			require.Error(t, libErr)
			assert.True(t, tc.lib(libErr), "the library error does not match the row: %v", libErr)

			_, client, err := planTyped(t, "configmap", "app", patchApplyObject, tc.patch, "")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "want a clierr.Error, got %v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, 2, ce.ExitCode())
			assert.Equal(t, tc.want, ce.Message)
			assertPlain(t, ce.Message)
			assert.Empty(t, client.updates)
		})
	}
}

// TestPatchApplyReplaceOfTheWholeObjectWithNullOrAListIsRefusedAfterApply
// asserts that the library accepts a replace of the whole object by null or a
// list, and that the check of the patched object refuses the result.
func TestPatchApplyReplaceOfTheWholeObjectWithNullOrAListIsRefusedAfterApply(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ value, class string }{{`null`, "null"}, {`[1]`, "array"}} {
		t.Run(tc.class, func(t *testing.T) {
			t.Parallel()
			patch := patchOf(opReplace("", tc.value))
			require.NoError(t, libraryResult(t, patch))
			_, client, err := planTyped(t, "configmap", "app", patchApplyObject, patch, "")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "want a clierr.Error, got %v", err)
			assert.Equal(t, notAnObject(tc.class), ce.Message)
			assert.Empty(t, client.updates)
		})
	}
}

// TestPatchApplyNamesTheOperationBeforeTheFinalMarshal asserts the failing
// operation is the first prefix of the patch that fails: a patch that adds
// null at the whole object fails at its first operation even though the
// library reports the second. Both are real failures; the message names the
// first.
func TestPatchApplyNamesTheOperationBeforeTheFinalMarshal(t *testing.T) {
	t.Parallel()
	patch := patchOf(opAdd("", `null`), opAdd("", `1`))
	libErr := libraryResult(t, patch)
	require.Error(t, libErr)
	assert.True(t, libNoReason(libErr), "%v", libErr)

	_, client, err := planTyped(t, "configmap", "app", patchApplyObject, patch, "")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, notAnObject("null"), ce.Message)
	assert.Empty(t, client.updates)

	p, perr := parsePatch([]byte(patch), patchTypeJSON)
	require.NoError(t, perr)
	doc, derr := objFromJSON(t, patchApplyObject).MarshalJSON()
	require.NoError(t, derr)
	_, wholeErr := p.runOps(doc, len(p.ops))
	require.Error(t, wholeErr)
	lo, _, ok, _ := firstFailingPrefix(len(p.ops), func(k int) ([]byte, error) { return p.runOps(doc, k) }, wholeErr)
	assert.True(t, ok)
	assert.Equal(t, 0, lo)
}

// TestPatchApplyFallback asserts that an error the mapping does not know, a
// formatting artifact of the library and a recovered panic all give the
// indexed fixed sentence and none of the error's text, and that a merge patch
// that fails gives its own fixed sentence.
func TestPatchApplyFallback(t *testing.T) {
	t.Parallel()
	ops, err := jsonpatch.DecodePatch([]byte(patchOf(opAdd("/data/x", `1`))))
	require.NoError(t, err)
	doc, err := objFromJSON(t, patchApplyObject).MarshalJSON()
	require.NoError(t, err)
	want := fmt.Sprintf(patchApplyFallbackFormat, 3, "add")

	for _, libErr := range []error{
		errors.New("boom"),
		fmt.Errorf("x: %w", error(nil)),
		jsonpatch.NewAccumulatedCopySizeError(1, 2),
		errors.New("json: cannot unmarshal number into Go value of type map[string]*jsonpatch.lazyNode"),
		errLibraryPanic,
	} {
		ce := explainOperation(3, ops[0], doc, libErr)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, want, ce.Message)
		assert.NotContains(t, ce.Message, libErr.Error())
		assertPlain(t, ce.Message)
	}

	for _, libErr := range []error{jsonpatch.ErrBadJSONDoc, errors.New("boom")} {
		var ce *clierr.Error
		require.ErrorAs(t, explainMergeApply(libErr), &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "the merge patch does not apply to the object; check it against the object", ce.Message)
		assertPlain(t, ce.Message)
	}
}

// TestPatchApplyFirstFailingPrefix asserts the search for the first failing
// prefix with a fake run: the result is an adjacent pair that passes then
// fails, both observed, whatever the runs in between do; the error is the
// one of the failing prefix, or the given one when only the whole patch
// fails; a failing empty prefix is reported; and a patch of 1000 operations
// takes at most 12 runs.
func TestPatchApplyFirstFailingPrefix(t *testing.T) {
	t.Parallel()
	whole := errors.New("whole")
	runner := func(fails func(k int) bool, seen *[]int) func(int) ([]byte, error) {
		return func(k int) ([]byte, error) {
			*seen = append(*seen, k)
			if fails(k) {
				return nil, fmt.Errorf("prefix %d", k)
			}
			return []byte(strconv.Itoa(k)), nil
		}
	}

	t.Run("fails from 3 on", func(t *testing.T) {
		t.Parallel()
		var seen []int
		lo, cur, ok, err := firstFailingPrefix(5, runner(func(k int) bool { return k >= 3 }, &seen), whole)
		require.True(t, ok)
		assert.Equal(t, 2, lo)
		assert.Equal(t, "2", string(cur))
		require.EqualError(t, err, "prefix 3")
	})
	t.Run("only the whole patch fails", func(t *testing.T) {
		t.Parallel()
		var seen []int
		lo, cur, ok, err := firstFailingPrefix(2, runner(func(k int) bool { return k >= 2 }, &seen), whole)
		require.True(t, ok)
		assert.Equal(t, 1, lo)
		assert.Equal(t, "1", string(cur))
		assert.Same(t, whole, err)
	})
	t.Run("fails at 3 and 5 and passes at 4", func(t *testing.T) {
		t.Parallel()
		var seen []int
		lo, _, ok, err := firstFailingPrefix(8, runner(func(k int) bool { return k == 3 || k >= 5 }, &seen), whole)
		require.True(t, ok)
		assert.Contains(t, seen, lo, "the passing prefix was run")
		assert.Contains(t, seen, lo+1, "the failing prefix was run")
		assert.Equal(t, 4, lo)
		require.EqualError(t, err, "prefix 5")
	})
	t.Run("fails at 1", func(t *testing.T) {
		t.Parallel()
		var seen []int
		lo, _, ok, _ := firstFailingPrefix(4, runner(func(k int) bool { return k >= 1 }, &seen), whole)
		require.True(t, ok)
		assert.Equal(t, 0, lo)
	})
	t.Run("the empty prefix fails", func(t *testing.T) {
		t.Parallel()
		var seen []int
		_, _, ok, _ := firstFailingPrefix(4, runner(func(int) bool { return true }, &seen), whole)
		assert.False(t, ok)

		ops, err := jsonpatch.DecodePatch([]byte(patchOf(opAdd("/data/x", `1`), opAdd("/data/y", `1`))))
		require.NoError(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, explainFirstFailure(ops, runner(func(int) bool { return true }, &seen), whole), &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "the patch does not apply to the object; check its operations against the object", ce.Message)
		assertPlain(t, ce.Message)
	})
	t.Run("a thousand operations", func(t *testing.T) {
		t.Parallel()
		var seen []int
		lo, _, ok, _ := firstFailingPrefix(1000, runner(func(k int) bool { return k >= 700 }, &seen), whole)
		require.True(t, ok)
		assert.Equal(t, 699, lo)
		assert.LessOrEqual(t, len(seen), 12)
	})
}

// TestPatchApplyPanicIsRecovered asserts guarded turns a panic into the
// internal error and passes a value and an error through unchanged.
func TestPatchApplyPanicIsRecovered(t *testing.T) {
	t.Parallel()
	out, err := guarded(func() ([]byte, error) { panic("x") })
	assert.Nil(t, out)
	require.ErrorIs(t, err, errLibraryPanic)

	boom := errors.New("boom")
	out, err = guarded(func() ([]byte, error) { return []byte("v"), boom })
	assert.Equal(t, "v", string(out))
	assert.Same(t, boom, err)
}

// patchApplySpacePaths are the paths and sources of the combination tests.
var patchApplySpacePaths = []string{"", "/", "/data", "/data/zz", "/data/a", "/data/a/x", "/items", "/items/0", "/items/9", "/items/-",
	"/items/-1", "/items/-9", "/items/abc", "/nope/x", "x"}

// patchApplySpace returns every single operation the combination tests run:
// each operation name with each path, for move and copy each source, and for
// add, replace and test each value.
func patchApplySpace() []string {
	paths := patchApplySpacePaths
	values := []string{`1`, `"s"`, `true`, `null`, `{}`, `[]`}
	var out []string
	for _, p := range paths {
		out = append(out, opRemove(p))
		for _, v := range values {
			out = append(out, opAdd(p, v), opReplace(p, v), opTest(p, v))
		}
		for _, f := range paths {
			out = append(out, opMove(f, p), opCopy(f, p))
		}
	}
	return out
}

// reasonPatterns holds one regular expression for each kind of reason that
// names an operation, and the one for a whole object replaced by a value
// that is not an object, written from the texts of the mapping.
var reasonPatterns = map[string]*regexp.Regexp{
	"no value":      regexp.MustCompile(`^patch operation \d+ \(\w+\) does not apply: the object has no value at ".*" to (replace|remove|test|move|copy); `),
	"no container":  regexp.MustCompile(`^patch operation \d+ \(\w+\) does not apply: there is no object or list at ".*" to put a value in; \w+ puts a value only inside an existing object or list$`),
	"test failed":   regexp.MustCompile(`^patch operation \d+ \(test\) does not apply: (the value at ".*" differs from the value in the "value" member, or there is no value there|the whole object differs from the value in the "value" member)$`),
	"index range":   regexp.MustCompile(`^patch operation \d+ \(\w+\) does not apply: (index \S+ is past the end of the list at ".*"; \w+ accepts an index up to the length of the list, or - to append|index \S+ is past the end of the list at ".*" after the entry at ".*" is taken out of it; move then accepts an index up to the length of the list, or - to append|the list at ".*" has no entry at index \S+; check the index against the length of the list)$`),
	"not an index":  regexp.MustCompile(`^patch operation \d+ \(\w+\) does not apply: the list at ".*" takes a whole number as index, not ".*"; use a whole number(, or - to append)?$`),
	"not an object": regexp.MustCompile(`^patched object is not valid: the patch replaces the object with a JSON (number|string|boolean|null); the object must stay a JSON object$`),
	"delete whole":  regexp.MustCompile(`^patch operation \d+ \(remove\) does not apply: a patch cannot delete the whole object; give a path to a field of it to remove$`),
	"move whole":    regexp.MustCompile(`^patch operation \d+ \(move\) does not apply: the whole object cannot be moved; give "from" a path below it$`),
	"own child":     regexp.MustCompile(`^patch operation \d+ \(move\) does not apply: ".*" cannot move into one of its own children; give "path" a path outside ".*"$`),
}

var fallbackPattern = regexp.MustCompile(`does not apply to the object; check `)

// TestPatchApplyEveryFailingOperationIsMapped asserts that every single
// operation of the space of patchApplySpace that parses and that the library
// refuses gets a plain message that is not the fixed fallback sentence, with
// the VALIDATION code, and that matches the pattern of exactly one kind of
// reason; every kind is matched by at least one operation. The operations
// with a path, or a from of a move or copy, that is not empty and does not
// start with a slash are exactly the ones parsePatch refuses (77 of 735), each
// with a message that names the member and says it is not a JSON Pointer. A
// move or copy with an empty path is refused too (28 more), with the text
// that says the command does not support it.
//
// Limits: the space is single operations. An earlier operation that fails as
// a prefix only (an add of null at the whole object) can be named when a
// later replacement of the whole object hides it. A library error outside it reaches
// the fixed sentence, which is plain but names no reason; a panic outside it
// is recovered and reaches the same sentence. The check asserts that exactly
// one pattern matches, not that it is the right one: an exchange of the
// texts for a missing value and a missing parent for operations other than
// move and copy passes it, and so does a source test that always answers
// true. The exact rows of TestPatchApplyErrorsHavePlainReasons catch both.
func TestPatchApplyEveryFailingOperationIsMapped(t *testing.T) {
	t.Parallel()
	doc, err := objFromJSON(t, patchApplyObject).MarshalJSON()
	require.NoError(t, err)
	space := patchApplySpace()
	assert.Len(t, space, 735)

	matched := map[string]int{}
	skipped, failed := 0, 0
	for _, op := range space {
		p, perr := parsePatch([]byte(patchOf(op)), patchTypeJSON)
		member := slashlessMember(t, op)
		switch {
		case member != "":
			require.Error(t, perr, op)
			require.ErrorContains(t, perr, fmt.Sprintf("has the %q ", member), op)
			require.ErrorContains(t, perr, "is not a JSON Pointer", op)
		case movesToTheWholeObject(t, op):
			require.ErrorContains(t, perr, `has the "path" "", which this command does not support for move and copy`, op)
		default:
			require.NoError(t, perr, op)
		}
		if perr != nil {
			skipped++
			continue
		}
		_, aerr := p.apply(doc)
		if aerr == nil {
			continue
		}
		failed++
		ce, ok := errors.AsType[*clierr.Error](aerr)
		if !assert.True(t, ok, "%s: %v", op, aerr) {
			continue
		}
		assert.Equal(t, clierr.CodeValidation, ce.Code, op)
		assert.False(t, fallbackPattern.MatchString(ce.Message), "%s: the fallback sentence: %s", op, ce.Message)
		assertPlain(t, ce.Message)
		var names []string
		for name, re := range reasonPatterns {
			if re.MatchString(ce.Message) {
				names = append(names, name)
			}
		}
		if assert.Len(t, names, 1, "%s: %s matches %v", op, ce.Message, names) {
			matched[names[0]]++
		}
	}
	t.Logf("%d operations, %d skipped by parsePatch, %d failed in the library, matched by reason: %v", len(space), skipped, failed, matched)
	for name := range reasonPatterns {
		assert.Positive(t, matched[name], "no operation reaches the reason %q", name)
	}
	assert.Equal(t, 77+28, skipped)
	assert.Greater(t, failed, 150)
}

// movesToTheWholeObject reports whether the operation, given as JSON, is a
// move or copy with an empty path.
func movesToTheWholeObject(t *testing.T, op string) bool {
	t.Helper()
	var members map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(op), &members))
	var kind, path string
	require.NoError(t, json.Unmarshal(members["op"], &kind))
	require.NoError(t, json.Unmarshal(members["path"], &path))
	return (kind == "move" || kind == "copy") && path == ""
}

// slashlessMember returns the member of the operation, given as JSON, that a
// JSON Pointer cannot be: "path", or "from" of a move or copy, when it is not
// empty and does not start with a slash. The path is read first. It returns
// "" when both are pointers.
func slashlessMember(t *testing.T, op string) string {
	t.Helper()
	var members map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(op), &members))
	text := func(name string) string {
		var v string
		_ = json.Unmarshal(members[name], &v)
		return v
	}
	var kind string
	require.NoError(t, json.Unmarshal(members["op"], &kind))
	names := []string{"path"}
	if kind == "move" || kind == "copy" {
		names = append(names, "from")
	}
	for _, name := range names {
		if v := text(name); v != "" && !strings.HasPrefix(v, "/") {
			return name
		}
	}
	return ""
}

// TestPatchPathWithNoSlashIsRefusedBeforeTheObjectIsRead asserts, for each
// operation and each path that the library would drop the first segment of
// or fail on, the refusal when the patch is read and no request to the
// server. The library applies a path of two or more segments to another
// key. The refusal is the only protection.
func TestPatchPathWithNoSlashIsRefusedBeforeTheObjectIsRead(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"x", "data/x", "foo/metadata"} {
		for name, op := range map[string]string{
			"add":     opAdd(path, `1`),
			"replace": opReplace(path, `1`),
			"test":    opTest(path, `1`),
			"remove":  opRemove(path),
			"move":    opMove("/data/a", path),
			"copy":    opCopy("/data/a", path),
		} {
			t.Run(name+" "+path, func(t *testing.T) {
				t.Parallel()
				_, client, err := planTyped(t, "configmap", "app", patchApplyObject, patchOf(op), "")
				requireValidation(t, err)
				require.ErrorContains(t, err, notAPointerRefusal(0, name, "path", path))
				assert.Empty(t, client.getName)
			})
		}
	}
}

// TestPatchApplyNamesTheFirstFailingOperation asserts that, for every
// operation of the space followed by the removal of a missing key, the
// message names the operation the library fails at: the first when it fails
// alone, the second when it does not. The expected index comes from the
// library alone. A reason with no operation name (the whole object replaced
// by a value that is not an object) is left out of the name check.
func TestPatchApplyNamesTheFirstFailingOperation(t *testing.T) {
	t.Parallel()
	doc, err := objFromJSON(t, patchApplyObject).MarshalJSON()
	require.NoError(t, err)
	fails := func(ops jsonpatch.Patch) bool {
		_, err := guarded(func() ([]byte, error) { return ops.Apply(doc) })
		return err != nil
	}
	seen := map[int]int{}
	checked := 0
	for _, op := range patchApplySpace() {
		alone, perr := parsePatch([]byte(patchOf(op)), patchTypeJSON)
		if perr != nil {
			continue
		}
		pair, perr := parsePatch([]byte(patchOf(op, opRemove("/data/zz"))), patchTypeJSON)
		require.NoError(t, perr, op)
		var want int
		switch {
		case fails(alone.ops):
			want = 0
		case fails(pair.ops):
			want = 1
		default:
			continue
		}
		checked++
		seen[want]++
		_, aerr := pair.apply(doc)
		require.Error(t, aerr, op)
		ce, ok := errors.AsType[*clierr.Error](aerr)
		require.True(t, ok, op)
		if strings.HasPrefix(ce.Message, "patched object is not valid") {
			continue
		}
		assert.True(t, strings.HasPrefix(ce.Message, fmt.Sprintf("patch operation %d (", want)), "%s: %s", op, ce.Message)
	}
	t.Logf("%d operations checked: %d fail at the first operation, %d at the second", checked, seen[0], seen[1])
	assert.Positive(t, seen[0])
	assert.Positive(t, seen[1])
	assert.Greater(t, checked, 150)
}

// TestPatchApplyPathIsNeverAFormat asserts a path with control characters and
// format verbs appears in the message escaped and literal.
func TestPatchApplyPathIsNeverAFormat(t *testing.T) {
	t.Parallel()
	_, _, err := planTyped(t, "configmap", "app", patchApplyObject, patchOf(opRemove("/data/\u001b[2J%s%d")), "")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a clierr.Error, got %v", err)
	assert.Contains(t, ce.Message, `"/data/\x1b[2J%s%d"`)
	assert.NotContains(t, ce.Message, "\u001b")
	assert.NotContains(t, ce.Message, "%!")
}

// TestPatchApplyHasValueFollowsTheLibrary asserts that hasValue finds a
// source exactly when the library reads it: every pointer of up to three
// segments over keys of the fixture object, the empty string and a list index,
// and every path of the combination space, is copied by the library to a free
// key, and a copy that succeeds is a source that exists.
func TestPatchApplyHasValueFollowsTheLibrary(t *testing.T) {
	t.Parallel()
	doc, err := objFromJSON(t, patchApplyObject).MarshalJSON()
	require.NoError(t, err)
	keys := []string{"", "e", "k", "data", "a", "items", "0", "1", "-", "zz", "l~2"}
	pointers := []string{""}
	for _, a := range keys {
		pointers = append(pointers, "/"+a)
		for _, b := range keys {
			pointers = append(pointers, "/"+a+"/"+b)
			for _, c := range keys {
				pointers = append(pointers, "/"+a+"/"+b+"/"+c)
			}
		}
	}
	pointers = append(pointers, patchApplySpacePaths...)
	disagreements := 0
	for _, pointer := range pointers {
		ops, err := jsonpatch.DecodePatch([]byte(patchOf(opCopy(pointer, "/data/zzz"))))
		require.NoError(t, err)
		_, libErr := guarded(func() ([]byte, error) { return ops.Apply(doc) })
		if hasValue(doc, pointer) != (libErr == nil) {
			disagreements++
			t.Errorf("%q: hasValue %v, library error %v", pointer, hasValue(doc, pointer), libErr)
		}
	}
	t.Logf("%d pointers, %d disagreements", len(pointers), disagreements)
	assert.Equal(t, 1464+len(patchApplySpacePaths), len(pointers))
}
