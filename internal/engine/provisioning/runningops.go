package provisioning

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
)

// runningOperationError is the refusal of a command that would write while
// Rancher runs another operation on the cluster: GENERIC and retryable, since
// the operation ends by itself. An operation whose command exists in this
// build gets a hint that waits for it; any other gets none: a hint names only
// a command that is registered.
func runningOperationError(clusterName, namespace string, op clusterops.Operation) *clierr.Error {
	head := fmt.Sprintf("cluster %s has %s %s in progress", clusterName, operationArticle(op.Kind), op.Kind)
	if state := operationState(op); state != "" {
		head += " (" + state + ")"
	}
	head += "; Rancher runs one operation at a time; "
	switch op.Kind {
	case clusterops.OperationUpgrade:
		version, _ := op.Request["kubernetesVersion"].(string)
		cmd := clierr.NewCommand("cluster upgrade").Arg(clusterName).FlagValue("--k8s-version", version)
		if namespace != clusterNamespace {
			cmd = cmd.FlagValue("--namespace", namespace)
		}
		return clierr.New(clierr.CodeGeneric, head+"to wait for it, run the command in the hint with --apply --yes and without --dry-run; while the operation runs, that command changes nothing and waits").
			WithRetryable(true).WithHintCommand(cmd.Flag("--dry-run"))
	}
	return clierr.New(clierr.CodeGeneric, head+"try again after it ends").WithRetryable(true)
}

// operationArticle is "an" before a word that starts with a vowel sound and "a"
// before the others: the kinds that start with "etcd" and "encryption" take
// "an".
func operationArticle(kind clusterops.OperationKind) string {
	switch kind {
	case clusterops.OperationSnapshot, clusterops.OperationRestore, clusterops.OperationEncryptionKeyRotation:
		return "an"
	}
	return "a"
}

// operationState is the part of the refusal in parentheses: the phase
// Rancher reports for an operation that has one, then the Ready message of the
// control plane when it has one. An operation with no phase and no message has
// no state.
func operationState(op clusterops.Operation) string {
	var state string
	switch op.Kind {
	case clusterops.OperationSnapshot, clusterops.OperationRestore, clusterops.OperationEncryptionKeyRotation:
		phase := op.Phase
		if phase == "" {
			phase = "requested, not started"
		}
		state = "phase " + phase
		if op.ReadyMessage != "" {
			state += ": " + op.ReadyMessage
		}
	default:
		state = op.ReadyMessage
	}
	return state
}
