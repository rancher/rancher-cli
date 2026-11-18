package output

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cellTable is a WideTabular fixture whose columns and cells, narrow and
// wide, are set by the test.
type cellTable struct {
	cols, wideCols []string
	rows, wideRows [][]string
}

func (c cellTable) Columns() []string     { return c.cols }
func (c cellTable) Rows() [][]string      { return c.rows }
func (c cellTable) WideColumns() []string { return c.wideCols }
func (c cellTable) WideRows() [][]string  { return c.wideRows }

var _ WideTabular = cellTable{}

// renderCells renders data in format and returns the text written.
func renderCells(t *testing.T, format string, data Tabular) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, format, NewEnvelope("ProjectList", data)))
	return buf.String()
}

// oneCellTable puts cell in the last column of a table whose other cells are
// fixed; the expected text of the cell needs no padding. The table has the
// columns NAME and URL, and the wide table also DETAIL.
func oneCellTable(cell string) cellTable {
	return cellTable{
		cols:     []string{"NAME", "URL"},
		rows:     [][]string{{"a", cell}},
		wideCols: []string{"NAME", "URL", "DETAIL"},
		wideRows: [][]string{{"a", "u", cell}},
	}
}

// hiddenCharacterCases are values with a character a terminal cannot show as
// text, or an invalid byte, and the text each prints as in a table cell and
// as a name.
var hiddenCharacterCases = []struct{ name, in, want string }{
	{"escape sequence", "p-1\x1b[31m", `"p-1\x1b[31m"`},
	{"newline and clear screen", "line1\nFAKE ROW\x1b[2J", `"line1\nFAKE ROW\x1b[2J"`},
	{"tab", "a\tb", `"a\tb"`},
	{"carriage return and newline", "a\r\nb", `"a\r\nb"`},
	{"line separator", "a\u2028b", `"a\u2028b"`},
	{"bidi override", "a\u202eb", `"a\u202eb"`},
	{"byte order mark", "a\ufeffb", `"a\ufeffb"`},
	{"invalid byte", "a\xffb", `"a\xffb"`},
	{"zero-width space after a single quote", "o'b\u200b", `o'"b\u200b"`},
}

// TestRenderTextFormatsWriteOrdinaryValuesAsTheyAre asserts a cell that has
// no hidden character, and is not itself the escaped form of one, prints with
// the same bytes in table and wide output, and as a name in name output.
func TestRenderTextFormatsWriteOrdinaryValuesAsTheyAre(t *testing.T) {
	t.Parallel()
	names := []string{"web-1", `a "quoted" 'word' \path`, "caf\u00e9", `"ops"`, `"$PASSWORD"`, `'"a"'`}
	rows := make([][]string, len(names))
	for i, n := range names {
		rows[i] = []string{n, "u"}
	}
	want := strings.Join([]string{
		`NAME                      URL`,
		`web-1                     u`,
		`a "quoted" 'word' \path   u`,
		"caf\u00e9                      u",
		`"ops"                     u`,
		`"$PASSWORD"               u`,
		`'"a"'                     u`,
	}, "\n") + "\n"
	data := cellTable{cols: []string{"NAME", "URL"}, rows: rows, wideCols: []string{"NAME", "URL"}, wideRows: rows}

	assert.Equal(t, want, renderCells(t, "table", data))
	assert.Equal(t, want, renderCells(t, "wide", data))
	assert.Equal(t, strings.Join(names, "\n")+"\n", renderCells(t, "name", data))
}

// TestRenderTextFormatsWriteHeaderCellsAsEscapes asserts a column header with
// a hidden character prints as the escaped form of the whole header, and the
// header stays one line.
func TestRenderTextFormatsWriteHeaderCellsAsEscapes(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, header, want string }{
		{"zero-width space", "NAME\u200b", `"NAME\u200b"   URL` + "\n" + `a              u` + "\n"},
		{"newline", "NA\nME", `"NA\nME"   URL` + "\n" + `a          u` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cols := []string{tt.header, "URL"}
			rows := [][]string{{"a", "u"}}
			data := cellTable{cols: cols, rows: rows, wideCols: cols, wideRows: rows}
			for _, format := range []string{"table", "wide"} {
				assert.Equal(t, tt.want, renderCells(t, format, data), format)
			}
		})
	}
}

// TestRenderTextFormatsWriteHiddenCharactersAsEscapes asserts a table cell, a
// wide cell and a name with a control character, an invisible format
// character, a line or paragraph separator or an invalid byte prints as the
// whole value in double quotes with each such character as an escape. The
// table keeps its header line and one line per row, and the output has none
// of the raw characters.
func TestRenderTextFormatsWriteHiddenCharactersAsEscapes(t *testing.T) {
	t.Parallel()
	for _, tt := range hiddenCharacterCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			table := renderCells(t, "table", oneCellTable(tt.in))
			assert.Equal(t, "NAME   URL\na      "+tt.want+"\n", table)
			wide := renderCells(t, "wide", oneCellTable(tt.in))
			assert.Equal(t, "NAME   URL   DETAIL\na      u     "+tt.want+"\n", wide)
			name := renderCells(t, "name", namedRows{rows: [][]string{{tt.in, "u"}}})
			assert.Equal(t, tt.want+"\n", name)

			for _, out := range []string{table, wide, name} {
				assert.True(t, utf8.ValidString(out), "the output has an invalid byte: %q", out)
				assert.NotContains(t, out, "\x1b")
				assert.NotContains(t, out, "\r")
				assert.NotContains(t, out, "\t")
				assert.NotContains(t, out, "\u2028")
				assert.NotContains(t, out, "\u202e")
				assert.NotContains(t, out, "\ufeff")
				assert.NotContains(t, out, "\u200b")
			}
			assert.Len(t, strings.Split(strings.TrimRight(table, "\n"), "\n"), 2, "header and one row: %q", table)
			assert.Len(t, strings.Split(strings.TrimRight(wide, "\n"), "\n"), 2, "header and one row: %q", wide)
			assert.Len(t, strings.Split(strings.TrimRight(name, "\n"), "\n"), 1, "one name: %q", name)
		})
	}
}

// TestRenderTextFormatsWriteAnEscapedFormInDoubleQuotesAgain asserts a value
// that is exactly the escaped form of a text with a hidden character prints
// differently from a value that has the hidden character, in table and name
// output.
func TestRenderTextFormatsWriteAnEscapedFormInDoubleQuotesAgain(t *testing.T) {
	t.Parallel()
	const quoted = "\"a\\u200bb\"" // the 10 characters quote, a, backslash, u200b, b, quote
	const hidden = "a\u200bb"      // a, U+200B, b
	rows := [][]string{{"a", quoted}, {"b", hidden}}
	wantTable := "NAME   URL\n" +
		`a      "\"a\\u200bb\""` + "\n" +
		`b      "a\u200bb"` + "\n"
	data := cellTable{cols: []string{"NAME", "URL"}, rows: rows, wideCols: []string{"NAME", "URL"}, wideRows: rows}
	table := renderCells(t, "table", data)
	assert.Equal(t, wantTable, table)
	assert.Equal(t, wantTable, renderCells(t, "wide", data))

	names := renderCells(t, "name", namedRows{rows: [][]string{{quoted, "u"}, {hidden, "u"}}})
	assert.Equal(t, `"\"a\\u200bb\""`+"\n"+`"a\u200bb"`+"\n", names)
	lines := strings.Split(strings.TrimRight(names, "\n"), "\n")
	require.Len(t, lines, 2)
	assert.NotEqual(t, lines[0], lines[1])
}

// TestRenderTextFormatsWriteRemedyCommands asserts a command in a cell or
// name prints as the plain command, a hidden character in a clause beside it
// quotes that clause, and a remedy marker that is not followed by rancher,
// from this process or another, prints as one quoted string. No marker rune
// is left in the output.
func TestRenderTextFormatsWriteRemedyCommands(t *testing.T) {
	t.Parallel()
	foreign := "\u2063" + strings.Repeat("\u2060\u2061\u2062\u2064", 8)
	tests := []struct{ name, in, want string }{
		{"command", clierr.Rancher + " get pods", `rancher get pods`},
		{"command after a clause with a hidden character", "x\u200b; " + clierr.Rancher + " get pods", `"x\u200b"; rancher get pods`},
		{"marker of this process without rancher", clierr.RemedyMarker + "get pods", strconv.Quote(clierr.RemedyMarker + "get pods")},
		{"marker of another process before rancher", foreign + "rancher x", strconv.Quote(foreign + "rancher x")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			table := renderCells(t, "table", oneCellTable(tt.in))
			assert.Equal(t, "NAME   URL\na      "+tt.want+"\n", table)
			wide := renderCells(t, "wide", oneCellTable(tt.in))
			assert.Equal(t, "NAME   URL   DETAIL\na      u     "+tt.want+"\n", wide)
			name := renderCells(t, "name", namedRows{rows: [][]string{{tt.in, "u"}}})
			assert.Equal(t, tt.want+"\n", name)
			for _, out := range []string{table, wide, name} {
				for _, r := range "\u2060\u2061\u2062\u2063\u2064" {
					assert.NotContains(t, out, string(r), "the output has U+%04X", r)
				}
			}
		})
	}
}

// TestRenderTableAlignsAQuotedCellByItsWidth asserts a column holding one
// quoted cell and one ordinary cell is as wide as the quoted cell plus the
// padding, and the next column starts at the same rune offset in every row.
func TestRenderTableAlignsAQuotedCellByItsWidth(t *testing.T) {
	t.Parallel()
	rows := [][]string{{"web-1", "u"}, {"a\u200bb", "v"}}
	data := cellTable{cols: []string{"NAME", "URL"}, rows: rows, wideCols: []string{"NAME", "URL"}, wideRows: rows}
	want := "NAME         URL\n" +
		"web-1        u\n" +
		`"a\u200bb"   v` + "\n"
	for _, format := range []string{"table", "wide"} {
		out := renderCells(t, format, data)
		assert.Equal(t, want, out, format)
		for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
			fields := strings.Fields(line)
			second := fields[len(fields)-1]
			assert.Equal(t, 13, utf8.RuneCountInString(line[:strings.LastIndex(line, second)]), "the second column of %q", line)
		}
	}
}

// TestRenderStyledTableBoldsOnlyTheHeaderWhenACellHasAnEscapeSequence asserts
// the bold code is on the header line only of a table with a cell that has an
// escape sequence. The cell is written as text in double quotes, with \x1b in
// place of the escape character, and no row line has an escape character.
func TestRenderStyledTableBoldsOnlyTheHeaderWhenACellHasAnEscapeSequence(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("ProjectList", namedRows{rows: [][]string{{"p-1\x1b[31m", "line1\nFAKE ROW\x1b[2J"}}})
	for _, format := range []string{"table", "wide"} {
		var buf bytes.Buffer
		require.NoError(t, RenderStyled(&buf, format, env, style.Style{Enabled: true}))
		lines := strings.SplitN(buf.String(), "\n", 2)
		require.Len(t, lines, 2, format)
		assert.Contains(t, lines[0], "\x1b[1m", "the header line has the bold code")
		assert.NotContains(t, lines[1], "\x1b", "a row line has no escape character")
		assert.Equal(t, `"p-1\x1b[31m"   "line1\nFAKE ROW\x1b[2J"`+"\n", lines[1])
	}
}
