package runtime

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

const (
	noteRead  = "# warning: this output had characters a terminal cannot show; run the command again with -o json to see the exact values\n"
	noteLocal = "# warning: this output had characters a terminal cannot show; -o json shows the exact values\n"
	noteWrite = "# warning: this output had characters a terminal cannot show; they were left out or replaced\n"
)

// runText runs a command with the descriptor d whose result is res, in
// format, and returns its stdout and stderr.
func runText(t *testing.T, d Descriptor, res Result, format string) (string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	streams := IO{Out: &out, Err: &errBuf, Format: format, Apply: true}
	var cmd Command = fakeRead{d: d, res: res}
	if d.CommandType == Mutating {
		cmd = fakeMutating{d: d, eff: safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionCreate, DisplayKind: "widget", DisplayName: "w1"}, exec: onceOnlyExec(res)}
	}
	_, ce := Run(t.Context(), cmd, mapInput{}, streams, safety.Binding{})
	require.Nil(t, ce)
	return out.String(), errBuf.String()
}

// TestFilterNoteFollowsAResultTextAFilterChanged asserts the line a result
// printed as text gets when a filter removed a character a terminal cannot
// show from it or wrote an invalid byte as U+FFFD: once, after the result's
// other warnings, worded by what the command may do on the server, for a
// value changed by output.FilterValue, by runtime.FilterRemedies or by the
// renderer's own filter, and never for a text whose only remedy commands
// lost their markers.
func TestFilterNoteFollowsAResultTextAFilterChanged(t *testing.T) {
	t.Parallel()
	read := Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeText}
	local := Descriptor{Name: "config widget", CommandType: Local, Shape: ShapeText}
	write := Descriptor{Name: "create widget", CommandType: Mutating, Shape: ShapeText}
	localWrite := Descriptor{Name: "config delete-widget", CommandType: Local, ServerAccess: ServerAccessWrite, Shape: ShapeText}
	for _, tc := range []struct {
		name, format string
		d            Descriptor
		text, wide   string
		warnings     []string
		out, err     string
	}{
		{name: "a hidden character output.FilterValue removed in a read command", d: read,
			text: clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), out: "name: ab\n", err: noteRead},
		{name: "an invalid byte output.FilterValue replaced in a read command", d: read,
			text: clierr.Sprintf("name: %s\n", output.FilterValue("a\xffb")).Marked(), out: "name: a\ufffdb\n", err: noteRead},
		{name: "a newline the remedy filter removed", d: read,
			text: clierr.Sprintf("note: %s\n", FilterRemedies("a\nb")).Marked(), out: "note: ab\n", err: noteRead},
		{name: "a value the renderer's own filter changes", d: read,
			text: "name: a\u200bb\n", out: "name: ab\n", err: noteRead},
		{name: "after the other warnings", d: read, warnings: []string{"first", "second"},
			text: clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), out: "name: ab\n",
			err: "# warning: first\n# warning: second\n" + noteRead},
		{name: "a local command", d: local,
			text: clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), out: "name: ab\n", err: noteLocal},
		{name: "a mutation", d: write,
			text: clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), out: "name: ab\n", err: noteWrite},
		{name: "a local command that writes to the server", d: localWrite,
			text: clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), out: "name: ab\n", err: noteWrite},
		{name: "the wide text", d: read, format: "wide",
			text: "name: ab\n", wide: clierr.Sprintf("name: %s wide\n", output.FilterValue("a\u200bb")).Marked(), out: "name: ab wide\n", err: noteRead},
		{name: "a relayed remedy command with nothing else to remove", d: read,
			text: clierr.Sprintf("run %s\n", FilterRemedies(clierr.Rancher+" auth status")).Marked(), out: "run rancher auth status\n"},
		{name: "a text with nothing to remove", d: read, text: "name: ab\n", out: "name: ab\n"},
		{name: "the output of another program", d: Descriptor{Name: "logs", CommandType: ReadOnly, Shape: ShapeText, PlainText: true},
			text: "a\u200bb\n", out: "a\u200bb\n"},
		{name: "a command that turns the note off", d: Descriptor{Name: "auth logout", CommandType: Local, Shape: ShapeText, NoFilterNote: true},
			text: clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), out: "name: ab\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, errText := runText(t, tc.d, Result{Kind: "Widget", Text: tc.text, WideText: tc.wide, Warnings: tc.warnings}, tc.format)
			assert.Equal(t, tc.out, out, "stdout")
			assert.Equal(t, tc.err, errText, "stderr")
		})
	}
}

// TestFilterNoteIsLeftOutOfJSONAndYAML asserts a result whose text has the
// mark gets no note under -o json or -o yaml, which print no text.
func TestFilterNoteIsLeftOutOfJSONAndYAML(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeText}
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			_, errText := runText(t, d, Result{Kind: "Widget", Text: clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), Data: map[string]string{"name": "a\u200bb"}}, format)
			assert.Empty(t, errText)
		})
	}
}

// TestFilterNoteOfAOnceOnlyResultFollowsItsOutput asserts the note of a
// result shown once is written after its output reached stdout, and is not
// written when that write fails or under -o json.
func TestFilterNoteOfAOnceOnlyResultFollowsItsOutput(t *testing.T) {
	t.Parallel()
	res := Result{
		Kind: "WidgetCreated", Text: clierr.Sprintf("secret: %s\n", output.FilterValue("s\u200b3")).Marked(),
		Data:             map[string]string{"secret": "s\u200b3"},
		CommittedContext: &CommittedContext{ResourceKind: "Widget", ResourceName: "w1"},
	}
	t.Run("written after the output", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := runOnceOnly(t, &out, &errBuf, res, "")
		require.Nil(t, ce)
		assert.Equal(t, "secret: s3\n", out.String())
		assert.Equal(t, noteWrite, errBuf.String())
	})
	t.Run("none when the output fails", func(t *testing.T) {
		t.Parallel()
		var errBuf bytes.Buffer
		cmd := fakeMutating{d: onceOnlyDesc(), eff: safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionCreate, DisplayKind: "token", DisplayName: "t-abc"}, exec: onceOnlyExec(res)}
		_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: partialWriteErr{n: 0}, Err: &errBuf, Apply: true}, safety.Binding{})
		require.NotNil(t, ce)
		assert.NotContains(t, errBuf.String(), "characters a terminal cannot show")
	})
	t.Run("none under json", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		_, ce := runOnceOnly(t, &out, &errBuf, res, "json")
		require.Nil(t, ce)
		assert.Empty(t, errBuf.String())
	})
}

// TestResultOutputCarriesNoRemovalMark asserts that result data that has
// clierr.RemovalMark is written without it in json, yaml and a template, and
// that a warning that has it prints without it.
func TestResultOutputCarriesNoRemovalMark(t *testing.T) {
	t.Parallel()
	marked := "a" + clierr.RemovalMark
	d := Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeText}
	res := Result{Kind: "Widget", Text: "name: a\n", Data: map[string]string{"name": marked}, Warnings: []string{"w " + marked}}
	for _, format := range []string{"json", "yaml", "template={{.name}}"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			out, errText := runText(t, d, res, format)
			assert.NotContains(t, out, clierr.RemovalMark)
			assert.NotContains(t, errText, clierr.RemovalMark)
			var env struct {
				Data     map[string]string `json:"data"`
				Warnings []string          `json:"warnings"`
			}
			switch format {
			case "json":
				require.NoError(t, json.Unmarshal([]byte(out), &env))
				assert.Equal(t, "a", env.Data["name"])
				assert.Equal(t, []string{"w a"}, env.Warnings)
			case "yaml":
				require.NoError(t, yaml.Unmarshal([]byte(out), &env))
				assert.Equal(t, "a", env.Data["name"])
				assert.Equal(t, []string{"w a"}, env.Warnings)
			default:
				assert.Equal(t, "a\n", out)
				assert.Equal(t, "# warning: w a\n", errText)
			}
		})
	}
}

// TestErrorOutputCarriesNoRemovalMark asserts that an error whose message,
// hint and warnings have clierr.RemovalMark is written without it in json,
// yaml and text, and with no escape of it.
func TestErrorOutputCarriesNoRemovalMark(t *testing.T) {
	t.Parallel()
	marked := "x" + clierr.RemovalMark
	ce := clierr.New(clierr.CodeNotFound, "missing "+marked).WithHint("try " + marked).WithWarnings("w " + marked)
	for _, format := range []string{"json", "yaml", ""} {
		t.Run("format "+format, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			WriteError(&errBuf, format, ce, style.Style{}, "", "notice "+marked)
			got := errBuf.String()
			assert.NotContains(t, got, clierr.RemovalMark)
			assert.NotContains(t, got, "\u2063")
			var env struct {
				Data struct {
					Error struct {
						Message string `json:"message"`
						Hint    string `json:"hint"`
					} `json:"error"`
				} `json:"data"`
				Warnings []string `json:"warnings"`
			}
			switch format {
			case "json":
				require.NoError(t, json.Unmarshal([]byte(got), &env))
			case "yaml":
				require.NoError(t, yaml.Unmarshal([]byte(got), &env))
			default:
				assert.Equal(t, "# warning: notice x\n# warning: w x\nerror (NOT_FOUND): missing x\n  hint: try x\n", got)
				return
			}
			assert.Equal(t, "missing x", env.Data.Error.Message)
			assert.Equal(t, "try x", env.Data.Error.Hint)
			assert.Equal(t, []string{"notice x", "w x"}, env.Warnings)
		})
	}
	assert.Contains(t, ce.Message, clierr.RemovalMark, "the error itself keeps its text")
}

// markedDetails is the details of an error whose one line has a value.
type markedDetails struct{ Value string }

func (d markedDetails) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("name: ", d.Value))}
}

// TestARelayedRemovalMarkPrintsNoEscape asserts that a value that has a
// hidden character and clierr.RemovalMark prints with an escape of the
// hidden character and nothing of the mark, in a warning, an error message,
// a details value and a table cell.
func TestARelayedRemovalMarkPrintsNoEscape(t *testing.T) {
	t.Parallel()
	marked := "a\u200bb" + clierr.RemovalMark
	t.Run("a warning", func(t *testing.T) {
		t.Parallel()
		d := Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeText}
		_, errText := runText(t, d, Result{Text: "done\n", Warnings: []string{"name " + marked}}, "")
		assert.Equal(t, "# warning: \"name a\\u200bb\"\n", errText)
	})
	t.Run("an error message", func(t *testing.T) {
		t.Parallel()
		var errBuf bytes.Buffer
		WriteError(&errBuf, "", clierr.New(clierr.CodeNotFound, "missing "+marked), style.Style{}, "")
		assert.Equal(t, "error (NOT_FOUND): \"missing a\\u200bb\"\n", errBuf.String())
	})
	t.Run("a details value", func(t *testing.T) {
		t.Parallel()
		var errBuf bytes.Buffer
		WriteError(&errBuf, "", clierr.New(clierr.CodeNotFound, "missing").WithDetails(markedDetails{Value: marked}), style.Style{}, "")
		assert.Equal(t, "error (NOT_FOUND): missing\n  name: \"a\\u200bb\"\n", errBuf.String())
	})
	t.Run("a table cell", func(t *testing.T) {
		t.Parallel()
		d := Descriptor{Name: "list widget", CommandType: ReadOnly, Shape: ShapeTabular}
		out, _ := runText(t, d, Result{Data: widgetTable{Name: marked}}, "")
		assert.Equal(t, "NAME\n\"a\\u200bb\"\n", out)
	})
}

// TestAResultWithAFilteredValueIsPrintedUnstyled asserts that on a terminal
// that styles text, a result text that has clierr.RemovalMark prints its
// plain text, followed by the note, and the same result with nothing removed
// prints its styled text.
func TestAResultWithAFilteredValueIsPrintedUnstyled(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "get widget", CommandType: ReadOnly, Shape: ShapeText}
	styled := func(s style.Style) string { return s.Bold("name: ab") + "\n" }
	for _, tc := range []struct {
		name, text, out, err string
	}{
		{"a filtered value", clierr.Sprintf("name: %s\n", output.FilterValue("a\u200bb")).Marked(), "name: ab\n", noteRead},
		{"nothing removed", "name: ab\n", style.Style{Enabled: true}.Bold("name: ab") + "\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			_, ce := renderResult(IO{Out: &out, Err: &errBuf, Format: "table", Color: "always", OutTTY: true}, d, Result{Kind: "Widget", Text: tc.text, StyledText: styled})
			require.Nil(t, ce)
			assert.Equal(t, tc.out, out.String(), "stdout")
			assert.Equal(t, tc.err, style.Unstyled(errBuf.String()), "stderr")
		})
	}
}

// TestAPlainTextResultPrintsNoRemovalMark asserts that the text of a command
// whose descriptor sets PlainText (the output of another program) prints
// without clierr.RemovalMark, in its text and its wide text, and gets no note:
// a value the command filtered before it joined that output carries the mark.
func TestAPlainTextResultPrintsNoRemovalMark(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "logs", CommandType: ReadOnly, Shape: ShapeText, PlainText: true}
	header := clierr.Sprintf("pod %s\n", output.FilterValue("a\u200bb")).Marked()
	for _, format := range []string{"", "wide"} {
		t.Run("format "+format, func(t *testing.T) {
			t.Parallel()
			out, errText := runText(t, d, Result{Kind: "Logs", Text: header + "raw\n", WideText: header + "raw wide\n"}, format)
			assert.NotContains(t, out, clierr.RemovalMark)
			want := "pod ab\nraw\n"
			if format == "wide" {
				want = "pod ab\nraw wide\n"
			}
			assert.Equal(t, want, out)
			assert.Empty(t, errText)
		})
	}
}
