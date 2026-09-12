// An exported function taking a function and an exported method taking an
// interface: both let an owner's code in through a value.
package complete

import "github.com/rancher/rancher-cli/internal/runtime/completion"

type Lister struct{}

func WithNames(names func() []string) completion.Source { return completion.Source{} }

func (Lister) Using(l interface{ List() []string }) completion.Source { return completion.Source{} }
