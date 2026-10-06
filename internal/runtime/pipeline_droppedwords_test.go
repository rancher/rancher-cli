package runtime

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// droppedWordsInput is an emptyBoolInput that also answers DroppedWords.
type droppedWordsInput struct {
	emptyBoolInput
	words  []string
	format string
	color  string
	ok     bool
}

func (d droppedWordsInput) DroppedWords() ([]string, string, string, bool) {
	return d.words, d.format, d.color, d.ok
}

// TestRunRefusesWordsAfterABareDash asserts that the pipeline refuses,
// before the command runs and ahead of every other check (an empty
// --output, an empty bool value, the read-only and dry-run gates), a
// command line the Input reports words after a bare "-" for: VALIDATION at
// exit 2, a message naming the words, shell-quoted, at most five (six
// when the fifth is a flag and the next its value) and a count of the
// rest, and a hint saying where a "-" goes. An Input that
// reports no such word, or does not answer DroppedWords, runs the command.
func TestRunRefusesWordsAfterABareDash(t *testing.T) {
	t.Parallel()
	read := Descriptor{Name: "get pod", CommandType: ReadOnly}
	mutating := Descriptor{Name: "create namespace", CommandType: Mutating}
	const (
		rule = "a - on its own ends the command line, so the words after it would be ignored: "
		hint = "put - last, or after -- when it is meant as an argument"
	)

	for _, tc := range []struct {
		name    string
		d       Descriptor
		in      mapInput
		empty   []string
		streams IO
		words   []string
		message string
	}{
		{
			name:    "one word",
			d:       read,
			words:   []string{"--dry-run"},
			message: rule + "'--dry-run'",
		},
		{
			name:    "words with a value to quote",
			d:       read,
			words:   []string{"--cluster", "c 1", "-o", "json"},
			message: rule + `'--cluster' 'c 1' '-o' 'json'`,
		},
		{
			name:    "more than five words",
			d:       read,
			words:   []string{"a", "b", "c", "d", "e", "f", "g"},
			message: rule + "'a' 'b' 'c' 'd' 'e' and 2 more",
		},
		{
			name:    "a flag named last is named with its value",
			d:       read,
			words:   []string{"a", "b", "c", "d", "-o", "json", "x"},
			message: rule + "'a' 'b' 'c' 'd' '-o' 'json' and 1 more",
		},
		{
			name:    "a flag named last and its value are the last words",
			d:       read,
			words:   []string{"a", "b", "c", "d", " --cluster", "c1"},
			message: rule + "'a' 'b' 'c' 'd' ' --cluster' 'c1'",
		},
		{
			name:    "a flag named last before another flag is named alone",
			d:       read,
			words:   []string{"a", "b", "c", "d", "--dry-run", "--apply", "x"},
			message: rule + "'a' 'b' 'c' 'd' '--dry-run' and 2 more",
		},
		{
			name:    "ahead of the empty --output check",
			d:       read,
			in:      mapInput{s: map[string]string{"output": ""}, is: map[string]bool{"output": true}},
			words:   []string{"x"},
			message: rule + "'x'",
		},
		{
			name:    "ahead of the empty bool value rule",
			d:       mutating,
			empty:   []string{"--apply"},
			words:   []string{"--dry-run"},
			message: rule + "'--dry-run'",
		},
		{
			name:    "ahead of the dry-run gate of a command that is not mutating",
			d:       read,
			streams: IO{DryRun: true},
			words:   []string{"x"},
			message: rule + "'x'",
		},
		{
			name:    "ahead of the read-only gate of a mutating command",
			d:       mutating,
			streams: IO{ReadOnly: true, ReadOnlySources: []string{"--read-only"}},
			words:   []string{"--apply"},
			message: rule + "'--apply'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			streams := tc.streams
			streams.Out, streams.Err, streams.Format = &out, &errBuf, "json"
			in := droppedWordsInput{
				emptyBoolInput: emptyBoolInput{mapInput: tc.in, flags: tc.empty, ok: len(tc.empty) > 0},
				words:          tc.words,
				ok:             true,
			}
			_, ce := Run(t.Context(), countingRead{d: tc.d, runs: &runs}, in, streams, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, hint, ce.Hint)
			assert.Equal(t, 2, ce.ExitCode())
			assert.Zero(t, runs, "the command must not run")
		})
	}

	for name, in := range map[string]Input{
		"the Input finds no word after a bare dash": droppedWordsInput{words: []string{"x"}},
		"the Input cannot tell":                     mapInput{},
	} {
		t.Run(name+" runs", func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), countingRead{d: read, runs: &runs}, in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
			require.Nil(t, ce)
			assert.Equal(t, 1, runs)
		})
	}
}

// TestWordsAfterABareDashRenderInTheFormatTheLineSelects asserts that the
// refusal of words after a bare "-" renders in the output format the
// Input reports for the whole typed line, which can come from the words
// after the "-", and not in the parsed format, which lacks them: the JSON
// or YAML envelope for json or yaml, the text form for none, whatever the
// parsed format is.
func TestWordsAfterABareDashRenderInTheFormatTheLineSelects(t *testing.T) {
	t.Parallel()
	read := Descriptor{Name: "get pod", CommandType: ReadOnly}
	for _, tc := range []struct{ parsed, typed string }{
		{"", "json"},
		{"", "yaml"},
		{"json", "yaml"},
		{"json", ""},
	} {
		t.Run(tc.parsed+" parsed, "+tc.typed+" typed", func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			in := droppedWordsInput{words: []string{"-o", tc.typed}, format: tc.typed, ok: true}
			_, ce := Run(t.Context(), countingRead{d: read, runs: &runs}, in, IO{Out: &out, Err: &errBuf, Format: tc.parsed}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Zero(t, runs)
			var env struct {
				Kind string `json:"kind"`
				Data struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				} `json:"data"`
			}
			switch tc.typed {
			case "json":
				require.NoError(t, json.Unmarshal(errBuf.Bytes(), &env), errBuf.String())
			case "yaml":
				require.False(t, json.Valid(errBuf.Bytes()), "YAML, not JSON: %s", errBuf.String())
				require.NoError(t, yaml.Unmarshal(errBuf.Bytes(), &env), errBuf.String())
			default:
				require.True(t, strings.HasPrefix(errBuf.String(), "error (VALIDATION): "), errBuf.String())
				return
			}
			assert.Equal(t, "Error", env.Kind)
			assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
		})
	}
}

// TestWordsAfterABareDashRenderInTheColorTheLineSelects asserts that the
// refusal of words after a bare "-" takes its color from the --color
// value the Input reports for the whole typed line, not from the parsed
// one, which lacks the words after the "-".
func TestWordsAfterABareDashRenderInTheColorTheLineSelects(t *testing.T) {
	t.Parallel()
	read := Descriptor{Name: "get pod", CommandType: ReadOnly}
	for _, tc := range []struct {
		parsed, typed string
		styled        bool
	}{
		{"never", "always", true},
		{"always", "never", false},
		{"", "always", true},
	} {
		t.Run(tc.parsed+" parsed, "+tc.typed+" typed", func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			in := droppedWordsInput{words: []string{"--color", tc.typed}, color: tc.typed, ok: true}
			_, ce := Run(t.Context(), countingRead{d: read, runs: &runs}, in, IO{Out: &out, Err: &errBuf, Color: tc.parsed}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Zero(t, runs)
			assert.Equal(t, tc.styled, strings.Contains(errBuf.String(), "\x1b["), "%q", errBuf.String())
		})
	}
}
