package observe

import (
	"github.com/rancher/rancher-cli/internal/engine/observe/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// LiveSource hands the surface the pod source: --pod completes the pods
// of the target typed as the first positional, on the cluster the
// target's row names; every other site of logs is declined.
func (*logsCmd) LiveSource() completion.Source { return complete.PodSource() }
