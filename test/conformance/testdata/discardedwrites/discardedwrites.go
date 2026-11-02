package discardedwrites

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"text/tabwriter"

	ucli "github.com/urfave/cli/v3"
)

func renderThing(w io.Writer) error {
	_, err := fmt.Fprintln(w, "thing")
	return err
}

func RenderOther() error { return nil }

func renderNoError() {}

type sink struct{}

func (*sink) Write(p []byte) (int, error)       { return len(p), nil }
func (*sink) WriteString(s string) (int, error) { return len(s), nil }

type renderer struct{}

func (renderer) Render(w io.Writer) error { return renderThing(w) }

func blankAssignedRender(w io.Writer) { _ = renderThing(w) }

func blankPairFprintln(w io.Writer) { _, _ = fmt.Fprintln(w, "x") }

func blankErrorOnly(w io.Writer) {
	n, _ := fmt.Fprint(w, "x")
	_ = n
}

func bareFprintln(w io.Writer) { fmt.Fprintln(w, "x") }

func bareFprintf(w io.Writer) { fmt.Fprintf(w, "%d", 1) }

func bareEncode(w io.Writer) {
	enc := json.NewEncoder(w)
	enc.Encode(1)
}

func deferredFlush(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 8, 1, ' ', 0)
	defer tw.Flush()
}

func bareRender(w io.Writer) { renderThing(w) }

func bareRenderMethod(w io.Writer) { renderer{}.Render(w) }

func bareExportedRender() { RenderOther() }

func goWrite(w io.Writer) { go fmt.Fprintln(w, "x") }

func bareWriteString(w io.Writer) { io.WriteString(w, "x") }

func bareCopy(w io.Writer, r io.Reader) { io.Copy(w, r) }

func barePrint() { fmt.Print("x") }

func barePrintf() { fmt.Printf("x") }

func barePrintln() { fmt.Println("x") }

func bareCopyN(w io.Writer, r io.Reader) { io.CopyN(w, r, 1) }

func bareCopyBuffer(w io.Writer, r io.Reader) { io.CopyBuffer(w, r, nil) }

func blankRootHelp(c *ucli.Command) { _ = ucli.ShowRootCommandHelp(c) }

func bareSubcommandHelp(c *ucli.Command) { ucli.ShowSubcommandHelp(c) }

func bareInterfaceWrite(w io.Writer) { w.Write(nil) }

func bareWriterMethods(s *sink) {
	s.Write(nil)
	s.WriteString("x")
}

func bareValueWriter() {
	var s sink
	s.Write(nil)
}

func closureDrop(w io.Writer) {
	f := func() { fmt.Fprintln(w, "x") }
	f()
}

var packageLevelDrop = func(w io.Writer) { fmt.Fprintln(w, "x") }

func skippedBuilders() {
	var b strings.Builder
	b.WriteString("x")
	fmt.Fprintln(&b, "x")
	pb := &strings.Builder{}
	pb.WriteString("x")
	fmt.Fprint(pb, "x")
	var buf bytes.Buffer
	buf.WriteString("x")
	buf.Write(nil)
	fmt.Fprintln(&buf, "x")
	pbuf := &bytes.Buffer{}
	pbuf.WriteByte('x')
	fmt.Fprintf(pbuf, "x")
}

func skippedHashes() {
	h := sha256.New()
	h.Write(nil)
	fmt.Fprint(h, "x")
	h64 := fnv.New64a()
	h64.Write(nil)
	io.WriteString(h64, "x")
}

func resultsUsed(w io.Writer) error {
	if err := renderThing(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "x"); err != nil {
		return err
	}
	n, err := w.Write(nil)
	_ = n
	renderNoError()
	if _, err := fmt.Println("x"); err != nil {
		return err
	}
	return err
}

func resultsUsedHelp(c *ucli.Command) error { return ucli.ShowRootCommandHelp(c) }

func resultReturned(w io.Writer) error { return renderThing(w) }

func resultReturnedFromWrite(w io.Writer) (int, error) { return w.Write(nil) }
