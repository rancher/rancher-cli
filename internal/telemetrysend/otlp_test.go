package telemetrysend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

const updateCommand = "go test ./internal/cli/ -run TestTelemetryContract -update"

var contractRoot = filepath.Join("..", "..", "telemetry", "contract")

var contractDir = filepath.Join(contractRoot, telemetry.ContractVersion)

// loadFlatExamples reads and decodes every committed flat example of both
// kinds ("valid" and "invalid") once per test binary, by kind and file name,
// as encoding/json decodes them. Callers get copies from flatExamples.
var loadFlatExamples = sync.OnceValues(func() (map[string]map[string]map[string]any, error) {
	out := map[string]map[string]map[string]any{}
	for _, kind := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(contractDir, "examples", kind, "*.json"))
		if err != nil {
			return nil, fmt.Errorf("list %s examples: %w", kind, err)
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("no %s examples", kind)
		}
		examples := map[string]map[string]any{}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("read %s example: %w", kind, err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				return nil, fmt.Errorf("decode %s example %s: %w", kind, filepath.Base(f), err)
			}
			examples[filepath.Base(f)] = fields
		}
		out[kind] = examples
	}
	return out, nil
})

// deepCopy returns a copy of a decoded JSON value in which no map or slice is
// shared with v.
func deepCopy(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}

// flatExamples returns the committed flat examples of one kind ("valid" or
// "invalid") by file name, as encoding/json decodes them. The caller owns the
// result: it shares no map or slice with the result of another call.
func flatExamples(t *testing.T, kind string) map[string]map[string]any {
	t.Helper()
	all, err := loadFlatExamples()
	require.NoError(t, err)
	_, ok := all[kind]
	require.True(t, ok, "no %s examples", kind)
	out := make(map[string]map[string]any, len(all[kind]))
	for name, fields := range all[kind] {
		out[name] = deepCopy(fields).(map[string]any)
	}
	return out
}

// flatExample returns a copy of one committed flat example of a kind, which
// the caller owns as it does the result of flatExamples.
func flatExample(t *testing.T, kind, name string) map[string]any {
	t.Helper()
	all, err := loadFlatExamples()
	require.NoError(t, err)
	fields, ok := all[kind][name]
	require.True(t, ok, "no %s example %s", kind, name)
	return deepCopy(fields).(map[string]any)
}

// TestFlatExamplesShareNothingBetweenCalls asserts a change to a top-level
// key or to an element of a list in one result does not show in the next
// result, from flatExamples or flatExample.
func TestFlatExamplesShareNothingBetweenCalls(t *testing.T) {
	const name = "command-run-full.json"
	const flags = "io.rancher.cli.command.flags"
	first := flatExamples(t, "valid")[name]
	list, ok := first[flags].([]any)
	require.True(t, ok, "%s is a list", flags)
	require.NotEmpty(t, list)
	uid, firstFlag := first["log.record.uid"], list[0]
	require.NotEqual(t, "changed", uid)
	require.NotEqual(t, "changed", firstFlag)
	first["log.record.uid"] = "changed"
	list[0] = "changed"

	for label, next := range map[string]map[string]any{
		"flatExamples": flatExamples(t, "valid")[name],
		"flatExample":  flatExample(t, "valid", name),
	} {
		assert.Equal(t, uid, next["log.record.uid"], label)
		assert.Equal(t, firstFlag, next[flags].([]any)[0], label)
	}
}

// fileNames lists the file names in dir, sorted.
func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "run: %s", updateCommand)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func encodeAll(t *testing.T, fields ...map[string]any) Request {
	t.Helper()
	var events []telemetry.StoredEvent
	for _, f := range fields {
		events = append(events, telemetry.StoredEvent{Fields: f})
	}
	req, err := NewOTLPEncoder().Encode(events)
	require.NoError(t, err)
	return req
}

func indented(t *testing.T, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, json.Indent(&out, body, "", "  "))
	out.WriteByte('\n')
	return out.Bytes()
}

// roundTripExclusions lists the flat examples whose OTLP form is, by design,
// not equal to them once rebuilt, by "kind/name", each with the reason. An
// example that needs an entry and has none fails the golden test with the
// reason; an entry no example needs fails it too.
var roundTripExclusions = map[string]string{}

// roundTripBreaker returns why the OTLP form of a flat example cannot be
// rebuilt into it: a key the encoder never sends, a time that is not sent, or
// a value with no OTLP form. It returns "" when the rebuilt event must equal
// the flat one.
func roundTripBreaker(fields map[string]any) string {
	known := map[string]bool{"event.name": true, "time": true, "schema_version": true}
	for _, a := range telemetry.Attributes() {
		known[a.Name] = true
	}
	for key, v := range fields {
		if !known[key] {
			return "the key " + key + " is not sent"
		}
		if _, isText := v.(string); !isText && (key == "event.name" || key == "time" || key == "schema_version") {
			return "the fixed key " + key + " is not text and is not sent"
		}
		switch x := v.(type) {
		case string, bool:
		case float64:
			if x != math.Trunc(x) || math.Abs(x) > maxExactInteger {
				return "the number in " + key + " has no OTLP form"
			}
		case []any:
			for _, e := range x {
				if _, ok := e.(string); !ok {
					return "the list in " + key + " holds a value that is not text"
				}
			}
		default:
			return "the value of " + key + " has no OTLP form"
		}
	}
	if text, ok := fields["time"].(string); ok {
		t, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return "the time is not RFC 3339 and is not sent"
		}
		if year := t.UTC().Year(); year < 1970 || year > lastNanosecondYear {
			return "the time is outside 1970 to 2261 and is not sent"
		}
		if t.UnixNano() <= 0 {
			return "the time is not after the Unix epoch and is not sent"
		}
		if rebuilt := t.UTC().Format(time.RFC3339Nano); rebuilt != text {
			return "the time is not written in UTC form " + rebuilt
		}
	}
	return ""
}

// TestRoundTripBreaker asserts the check above names each case that makes the
// round trip differ, and accepts the committed examples.
func TestRoundTripBreaker(t *testing.T) {
	base := func() map[string]any {
		return maps.Clone(flatExamples(t, "valid")["command-run-full.json"])
	}
	assert.Empty(t, roundTripBreaker(base()))
	cases := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"unknown key", func(f map[string]any) { f["io.rancher.cli.added.later"] = "x" }, "the key io.rancher.cli.added.later is not sent"},
		{"event.name a number", func(f map[string]any) { f["event.name"] = float64(1) }, "the fixed key event.name is not text and is not sent"},
		{"schema_version a boolean", func(f map[string]any) { f["schema_version"] = true }, "the fixed key schema_version is not text and is not sent"},
		{"time a number", func(f map[string]any) { f["time"] = float64(5) }, "the fixed key time is not text and is not sent"},
		{"time not RFC 3339", func(f map[string]any) { f["time"] = "yesterday" }, "the time is not RFC 3339 and is not sent"},
		{"time before 1970", func(f map[string]any) { f["time"] = "1969-12-31T23:59:59Z" }, "the time is outside 1970 to 2261 and is not sent"},
		{"time in 2262", func(f map[string]any) { f["time"] = "2262-01-01T00:00:00Z" }, "the time is outside 1970 to 2261 and is not sent"},
		{"time at the epoch", func(f map[string]any) { f["time"] = "1970-01-01T00:00:00Z" }, "the time is not after the Unix epoch and is not sent"},
		{"time with an offset", func(f map[string]any) { f["time"] = "2026-10-23T16:00:00+02:00" }, "the time is not written in UTC form 2026-10-23T14:00:00Z"},
		{"fraction", func(f map[string]any) { f["process.exit.code"] = 1.5 }, "the number in process.exit.code has no OTLP form"},
		{"integer above 2^53-1", func(f map[string]any) { f["process.exit.code"] = float64(1 << 53) }, "the number in process.exit.code has no OTLP form"},
		{"list of numbers", func(f map[string]any) { f["io.rancher.cli.command.flags"] = []any{float64(1)} }, "the list in io.rancher.cli.command.flags holds a value that is not text"},
		{"null", func(f map[string]any) { f["io.rancher.cli.command"] = nil }, "the value of io.rancher.cli.command has no OTLP form"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.edit(f)
			assert.Equal(t, tc.want, roundTripBreaker(f))
		})
	}
}

// TestOTLPGolden asserts each committed OTLP example is the encoder's body
// for the flat example of the same name, re-indented, that the two
// directories hold the same names, that no other file is under
// examples/otlp, and that a strict decoder rebuilds each flat example from its
// OTLP file, except the examples roundTripExclusions lists. The examples are
// written by the encoder, so this test does not check the format
// independently; the shape test does, in part.
func TestOTLPGolden(t *testing.T) {
	needed := map[string]bool{}
	for _, kind := range []string{"valid", "invalid"} {
		flat := flatExamples(t, kind)
		dir := filepath.Join(contractDir, "examples", "otlp", kind)
		var flatNames []string
		for name := range flat {
			flatNames = append(flatNames, name)
		}
		slices.Sort(flatNames)
		assert.Equal(t, flatNames, fileNames(t, dir), "%s: the OTLP examples and the flat examples must have the same names; run: %s", kind, updateCommand)
		for _, name := range flatNames {
			committed, err := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, err, "run: %s", updateCommand)
			req := encodeAll(t, flat[name])
			assert.Equal(t, string(indented(t, req.Body)), string(committed), "%s/%s is stale; run: %s", kind, name, updateCommand)
			if reason := roundTripBreaker(flat[name]); reason != "" {
				needed[kind+"/"+name] = true
				_, listed := roundTripExclusions[kind+"/"+name]
				assert.True(t, listed, "%s/%s cannot be rebuilt from its OTLP form (%s); list it in roundTripExclusions with that reason", kind, name, reason)
				continue
			}
			rebuilt := rebuildFlat(t, committed)
			require.Len(t, rebuilt, 1, name)
			assert.Equal(t, flat[name], rebuilt[0], "%s/%s does not rebuild the flat example", kind, name)
		}
	}
	for key := range roundTripExclusions {
		assert.True(t, needed[key], "roundTripExclusions lists %s, which round trips", key)
	}
	root := filepath.Join(contractDir, "examples", "otlp")
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		require.NoError(t, err)
		kind, name, nested := strings.Cut(filepath.ToSlash(rel), "/")
		ok := nested && (kind == "valid" || kind == "invalid")
		if ok {
			_, ok = flatExamples(t, kind)[name]
		}
		assert.True(t, ok, "%s is not examples/otlp/<kind>/<flat example name>; run: %s", filepath.ToSlash(rel), updateCommand)
		return nil
	}))
}

// TestOTLPBatchKeepsOrder asserts the five valid examples encoded as one list
// give five resourceLogs entries, in the order of the list.
func TestOTLPBatchKeepsOrder(t *testing.T) {
	flat := flatExamples(t, "valid")
	var names []string
	for name := range flat {
		names = append(names, name)
	}
	slices.Sort(names)
	require.Len(t, names, 5)
	var fields []map[string]any
	for _, name := range names {
		fields = append(fields, flat[name])
	}
	rebuilt := rebuildFlat(t, encodeAll(t, fields...).Body)
	require.Len(t, rebuilt, 5)
	for i, name := range names {
		assert.Equal(t, flat[name], rebuilt[i], name)
	}
}

// shapeKeys lists the JSON keys each level of the body may hold.
var shapeKeys = map[string][]string{
	"root":         {"resourceLogs"},
	"resourceLogs": {"resource", "scopeLogs", "schemaUrl"},
	"resource":     {"attributes"},
	"scopeLogs":    {"scope", "logRecords", "schemaUrl"},
	"scope":        {"name", "version"},
	"logRecord":    {"timeUnixNano", "eventName", "attributes"},
	"attribute":    {"key", "value"},
	"anyValue":     {"stringValue", "boolValue", "intValue", "arrayValue"},
	"arrayValue":   {"values"},
}

// neverWritten lists the OTLP fields the encoder never writes.
var neverWritten = []string{"severityNumber", "severityText", "body", "observedTimeUnixNano", "traceId", "spanId", "flags", "droppedAttributesCount"}

var (
	lowerCamelCase = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	intText        = regexp.MustCompile(`^-?[0-9]+$`)
	timeText       = regexp.MustCompile(`^[0-9]+$`)
)

// checkShape walks a decoded JSON node of the given level and asserts the
// closed key set, lowerCamelCase keys and the type of each leaf.
func checkShape(t *testing.T, node any, level, where string) {
	t.Helper()
	list := func(key string, n any, next string) {
		items, ok := n.([]any)
		require.True(t, ok, "%s.%s must be a JSON array", where, key)
		for i, item := range items {
			checkShape(t, item, next, where+"."+key+"["+strconv.Itoa(i)+"]")
		}
	}
	obj, ok := node.(map[string]any)
	require.True(t, ok, "%s must be a JSON object", where)
	for key, val := range obj {
		assert.Contains(t, shapeKeys[level], key, "%s: %s is not a key of the %s level", where, key, level)
		assert.NotContains(t, neverWritten, key, where)
		assert.Regexp(t, lowerCamelCase, key, where)
		switch {
		case level == "root" && key == "resourceLogs":
			list(key, val, "resourceLogs")
		case level == "resourceLogs" && key == "resource":
			checkShape(t, val, "resource", where+".resource")
		case level == "resourceLogs" && key == "scopeLogs":
			list(key, val, "scopeLogs")
		case level == "resource" && key == "attributes", level == "logRecord" && key == "attributes":
			list(key, val, "attribute")
		case level == "scopeLogs" && key == "scope":
			checkShape(t, val, "scope", where+".scope")
		case level == "scopeLogs" && key == "logRecords":
			list(key, val, "logRecord")
		case key == "schemaUrl", key == "name", key == "version", key == "eventName", key == "key":
			assert.IsType(t, "", val, "%s.%s", where, key)
		case key == "timeUnixNano":
			assert.IsType(t, "", val, "%s.timeUnixNano must be a JSON string", where)
			assert.Regexp(t, timeText, val, where)
		case level == "attribute" && key == "value":
			checkShape(t, val, "anyValue", where+".value")
		case key == "stringValue":
			assert.IsType(t, "", val, where)
		case key == "boolValue":
			assert.IsType(t, true, val, "%s.boolValue must be a JSON boolean", where)
		case key == "intValue":
			assert.IsType(t, "", val, "%s.intValue must be a JSON string", where)
			assert.Regexp(t, intText, val, where)
		case key == "arrayValue":
			checkShape(t, val, "arrayValue", where+".arrayValue")
		case level == "arrayValue" && key == "values":
			list(key, val, "anyValue")
		}
	}
	if level == "anyValue" {
		assert.Len(t, obj, 1, "%s must hold exactly one value field", where)
	}
	if level == "arrayValue" {
		assert.Contains(t, obj, "values", "%s: an arrayValue always has values", where)
	}
}

// TestOTLPJSONFollowsTheSpecShape asserts, for every flat example and for a
// batch, a closed set of keys at each level, lowerCamelCase keys, the JSON
// type of each leaf (a string for timeUnixNano and intValue, a boolean for
// boolValue, an array for values, also when empty), none of the fields the
// encoder never writes, and on the raw bytes: compact JSON, no trailing line
// feed and no HTML escape.
func TestOTLPJSONFollowsTheSpecShape(t *testing.T) {
	var all []map[string]any
	for _, kind := range []string{"valid", "invalid"} {
		for name, fields := range flatExamples(t, kind) {
			all = append(all, fields)
			assertShape(t, name, encodeAll(t, fields).Body)
		}
	}
	html := maps.Clone(flatExamples(t, "valid")["command-run-full.json"])
	html["io.rancher.cli.command"] = "a<b>&c"
	req := encodeAll(t, html)
	assert.Contains(t, string(req.Body), `"stringValue":"a<b>&c"`, "the HTML characters must not be escaped")
	assertShape(t, "html", req.Body)
	assertShape(t, "batch", encodeAll(t, all...).Body)
}

func assertShape(t *testing.T, name string, body []byte) {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal(body, &doc), name)
	checkShape(t, doc, "root", name)
	var compact bytes.Buffer
	require.NoError(t, json.Compact(&compact, body), name)
	assert.Equal(t, compact.String(), string(body), "%s: the body must be compact", name)
	assert.False(t, bytes.HasSuffix(body, []byte("\n")), "%s: the body must not end with a line feed", name)
}

// sampleValue returns a value of the kind of an attribute, and fails on a
// kind it does not know, so a new kind must be added here with its form.
func sampleValue(t *testing.T, a telemetry.Attribute) (value any, form string) {
	t.Helper()
	switch a.Kind {
	case telemetry.KindString:
		return "sample", "stringValue"
	case telemetry.KindBoolean:
		return true, "boolValue"
	case telemetry.KindInteger:
		return float64(7), "intValue"
	case telemetry.KindStrings:
		return []any{"a", "b"}, "arrayValue"
	}
	t.Fatalf("attribute %s has the kind %q, which the encoder test does not know", a.Name, a.Kind)
	return nil, ""
}

// TestEveryAttributeIsEncodedAtItsPlacement asserts each attribute of the
// metadata is written once, under the resource for a resource attribute and
// under the record for a record attribute, in the value form of its kind.
func TestEveryAttributeIsEncodedAtItsPlacement(t *testing.T) {
	for _, a := range telemetry.Attributes() {
		value, form := sampleValue(t, a)
		rl := decodeExport(t, encodeAll(t, map[string]any{a.Name: value}).Body).ResourceLogs[0]
		var inResource, inRecord []decKeyValue
		for _, kv := range rl.Resource.Attributes {
			if kv.Key == a.Name {
				inResource = append(inResource, kv)
			}
		}
		for _, kv := range rl.ScopeLogs[0].LogRecords[0].Attributes {
			if kv.Key == a.Name {
				inRecord = append(inRecord, kv)
			}
		}
		var found []decKeyValue
		switch a.Placement {
		case telemetry.PlaceResource:
			found = inResource
			assert.Empty(t, inRecord, a.Name)
		case telemetry.PlaceRecord:
			found = inRecord
			assert.Empty(t, inResource, a.Name)
		default:
			t.Fatalf("attribute %s has the placement %q, which the encoder test does not know", a.Name, a.Placement)
		}
		require.Len(t, found, 1, "%s must be written once at its placement", a.Name)
		raw, err := json.Marshal(found[0].Value)
		require.NoError(t, err)
		var shape map[string]any
		require.NoError(t, json.Unmarshal(raw, &shape))
		var forms []string
		for k, v := range shape {
			if v != nil {
				forms = append(forms, k)
			}
		}
		assert.Equal(t, []string{form}, forms, a.Name)
	}
}

// recordOf decodes one body and returns its first record and resource.
func recordOf(t *testing.T, body []byte) (decResourceLogs, decLogRecord) {
	t.Helper()
	rl := decodeExport(t, body).ResourceLogs[0]
	return rl, rl.ScopeLogs[0].LogRecords[0]
}

// valueOf encodes an event that holds one record attribute and returns the
// value as the decoded JSON of its OTLP form, or nil when it was not sent.
func valueOf(t *testing.T, raw any) map[string]any {
	t.Helper()
	_, rec := recordOf(t, encodeAll(t, map[string]any{"io.rancher.cli.command": raw}).Body)
	if len(rec.Attributes) == 0 {
		return nil
	}
	require.Len(t, rec.Attributes, 1)
	require.Equal(t, "io.rancher.cli.command", rec.Attributes[0].Key)
	data, err := json.Marshal(rec.Attributes[0].Value)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	for k, v := range out {
		if v == nil {
			delete(out, k)
		}
	}
	return out
}

// TestOTLPValueRules asserts the form of each kind of value, the bounds of an
// integer, and that a value with no form is not sent.
func TestOTLPValueRules(t *testing.T) {
	const maxInt = 1<<53 - 1
	rows := []struct {
		name string
		in   any
		want map[string]any
	}{
		{"empty string", "", map[string]any{"stringValue": ""}},
		{"string", "x", map[string]any{"stringValue": "x"}},
		{"true", true, map[string]any{"boolValue": true}},
		{"false", false, map[string]any{"boolValue": false}},
		{"zero", float64(0), map[string]any{"intValue": "0"}},
		{"negative zero", math0(), map[string]any{"intValue": "0"}},
		{"255", float64(255), map[string]any{"intValue": "255"}},
		{"300", float64(300), map[string]any{"intValue": "300"}},
		{"minus one", float64(-1), map[string]any{"intValue": "-1"}},
		{"largest exact integer", float64(maxInt), map[string]any{"intValue": "9007199254740991"}},
		{"smallest exact integer", float64(-maxInt), map[string]any{"intValue": "-9007199254740991"}},
		{"2^53", float64(maxInt + 1), nil},
		{"minus 2^53", float64(-maxInt - 1), nil},
		{"fraction", 1.5, nil},
		{"null", nil, nil},
		{"object", map[string]any{"a": "b"}, nil},
		{"array with a number", []any{"a", float64(1)}, nil},
		{"nested array", []any{[]any{"a"}}, nil},
		{"empty array", []any{}, map[string]any{"arrayValue": map[string]any{"values": []any{}}}},
		{"array of strings", []any{"a", "b"}, map[string]any{"arrayValue": map[string]any{"values": []any{
			map[string]any{"stringValue": "a"}, map[string]any{"stringValue": "b"}}}}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := valueOf(t, row.in)
			if row.want == nil {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, row.want, got)
		})
	}
}

// math0 returns -0 as a decoded JSON number.
func math0() float64 {
	var v any
	if err := json.Unmarshal([]byte("-0"), &v); err != nil {
		panic(err)
	}
	return v.(float64)
}

// TestOTLPUnknownKeysAreNotSent asserts a key the metadata does not name is
// in no attribute list, and that every known key of the same event is.
func TestOTLPUnknownKeysAreNotSent(t *testing.T) {
	fields := maps.Clone(flatExamples(t, "valid")["command-run-full.json"])
	fields["io.rancher.cli.added.later"] = "value"
	fields["local_format"] = "1"
	fields["log.record.uid.copy"] = "value"
	rl, rec := recordOf(t, encodeAll(t, fields).Body)
	sent := map[string]bool{}
	for _, kv := range slices.Concat(rl.Resource.Attributes, rec.Attributes) {
		sent[kv.Key] = true
	}
	for key := range fields {
		known := slices.ContainsFunc(telemetry.Attributes(), func(a telemetry.Attribute) bool { return a.Name == key })
		fixed := key == "event.name" || key == "time" || key == "schema_version"
		switch {
		case known:
			assert.True(t, sent[key], "the known key %s must be sent", key)
		case fixed:
			assert.False(t, sent[key], "the fixed key %s is not an attribute", key)
		default:
			assert.False(t, sent[key], "the unknown key %s must not be sent", key)
		}
	}
	assert.NotNil(t, rec.EventName)
	assert.NotNil(t, rec.TimeUnixNano)
}

// TestOTLPAttributeWithAnUnknownPlacementIsNotSent asserts an attribute whose
// placement is neither resource nor record is written nowhere, and that the
// attributes with a placement the encoder knows are.
func TestOTLPAttributeWithAnUnknownPlacementIsNotSent(t *testing.T) {
	list := []placed{{"a", telemetry.PlaceResource}, {"b", "elsewhere"}, {"c", telemetry.PlaceRecord}}
	rl := encodeEvent(map[string]any{"a": "1", "b": "2", "c": "3"}, list)
	require.Len(t, rl.Resource.Attributes, 1)
	assert.Equal(t, "a", rl.Resource.Attributes[0].Key)
	rec := rl.ScopeLogs[0].LogRecords[0]
	require.Len(t, rec.Attributes, 1)
	assert.Equal(t, "c", rec.Attributes[0].Key)
}

// TestOTLPTimeRules asserts the timeUnixNano of an event for each form of
// the time key, on both sides of 1970 and of the year 2261.
func TestOTLPTimeRules(t *testing.T) {
	utc := func(y int, mo time.Month, d, h, mi, s, ns int) string {
		return strconv.FormatInt(time.Date(y, mo, d, h, mi, s, ns, time.UTC).UnixNano(), 10)
	}
	rows := []struct {
		name string
		in   any
		want string
	}{
		{"absent", nil, ""},
		{"not a string", float64(5), ""},
		{"not a time", "not a time", ""},
		{"offset", "2026-10-23T14:37:12+02:00", utc(2026, 10, 23, 12, 37, 12, 0)},
		{"fraction", "2026-10-23T14:00:00.5Z", utc(2026, 10, 23, 14, 0, 0, 500000000)},
		{"one second before the epoch", "1969-12-31T23:59:59Z", ""},
		{"the epoch", "1970-01-01T00:00:00Z", ""},
		{"a fraction after the epoch", "1970-01-01T00:00:00.000000001Z", "1"},
		{"one second after the epoch", "1970-01-01T00:00:01Z", "1000000000"},
		{"after the epoch in UTC and before it in the offset", "1969-12-31T23:30:00-01:00", utc(1970, 1, 1, 0, 30, 0, 0)},
		{"before the epoch in UTC and after it in the offset", "1970-01-01T00:30:00+01:00", ""},
		{"a year far before the epoch", "1000-01-01T00:00:00Z", ""},
		{"the last second of 2261", "2261-12-31T23:59:59Z", utc(2261, 12, 31, 23, 59, 59, 0)},
		{"the first second of 2262", "2262-01-01T00:00:00Z", ""},
		{"a year far after 2262", "9999-01-01T00:00:00Z", ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			fields := map[string]any{}
			if row.in != nil {
				fields["time"] = row.in
			}
			_, rec := recordOf(t, encodeAll(t, fields).Body)
			if row.want == "" {
				assert.Nil(t, rec.TimeUnixNano)
				return
			}
			require.NotNil(t, rec.TimeUnixNano)
			assert.Equal(t, row.want, *rec.TimeUnixNano)
		})
	}
}

// TestOTLPFixedKeys asserts the fixed keys go to the log record and the
// scope, that a missing or non-text one leaves its field out, and the
// omission rules of the resource, the scope and the record.
func TestOTLPFixedKeys(t *testing.T) {
	full := map[string]any{"event.name": "e", "schema_version": "1.0.0", "service.name": "s"}
	rl, rec := recordOf(t, encodeAll(t, full).Body)
	require.NotNil(t, rec.EventName)
	assert.Equal(t, "e", *rec.EventName)
	require.NotNil(t, rl.ScopeLogs[0].Scope.Version)
	assert.Equal(t, "1.0.0", *rl.ScopeLogs[0].Scope.Version)
	require.NotNil(t, rl.ScopeLogs[0].Scope.Name)
	assert.Equal(t, "s", *rl.ScopeLogs[0].Scope.Name)
	require.Len(t, rl.Resource.Attributes, 1, "service.name is also a resource attribute")
	assert.Equal(t, "service.name", rl.Resource.Attributes[0].Key)

	rl, rec = recordOf(t, encodeAll(t, map[string]any{"schema_version": "1.0.0", "service.name": "s"}).Body)
	assert.Nil(t, rec.EventName, "event.name is absent")
	assert.NotNil(t, rl.ScopeLogs[0].Scope.Name)

	rl, _ = recordOf(t, encodeAll(t, map[string]any{"event.name": "e", "schema_version": "1.0.0"}).Body)
	assert.Nil(t, rl.ScopeLogs[0].Scope.Name, "service.name is absent")
	assert.NotNil(t, rl.ScopeLogs[0].Scope.Version)

	rl, rec = recordOf(t, encodeAll(t, map[string]any{"event.name": float64(1), "schema_version": true, "service.name": []any{}}).Body)
	assert.Nil(t, rec.EventName, "event.name is not text")
	assert.Nil(t, rl.ScopeLogs[0].Scope.Version, "schema_version is not text")
	assert.Nil(t, rl.ScopeLogs[0].Scope.Name, "service.name is not text")

	rl, rec = recordOf(t, encodeAll(t, map[string]any{"event.name": "e", "service.name": "", "schema_version": ""}).Body)
	require.NotNil(t, rl.ScopeLogs[0].Scope.Name, "an empty text is written")
	assert.Equal(t, "", *rl.ScopeLogs[0].Scope.Name)
	require.NotNil(t, rl.ScopeLogs[0].Scope.Version)
	assert.Equal(t, "", *rl.ScopeLogs[0].Scope.Version)

	body := encodeAll(t, map[string]any{"event.name": "e"}).Body
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	entry := doc["resourceLogs"].([]any)[0].(map[string]any)
	resource, ok := entry["resource"].(map[string]any)
	require.True(t, ok, "resource is always written")
	assert.NotContains(t, resource, "attributes", "an empty attribute list is left out")
	scopeLogs := entry["scopeLogs"].([]any)[0].(map[string]any)
	scope, ok := scopeLogs["scope"].(map[string]any)
	require.True(t, ok, "scope is always written")
	assert.Empty(t, scope)
	record := scopeLogs["logRecords"].([]any)[0].(map[string]any)
	assert.NotContains(t, record, "attributes", "an empty attribute list is left out")
	assert.Equal(t, "e", record["eventName"])
}

// TestOTLPEncodeRequest asserts the path and content type of a request, the
// error for an empty list and that the same event gives the same bytes each
// time.
func TestOTLPEncodeRequest(t *testing.T) {
	enc := NewOTLPEncoder()
	req, err := enc.Encode(nil)
	require.ErrorContains(t, err, "no events to encode")
	assert.Equal(t, Request{}, req)
	_, err = enc.Encode([]telemetry.StoredEvent{})
	require.ErrorContains(t, err, "no events to encode")

	fields := maps.Clone(flatExamples(t, "valid")["command-run-full.json"])
	fields["io.rancher.cli.added.later"] = "value"
	fields["another.unknown.key"] = []any{"x"}
	first := encodeAll(t, fields)
	assert.Equal(t, "/v1/logs", first.Path)
	assert.Equal(t, "application/json", first.ContentType)
	for range 50 {
		assert.Equal(t, string(first.Body), string(encodeAll(t, fields).Body))
	}
	assert.Len(t, decodeExport(t, encodeAll(t, fields, fields, fields).Body).ResourceLogs, 3, "events are never grouped")
}

// TestOTLPSchemaURL asserts both schemaUrl fields hold the one URL.
func TestOTLPSchemaURL(t *testing.T) {
	rl, _ := recordOf(t, encodeAll(t, map[string]any{}).Body)
	assert.Equal(t, otlpSchemaURL, rl.SchemaURL)
	assert.Equal(t, rl.SchemaURL, rl.ScopeLogs[0].SchemaURL)
}
