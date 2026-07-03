package output

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"text/template"

	"sigs.k8s.io/yaml"
)

// Tabular lets a payload describe its own table columns/rows for -o table.
type Tabular interface {
	Columns() []string
	Rows() [][]string
}

// Render writes env to w in the requested format.
// format: "" or "table" (human), "json", "yaml", "quiet" (ids), else a Go template.
func Render(w io.Writer, format string, env *Envelope) error {
	switch format {
	case "", "table":
		return renderTable(w, env)
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	case "yaml":
		b, err := yaml.Marshal(env)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case "quiet":
		return renderQuiet(w, env)
	default:
		tmpl, err := template.New("out").Parse(format)
		if err != nil {
			return fmt.Errorf("invalid template: %w", err)
		}
		if err := tmpl.Execute(w, env.Data); err != nil {
			return err
		}
		_, err = fmt.Fprintln(w)
		return err
	}
}

func renderTable(w io.Writer, env *Envelope) error {
	t, ok := env.Data.(Tabular)
	if !ok {
		// Fallback: pretty JSON so table mode never loses data.
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(env.Data)
	}
	tw := tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
	cols := t.Columns()
	for i, c := range cols {
		if i > 0 {
			_, _ = fmt.Fprint(tw, "\t")
		}
		_, _ = fmt.Fprint(tw, c)
	}
	_, _ = fmt.Fprintln(tw)
	for _, row := range t.Rows() {
		for i, cell := range row {
			if i > 0 {
				_, _ = fmt.Fprint(tw, "\t")
			}
			_, _ = fmt.Fprint(tw, cell)
		}
		_, _ = fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func renderQuiet(w io.Writer, env *Envelope) error {
	t, ok := env.Data.(Tabular)
	if !ok {
		return nil
	}
	idIdx := -1
	for i, c := range t.Columns() {
		if c == "ID" {
			idIdx = i
			break
		}
	}
	if idIdx < 0 {
		return nil
	}
	for _, row := range t.Rows() {
		if idIdx < len(row) {
			_, _ = fmt.Fprintln(w, row[idIdx])
		}
	}
	return nil
}
