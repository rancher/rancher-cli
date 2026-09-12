package provisioning

import (
	"github.com/rancher/rancher-cli/internal/engine/provisioning/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// LiveSource hands the surface create cluster's own source: the live
// half of --machine-config (the typed kind's configs behind its `<kind>/`
// prefix) and --cloud-credential; every other site of the command is
// declined.
func (createClusterCmd) LiveSource() completion.Source { return complete.CreateClusterSource() }

// LiveSource hands the surface the pool source: --pool completes the
// machine pools of the cluster typed as the first positional; every
// other site is declined. The three verbs that take a pool share it;
// pause-autoscaling and resume-autoscaling act on the whole cluster and
// declare no --pool.
func (scaleCmd) LiveSource() completion.Source { return complete.PoolSource() }

// LiveSource hands the surface the pool source.
func (setAutoscalingCmd) LiveSource() completion.Source { return complete.PoolSource() }

// LiveSource hands the surface the pool source.
func (clearAutoscalingCmd) LiveSource() completion.Source { return complete.PoolSource() }
