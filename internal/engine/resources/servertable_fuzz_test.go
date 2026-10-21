package resources

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// FuzzTableRender fuzzes what the server can put in a Table: arbitrary
// column names, namespaces and cells (strings with control, format and
// separator runes and newlines, int64, float64, bool, nil) rendered through
// the payload and the table writer, as -o table and -o wide, must give
// exactly one header line plus one line per row, and no line may hold a
// rune that output.Printable drops (a control, a format character, U+2028,
// U+2029). The output is split on "\n" only: the line-ending newline is the
// separator, so a stray U+0085 or U+2028 cannot pass as one. Never call
// t.Parallel() here - it panics inside f.Fuzz.
func FuzzTableRender(f *testing.F) {
	f.Add("Name", "web-0", "ns-1", uint8(0), int64(1), 0.5, true, uint8(2))
	f.Add("Status", "line one\nline two", "ns\r\n1", uint8(1), int64(-7), 1e21, false, uint8(3))
	f.Add("Esc\x1b[31m", "\x1b]0;title\x07", "\x1b[2J", uint8(2), int64(0), math.Copysign(0, -1), true, uint8(1))
	f.Add("bidi\u202e", "\u200b  \u0085\u2028", "\u009b31m\u2029", uint8(3), int64(1)<<62, 3.0, false, uint8(4))
	f.Add("tab\there", "a\tb\vc\fd\re", "form\ffeed", uint8(4), int64(42), 0.0, true, uint8(0))
	f.Add("", "", "", uint8(5), int64(0), 0.0, false, uint8(2))
	f.Add("\xff\xfe", "\xc3\x28", "ns", uint8(0), int64(0), 0.0, true, uint8(2))
	files, err := filepath.Glob(filepath.Join("testdata", "tables", "*.json"))
	require.NoError(f, err)
	for _, file := range files {
		table := tableFixture(f, strings.TrimSuffix(filepath.Base(file), ".json"))
		cell := func(row, col int) string {
			if row < len(table.Rows) && col < len(table.Rows[row].Cells) {
				if s, ok := table.Rows[row].Cells[col].(string); ok {
					return s
				}
			}
			return ""
		}
		colName := table.ColumnDefinitions[0].Name
		ns := ""
		if len(table.Rows) > 0 {
			ns = table.Rows[0].Object.Object.(*metav1.PartialObjectMetadata).Namespace
		}
		f.Add(colName, cell(0, 0), ns, uint8(0), int64(len(table.Rows)), 0.5, true, uint8(len(table.Rows)))
	}

	f.Fuzz(func(t *testing.T, name, a, b string, sel uint8, n int64, x float64, flag bool, rowCount uint8) {
		values := []any{a, n, x, flag, nil, b}
		table := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: name}, {Name: a}, {Name: b, Priority: 1}, {Name: "Last"},
		}}
		rows := int(rowCount % 6)
		for r := range rows {
			cells := make([]any, 0, 4)
			for c := range 4 - int(sel%2) { // some rows are short
				cells = append(cells, values[(int(sel)+r+c)%len(values)])
			}
			table.Rows = append(table.Rows, metav1.TableRow{
				Cells:  cells,
				Object: runtime.RawExtension{Object: &metav1.PartialObjectMetadata{Name: a, Namespace: b}},
			})
		}
		st, err := newServerTable(table, flag)
		require.NoError(t, err)
		for _, format := range []string{"table", "wide"} {
			out := renderPayload(t, st, format)
			lines := strings.Split(out, "\n")
			require.Len(t, lines, rows+2, "one header line plus one line per row, then the empty tail after the last newline: %q", out)
			require.Equal(t, "", lines[len(lines)-1], "the output ends with a newline: %q", out)
			for _, line := range lines {
				require.Equal(t, output.Printable(line), line, "a line holds a rune the terminal-safety boundary drops: %q", line)
			}
		}
	})
}
