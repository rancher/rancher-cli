package ioout

import (
	"io"

	"github.com/rancher/rancher-cli/internal/runtime"
)

type Holder struct{ runtime.IO }

// Other has a field named Out that is not the field of runtime.IO.
type Other struct{ Out io.Writer }

var packageLevel = runtime.IO{Out: nil}

func selector(streams runtime.IO) { _, _ = streams.Out.Write(nil) }

func promoted(h Holder) { _, _ = h.Out.Write(nil) }

func literalKey(w io.Writer) runtime.IO { return runtime.IO{Out: w} }

func assigned(w io.Writer, streams *runtime.IO) { streams.Out = w }

func closure(streams runtime.IO) func() {
	return func() { _, _ = streams.Out.Write(nil) }
}

func otherType(o Other) { _, _ = o.Out.Write(nil) }

func otherLiteral(w io.Writer) Other { return Other{Out: w} }
