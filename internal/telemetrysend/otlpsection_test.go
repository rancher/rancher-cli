package telemetrysend

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

func generatedMapping(t *testing.T, section string) string {
	t.Helper()
	files, err := telemetry.ContractFiles(telemetry.ContractInputs{ErrorCodes: []string{"GENERIC"}, OTLPEncoding: section})
	require.NoError(t, err)
	return string(files[telemetry.OTLPMappingFile])
}

// sectionPhrases are the statements the JSON encoding section must make.
var sectionPhrases = []string{
	"one `resourceLogs` entry per event",
	"never grouped",
	"lowerCamelCase",
	"`timeUnixNano` and `intValue` are decimal strings",
	"`scope.name` equals `service.name`",
	"`scope.version` is `schema_version`",
	"`eventName` is `event.name`",
	"writes no `body`, `severityNumber`, `severityText`, `observedTimeUnixNano`, `traceId`, `spanId`, `flags` or `droppedAttributesCount`",
	"`false`, `0` and the empty string are written",
	"`stringValue`", "`boolValue`", "`intValue`", "`arrayValue`",
	"`resourceLogs[].schemaUrl`",
	"`resourceLogs[].scopeLogs[].schemaUrl`",
	"attributes follow the order of the table above",
	"an event whose `time` cannot be read is sent without `timeUnixNano`",
	"keys this contract does not list are not sent",
	"the body is compact JSON and is not compressed",
	"`examples/otlp/`",
	"pretty-printed",
	"`partialSuccess.rejectedLogRecords`",
}

// TestJSONEncodingSectionMatchesTheEncoder asserts the generated mapping
// holds each statement the section must make, a code span for every JSON key
// the shape test allows, and the path, content type and schema URL the
// encoder writes; that the schema URL appears once and equals the schemaUrl of
// the encoded examples; that the phrase "nothing sends it yet" is in the
// first paragraph of the mapping and on one line of the contract README only;
// and that the section names no version of any receiver.
func TestJSONEncodingSectionMatchesTheEncoder(t *testing.T) {
	section := JSONEncodingSection()
	mapping := generatedMapping(t, section)
	for _, phrase := range sectionPhrases {
		assert.Contains(t, mapping, phrase)
	}
	var keys []string
	for _, level := range shapeKeys {
		keys = append(keys, level...)
	}
	keys = append(keys, "values")
	for _, key := range keys {
		assert.Contains(t, mapping, "`"+key+"`")
	}

	req := encodeAll(t, flatExamples(t, "valid")["command-run-full.json"])
	assert.Contains(t, mapping, "`"+req.Path+"`")
	assert.Contains(t, mapping, "`"+req.ContentType+"`")
	url := decodeExport(t, req.Body).ResourceLogs[0].SchemaURL
	require.NotEmpty(t, url)
	assert.Equal(t, 1, strings.Count(mapping, url), "the schema URL is written once")

	assert.Contains(t, mapping, "Resource attributes state who produced an event. How many events a producer puts under one resource is that producer's rule", "the paragraph before the section agrees with it")
	assert.NotContains(t, mapping, "groups them by their resource attributes")
	assert.NotContains(t, mapping, "the same for every event of a batch")

	const phrase = "nothing sends it yet"
	assert.Equal(t, 1, strings.Count(mapping, phrase))
	paragraphs := strings.Split(mapping, "\n\n")
	require.Greater(t, len(paragraphs), 1)
	assert.Contains(t, paragraphs[1], phrase, "it must be in the first paragraph")
	readme, err := os.ReadFile(filepath.Join(contractRoot, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(readme), phrase))
	lines := 0
	for line := range strings.SplitSeq(string(readme), "\n") {
		if strings.Contains(line, phrase) {
			lines++
		}
	}
	assert.Equal(t, 1, lines, "the phrase must sit on one README line")

	assert.Empty(t, receiverVersionFindings(section))
}

var (
	versionNumber = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)
	digit         = regexp.MustCompile(`\d`)
	sentenceEnd   = regexp.MustCompile(`[.!?]\s|\n`)
)

// receiverVersionFindings returns one line for each version number left in
// text after the schema URL and the contract version are removed, and for each
// sentence that names a Collector and holds a digit.
func receiverVersionFindings(text string) []string {
	var out []string
	rest := strings.ReplaceAll(strings.ReplaceAll(text, otlpSchemaURL, ""), telemetry.ContractVersion, "")
	for _, v := range versionNumber.FindAllString(rest, -1) {
		out = append(out, "version number "+v)
	}
	for _, sentence := range sentenceEnd.Split(text, -1) {
		if strings.Contains(sentence, "Collector") && digit.MatchString(sentence) {
			out = append(out, "a Collector with a digit: "+sentence)
		}
	}
	return out
}

// TestReceiverVersionRule asserts the rule finds a version number and a
// Collector named in a sentence with a digit, and accepts the section, which
// holds the schema URL.
func TestReceiverVersionRule(t *testing.T) {
	assert.Empty(t, receiverVersionFindings(JSONEncodingSection()))
	assert.Contains(t, JSONEncodingSection(), otlpSchemaURL)
	assert.NotEmpty(t, receiverVersionFindings("It needs version 0.99.0 of a receiver."))
	assert.NotEmpty(t, receiverVersionFindings("A Collector from 2025 reads it."))
	assert.Empty(t, receiverVersionFindings("A Collector reads it. It was built in 2025."))
}

// TestPackageCommentNamesTheEncoder asserts the package comment says the
// package encodes events and that it makes no request.
func TestPackageCommentNamesTheEncoder(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "origin.go", nil, parser.ParseComments|parser.PackageClauseOnly)
	require.NoError(t, err)
	require.NotNil(t, f.Doc, "origin.go has the package comment")
	doc := strings.Join(strings.Fields(f.Doc.Text()), " ")
	assert.Contains(t, doc, "encodes stored events as OTLP/HTTP JSON")
	assert.Contains(t, doc, "It makes no request and imports no HTTP package.")
}
