package nodes

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type stopDrainCmd struct {
	planner
}

// NewStopDrain builds `node stop-drain <node> --cluster <cluster>`
// (mutating): it writes the stop request on the node's management node, and
// Rancher stops the running drain. The node stays cordoned. A caller with no
// access to Rancher's local cluster asks Rancher through its node action
// instead.
func NewStopDrain(clients engine.ClusterClientFactory, roots engine.RootClientFactory, nodeActions engine.NodeActionClientFactory) runtime.Command {
	return &stopDrainCmd{planner{clients: clients, roots: roots, nodeActions: nodeActions}}
}

func (stopDrainCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "node stop-drain",
		Usage: "Stop a running drain of a node; the node stays cordoned",
		Desc: "Asks Rancher to stop the running drain of the node <node> of the cluster given with --cluster. Pods already evicted are not put back and the node " +
			"stays cordoned; uncordon it by setting spec.unschedulable to false on the node. By default it waits until Rancher " +
			"reports the drain stopped (--wait=false returns once the server accepts the request). " +
			"Without access to Rancher's local cluster it sends the request through the action the Rancher UI uses; the preview is built from reads with no dry-run, and Rancher accepts the request for a node in any state.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "node", Type: "string", Required: true, Desc: "node `name`, the NAME column of `rancher get node --cluster <cluster>`"}},
		Flags: []runtime.ArgSpec{
			engine.ClusterFlag(true, false),
			{Name: flagWait, Type: "bool", Desc: "wait until Rancher reports the drain stopped (default true; --wait=false returns once the server accepts the request)"},
			{Name: flagTimeout, Type: "string", Desc: "wait `timeout`, a positive duration (default 2m)"},
		},
		Examples: []string{
			"rancher node stop-drain worker-1 --cluster prod --dry-run",
			"rancher node stop-drain worker-1 --cluster prod --apply",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// LiveSource completes <node> with the nodes stop-drain is allowed on.
func (stopDrainCmd) LiveSource() completion.Source { return kindscomplete.DrainNodeSource() }

var (
	_ runtime.MutatingCommand = (*stopDrainCmd)(nil)
	_ completion.Completer    = (*stopDrainCmd)(nil)
)

func (c stopDrainCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	w, err := parseWait(in, defaultStopTimeout)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	fd, err := c.find(ctx, in, kinds.NodeStopDrain)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	obj := fd.listed.DeepCopy()
	if err := unstructured.SetNestedField(obj.Object, "stopDrain", "spec", "desiredNodeUnschedulable"); err != nil {
		return safety.Effect{}, nil, internalError(err.Error())
	}
	stayCordoned := "the node stays cordoned; evicted pods are not put back; uncordon it by setting spec.unschedulable to false on the node"
	var eff safety.Effect
	var put runtime.ExecuteFunc
	if fd.norman != nil {
		eff, put = fd.normanUpdate(obj, kinds.NodeStopDrain, nil)
		eff.Warnings = append(fd.normanWarnings(), stayCordoned)
	} else {
		eff, put, err = fd.update(ctx, obj, false)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		eff.Warnings = []string{stayCordoned}
	}
	return eff, stopDrainRun{found: fd, wait: w, planned: readDrained(fd.listed.Object)}.exec(put), nil
}

// stopDrainRun is what runs after the stop request is planned: the write, then
// the wait.
type stopDrainRun struct {
	found
	wait waitFlags
	// planned is the Drained condition the management node had when it was
	// listed.
	planned drainedCondition
}

func (r stopDrainRun) exec(put runtime.ExecuteFunc) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		if _, err := put(ctx); err != nil {
			return runtime.Result{}, err
		}
		if !r.wait.wait {
			return r.requested(), nil
		}
		w := &nodeWait{
			action: kinds.NodeStopDrain, client: r.waitClient, gvr: r.row.GVR, t: r.t,
			timeout: r.wait.timeout, timeoutText: r.wait.timeoutText,
			planned: r.planned,
		}
		end, err := w.run(ctx)
		if err != nil {
			return runtime.Result{}, err
		}
		if end.refused {
			res := r.requested()
			res.Warnings = []string{fmt.Sprintf("your access to node %s ended after the stop request; the stop was requested and not confirmed", r.t.node)}
			return res, nil
		}
		res := runtime.MutationText("node", r.t.node, "drain stopped; the node stays cordoned")
		res.Kind = "NodeStopDrain"
		res.Data = r.data(true)
		return res, nil
	}
}

// requested is the result of a stop request Rancher accepted and the command
// did not confirm.
func (r stopDrainRun) requested() runtime.Result {
	res := runtime.MutationText("node", r.t.node, "stop requested")
	res.Kind = "NodeStopDrain"
	res.Data = r.data(false)
	return res
}
