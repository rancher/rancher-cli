package telemetrysend

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

// The OTLP/HTTP request this file writes. Nothing else in the module names
// these values.
const (
	otlpPath        = "/v1/logs"
	otlpContentType = "application/json"
	otlpSchemaURL   = "https://opentelemetry.io/schemas/1.44.0"
)

// The keys of a flat event that are not attributes in the metadata, and the
// one attribute that also names the scope.
const (
	keyEventName     = "event.name"
	keyTime          = "time"
	keySchemaVersion = "schema_version"
	keyServiceName   = "service.name"
)

// maxExactInteger is the largest magnitude a JSON number holds without losing
// an integer: 2^53 - 1.
const maxExactInteger = 1<<53 - 1

// The latest year whose instants UnixNano represents; it is undefined from
// 2262-04-11.
const lastNanosecondYear = 2261

var errNoEvents = errors.New("no events to encode")

type otlpEncoder struct{}

// NewOTLPEncoder returns the encoder that writes OTLP/HTTP JSON.
func NewOTLPEncoder() Encoder { return otlpEncoder{} }

type exportRequest struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

type resourceLogs struct {
	Resource  resource    `json:"resource"`
	ScopeLogs []scopeLogs `json:"scopeLogs"`
	SchemaURL string      `json:"schemaUrl"`
}

type resource struct {
	Attributes []keyValue `json:"attributes,omitempty"`
}

type scopeLogs struct {
	Scope      scope       `json:"scope"`
	LogRecords []logRecord `json:"logRecords"`
	SchemaURL  string      `json:"schemaUrl"`
}

type scope struct {
	Name    *string `json:"name,omitempty"`
	Version *string `json:"version,omitempty"`
}

type logRecord struct {
	TimeUnixNano string     `json:"timeUnixNano,omitempty"`
	EventName    *string    `json:"eventName,omitempty"`
	Attributes   []keyValue `json:"attributes,omitempty"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue *string     `json:"stringValue,omitempty"`
	BoolValue   *bool       `json:"boolValue,omitempty"`
	IntValue    *string     `json:"intValue,omitempty"`
	ArrayValue  *arrayValue `json:"arrayValue,omitempty"`
}

type arrayValue struct {
	Values []anyValue `json:"values"`
}

// placed is one attribute of the metadata: its name and where it sits.
type placed struct {
	name      string
	placement string
}

// placements lists the attributes of the metadata in the order they are
// written. The metadata lists each name once; a test of internal/telemetry
// asserts it. The list is built on first use, so importing the package costs
// nothing.
var placements = sync.OnceValue(func() []placed {
	var out []placed
	for _, a := range telemetry.Attributes() {
		out = append(out, placed{a.Name, a.Placement})
	}
	return out
})

// Encode writes one resourceLogs entry per event, in the order given. An
// event is never skipped and events are never grouped. Only the fixed keys
// and the attributes the metadata names are written; any other key of a stored
// event is left out.
func (otlpEncoder) Encode(events []telemetry.StoredEvent) (Request, error) {
	if len(events) == 0 {
		return Request{}, errNoEvents
	}
	list := placements()
	req := exportRequest{ResourceLogs: make([]resourceLogs, 0, len(events))}
	for _, ev := range events {
		req.ResourceLogs = append(req.ResourceLogs, encodeEvent(ev.Fields, list))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// The request holds only strings, booleans, slices and structs of them,
	// so encoding cannot fail.
	_ = enc.Encode(req)
	return Request{
		Path:        otlpPath,
		ContentType: otlpContentType,
		Body:        bytes.TrimSuffix(buf.Bytes(), []byte("\n")),
	}, nil
}

// encodeEvent writes one event with the attributes of list: a name goes to the
// resource or the record as its placement says, and nowhere for any other
// placement.
func encodeEvent(fields map[string]any, list []placed) resourceLogs {
	var rec logRecord
	rec.EventName = stringField(fields, keyEventName)
	rec.TimeUnixNano = unixNano(fields[keyTime])
	var res resource
	for _, p := range list {
		raw, ok := fields[p.name]
		if !ok {
			continue
		}
		v, ok := value(raw)
		if !ok {
			continue
		}
		kv := keyValue{Key: p.name, Value: v}
		switch p.placement {
		case telemetry.PlaceResource:
			res.Attributes = append(res.Attributes, kv)
		case telemetry.PlaceRecord:
			rec.Attributes = append(rec.Attributes, kv)
		}
	}
	return resourceLogs{
		Resource: res,
		ScopeLogs: []scopeLogs{{
			Scope:      scope{Name: stringField(fields, keyServiceName), Version: stringField(fields, keySchemaVersion)},
			LogRecords: []logRecord{rec},
			SchemaURL:  otlpSchemaURL,
		}},
		SchemaURL: otlpSchemaURL,
	}
}

// stringField returns the value of key when it is a string.
func stringField(fields map[string]any, key string) *string {
	s, ok := fields[key].(string)
	if !ok {
		return nil
	}
	return &s
}

// unixNano returns the decimal text of the instant in v, or "" when v is not
// an RFC 3339 text, the instant is not after the Unix epoch (OTLP reads 0 as
// a missing time) or UnixNano cannot represent it.
func unixNano(v any) string {
	text, ok := v.(string)
	if !ok {
		return ""
	}
	t, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return ""
	}
	if year := t.UTC().Year(); year < 1970 || year > lastNanosecondYear {
		return ""
	}
	n := t.UnixNano()
	if n <= 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

// value returns the OTLP form of a decoded JSON value: a string, a boolean, a
// number that is an integer of at most 2^53-1 in magnitude, or an array of
// strings. Anything else has no form.
func value(raw any) (anyValue, bool) {
	switch v := raw.(type) {
	case string:
		return anyValue{StringValue: &v}, true
	case bool:
		return anyValue{BoolValue: &v}, true
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > maxExactInteger {
			return anyValue{}, false
		}
		s := strconv.FormatInt(int64(v), 10)
		return anyValue{IntValue: &s}, true
	case []any:
		values := make([]anyValue, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return anyValue{}, false
			}
			values = append(values, anyValue{StringValue: &s})
		}
		return anyValue{ArrayValue: &arrayValue{Values: values}}, true
	}
	return anyValue{}, false
}

// RefusedCount reads partialSuccess.rejectedLogRecords of an answer. An answer
// that is a JSON object and states no count is a full success: (0, true). An
// answer that is not a JSON object, or whose count is negative, fractional,
// too large for an int or not a number, gives (0, false).
func (otlpEncoder) RefusedCount(answer []byte) (int, bool) {
	var doc struct {
		PartialSuccess json.RawMessage `json:"partialSuccess"`
	}
	if trimmed := bytes.TrimSpace(answer); len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, false
	}
	if json.Unmarshal(answer, &doc) != nil {
		return 0, false
	}
	if isAbsent(doc.PartialSuccess) {
		return 0, true
	}
	var partial struct {
		Rejected json.RawMessage `json:"rejectedLogRecords"`
	}
	if json.Unmarshal(doc.PartialSuccess, &partial) != nil {
		return 0, false
	}
	if isAbsent(partial.Rejected) {
		return 0, true
	}
	return count(partial.Rejected)
}

// isAbsent reports whether a field of an answer is missing or null.
func isAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// count reads a count written as a decimal string or as a JSON number.
func count(raw json.RawMessage) (int, bool) {
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" || strings.Trim(s, "0123456789") != "" {
			return 0, false
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	if n, err := strconv.Atoi(string(raw)); err == nil {
		if n < 0 {
			return 0, false
		}
		return n, true
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || f < 0 || f != math.Trunc(f) || f >= float64(math.MaxInt) {
		return 0, false
	}
	return int(f), true
}

// JSONEncodingSection returns the "JSON encoding" section of the generated
// OTLP mapping: what the encoder writes, stated from the encoder's own
// constants. It ends with one line feed.
func JSONEncodingSection() string {
	var b strings.Builder
	b.WriteString("## JSON encoding\n\n")
	b.WriteString("The CLI writes events in the JSON Protobuf encoding of OTLP. A request goes to `" + otlpPath +
		"` with the content type `" + otlpContentType + "`; the body is compact JSON and is not compressed.\n\n")
	b.WriteString("The body holds one `resourceLogs` entry per event, in the order of the events. Entries are never grouped: " +
		"`service.instance.id` is different for every process, and the events of one hour come from several processes. " +
		"An entry holds `resource`, `scopeLogs` and `schemaUrl`. Its one `scopeLogs` entry holds `scope` (with `name` and `version`), " +
		"`logRecords` and `schemaUrl`. Its one record holds `timeUnixNano`, `eventName` and `attributes`. " +
		"Each attribute is a `key` and a `value`. " +
		"`resourceLogs[].schemaUrl` and `resourceLogs[].scopeLogs[].schemaUrl` are both `" + otlpSchemaURL + "`.\n\n")
	b.WriteString("- Keys are lowerCamelCase. `timeUnixNano` and `intValue` are decimal strings.\n")
	b.WriteString("- `scope.name` equals `service.name`, `scope.version` is `schema_version` and `eventName` is `event.name`. " +
		"A fixed key that is absent or not text leaves its field out.\n")
	b.WriteString("- `timeUnixNano` is the `time` of the event as nanoseconds since the Unix epoch: " +
		"an event whose `time` cannot be read is sent without `timeUnixNano`, and so is one whose `time` is not after 1970-01-01T00:00:00Z or is after the year 2261.\n")
	b.WriteString("- The encoder writes no `body`, `severityNumber`, `severityText`, `observedTimeUnixNano`, `traceId`, `spanId`, `flags` or `droppedAttributesCount`.\n")
	b.WriteString("- A value is written in one of four forms: `stringValue`, `boolValue`, `intValue` or `arrayValue` (an object with `values`, a list of `stringValue`). " +
		"`false`, `0` and the empty string are written, and an empty list is written as an empty `values`. " +
		"An integer is written only when its magnitude is at most 2^53-1; any other number, a null, an object and a list that holds anything but text have no form, and the attribute is not sent.\n")
	b.WriteString("- Resource attributes and log record attributes go where the table above puts them, and attributes follow the order of the table above.\n")
	b.WriteString("- A stored event may hold other keys: keys this contract does not list are not sent.\n")
	b.WriteString("- The directory `examples/otlp/` holds the pretty-printed OTLP form of each flat example, under the same file name. " +
		"A receiver compares their structure; the request body is compact.\n")
	b.WriteString("- A receiver's answer states the number of records it refused in `partialSuccess.rejectedLogRecords`. " +
		"An answer without that number counts none refused.\n")
	return b.String()
}
