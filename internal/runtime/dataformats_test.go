package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
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
