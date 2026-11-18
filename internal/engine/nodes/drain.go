package nodes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type drainCmd struct {
	planner
}

// NewDrain builds `node drain <node> --cluster <cluster>` (mutating): it
// writes the drain request and its options on the node's management node,
// and Rancher runs the drain.
func NewDrain(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &drainCmd{planner{clients: clients, roots: roots}}
}

func (drainCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "node drain",
		Usage: "Drain a node: Rancher cordons it and evicts its pods",
		Desc: "Asks Rancher to drain the node <node> of the cluster given with --cluster: the command writes the drain request and its options " +
			"on the node's management node, and Rancher runs kubectl drain as its system user for the cluster, which cordons the node and " +
			"evicts its pods through the Eviction API, respecting PodDisruptionBudgets. The preview lists the pods on the node and what " +
			"the drain does to each (text output names up to five pods per outcome; -o json lists every pod). Rancher offers drain only " +
			"for worker nodes that are active or cordoned (it shows a node whose drain failed on a kubectl flag or a timeout as cordoned); " +
			"the command refuses the others with Rancher's state. By default it waits until Rancher reports the drain done or failed " +
			"(--wait=false returns once the server accepts the request). Requires --yes with --apply. It needs access to Rancher's local cluster.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "node", Type: "string", Required: true, Desc: "node `name`, the NAME column of `rancher get node --cluster <cluster>`"}},
		Flags: []runtime.ArgSpec{
			engine.ClusterFlag(true, false),
			{Name: flagForce, Type: "bool", Desc: "also evict pods that no controller manages"},
			{Name: flagDeleteEmptyDir, Type: "bool", Desc: "also evict pods with emptyDir volumes; their emptyDir data is deleted"},
			{Name: flagGracePeriod, Type: "string", Desc: "`seconds` each pod gets to stop, 1 or more (default: each pod's own)"},
			{Name: flagDrainTimeout, Type: "string", Desc: "how long Rancher's drain may run, a duration such as 5m, from 1s to 3h in whole seconds (default 2m, as in Rancher; a large node can need more)"},
			{Name: flagWait, Type: "bool", Desc: "wait until Rancher reports the drain done or failed (default true; --wait=false returns once the server accepts the request)"},
			{Name: flagTimeout, Type: "string", Desc: "wait `timeout`, a positive duration (default --drain-timeout plus 1m)"},
		},
		Examples: []string{
			"rancher node drain worker-1 --cluster prod --dry-run",
			"rancher node drain worker-1 --cluster prod --force --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// LiveSource completes <node> with the nodes drain is allowed on.
func (drainCmd) LiveSource() completion.Source { return kindscomplete.DrainNodeSource() }

var (
	_ runtime.MutatingCommand = (*drainCmd)(nil)
	_ completion.Completer    = (*drainCmd)(nil)
)

func (c drainCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	f, err := parseDrainFlags(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	fd, err := c.find(ctx, in, kinds.NodeDrain)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	pods, readErr, err := listNodePods(ctx, c.clients, fd.t.id, fd.t.node)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	plan := planPods(pods, f.force, f.deleteEmptyDir)
	obj, err := drainObject(fd.listed, f)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff, put, err := fd.update(ctx, obj, true)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff.Warnings = drainWarnings(fd.t, f, plan, readErr)
	eff.Objects = plan.objects
	run := drainRun{found: fd, flags: f, evictedNamespaces: plan.evictedNamespaces, planned: readDrained(fd.listed.Object)}
	return eff, run.exec(put), nil
}

// drainObject is the management node with the drain request and the five
// options Rancher's own drain action writes, each with the default Rancher
// gives it.
func drainObject(listed *unstructured.Unstructured, f drainFlags) (*unstructured.Unstructured, error) {
	obj := listed.DeepCopy()
	input := map[string]any{
		"force":            f.force,
		"ignoreDaemonSets": true,
		"deleteLocalData":  f.deleteEmptyDir,
		"gracePeriod":      f.gracePeriod,
		"timeout":          int64(f.drainTimeout / time.Second),
	}
	if err := unstructured.SetNestedField(obj.Object, input, "spec", "nodeDrainInput"); err != nil {
		return nil, internalError(err.Error())
	}
	if err := unstructured.SetNestedField(obj.Object, "drain", "spec", "desiredNodeUnschedulable"); err != nil {
		return nil, internalError(err.Error())
	}
	return obj, nil
}

// drainWarnings are the preview's notes, in order; the conditional ones only
// when they apply.
func drainWarnings(t target, f drainFlags, plan podPlan, readErr error) []string {
	warnings := []string{
		"the server-side dry-run checks the request to Rancher; it does not run the drain. Rancher runs kubectl drain, which cordons the node and then evicts the pods listed here through the Eviction API, respecting PodDisruptionBudgets",
		fmt.Sprintf("Rancher runs the drain as its system user for cluster %s, not with your permissions", t.id),
		"stopping a drain leaves the node cordoned; evicted pods are not put back",
	}
	if len(plan.blockers) > 0 {
		warnings = append(warnings, "kubectl drain refuses this node before it evicts anything: "+strings.Join(plan.blockers, "; ")+"; Rancher then marks the drain failed and the node stays cordoned")
	}
	if readErr != nil {
		warnings = append(warnings, podListWarning(t.node, readErr))
	} else {
		warnings = append(warnings, "this pod list is read now; Rancher lists the pods again when the drain runs")
	}
	if plan.daemonSet && !f.force {
		warnings = append(warnings, "a DaemonSet pod whose DaemonSet is gone is evicted with --force and blocks the drain without it")
	}
	if t.id == engine.LocalCluster {
		warnings = append(warnings, "this node is in the cluster Rancher runs in; evicting Rancher's own pods can stop the drain and the API this command uses")
	}
	if f.wait && f.timeout < f.drainTimeout {
		warnings = append(warnings, "the wait ends before Rancher's drain timeout; the drain goes on")
	}
	return warnings
}

// drainRun is what runs after the drain request is planned: the write, then
// the wait.
type drainRun struct {
	found
	flags             drainFlags
	evictedNamespaces []string
	// planned is the Drained condition the management node had when it was
	// listed.
	planned drainedCondition
}

func (r drainRun) exec(put runtime.ExecuteFunc) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		if _, err := put(ctx); err != nil {
			return runtime.Result{}, err
		}
		if !r.flags.wait {
			return r.requested(), nil
		}
		w := &nodeWait{
			action: kinds.NodeDrain, client: r.local, gvr: r.row.GVR, t: r.t,
			timeout: r.flags.timeout, timeoutText: r.flags.timeoutText,
			planned: r.planned, flags: r.flags, evictedNamespaces: r.evictedNamespaces,
		}
		end, err := w.run(ctx)
		if err != nil {
			return runtime.Result{}, err
		}
		if end.refused {
			res := r.requested()
			res.Warnings = []string{fmt.Sprintf("your access to node %s ended after the drain request; the drain was requested and not confirmed", r.t.node)}
			return res, nil
		}
		res := runtime.MutationText("node", r.t.node, "drained")
		res.Kind = "NodeDrain"
		data := r.data(true)
		data["drained"] = true
		data["condition"] = map[string]any{"type": end.condition.typ, "status": end.condition.status, "reason": end.condition.reason, "message": end.condition.message}
		res.Data = data
		return res, nil
	}
}

// requested is the result of a drain request Rancher accepted and the command
// did not confirm.
func (r drainRun) requested() runtime.Result {
	res := runtime.MutationText("node", r.t.node, "drain requested")
	res.Kind = "NodeDrain"
	res.Data = r.data(false)
	return res
}
