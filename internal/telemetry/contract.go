package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Contract file names, relative to the version directory.
const (
	SchemaFile      = "event.schema.json"
	FieldsFile      = "fields.md"
	OTLPMappingFile = "otlp-mapping.md"
)

// SchemaID is the stable $id of the event schema of this contract version.
const SchemaID = "urn:io.rancher:telemetry:contract:" + ContractVersion + ":event"

// member is one key of an object whose key order is the order written.
type member struct {
	key string
	val any
}

// object is a JSON object that keeps its keys in the order given.
type object []member

func (o object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(m.val)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// ContractFiles returns the generated files of the contract directory for
// the current version, by file name: the JSON Schema of one event, the
// field table and the OTLP mapping. All three come from the attribute
// metadata and the closed value lists in in.
func ContractFiles(in ContractInputs) (map[string][]byte, error) {
	schema, err := schemaJSON(in)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		SchemaFile:      schema,
		FieldsFile:      []byte(fieldsMarkdown(in)),
		OTLPMappingFile: []byte(otlpMarkdown()),
	}, nil
}

func schemaJSON(in ContractInputs) ([]byte, error) {
	defs := object{}
	var refs []any
	for _, ev := range Events() {
		defs = append(defs, member{ev.Name, eventSchema(ev, in)})
		refs = append(refs, object{{"$ref", "#/$defs/" + ev.Name}})
	}
	root := object{
		{"$schema", "https://json-schema.org/draft/2020-12/schema"},
		{"$id", SchemaID},
		{"title", "Rancher usage event, flat form, contract " + ContractVersion},
		{"description", "One event as the flat contract form: the fixed keys event.name, time and schema_version, then one key per attribute. Consumers accept keys they do not know."},
		{"oneOf", refs},
		{"$defs", defs},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func eventSchema(ev EventSpec, in ContractInputs) object {
	attrs := AttributesOf(ev.Name)
	required := []string{keyEventName, keyTime, keySchemaVersion}
	props := object{
		{keyEventName, object{
			{"type", "string"}, {"const", ev.Name}, {"description", ev.Description},
		}},
		{keyTime, object{
			{"type", "string"}, {"format", "date-time"}, {"pattern", timePattern},
			{"description", "The start of the UTC hour of the event, RFC 3339, as a whole hour."},
		}},
		{keySchemaVersion, object{
			{"type", "string"}, {"const", ContractVersion},
			{"description", "The contract version the event follows."},
		}},
	}
	for _, a := range attrs {
		props = append(props, member{a.Name, propertySchema(a, in)})
		if a.Required {
			required = append(required, a.Name)
		}
	}
	out := object{
		{"type", "object"},
		{"description", ev.Description},
		{"required", required},
		{"properties", props},
	}
	if ev.Name == EventCommandRun {
		out = append(out, member{"allOf", []any{
			object{
				{"if", object{{"required", []string{attrInstallID}}}},
				{"then", object{{"not", object{{"required", []string{attrMissingReason}}}}}},
				{"else", object{{"required", []string{attrMissingReason}}}},
			},
			object{
				{"if", object{
					{"required", []string{attrOutcome}},
					{"properties", object{{attrOutcome, object{{"const", string(OutcomeOK)}}}}},
				}},
				{"then", object{{"not", object{{"required", []string{attrErrorType}}}}}},
			},
		}})
	}
	return out
}

func scalarSchema(a Attribute, in ContractInputs) object {
	var o object
	switch a.Kind {
	case KindBoolean:
		o = object{{"type", "boolean"}}
	case KindInteger:
		o = object{{"type", "integer"}}
		if a.HasMinimum {
			o = append(o, member{"minimum", a.Minimum})
		}
		if a.HasMaximum {
			o = append(o, member{"maximum", a.Maximum})
		}
	default:
		o = object{{"type", "string"}}
		switch {
		case a.Const != "":
			o = append(o, member{"const", a.Const})
		case a.Values != nil:
			o = append(o, member{"enum", a.Values(in)})
		}
		if a.Pattern != "" {
			o = append(o, member{"pattern", a.Pattern})
		}
		if a.MaxLength > 0 {
			o = append(o, member{"maxLength", a.MaxLength})
		}
	}
	return o
}

func propertySchema(a Attribute, in ContractInputs) object {
	var o object
	if a.Kind == KindStrings {
		item := object{{"type", "string"}}
		if a.Pattern != "" {
			item = append(item, member{"pattern", a.Pattern})
		}
		if a.MaxLength > 0 {
			item = append(item, member{"maxLength", a.MaxLength})
		}
		o = object{{"type", "array"}, {"items", item}}
		if a.MaxItems > 0 {
			o = append(o, member{"maxItems", a.MaxItems})
		}
	} else {
		o = scalarSchema(a, in)
	}
	return append(o,
		member{"description", a.Why},
		member{"x-stability", string(a.Stability)},
		member{"x-absent", a.Absent},
	)
}

func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func valuesCell(a Attribute, in ContractInputs) string {
	switch {
	case a.Kind == KindBoolean:
		return "true or false"
	case a.Kind == KindInteger:
		switch {
		case a.HasMinimum && a.HasMaximum:
			return fmt.Sprintf("integer from %d to %d", a.Minimum, a.Maximum)
		case a.HasMinimum:
			return fmt.Sprintf("integer, at least %d", a.Minimum)
		}
		return "integer"
	case a.Kind == KindStrings:
		return fmt.Sprintf("at most %d names, each matching `%s`, sorted", a.MaxItems, cell(a.Pattern))
	case a.Const != "":
		return "`" + a.Const + "`"
	case a.Values != nil:
		vals := a.Values(in)
		quoted := make([]string, len(vals))
		for i, v := range vals {
			quoted[i] = "`" + v + "`"
		}
		return strings.Join(quoted, ", ")
	case a.Pattern != "":
		return "matches `" + cell(a.Pattern) + "`"
	}
	return "text"
}

func fieldsMarkdown(in ContractInputs) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("# Rancher usage events: what is recorded (contract %s)\n\n", ContractVersion)
	w("This file is generated; do not edit it by hand.\n")
	w("A version directory that holds a file named `DRAFT` is not published and may still change; see `published.json` in the directory above.\n\n")
	w("## The schema's own keys\n\n")
	w("`event.schema.json` carries two keys that are not part of JSON Schema: `x-stability`, the stability marker of an attribute, and `x-absent`, what the attribute's absence means. A validator in strict mode may need them declared as annotations; the events never contain them.\n\n")
	w("## The flat event\n\n")
	w("One event is one JSON object. The fixed keys are `event.name`, `time` (RFC 3339, the start of the UTC hour: a whole hour, never a precise time) and `schema_version` (the contract version). Every other key is an attribute name from the tables below. `event.schema.json` describes the same object. Consumers accept keys they do not know; producers write only the keys listed.\n\n")
	w("An attribute is optional unless the table says required. A producer may stop sending an optional attribute at any time within a major version; absent is always a valid value, and the last column says what it means. Booleans and the flag list are always written, never left absent.\n\n")
	w("Every attribute has a stability marker. All start as `development`; one becomes `stable` once a consumer depends on it, and every attribute a consumer metric uses is `stable` before any upload starts. A `development` attribute may be renamed or removed in a minor version; a `stable` or required attribute only in a new major version.\n\n")
	w("## Limits\n\n")
	w("- Each event is written to a slot: a numbered file the process creates for it in the directory of its UTC hour. An hour has %d slots (N = %d). A process tries %d random slots (k = %d) and drops the event when every one is taken; an hour can lose events before it is full. A process that drops an event leaves a drop marker.\n", SlotsPerHour, SlotsPerHour, SlotTries, SlotTries)
	w("- Once an hour, a process that records an event also runs a short pass. It copies the events of one finished hour into one file, and each file it writes after reading the whole hour carries at most one `%s` event with the number of drop markers that pass read (at most %d markers per file). A process that dies between writing a file and cleaning up can make a later file count the same drops again. Consumers report usage as lower bounds when such an event exists. The version, operating system and architecture of a dropped event are those of the process that ran the pass, not of the processes that dropped events; its `service.instance.id` is a random id of its own and joins to no run.\n", EventEventsDropped, maxMarkersPerFile)
	w("- The pass stops reading slots after %d milliseconds and writes what it read; a file written that way carries no dropped event, and the rest of the hour waits for the next hourly pass.\n", int(readLimit/time.Millisecond))
	w("- Events are removed no earlier than %d hours after their hour ends, and only while the CLI runs; an hour that gets a late event has a second file.\n", int(AgeCap.Hours()))
	w("- An uploaded event is at most %d days old; older events are not uploaded.\n\n", int(MaxUploadAge.Hours()/24))
	w("## Never recorded\n\n")
	w("Resource, namespace, cluster and project names; host names and server URLs; user names; tokens; flag and positional values (the value of `-o` included); file paths; error messages. Every value is from a closed list in this contract, matches a pattern in it, or is a count, a bucket or a random id.\n\n")
	w("## Never uploaded\n\n")
	w("Events written while nothing is sent stay on the machine. A later upload stage writes to a separate directory and starts empty; today's local events are never uploaded.\n\n")
	w("## Command and flag names\n\n")
	w("The names change with every release; this contract does not list them. The valid names for an event are in the `rancher schema` manifest of the event's `service.version`; the release attaches it as the asset `rancher-schema.json`. The name rule is part of the contract: a command is one to three words, each matching `[a-z][a-z0-9-]*` and at most 32 characters; a flag is its long name without dashes, matching `[a-z][a-z0-9-]*`, at most 32 characters; at most %d flags per event, the first %d in sorted order kept.\n\n", maxFlags, maxFlags)
	w("## Version rule\n\n")
	w("`service.version` and `io.rancher.manager.version` hold `MAJOR.MINOR.PATCH` with a leading `v` removed, optionally followed by `-alphaN`, `-betaN` or `-rcN`, with or without a dot before the number; the development forms `dev`, `dev-<hex sha>`, `dev-unknown` and `unknown` as they are; a hotfix tag `X.Y.Z-hotfix-<id>` as `X.Y.Z-hotfix` with the id removed, because a hotfix id can belong to one customer. Anything else, including a build suffix, other text or more than 64 characters, is `_OTHER`.\n\n")
	for _, ev := range Events() {
		w("## Event `%s`\n\n%s\n\n", ev.Name, ev.Description)
		w("| Attribute | Type | Required | Values | Stability | Standard | Why it is recorded | When absent |\n")
		w("|---|---|---|---|---|---|---|---|\n")
		w("| `%s` | string | yes | `%s` | development | new | The event name. | Never absent. |\n", keyEventName, ev.Name)
		w("| `%s` | string | yes | whole UTC hour, RFC 3339 | development | OpenTelemetry | The event time, truncated to the hour. | Never absent. |\n", keyTime)
		w("| `%s` | string | yes | `%s` | development | new | The contract version. | Never absent. |\n", keySchemaVersion, ContractVersion)
		for _, a := range AttributesOf(ev.Name) {
			req := "no"
			if a.Required {
				req = "yes"
			}
			w("| `%s` | %s | %s | %s | %s | %s | %s | %s |\n", a.Name, a.Kind, req, valuesCell(a, in), a.Stability, a.Standard, cell(a.Why), cell(a.Absent))
		}
		w("\n")
	}
	w("## Rules between attributes\n\n")
	w("- `%s` is present exactly when `%s` is absent.\n", attrMissingReason, attrInstallID)
	w("- `%s` is present only when `%s` is `error`.\n", attrErrorType, attrOutcome)
	return b.String()
}

func otlpMarkdown() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("# Mapping the flat event to OTLP (contract %s)\n\n", ContractVersion)
	w("Documentation only: nothing in the CLI encodes OTLP. The wire format is decided with the upload stage. This table says where each flat key would sit in an OTLP logs export; a relay or a Collector can be built from it without a change to the contract.\n\n")
	w("An event is one OTLP log record with an event name. Who produced a batch is stated once, as resource attributes. The scope name is the value of `service.name`; the scope version is the contract version, which is this contract's own use of that field. `schemaUrl` is `https://opentelemetry.io/schemas/1.44.0` on the resource and on the scope.\n\n")
	w("| Flat key | OTLP location | Notes |\n|---|---|---|\n")
	w("| `%s` | log record `eventName` | |\n", keyEventName)
	w("| `%s` | log record `timeUnixNano` | A whole UTC hour. `observedTimeUnixNano` is never written. |\n", keyTime)
	w("| `%s` | scope `version` | |\n", keySchemaVersion)
	seen := map[string]bool{}
	for _, a := range attributes {
		if seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		loc := "log record attribute"
		if a.Placement == PlaceResource {
			loc = "resource attribute"
		}
		note := "string value"
		switch a.Kind {
		case KindBoolean:
			note = "`boolValue`"
		case KindInteger:
			note = "`intValue`"
		case KindStrings:
			note = "`arrayValue` of `stringValue`"
		}
		w("| `%s` | %s | %s |\n", a.Name, loc, note)
	}
	w("\nA resource attribute is the same for every event of a batch; a producer that writes events from several processes groups them by their resource attributes.\n")
	return b.String()
}

// SortedNames returns the attribute names of an event, for tests and docs.
func SortedNames(event string) []string {
	var out []string
	for _, a := range AttributesOf(event) {
		out = append(out, a.Name)
	}
	slices.Sort(out)
	return out
}
