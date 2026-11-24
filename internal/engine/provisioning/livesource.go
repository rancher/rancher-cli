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

// LiveSource hands the surface cluster upgrade's own source: the <cluster>
// positional and --k8s-version after it, each completing only values the
// command accepts; every other site is declined.
func (upgradeCmd) LiveSource() completion.Source { return complete.UpgradeSource() }

// LiveSource hands the surface cluster set-upgrade-strategy's own source: the
// <cluster> positional and the grace period and drain timeout flags after it,
// each completing only values the command accepts; every other site is
// declined.
func (upgradeStrategyCmd) LiveSource() completion.Source { return complete.UpgradeStrategySource() }

// LiveSource returns the source that completes the <cluster> argument of the
// command; every other site is declined.
func (saveEtcdSnapshotCmd) LiveSource() completion.Source { return complete.SaveEtcdSnapshotSource() }

// LiveSource completes the <driver> argument and the field names after --set and
// --from-file; every other site is declined.
func (createCloudCredentialCmd) LiveSource() completion.Source {
	return complete.CreateCloudCredentialSource()
}

// LiveSource completes the <credential> argument; every other site is declined.
func (*getCloudCredCmd) LiveSource() completion.Source { return complete.GetCloudCredentialSource() }

// LiveSource completes the <credential> argument and the field names after --set
// and --from-file; every other site is declined.
func (patchCloudCredentialCmd) LiveSource() completion.Source {
	return complete.PatchCloudCredentialSource()
}

// LiveSource completes the <credential> argument; every other site is declined.
func (deleteCloudCredentialCmd) LiveSource() completion.Source {
	return complete.DeleteCloudCredentialSource()
}
