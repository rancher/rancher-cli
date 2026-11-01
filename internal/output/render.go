package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"text/template"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/output/style"
	"sigs.k8s.io/yaml"
)

// Tabular lets a payload describe its own table columns/rows for -o table.
type Tabular interface {
	Columns() []string
	Rows() [][]string
}

// WideTabular is implemented by payloads that carry extra wide-only columns.
// Render's "wide" case prefers it and falls back to Tabular, so a payload
// with no wide columns renders identically under both -o table and -o wide.
type WideTabular interface {
	Tabular
	WideColumns() []string
	WideRows() [][]string
}

// TemplatePrefix marks an --output value as Go template source: everything
// after it is the template. The single source of truth for the prefix - the
// pipeline's format validation and Render both key off it.
const TemplatePrefix = "template="

// renderSource picks what the structured encoders serialize: the original
// envelope on the fast path, the scrubbed tree otherwise.
func renderSource(env *Envelope) (any, error) {
	tree, hit, err := scrubbedTree(env)
	if err != nil {
		return nil, err
	}
	if !hit {
		return env, nil
	}
	return tree, nil
}

// Render writes env to w in the requested format, with no ANSI styling.
// format: "" or "table" (user-facing), "wide" (table plus any Wide-marked
// columns), "json", "yaml", "name" (ids), or "template=<go-template>".
// Anything else is an error; the pipeline validates the format before the
// command runs, so hitting it here is defense in depth.
func Render(w io.Writer, format string, env *Envelope) error {
	return RenderStyled(w, format, env, style.Style{})
}

// RenderStyled is RenderWith with no rewrite: under "table" or the ""
// default, s bolds the rendered header row - applied to the header LINE
// after tabwriter has already column-aligned it, never to a column's own
// bytes, which would inflate the byte-counted widths tabwriter measures.
// "wide" gets the same treatment; every other format ignores s and renders
// byte-identical to Render, regardless of s.Enabled - json, yaml, name, and
// template stay plain no matter what the caller passes.
func RenderStyled(w io.Writer, format string, env *Envelope, s style.Style) error {
	return RenderWith(w, format, env, s, nil)
}

// RenderWith is RenderStyled with a rewrite of the structured output: rewrite
// gets the whole encoded JSON document of the json format, and the JSON the
// yaml format is converted from, before it is written, and returns the
// document to write. A nil rewrite writes what RenderStyled writes. No other
// format is rewritten. The yaml format writes the characters the YAML library
// it converts through refuses (DEL, U+0080 to U+009F, U+FFFE and U+FFFF; it
// changes U+0085 into a space) as escapes after the rewrite. The json and yaml
// formats each make one Write.
func RenderWith(w io.Writer, format string, env *Envelope, s style.Style, rewrite func([]byte) []byte) error {
	switch format {
	case "", "table":
		return renderTable(w, env, s)
	case "wide":
		return renderWide(w, env, s)
	case "json":
		src, err := renderSource(env)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := encodeJSON(&buf, src); err != nil {
			return err
		}
		b := buf.Bytes()
		if rewrite != nil {
			b = rewrite(b)
		}
		_, err = w.Write(b)
		return err
	case "yaml":
		src, err := renderSource(env)
		if err != nil {
			return err
		}
		b, err := json.Marshal(src)
		if err != nil {
			return fmt.Errorf("error marshaling into JSON: %w", err)
		}
		if rewrite != nil {
			b = rewrite(b)
		}
		b = escapeRefusedRunes(b)
		y, err := yaml.JSONToYAML(b)
		if err != nil {
			return err
		}
		_, err = w.Write(y)
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
			return fmt.Errorf("the template is empty")
		}
		return renderTemplate(w, src, env)
	}
}

// escapeRefusedRunes returns the JSON text b with DEL, U+0080 to U+009F,
// U+FFFE and U+FFFF each written as a \uXXXX escape, and b itself when none
// occurs. The YAML library that parses the text refuses these characters (it
// changes U+0085 into a space). They occur only inside JSON strings; an escape
// at any position of the text is valid, the library then decodes each one
// exactly and its emitter writes it back as a YAML escape. Every other rune is
// copied as its original bytes, an invalid byte included.
func escapeRefusedRunes(b []byte) []byte {
	var out []byte
	copied := 0
	for i := 0; i < len(b); {
		switch b[i] {
		case 0x7f, 0xc2, 0xef:
		default:
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r != 0x7f && (r < 0x80 || r > 0x9f) && r != 0xfffe && r != 0xffff {
			i += size
			continue
		}
		out = append(out, b[copied:i]...)
		out = fmt.Appendf(out, `\u%04x`, r)
		i += size
		copied = i
	}
	if out == nil {
		return b
	}
	return append(out, b[copied:]...)
}

// renderTemplate executes a Go template over the JSON view of env.Data with
// managedFields scrubbed. Templates address the same shape agents see under
// -o json. UseNumber preserves large int64 values verbatim.
func renderTemplate(w io.Writer, src string, env *Envelope) error {
	tmpl, err := template.New("out").Parse(src)
	if err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}
	// Execute the template over the JSON view of data, so templates address
	// the same shape agents see under -o json (and json.Marshaler-only
	// payloads like get's table are reachable at all). Decode with
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
	// No detection fast path here: the branch must decode the view
	// regardless, and a detection marshal to skip an already-decoded
	// tree walk would cost more than the walk itself.
	scrubManagedFields(view)
	if err := tmpl.Execute(w, view); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

// encodeJSON writes v to w as indented JSON with HTML escaping disabled. This
// JSON is a CLI/agent contract read raw, not embedded in HTML, so `<`, `>`, `&`
// stay literal instead of becoming < / > / &.
func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// renderTable is -o table (and the "" default): the closed-world contract
// requires Data to implement Tabular - there is no pretty-JSON fallback. A
// non-Tabular Data value here is a command bug: the command's declared
// OutputShape promised Tabular and its Data does not honor that.
func renderTable(w io.Writer, env *Envelope, s style.Style) error {
	t, ok := env.Data.(Tabular)
	if !ok {
		return fmt.Errorf("internal error: this command's result cannot be shown as a table (result type %T)", env.Data)
	}
	return writeTable(w, t.Columns(), t.Rows(), s)
}

// renderWide is -o wide: it prefers WideTabular's superset columns/rows and
// falls back to plain Tabular (byte-identical to -o table) for every payload
// that carries no wide-only columns - presentation, not a scope change. Like
// renderTable, there is no pretty-JSON fallback for non-Tabular Data.
func renderWide(w io.Writer, env *Envelope, s style.Style) error {
	if wt, ok := env.Data.(WideTabular); ok {
		return writeTable(w, wt.WideColumns(), wt.WideRows(), s)
	}
	t, ok := env.Data.(Tabular)
	if !ok {
		return fmt.Errorf("internal error: this command's result cannot be shown as a wide table (result type %T)", env.Data)
	}
	return writeTable(w, t.Columns(), t.Rows(), s)
}

func writeTable(w io.Writer, cols []string, rows [][]string, s style.Style) error {
	if !s.Enabled {
		return writePlainTable(w, cols, rows)
	}
	// Bold the header LINE in the already-aligned output - bolding a
	// column's own bytes before tabwriter measures it would inflate that
	// column's width. The writer styles only the bytes up to the first
	// newline and streams every row through unchanged, so the table is
	// never buffered whole just to style one line.
	hw := &headerBoldWriter{w: w, s: s}
	if err := writePlainTable(hw, cols, rows); err != nil {
		return err
	}
	return hw.flush()
}

// headerBoldWriter bolds the bytes up to (not including) the first newline
// written through it and passes every later byte to the underlying writer
// unchanged. Bytes seen before the first newline are held until it arrives
// (the source may split the header line across Writes); flush releases them
// unstyled when no newline ever arrives.
type headerBoldWriter struct {
	w      io.Writer
	s      style.Style
	done   bool
	header []byte
}

func (h *headerBoldWriter) Write(p []byte) (int, error) {
	if h.done {
		return h.w.Write(p)
	}
	i := bytes.IndexByte(p, '\n')
	if i < 0 {
		h.header = append(h.header, p...)
		return len(p), nil
	}
	h.header = append(h.header, p[:i]...)
	h.done = true
	if _, err := fmt.Fprintf(h.w, "%s\n", h.s.Bold(string(h.header))); err != nil {
		return 0, err
	}
	h.header = nil
	n, err := h.w.Write(p[i+1:])
	return i + 1 + n, err
}

// flush releases header bytes still held when the stream carried no newline
// at all - a headerless fragment stays unstyled, byte-identical to the
// plain render.
func (h *headerBoldWriter) flush() error {
	if h.done || len(h.header) == 0 {
		return nil
	}
	h.done = true
	_, err := h.w.Write(h.header)
	h.header = nil
	return err
}

// writePlainTable is the one place table cells reach the writer, so it is
// where every header and cell passes the terminal-safety boundary
// (Printable): a server-sourced value with an embedded newline or escape
// sequence cannot forge a row or drive the terminal, whatever Tabular
// produced it.
func writePlainTable(w io.Writer, cols []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
	for i, c := range cols {
		if i > 0 {
			_, _ = fmt.Fprint(tw, "\t")
		}
		_, _ = fmt.Fprint(tw, Printable(c))
	}
	_, _ = fmt.Fprintln(tw)
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				_, _ = fmt.Fprint(tw, "\t")
			}
			_, _ = fmt.Fprint(tw, Printable(cell))
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
			_, _ = fmt.Fprintln(w, Printable(row[nameIdx]))
		}
	}
	return nil
}
