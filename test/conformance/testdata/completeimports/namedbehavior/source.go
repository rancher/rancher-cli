// Behavior hidden behind names: standard-library interfaces and function
// types, an alias of the seam's Source, and containers of functions. A
// syntactic scan sees only identifiers; the type-aware scan sees what
// they are.
package complete

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

type Src = completion.Source

type Cancel struct {
	Stop context.CancelFunc
}

type Filters []func(string) bool

var ExternalHook context.CancelFunc

var names = []string{"a"}

type Lookup map[string]fmt.Stringer

func WithContext(ctx context.Context) completion.Source { return completion.Source{} }

func WithError(err error) completion.Source { return completion.Source{} }

func WithStringer(s fmt.Stringer) completion.Source { return completion.Source{} }

func WithAlias(src Src) completion.Source { return completion.Source{} }

func WithPointer(c *Cancel) completion.Source { return completion.Source{} }

func plain(more []string) completion.Result { return completion.Result{Names: append(names, more...)} }

var _ = plain
