package access

import (
	"github.com/rancher/rancher-cli/internal/engine/access/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// LiveSource hands the surface the question source: the resource
// positional, --group and --subresource complete from the typed
// cluster's discovery; every other site of the command is declined, --as
// among them, which the shared subject source serves.
func (*canCmd) LiveSource() completion.Source { return complete.QuestionSource() }

// LiveSource hands the surface the question source.
func (*whoCanCmd) LiveSource() completion.Source { return complete.QuestionSource() }

// LiveSource hands the surface the question source.
func (*explainCmd) LiveSource() completion.Source { return complete.QuestionSource() }
