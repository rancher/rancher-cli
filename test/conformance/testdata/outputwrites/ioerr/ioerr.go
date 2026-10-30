package ioerr

import (
	"fmt"
	"io"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// Other has a field named Err that is not the field of runtime.IO.
type Other struct{ Err io.Writer }

type sink struct{}

func (sink) write(w io.Writer) {}

type holder struct{ W io.Writer }

func consume(w io.Writer) {}

func consumeWrite(f func([]byte) (int, error)) {}

func run(f func()) {}

func take(h *holder) {}

func passedToFunction(streams runtime.IO) { consume(streams.Err) }

func passedToFmt(streams runtime.IO) { _, _ = fmt.Fprint(streams.Err, "x") }

func passedToMethod(streams runtime.IO) { sink{}.write(streams.Err) }

func passedToClosure(streams runtime.IO) {
	f := func(w io.Writer) {}
	f(streams.Err)
}

func receiver(streams runtime.IO) { _, _ = streams.Err.Write(nil) }

func literalKey(w io.Writer) runtime.IO { return runtime.IO{Err: w} }

func assigned(streams runtime.IO) {
	w := streams.Err
	_ = w
}

func returned(streams runtime.IO) io.Writer { return streams.Err }

func methodValue(streams runtime.IO) {
	f := streams.Err.Write
	_ = f
}

func inClosure(streams runtime.IO) func() {
	return func() { _, _ = streams.Err.Write(nil) }
}

func compared(streams runtime.IO) bool { return streams.Err == nil }

func inFuncLitArgument(streams runtime.IO) {
	run(func() {
		w := streams.Err
		_ = w
	})
}

func inCompositeArgument(streams runtime.IO) { take(&holder{W: streams.Err}) }

func converted(streams runtime.IO) io.Writer { return io.Writer(streams.Err) }

func appended(streams runtime.IO, ws []io.Writer) []io.Writer { return append(ws, streams.Err) }

func methodValueArgument(streams runtime.IO) { consumeWrite(streams.Err.Write) }

func otherType(o Other) { _, _ = o.Err.Write(nil) }

func otherLiteral(w io.Writer) Other { return Other{Err: w} }
