package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// nodeNameLabel is the label Rancher puts on a management node, with the name
// of its node.
const nodeNameLabel = "management.cattle.io/nodename"

const steveNodes = "management.cattle.io.nodes"

// target is the node a command acts on and the management node Rancher keeps
// for it.
type target struct {
	// node is the name of the downstream Node, as typed.
	node string
	// cluster is --cluster as typed; id is the cluster id it resolves to.
	cluster, id string
	// mid is the name of the management node.
	mid string
}

// recovery is the command that reads the management node.
func (t target) recovery() clierr.Command {
	return recoveryCommand(t.mid, t.cluster)
}

func recoveryCommand(mid, cluster string) clierr.Command {
	return clierr.NewCommand("get management-node").Arg(mid).FlagValue("--cluster", cluster)
}

func stopDrainCommand(node, cluster string) clierr.Command {
	return clierr.NewCommand("node stop-drain").Arg(node).FlagValue("--cluster", cluster).Flag("--dry-run")
}

// findManagementNode lists the management nodes of cluster id on the local
// cluster and returns the one whose status.nodeName is node. A node name that
// is a valid label value is looked up by the label first and, when the label
// finds none, among every management node of the namespace.
func findManagementNode(ctx context.Context, local engine.ClusterClient, row kinds.Kind, t target) (*unstructured.Unstructured, error) {
	selector := ""
	if len(validation.IsValidLabelValue(t.node)) == 0 {
		selector = nodeNameLabel + "=" + t.node
	}
	matches, err := listMatches(ctx, local, row, t, selector)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 && selector != "" {
		if matches, err = listMatches(ctx, local, row, t, ""); err != nil {
			return nil, err
		}
	}
	switch len(matches) {
	case 0:
		return nil, clierr.New(clierr.CodeNotFound, fmt.Sprintf("cluster %s has no node %s that Rancher manages", t.id, t.node)).
			WithHintCommand(clierr.NewCommand("get node").FlagValue("--cluster", t.cluster))
	case 1:
		return &matches[0], nil
	}
	candidates := make([]clierr.Candidate, 0, len(matches))
	for _, m := range matches {
		candidates = append(candidates, clierr.Candidate{ID: m.GetName(), Name: t.node})
	}
	return nil, clierr.New(clierr.CodeAmbiguousSelection, fmt.Sprintf("cluster %s has %d management nodes for node %s", t.id, len(matches), t.node)).WithCandidates(candidates)
}

// listMatches lists the management nodes of the cluster with selector and
// keeps those whose status.nodeName is the node, sorted by name.
func listMatches(ctx context.Context, local engine.ClusterClient, row kinds.Kind, t target, selector string) ([]unstructured.Unstructured, error) {
	list, err := local.List(ctx, row.GVR, t.id, selector)
	if err != nil {
		if resources.ReadRefused(err) {
			return nil, clierr.New(clierr.CodeRBAC, fmt.Sprintf("you cannot list the management nodes of cluster %s (refused with HTTP 403); this command needs access to the local cluster", t.id)).WithCause(err)
		}
		return nil, err
	}
	var matches []unstructured.Unstructured
	for _, item := range list.Items {
		if name, _, _ := unstructured.NestedString(item.Object, "status", "nodeName"); name == t.node {
			matches = append(matches, item)
		}
	}
	slices.SortFunc(matches, func(a, b unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	return matches, nil
}

// readSteveNode reads the management node through the Steve API, which
// serves its state, and checks it is the version that was listed.
func readSteveNode(ctx context.Context, get steve.Getter, t target, listed *unstructured.Unstructured) (map[string]any, error) {
	req := steve.Request{Type: steveNodes, Namespace: t.id, Name: t.mid}
	body, err := get(ctx, req)
	if err != nil {
		switch {
		case resources.ReadRefused(err):
			return nil, clierr.New(clierr.CodeRBAC, fmt.Sprintf("you cannot read management node %s of cluster %s (GET %s was refused with HTTP 403)", t.mid, t.id, req.Path())).WithCause(err)
		case clierr.IsNotFound(err):
			return nil, changedWhileRead(t.node)
		}
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("could not read management node %s of cluster %s: %v", t.mid, t.id, err)).WithRetryable(false)
	}
	if rv, _, _ := unstructured.NestedString(obj, "metadata", "resourceVersion"); rv != listed.GetResourceVersion() {
		return nil, changedWhileRead(t.node)
	}
	return obj, nil
}

func changedWhileRead(node string) *clierr.Error {
	return clierr.Retryable(clierr.CodeConflict, fmt.Sprintf("node %s changed while it was read; run the command again", node))
}

// refusal is the error for an action Rancher does not offer on a node. It is
// nil for an allowed action.
func refusal(c kinds.NodeDrainCase, s kinds.NodeDrainState, a kinds.NodeDrainAction, t target) *clierr.Error {
	word := "drain"
	if a == kinds.NodeStopDrain {
		word = "stop drain"
	}
	refuse := func(msg string) *clierr.Error { return clierr.New(clierr.CodeValidation, msg) }
	switch c {
	case kinds.NodeDrainNotWorker:
		msg := "Rancher offers " + word + " only for worker nodes"
		if roles := rolesText(s); roles != "" {
			msg += "; node " + t.node + " has the roles " + roles
		}
		return refuse(msg)
	case kinds.NodeDrainNoAction:
		state := s.StateAfterRules()
		msg := fmt.Sprintf("Rancher offers no action on node %s while it is %s", t.node, state)
		if s.StateMessage != "" {
			msg += ": " + s.StateMessage
		}
		if a == kinds.NodeDrain && state == "unavailable" {
			msg += "; to drain it anyway, cordon it first by setting spec.unschedulable to true on the node: Rancher then shows it cordoned and offers drain"
		}
		return refuse(msg)
	case kinds.NodeDrainAlreadyDraining:
		return refuse(fmt.Sprintf("node %s is already draining", t.node)).WithHintCommand(stopDrainCommand(t.node, t.cluster))
	case kinds.NodeDrainAlreadyDrained:
		return refuse(fmt.Sprintf("node %s is already drained", t.node))
	case kinds.NodeDrainNotDraining:
		return refuse(fmt.Sprintf("node %s is not draining", t.node))
	case kinds.NodeDrainNotDrainingDrained:
		return refuse(fmt.Sprintf("node %s is not draining; it is drained", t.node))
	case kinds.NodeDrainNotStarted:
		return refuse(fmt.Sprintf("a drain of node %s is requested and has not started; Rancher offers no stop until it starts", t.node))
	case kinds.NodeDrainConditionLeft:
		return clierr.Retryable(clierr.CodeConflict, fmt.Sprintf("Rancher has not yet removed the Drained condition of node %s; run the command again", t.node))
	}
	return nil
}

// rolesText names the roles of a node that is not a worker, joined with
// "and"; "" when it has none.
func rolesText(s kinds.NodeDrainState) string {
	var roles []string
	if s.Etcd {
		roles = append(roles, "etcd")
	}
	if s.ControlPlane {
		roles = append(roles, "controlPlane")
	}
	return strings.Join(roles, " and ")
}
