// An exported constructor that takes a Source: an owner could hand its own
// functions in, and the closure declared here would run them at TAB time.
package complete

import "github.com/rancher/rancher-cli/internal/runtime/completion"

func Wrap(src completion.Source) completion.Source {
	return completion.Source{
		Claim:    func(req completion.Request) (completion.Requirements, bool) { return src.Claim(req) },
		Complete: src.Complete,
	}
}
