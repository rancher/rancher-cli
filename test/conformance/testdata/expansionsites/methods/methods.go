package methods

import (
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type Holder struct{ runtime.IO }

func call(io runtime.IO, s string) string { return io.ExpandText(s) }

func lines(io runtime.IO, s string) string { return io.ExpandLines(s) }

func value(io runtime.IO) func(*clierr.Error) *clierr.Error { return io.ExpandError }

func embedded(h Holder, s string) string { return h.ExpandText(s) }
