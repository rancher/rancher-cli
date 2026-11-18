package nodes

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// planner is what node drain and node stop-drain share: the reads that find
// the management node and the state Rancher shows for it.
type planner struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// found is the management node of a node, with the state Rancher shows for
// it.
type found struct {
	t      target
	local  engine.ClusterClient
	row    kinds.Kind
	listed *unstructured.Unstructured
}

// find resolves --cluster, finds the management node of the node, reads its
// state through the Steve API and refuses an action Rancher does not offer.
func (p planner) find(ctx context.Context, in runtime.Input, action kinds.NodeDrainAction) (found, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return found{}, clierr.New(clierr.CodeValidation, "argument <node> is required")
	}
	t := target{node: args[0], cluster: in.String(engine.FlagCluster)}
	id, err := p.clients.ResolveCluster(ctx, t.cluster)
	if err != nil {
		return found{}, err
	}
	t.id = id
	row, ok := kinds.ByKey("managementnode")
	if !ok {
		return found{}, internalError("the management-node kind is missing")
	}
	local, err := p.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return found{}, err
	}
	listed, err := findManagementNode(ctx, local, row, t)
	if err != nil {
		return found{}, err
	}
	t.mid = listed.GetName()
	obj, err := readSteveNode(ctx, steve.RootFactoryGetter(p.roots), t, listed)
	if err != nil {
		return found{}, err
	}
	state := kinds.NodeDrainStateFrom(obj)
	if ce := refusal(kinds.NodeDrainRule(state, action), state, action, t); ce != nil {
		return found{}, ce
	}
	return found{t: t, local: local, row: row, listed: listed}, nil
}

// update is the planned write of obj on the management node of f: its
// dry-run, the Effect and the executor that sends the write.
func (f found) update(ctx context.Context, obj *unstructured.Unstructured, destructive bool) (safety.Effect, runtime.ExecuteFunc, error) {
	diff := engine.DiffObjects(f.listed.Object, obj.Object)
	eff, _, exec, err := engine.PlanUpdateObserved(ctx, f.local, f.row.GVR, f.t.id, obj, f.row.Redaction(), f.t.id, "node", "Node", diff, destructive)
	if err != nil {
		return safety.Effect{}, nil, rejectedUpdate(err, f.t.id)
	}
	eff.DisplayName = f.t.node
	eff.Reversible = false
	return eff, exec, nil
}

// rejectedUpdate adds to a 403 on the write what Rancher's roles grant. An
// admission refusal and every other error pass unchanged.
func rejectedUpdate(err error, id string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeRBAC {
		return err
	}
	return ce.WithMessage(ce.Message + fmt.Sprintf("; this command needs access to the local cluster, with update on nodes.management.cattle.io in namespace %s there", id))
}

// data is the data every result carries: the node, its management node and
// whether the command waited.
func (f found) data(waited bool) map[string]any {
	return map[string]any{"node": f.t.node, "managementNode": f.t.id + "/" + f.t.mid, "waited": waited}
}
