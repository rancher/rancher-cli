package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

const managementNodesResource = "management.cattle.io.nodes"

// steveManagementNodes is a Steve API collection of management nodes: each item
// is the whole object with its metadata.state; only the fields the drain rules
// and the id source read are kept.
type steveManagementNodes struct {
	Data []steveManagementNode `json:"data"`
}

type steveManagementNode struct {
	Metadata struct {
		Name  string `json:"name"`
		State struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"state"`
	} `json:"metadata"`
	Spec struct {
		Worker                   bool   `json:"worker"`
		Etcd                     bool   `json:"etcd"`
		ControlPlane             bool   `json:"controlPlane"`
		DesiredNodeUnschedulable string `json:"desiredNodeUnschedulable"`
		InternalNodeSpec         struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"internalNodeSpec"`
	} `json:"spec"`
	Status struct {
		NodeName   string `json:"nodeName"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// object is the management node as the object the drain rules read, so that
// one decoder, kinds.NodeDrainStateFrom, serves every caller.
func (n steveManagementNode) object() map[string]any {
	conditions := make([]any, 0, len(n.Status.Conditions))
	for _, c := range n.Status.Conditions {
		conditions = append(conditions, map[string]any{"type": c.Type, "status": c.Status, "message": c.Message})
	}
	return map[string]any{
		"metadata": map[string]any{"name": n.Metadata.Name, "state": map[string]any{"name": n.Metadata.State.Name, "message": n.Metadata.State.Message}},
		"spec": map[string]any{
			"worker": n.Spec.Worker, "etcd": n.Spec.Etcd, "controlPlane": n.Spec.ControlPlane,
			"desiredNodeUnschedulable": n.Spec.DesiredNodeUnschedulable,
			"internalNodeSpec":         map[string]any{"unschedulable": n.Spec.InternalNodeSpec.Unschedulable},
		},
		"status": map[string]any{"nodeName": n.Status.NodeName, "conditions": conditions},
	}
}

// readManagementNodes reads the management nodes of the typed cluster in one
// Steve API request; false when the read or its path fails.
func readManagementNodes(ctx context.Context, conn *completion.Connection, req completion.Request) ([]steveManagementNode, bool) {
	p, err := completion.SteveNamespacedCollectionPath(managementNodesResource, req.ClusterID)
	if err != nil {
		return nil, false
	}
	var list steveManagementNodes
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return nil, false
	}
	return list.Data, true
}

// DrainNodeSource is the source of the node positional of node drain and
// node stop-drain: the names of the typed cluster's nodes for which the
// command is allowed, as one Steve API list of the cluster's management nodes
// gives them, each name once and no name that two management nodes share.
// It claims no other site, so the shared --node flag keeps its own source,
// and it needs the typed cluster resolved.
func DrainNodeSource() completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Flag != "" || req.Index != 0 {
				return completion.Requirements{}, false
			}
			if _, ok := drainAction(req.Command); !ok {
				return completion.Requirements{}, false
			}
			return completion.ClusterListing(), true
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			action, ok := drainAction(req.Command)
			if !ok {
				return completion.Result{}
			}
			items, ok := readManagementNodes(ctx, conn, req)
			if !ok {
				return completion.Result{}
			}
			// Two management nodes of one node name make the command refuse the
			// name as ambiguous, so a name two nodes share is not offered.
			count := map[string]int{}
			for _, item := range items {
				count[item.Status.NodeName]++
			}
			var names []string
			for _, item := range items {
				if count[item.Status.NodeName] > 1 {
					continue
				}
				state := kinds.NodeDrainStateFrom(item.object())
				if kinds.NodeDrainRule(state, action) != kinds.NodeDrainAllowed {
					continue
				}
				if name := item.Status.NodeName; name != "" {
					names = append(names, name)
				}
			}
			return completion.Result{Names: names}
		},
	}
}

// ManagementNodeIDSource is the source of the management-node kind's name
// positional: the ids of the typed cluster's management nodes, each described
// by the name of its node. It needs the typed cluster resolved.
func ManagementNodeIDSource() completion.Source {
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if req.Flag != "" || req.Index != 0 {
				return completion.Requirements{}, false
			}
			return completion.ClusterListing(), true
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			items, ok := readManagementNodes(ctx, conn, req)
			if !ok {
				return completion.Result{}
			}
			var names []string
			descriptions := map[string]string{}
			for _, item := range items {
				id := item.Metadata.Name
				if id == "" {
					continue
				}
				names = append(names, id)
				if node := item.Status.NodeName; node != "" {
					descriptions[id] = node
				}
			}
			return completion.Result{Names: names, Descriptions: descriptions}
		},
	}
}

// drainAction maps a command name to the drain action it takes.
func drainAction(command string) (kinds.NodeDrainAction, bool) {
	switch command {
	case "node drain":
		return kinds.NodeDrain, true
	case "node stop-drain":
		return kinds.NodeStopDrain, true
	}
	return 0, false
}
