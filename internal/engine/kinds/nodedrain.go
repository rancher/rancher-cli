package kinds

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// NodeDrainState is what Rancher's node store and action formatter read to decide the drain actions of a node.
type NodeDrainState struct {
	Worker, Etcd, ControlPlane bool   // spec.worker, spec.etcd, spec.controlPlane
	State, StateMessage        string // metadata.state.name and .message as the Steve API serves them; "" State reads "unknown"
	Unschedulable              bool   // spec.internalNodeSpec.unschedulable
	Drained, DrainedMessage    string // status and message of the Drained condition; "" when absent
	Request                    string // spec.desiredNodeUnschedulable
}

// NodeDrainStateFrom reads a management node as the Steve API serves it: the whole
// object, with metadata.state.
func NodeDrainStateFrom(obj map[string]any) NodeDrainState {
	var s NodeDrainState
	s.Worker, _, _ = unstructured.NestedBool(obj, "spec", "worker")
	s.Etcd, _, _ = unstructured.NestedBool(obj, "spec", "etcd")
	s.ControlPlane, _, _ = unstructured.NestedBool(obj, "spec", "controlPlane")
	s.State, _, _ = unstructured.NestedString(obj, "metadata", "state", "name")
	s.StateMessage, _, _ = unstructured.NestedString(obj, "metadata", "state", "message")
	s.Unschedulable, _, _ = unstructured.NestedBool(obj, "spec", "internalNodeSpec", "unschedulable")
	s.Request, _, _ = unstructured.NestedString(obj, "spec", "desiredNodeUnschedulable")
	conditions, _, _ := unstructured.NestedFieldNoCopy(obj, "status", "conditions")
	list, _ := conditions.([]any)
	for _, c := range list {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := cond["type"].(string); typ != "Drained" {
			continue
		}
		s.Drained, _ = cond["status"].(string)
		s.DrainedMessage, _ = cond["message"].(string)
		break
	}
	return s
}

// NodeDrainAction is a drain action of a node.
type NodeDrainAction int

const (
	// NodeDrain is the drain action.
	NodeDrain NodeDrainAction = iota
	// NodeStopDrain is the stop drain action.
	NodeStopDrain
)

// NodeDrainCase is how Rancher's rules treat one action on one node.
type NodeDrainCase int

const (
	// NodeDrainAllowed means Rancher offers the action.
	NodeDrainAllowed NodeDrainCase = iota
	// NodeDrainNotWorker means the node has the etcd or controlPlane role and
	// no worker role. A node with none of the three roles is a worker.
	NodeDrainNotWorker
	// NodeDrainNoAction means the node state is none of active, cordoned,
	// draining or drained.
	NodeDrainNoAction
	// NodeDrainAlreadyDraining means a drain is running and the action is drain.
	NodeDrainAlreadyDraining
	// NodeDrainAlreadyDrained means the node is drained and the action is drain.
	NodeDrainAlreadyDrained
	// NodeDrainNotDraining means no drain is running and the action is stop
	// drain.
	NodeDrainNotDraining
	// NodeDrainNotDrainingDrained means the node is drained and the action
	// is stop drain.
	NodeDrainNotDrainingDrained
	// NodeDrainNotStarted means a drain is requested and Rancher has not
	// started it, and the action is stop drain.
	NodeDrainNotStarted
	// NodeDrainConditionLeft means the node is schedulable and its Drained
	// condition is still True, and the action is drain: Rancher removes the
	// condition on its next node sync.
	NodeDrainConditionLeft
)

// ignorableDrainMessages are the texts that make Rancher show a node whose
// drain failed as cordoned instead of draining.
var ignorableDrainMessages = []string{"--ignore-daemonsets", "--delete-emptydir-data", "--force", "did not complete within"}

// StateAfterRules returns the state of the node after the node store's rule,
// then the formatter's rule. An empty State reads "unknown" and no rule
// changes it.
func (s NodeDrainState) StateAfterRules() string {
	state := s.State
	if state == "" {
		return "unknown"
	}
	if state != "draining" && s.Unschedulable {
		if s.Drained == "True" {
			state = "drained"
		} else {
			state = "cordoned"
		}
	}
	if state == "draining" && s.Drained == "False" {
		for _, text := range ignorableDrainMessages {
			if strings.Contains(s.DrainedMessage, text) {
				return "cordoned"
			}
		}
	}
	return state
}

// NodeDrainRule decides whether Rancher offers action a on a node in state s,
// and the reason when it does not.
func NodeDrainRule(s NodeDrainState, a NodeDrainAction) NodeDrainCase {
	if !s.Worker && (s.Etcd || s.ControlPlane) {
		return NodeDrainNotWorker
	}
	switch state := s.StateAfterRules(); state {
	case "active":
		if a == NodeDrain {
			if s.Drained == "True" {
				return NodeDrainConditionLeft
			}
			return NodeDrainAllowed
		}
		if s.Request == "drain" {
			return NodeDrainNotStarted
		}
		return NodeDrainNotDraining
	case "cordoned":
		if a == NodeDrain {
			return NodeDrainAllowed
		}
		return NodeDrainNotDraining
	case "draining":
		if a == NodeDrain {
			return NodeDrainAlreadyDraining
		}
		return NodeDrainAllowed
	case "drained":
		if a == NodeDrain {
			return NodeDrainAlreadyDrained
		}
		return NodeDrainNotDrainingDrained
	default:
		return NodeDrainNoAction
	}
}
