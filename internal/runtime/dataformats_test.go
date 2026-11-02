package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// holdsMarkerRune reports whether s holds one of the five characters a
// remedy marker is made of, U+2060 to U+2064.
func holdsMarkerRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
}

// markedRow is result data with a marked command in two of its cells and a
// value a server could send in the third.
type markedRow struct {
	Name   string `json:"name"`
	Next   string `json:"next"`
	Server string `json:"server"`
}

func (r markedRow) Columns() []string { return []string{"NAME", "NEXT", "SERVER"} }
func (r markedRow) Rows() [][]string  { return [][]string{{r.Name, r.Next, r.Server}} }

func newMarkedRow(server string) markedRow {
	return markedRow{Name: clierr.Rancher + " get pods", Next: clierr.Rancher + " config status", Server: server}
}

// dataFormats are the formats a result renders in: every literal of
// runtime.OutputFormats, and the prefix of a template.
var dataFormats = []struct{ name, format string }{
	{"table", "table"},
	{"wide", "wide"},
	{"name", "name"},
	{"json", "json"},
	{"yaml", "yaml"},
	{"template", "template={{.name}} {{.next}} {{.server}}"},
}

// TestEveryOutputFormatHasADataRow asserts the rows below are the formats
// the CLI accepts: a format added to runtime.OutputFormats fails until it has
// a row.
func TestEveryOutputFormatHasADataRow(t *testing.T) {
	t.Parallel()
	var got []string
	for _, f := range dataFormats {
		name, _, _ := strings.Cut(f.format, "=")
		got = append(got, name)
	}
	want := append(slices.Clone(runtime.OutputFormats), "template")
	assert.ElementsMatch(t, want, got)
}

// dataCommand returns res from a read command that renders in every format.
type dataCommand struct{ res runtime.Result }

func (dataCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "get thing", CommandType: runtime.ReadOnly, Shape: runtime.ShapeTabular}
}

func (c dataCommand) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	return c.res, nil
}

// TestAMarkedCommandInResultDataLeavesNoMarkerInAnyFormat asserts, one
// format at a time, with a config directory in use, that a marked command in
// the data of a result leaves no character of the remedy marker (U+2060 to
// U+2064): json and yaml write it with the directory after the word rancher,
// and table, wide, name and template write the plain command.
func TestAMarkedCommandInResultDataLeavesNoMarkerInAnyFormat(t *testing.T) {
	t.Parallel()
	for _, f := range dataFormats {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			dir := t.TempDir()
			prefix, printable := runtime.RemedyPrefix(dir)
			require.True(t, printable)
			streams := runtime.IO{Out: &out, Err: &errBuf, Format: f.format, RemedyDir: dir}
			_, ce := runtime.Run(t.Context(), dataCommand{runtime.Result{Kind: "Report", Data: newMarkedRow("a value")}}, siteInput{}, streams, safety.Binding{})
			require.Nil(t, ce)
			assert.False(t, holdsMarkerRune(out.String()), "%s output holds a character of the remedy marker: %q", f.name, out.String())
			assert.False(t, holdsMarkerRune(errBuf.String()), "%s: %q", f.name, errBuf.String())
			switch f.name {
			case "json", "yaml":
				var doc struct {
					Data markedRow `json:"data"`
				}
				require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
				assert.Equal(t, "rancher"+prefix+" get pods", doc.Data.Name)
				assert.Equal(t, "rancher"+prefix+" config status", doc.Data.Next)
			case "name":
				assert.Contains(t, out.String(), "rancher get pods")
			default:
				assert.Contains(t, out.String(), "rancher config status")
			}
		})
	}
}

// TestAServerValueInResultDataKeepsItsBytesInStructuredOutput asserts text
// in the data of a result that holds U+2063 and the word rancher, which a
// server, a file or another process can send, is written as it is in json and
// yaml: it is not this process's marker, so nothing removes it or names the
// directory in it, while the marked command beside it names the directory.
func TestAServerValueInResultDataKeepsItsBytesInStructuredOutput(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"json", "yaml"} {
		var out, errBuf bytes.Buffer
		dir := t.TempDir()
		prefix, printable := runtime.RemedyPrefix(dir)
		require.True(t, printable)
		streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: dir}
		_, ce := runtime.Run(t.Context(), dataCommand{runtime.Result{Kind: "Report", Data: newMarkedRow(plantedRemedy)}}, siteInput{}, streams, safety.Binding{})
		require.Nil(t, ce)
		var doc struct {
			Data markedRow `json:"data"`
		}
		require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
		assert.Equal(t, plantedRemedy, doc.Data.Server, format)
		assert.Equal(t, "rancher"+prefix+" get pods", doc.Data.Name, "the marked command beside it names the directory: %s", format)
		if format == "json" {
			require.True(t, json.Valid(out.Bytes()))
		}
	}
}

// dryRunData is a mutating command whose preview carries a marked command in
// the field the envelope shows for the target.
type dryRunData struct{}

func (dryRunData) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "clusters scale", CommandType: runtime.Mutating}
}

func (dryRunData) Plan(context.Context, runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	eff := safety.Effect{Action: safety.ActionUpdate, Target: clierr.Rancher + " get pods", DisplayKind: "Cluster", DisplayName: clierr.Rancher + " config status"}
	return eff, func(context.Context) (runtime.Result, error) { return runtime.Result{}, nil }, nil
}

// TestAMarkedCommandInADryRunLeavesNoMarkerInStructuredOutput asserts the
// preview of a dry run, written as json, yaml or a template, holds a marked
// command in a field its envelope carries with no character of the remedy
// marker: json and yaml write the command with the directory after the word
// rancher, a template writes the plain command.
func TestAMarkedCommandInADryRunLeavesNoMarkerInStructuredOutput(t *testing.T) {
	t.Parallel()
	for name, format := range map[string]string{"json": "json", "yaml": "yaml", "template": "template={{.displayName}} {{.target}}"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			dir := t.TempDir()
			prefix, printable := runtime.RemedyPrefix(dir)
			require.True(t, printable)
			streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, DryRun: true, RemedyDir: dir}
			_, ce := runtime.Run(t.Context(), dryRunData{}, siteInput{}, streams, safety.Binding{})
			require.Nil(t, ce)
			assert.False(t, holdsMarkerRune(out.String()), "%s output holds a character of the remedy marker: %q", name, out.String())
			if name == "template" {
				assert.Contains(t, out.String(), "rancher config status rancher get pods")
				return
			}
			var doc struct {
				Data struct {
					DisplayName string `json:"displayName"`
					Target      string `json:"target"`
				} `json:"data"`
			}
			require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
			assert.Equal(t, "rancher"+prefix+" config status", doc.Data.DisplayName)
			assert.Equal(t, "rancher"+prefix+" get pods", doc.Data.Target)
		})
	}
}

// refusedByYAMLLibrary is a text that holds DEL, U+009B, U+0085, U+FFFE and
// U+FFFF: a character of each kind the YAML library refuses (it changes U+0085
// into a space).
const refusedByYAMLLibrary = "plain\x7f\u009b\u0085\ufffe\uffff" + "tail"

// holdsRefusedCharacter reports whether s holds a character the yaml output
// writes as an escape.
func holdsRefusedCharacter(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == 0x7f || r >= 0x80 && r <= 0x9f || r == 0xfffe || r == 0xffff
	})
}

// TestAServerValueThatTheYamlLibraryRefusesIsWrittenAsAnEscape asserts a value
// in the data of a result that holds a character the YAML library refuses (DEL,
// U+0080 to U+009F, U+FFFE or U+FFFF) is written by -o yaml with no error and no raw character, and reads back as
// the original text beside a marked command that is rewritten, also when the
// value holds the marker itself; -o json writes the same value with its raw
// characters.
func TestAServerValueThatTheYamlLibraryRefusesIsWrittenAsAnEscape(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		server func(prefix string) (sent, want string)
	}{
		{"a plain value", func(string) (string, string) { return refusedByYAMLLibrary, refusedByYAMLLibrary }},
		{"a value that holds this process's marker", func(prefix string) (string, string) {
			return clierr.Rancher + " get pods" + refusedByYAMLLibrary, "rancher" + prefix + " get pods" + refusedByYAMLLibrary
		}},
	} {
		for _, format := range []string{"yaml", "json"} {
			t.Run(row.name+" as "+format, func(t *testing.T) {
				t.Parallel()
				var out, errBuf bytes.Buffer
				dir := t.TempDir()
				prefix, printable := runtime.RemedyPrefix(dir)
				require.True(t, printable)
				sent, want := row.server(prefix)
				streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: dir}
				_, ce := runtime.Run(t.Context(), dataCommand{runtime.Result{Kind: "Report", Data: newMarkedRow(sent)}}, siteInput{}, streams, safety.Binding{})
				require.Nil(t, ce)
				require.NotEmpty(t, out.String())
				assert.Empty(t, errBuf.String())
				var doc struct {
					Data markedRow `json:"data"`
				}
				if format == "yaml" {
					require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
					assert.False(t, holdsRefusedCharacter(out.String()), "a raw character in %q", out.String())
				} else {
					require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
					assert.True(t, strings.ContainsRune(out.String(), '\x7f'), "json writes DEL as it is")
				}
				assert.Equal(t, want, doc.Data.Server)
				assert.Equal(t, "rancher"+prefix+" get pods", doc.Data.Name)
			})
		}
	}
}

// dryRunControlData is a mutating command whose preview carries a marked
// command and characters the YAML library refuses in the field the envelope
// shows for the target.
type dryRunControlData struct{}

func (dryRunControlData) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "clusters scale", CommandType: runtime.Mutating}
}

func (dryRunControlData) Plan(context.Context, runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	eff := safety.Effect{Action: safety.ActionUpdate, Target: "cluster-1", DisplayKind: "Cluster", DisplayName: clierr.Rancher + " config status\x7f\u009b"}
	return eff, func(context.Context) (runtime.Result, error) { return runtime.Result{}, nil }, nil
}

// TestAPreviewThatHoldsACharacterTheYamlLibraryRefusesIsWrittenAsAnEscape
// asserts the preview of a dry run written as yaml, which holds a marked
// command beside DEL and U+009B, is written with no error and no raw
// character and reads back as the rewritten command followed by both
// characters.
func TestAPreviewThatHoldsACharacterTheYamlLibraryRefusesIsWrittenAsAnEscape(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)
	streams := runtime.IO{Out: &out, Err: &errBuf, Format: "yaml", DryRun: true, RemedyDir: dir}
	_, ce := runtime.Run(t.Context(), dryRunControlData{}, siteInput{}, streams, safety.Binding{})
	require.Nil(t, ce)
	var doc struct {
		Data struct {
			DisplayName string `json:"displayName"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
	assert.Equal(t, "rancher"+prefix+" config status\x7f\u009b", doc.Data.DisplayName)
	assert.False(t, holdsRefusedCharacter(out.String()), "a raw character in %q", out.String())
	assert.Empty(t, errBuf.String())
}

// failingCommand is a read command that fails with err.
type failingCommand struct{ err error }

func (failingCommand) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "get thing", CommandType: runtime.ReadOnly, Shape: runtime.ShapeTabular}
}

func (c failingCommand) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, c.err
}

// TestAnErrorThatHoldsACharacterTheYamlLibraryRefusesIsWrittenAsAnEscape
// asserts the error a command fails with, written to standard error under
// -o yaml, holds its message and hint exactly with no raw DEL or U+009B.
func TestAnErrorThatHoldsACharacterTheYamlLibraryRefusesIsWrittenAsAnEscape(t *testing.T) {
	t.Parallel()
	failure := clierr.New(clierr.CodeNotLoggedIn, "bad "+refusedByYAMLLibrary).WithHint("try " + refusedByYAMLLibrary + " again")
	var out, errBuf bytes.Buffer
	streams := runtime.IO{Out: &out, Err: &errBuf, Format: "yaml"}
	_, ce := runtime.Run(t.Context(), failingCommand{failure}, siteInput{}, streams, safety.Binding{})
	require.Same(t, failure, ce)
	var doc struct {
		Data struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal(errBuf.Bytes(), &doc))
	assert.Equal(t, clierr.CodeNotLoggedIn, doc.Data.Error.Code)
	assert.Equal(t, "bad "+refusedByYAMLLibrary, doc.Data.Error.Message)
	assert.Equal(t, "try "+refusedByYAMLLibrary+" again", doc.Data.Error.Hint)
	assert.False(t, holdsRefusedCharacter(errBuf.String()), "a raw character in %q", errBuf.String())
	assert.Empty(t, out.String())
}

var errScriptedWrite = errors.New("scripted write failure")

// scriptedWriter fails every call from its failFrom-th on and the one call
// named failOnly, 1-based, and keeps the bytes of the calls it accepted. A
// zero value of each turns it off.
type scriptedWriter struct {
	buf      bytes.Buffer
	calls    int
	failFrom int
	failOnly int
}

func (w *scriptedWriter) Write(p []byte) (int, error) {
	w.calls++
	if (w.failFrom > 0 && w.calls >= w.failFrom) || w.calls == w.failOnly {
		return 0, errScriptedWrite
	}
	return w.buf.Write(p)
}

// everyErrorFormat is every format an error is written in, the empty one
// included.
var everyErrorFormat = []string{"", "table", "wide", "json", "yaml", "name", "template={{.}}"}

// TestAnErrorThatCannotBeWrittenKeepsItsExitCode asserts that when every write
// to standard error fails, in every format, Run still returns the error the
// command failed with; the exit code is the code's own.
func TestAnErrorThatCannotBeWrittenKeepsItsExitCode(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		code string
		exit int
	}{{clierr.CodeValidation, 2}, {clierr.CodeNotFound, 4}} {
		for _, format := range everyErrorFormat {
			failure := clierr.New(row.code, "message")
			var out bytes.Buffer
			errW := scriptedWriter{failFrom: 1}
			streams := runtime.IO{Out: &out, Err: &errW, Format: format}
			status, ce := runtime.Run(t.Context(), failingCommand{failure}, siteInput{}, streams, safety.Binding{})
			require.Same(t, failure, ce, "%s %q", row.code, format)
			assert.Zero(t, status, "%s %q", row.code, format)
			assert.Equal(t, row.exit, ce.ExitCode(), "%s %q", row.code, format)
			assert.Zero(t, errW.buf.Len(), "%s %q", row.code, format)
			assert.Empty(t, out.String(), "%s %q", row.code, format)
		}
	}
}

// TestAnErrorWhoseFirstWriteFailsIsWrittenAsOneLine asserts that when the
// first write to standard error fails, the one line is written, in every
// format, and the exit code is the error's own.
func TestAnErrorWhoseFirstWriteFailsIsWrittenAsOneLine(t *testing.T) {
	t.Parallel()
	for _, format := range everyErrorFormat {
		failure := clierr.New(clierr.CodeValidation, "message")
		var out bytes.Buffer
		errW := scriptedWriter{failOnly: 1}
		streams := runtime.IO{Out: &out, Err: &errW, Format: format}
		_, ce := runtime.Run(t.Context(), failingCommand{failure}, siteInput{}, streams, safety.Binding{})
		require.Same(t, failure, ce, "%q", format)
		assert.Equal(t, 2, ce.ExitCode(), "%q", format)
		assert.Equal(t, "error (VALIDATION): message\n", errW.buf.String(), "%q", format)
	}
}

// TestNameOutputReturnsAFailedWriteToStandardOutput asserts -o name over a
// standard output that cannot be written ends with a GENERIC error that names
// the write error, and that error is written to standard error.
func TestNameOutputReturnsAFailedWriteToStandardOutput(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	outW := scriptedWriter{failFrom: 1}
	streams := runtime.IO{Out: &outW, Err: &errBuf, Format: "name"}
	_, ce := runtime.Run(t.Context(), dataCommand{runtime.Result{Kind: "Report", Data: newMarkedRow("a value")}}, siteInput{}, streams, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, errScriptedWrite.Error())
	assert.Contains(t, errBuf.String(), "error (GENERIC):")
}
