package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"text/template"

	"sigs.k8s.io/yaml"
)

// Tabular lets a payload describe its own table columns/rows for -o table.
type Tabular interface {
	Columns() []string
	Rows() [][]string
}

// TemplatePrefix marks an --output value as Go template source: everything
// after it is the template. The single source of truth for the prefix — the
// pipeline's format validation and Render both key off it.
const TemplatePrefix = "template="

// Render writes env to w in the requested format.
// format: "" or "table" (user-facing), "json", "yaml", "name" (ids), or
// "template=<go-template>". Anything else is an error; the pipeline validates
// the format before the command runs, so hitting it here is defense in depth.
func Render(w io.Writer, format string, env *Envelope) error {
	switch format {
	case "", "table":
		return renderTable(w, env)
	case "json":
		tree, hit, err := scrubbedEnvelopeTree(env)
		if err != nil {
			return err
		}
		if !hit {
			return EncodeJSON(w, env)
		}
		return EncodeJSON(w, tree)
	case "yaml":
		tree, hit, err := scrubbedEnvelopeTree(env)
		if err != nil {
			return err
		}
		src := any(env)
		if hit {
			src = tree
		}
		b, err := yaml.Marshal(src)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case "name":
		return renderName(w, env)
	default:
		src, ok := strings.CutPrefix(format, TemplatePrefix)
		if !ok {
			return fmt.Errorf("unsupported output format %q", format)
		}
		// An empty source parses fine but would render a bare newline; the
		// pipeline's validateFormat already rejects it, so match that contract
		// here for direct callers.
		if src == "" {
			return fmt.Errorf("empty template source")
		}
		tmpl, err := template.New("out").Parse(src)
		if err != nil {
			return fmt.Errorf("invalid template: %w", err)
		}
		// Execute the template over the JSON view of data, so templates address
		// the same shape agents see under -o json (and json.Marshaler-only
		// payloads like get's table are reachable at all) — CW-4. Decode with
		// UseNumber so a large int64 (byte counts, unix-nano timestamps) prints
		// verbatim as json.Number instead of round-tripping through float64 and
		// rendering as scientific notation.
		b, err := json.Marshal(env.Data)
		if err != nil {
			return err
		}
		var view any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&view); err != nil {
			return err
		}
		scrubManagedFields(view)
		if err := tmpl.Execute(w, view); err != nil {
			return err
		}
		_, err = fmt.Fprintln(w)
		return err
	}
}

// EncodeJSON writes v to w as indented JSON with HTML escaping disabled. This
// JSON is a CLI/agent contract read raw, not embedded in HTML, so `<`, `>`, `&`
// stay literal instead of becoming < / > / &.
func EncodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func renderTable(w io.Writer, env *Envelope) error {
	t, ok := env.Data.(Tabular)
	if !ok {
		// Fallback: pretty JSON so table mode never loses data.
		return EncodeJSON(w, env.Data)
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

// renderName emits one identifier per line (the NAME column) for `-o name`.
func renderName(w io.Writer, env *Envelope) error {
	t, ok := env.Data.(Tabular)
	if !ok {
		return nil
	}
	nameIdx := -1
	for i, c := range t.Columns() {
		if c == "NAME" {
			nameIdx = i
			break
		}
	}
	if nameIdx < 0 {
		return nil
	}
	for _, row := range t.Rows() {
		if nameIdx < len(row) {
			_, _ = fmt.Fprintln(w, row[nameIdx])
		}
	}
	return nil
}
