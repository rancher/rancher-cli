package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
)

// normanPath is what a command that asks Rancher through its node action
// holds after Plan: the port, and whether Rancher listed the actions of the
// node when Plan read it.
type normanPath struct {
	client engine.NodeActionClient
	// listedActions is true when Plan read an actions map with a key in it.
	listedActions bool
	// noActions is true when Rancher listed no actions and the command checked
	// the state of the node with its own rules.
	noActions bool
}

// nodeAction is the Rancher action of a drain action.
func nodeAction(a kinds.NodeDrainAction) engine.NodeAction {
	if a == kinds.NodeStopDrain {
		return engine.NodeActionStopDrain
	}
	return engine.NodeActionDrain
}

// offerPhrase names a drain action on a node in the message of a refusal.
func offerPhrase(a kinds.NodeDrainAction, node string) string {
	if a == kinds.NodeStopDrain {
		return "stop the drain of node " + node
	}
	return "drain node " + node
}

// findNorman finds the management node for a caller the local cluster
// refused: it reads the management nodes through the Steve API, asks the local
// cluster whether the caller may update the node, reads the node through
// Rancher's Norman API and refuses an action Rancher does not offer.
func (p planner) findNorman(ctx context.Context, t target, row kinds.Kind, local engine.ClusterClient, action kinds.NodeDrainAction) (found, error) {
	get := steve.RootFactoryGetter(p.roots)
	item, err := steveManagementNode(ctx, get, t)
	if err != nil {
		return found{}, err
	}
	t.mid = item.GetName()
	if err := requireUpdate(ctx, local, t); err != nil {
		return found{}, err
	}
	if p.nodeActions == nil {
		return found{}, internalError("the node action client is missing")
	}
	client, err := p.nodeActions.NodeActions()
	if err != nil {
		return found{}, err
	}
	node, err := client.GetNode(ctx, t.id, t.mid)
	if err != nil {
		return found{}, err
	}
	noActions, ce := normanRule(node, item.Object, action, t)
	if ce != nil {
		return found{}, ce
	}
	return found{
		t: t, local: local, row: row, listed: item,
		norman:     &normanPath{client: client, listedActions: node.HasActions, noActions: noActions},
		waitClient: steveWaitClient{ClusterClient: local, get: get},
	}, nil
}

// normanRule decides whether Rancher offers action on the node. An actions map
// with any key decides alone; when Rancher lists no actions, the rules of
// kinds.NodeDrainRule apply to the management node as the Steve API serves it,
// and noActions is true. The error is nil for an allowed action.
func normanRule(node engine.NormanNode, item map[string]any, action kinds.NodeDrainAction, t target) (noActions bool, ce *clierr.Error) {
	state := kinds.NodeDrainStateFrom(item)
	switch {
	case !node.HasActions:
		return true, refusal(kinds.NodeDrainRule(state, action), state, action, t)
	case node.Offers(nodeAction(action)):
		return false, nil
	}
	if ce := refusal(kinds.NodeDrainRule(state, action), state, action, t); ce != nil {
		return false, ce
	}
	return false, noActionOffered(node, action, t)
}

// noActionOffered is the error for an action missing from Rancher's actions
// map of a node that the rules of kinds.NodeDrainRule allow.
func noActionOffered(node engine.NormanNode, action kinds.NodeDrainAction, t target) *clierr.Error {
	msg := "Rancher does not offer to " + offerPhrase(action, t.node)
	if node.State == "" {
		return clierr.New(clierr.CodeValidation, msg+": Rancher gave no state for the node")
	}
	return clierr.New(clierr.CodeValidation, msg+" while it is "+node.State)
}

// steveManagementNode lists the management nodes of the cluster through the
// Steve API and returns the one whose status.nodeName is the node: among the
// items with the label management.cattle.io/nodename set to the node, or, when
// none has it, among every item.
func steveManagementNode(ctx context.Context, get steve.Getter, t target) (*unstructured.Unstructured, error) {
	req := steve.Request{Type: steveNodes, Namespace: t.id}
	rows, err := steve.ReadCollection(ctx, get, req, decodeSteveItem(t.id))
	if err != nil {
		return nil, steveListError(err, req, t)
	}
	var labeled, all []unstructured.Unstructured
	for _, row := range rows {
		var obj map[string]any
		if err := utiljson.Unmarshal([]byte(row), &obj); err != nil || obj == nil {
			return nil, unreadableList(t.id, req.Path(), errors.New("an item is not a JSON object"))
		}
		item := unstructured.Unstructured{Object: obj}
		if name, _, _ := unstructured.NestedString(item.Object, "status", "nodeName"); name != t.node {
			continue
		}
		all = append(all, item)
		if item.GetLabels()[nodeNameLabel] == t.node {
			labeled = append(labeled, item)
		}
	}
	matches := labeled
	if len(matches) == 0 {
		matches = all
	}
	slices.SortFunc(matches, func(a, b unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	return selectManagementNode(t, matches)
}

// decodeSteveItem keeps each item of the collection as JSON text, under its
// id. An item that is not a JSON object or has no id is an error.
func decodeSteveItem(clusterID string) steve.Decoder[string] {
	return func(path string, index int, raw json.RawMessage) (string, string, error) {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil || item == nil {
			return "", "", unreadableList(clusterID, path, fmt.Errorf("item %d is not a JSON object", index))
		}
		id, _ := item["id"].(string)
		if id == "" {
			return "", "", unreadableList(clusterID, path, fmt.Errorf("item %d has no id", index))
		}
		return id, string(raw), nil
	}
}

func unreadableList(clusterID, path string, err error) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf("could not read the management nodes of cluster %s from GET %s: %v", clusterID, path, err)).WithRetryable(false)
}

// steveListError is the error for a refused list of the management nodes: 403
// and 404 are RBAC. Any other error passes unchanged.
func steveListError(err error, req steve.Request, t target) error {
	status, ok := clierr.HTTPStatus(err)
	if !resources.ReadRefused(err) && (!ok || status != http.StatusNotFound) {
		return err
	}
	if !ok {
		status = http.StatusForbidden
	}
	return clierr.New(clierr.CodeRBAC, fmt.Sprintf("you cannot read the management nodes of cluster %s through Rancher's Steve API (GET %s was refused with HTTP %d)", t.id, req.Path(), status)).WithCause(err)
}

// requireUpdate asks the local cluster whether the caller may update the
// management nodes of the cluster's namespace; nothing is sent to Rancher's
// node action before it says yes.
func requireUpdate(ctx context.Context, local engine.ClusterClient, t target) error {
	gvk, ok := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
	if !ok {
		return internalError("the access review kind is missing")
	}
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version,
		"kind":       gvk.Kind,
		"spec": map[string]any{"resourceAttributes": map[string]any{
			"group": "management.cattle.io", "resource": "nodes", "verb": "update", "namespace": t.id,
		}},
	}}
	resp, err := local.PostReview(ctx, engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: review})
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		if resp.Mapped != nil {
			return resp.Mapped
		}
		return clierr.New(clierr.CodeGeneric, fmt.Sprintf("the access review of the local cluster was refused with HTTP %d", resp.Status))
	}
	if resp.Object != nil {
		if allowed, _, _ := unstructured.NestedBool(resp.Object.Object, "status", "allowed"); allowed {
			return nil
		}
	}
	msg := fmt.Sprintf("you cannot change management node %s of cluster %s: the local cluster does not allow you to update nodes.management.cattle.io in namespace %s; the cluster roles Cluster Owner and Manage Nodes grant that", t.mid, t.id, t.id)
	if resp.Object != nil {
		if evalErr, _, _ := unstructured.NestedString(resp.Object.Object, "status", "evaluationError"); evalErr != "" {
			msg += "; the review reported: " + evalErr
		}
	}
	return clierr.New(clierr.CodeRBAC, msg)
}

// The warnings of the Norman path that come before the command's own.
const (
	normanNoLocalAccess = "you have no access to Rancher's local cluster; the command asks Rancher through the action the Rancher UI uses"
	normanNoDryRun      = "Rancher has no dry-run for this action and accepts it in any state; the preview is built from reads, and the request is not checked against it: the node can change between the preview and the apply"
	normanTaints        = "Rancher's API also sets metadataUpdate, desiredNodeTaints (its copy of the node's taints) and updateTaintsFromAPI; Rancher then replaces the node's taints with that copy, which can undo a taint changed since Rancher last copied them"
)

// normanWarnings are the warnings of the Norman path that come before the
// command's own.
func (f found) normanWarnings() []string {
	warnings := []string{normanNoLocalAccess, normanNoDryRun, normanTaints}
	if f.norman.noActions {
		warnings = append(warnings, fmt.Sprintf("Rancher's API listed no actions for node %s; the command checked the node's state against the rules the Rancher UI uses", f.t.node))
	}
	return warnings
}

// normanUpdate is the planned write of a management node through Rancher's
// node action: the Effect, built from the Steve read and the object the action
// writes, and the executor that sends the request.
func (f found) normanUpdate(obj *unstructured.Unstructured, action kinds.NodeDrainAction, body []byte) (safety.Effect, runtime.ExecuteFunc) {
	diff := engine.RedactDiff(engine.DiffObjects(f.listed.Object, obj.Object), f.row.Redaction())
	eff := safety.Effect{
		Action:            safety.ActionUpdate,
		Target:            f.row.GVR.Resource + "/" + f.t.mid,
		DisplayKind:       "node",
		DisplayName:       f.t.node,
		PreviewSource:     safety.PreviewServerReads,
		WriteVia:          safety.WriteViaRancherAction,
		Cluster:           f.t.id,
		Diff:              diff,
		ConflictDetection: false,
		Reversible:        false,
		Destructive:       action == kinds.NodeDrain,
	}
	return eff, f.normanExec(action, body)
}

// normanExec reads the node again, refuses when Rancher no longer offers the
// action, then sends the request once.
func (f found) normanExec(action kinds.NodeDrainAction, body []byte) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		node, err := f.norman.client.GetNode(ctx, f.t.id, f.t.mid)
		if err != nil {
			return runtime.Result{}, err
		}
		if f.norman.listedActions && !node.HasActions {
			return runtime.Result{}, noActionOffered(node, action, f.t)
		}
		if _, ce := normanRule(node, f.listed.Object, action, f.t); ce != nil {
			return runtime.Result{}, ce
		}
		if err := f.norman.client.PostNodeAction(ctx, f.t.id, f.t.mid, nodeAction(action), body); err != nil {
			return runtime.Result{}, f.postError(err, action)
		}
		return runtime.Result{}, nil
	}
}

// postError is the error of the action request. A response that does not say
// whether Rancher accepted the request is GENERIC and not retryable, naming
// the command that reads the management node. A 409 is retryable CONFLICT.
// Every other refusal keeps the code of the status table.
func (f found) postError(err error, action kinds.NodeDrainAction) error {
	if u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err); ok {
		recovery := f.t.recovery()
		sent := "the request to drain node " + f.t.node
		if action == kinds.NodeStopDrain {
			sent = "the request to stop the drain of node " + f.t.node
		}
		text := clierr.Text(sent+" was sent, but the answer did not say whether Rancher accepted it ("+outcomeCause(u)+"); before you run the command again, read the management node",
			clierr.Lead(": "), recovery)
		details := clierr.CommittedContextDetails{Committed: true, ResourceKind: "node", ResourceName: f.t.node, Recovery: recovery.Field()}
		return clierr.New(clierr.CodeGeneric, text).WithRetryable(false).WithHintCommand(recovery).WithDetails(details)
	}
	if r, ok := errors.AsType[*engine.ActionRefused](err); ok {
		if status, ok := clierr.HTTPStatus(r.Err); ok && status == http.StatusConflict {
			return clierr.Retryable(clierr.CodeConflict, fmt.Sprintf("Rancher reported a conflict (HTTP 409) on node %s; the request may or may not have been written; read the management node, then run the command again", f.t.node))
		}
		return r.Err
	}
	return err
}

// outcomeCause words the failure of a request that was sent without the URL,
// which names nothing the reader needs.
func outcomeCause(err error) string {
	if status, ok := clierr.HTTPStatus(err); ok {
		return fmt.Sprintf("the server answered %d", status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "the request was cancelled"
	}
	return "the connection failed"
}
