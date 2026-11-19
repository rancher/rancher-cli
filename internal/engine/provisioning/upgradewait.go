package provisioning

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// upgradePlan is what the executor of `cluster upgrade` needs after the
// Plan: the cluster, the target and how long to wait.
type upgradePlan struct {
	read    upgradeRead
	target  string
	wait    clusterWait
	gvr     schema.GroupVersionResource
	objNS   string
	row     kinds.Kind
	client  engine.ClusterClient
	clients engine.ClusterClientFactory
}

// resourceKind and recovery name the cluster for a committed error: the
// provisioning Cluster of a provisioned cluster, the management Cluster of an
// imported one, and the get command that reads it.
func (p upgradePlan) resourceKind() string {
	if p.read.facts.Provisioned {
		return "provisioning-cluster"
	}
	return "cluster"
}

func (p upgradePlan) objectName() string {
	if p.read.facts.Provisioned {
		return p.read.name
	}
	return p.read.facts.ID
}

func (p upgradePlan) recovery() clierr.Command {
	if p.read.facts.Provisioned {
		return clierr.NewCommand("get provisioning-cluster").Arg(p.read.name).FlagValue("--namespace", p.read.ns)
	}
	return clierr.NewCommand("get cluster").Arg(p.read.facts.ID)
}

// wrap makes the executor that sends the update, then reports it or waits
// until Rancher reports the upgrade done.
func (p upgradePlan) wrap(exec runtime.ExecuteFunc) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		generation := int64(0)
		if obj, ok := res.Data.(map[string]any); ok {
			v, _, _ := unstructured.NestedFieldNoCopy(obj, "metadata", "generation")
			generation = numberOf(v)
		}
		data := map[string]any{
			"cluster":   p.read.name,
			"namespace": p.read.ns,
			"clusterId": p.read.clusterID,
			"from":      p.read.facts.Current,
			"to":        p.target,
			"waited":    false,
		}
		requested := runtime.MutationText("cluster", p.read.name, "upgrade requested")
		finish := func(r runtime.Result, text runtime.Result, d map[string]any, warnings ...string) runtime.Result {
			r.Text, r.StyledText, r.Data = text.Text, text.StyledText, d
			r.Warnings = append(r.Warnings, warnings...)
			return r
		}
		if !p.wait.On {
			return finish(res, requested, data), nil
		}
		end, werr := p.waitDone(ctx, generation)
		if werr != nil {
			return runtime.Result{}, werr
		}
		if end.refused {
			return finish(res, requested, data, fmt.Sprintf("your access to cluster %s ended after the upgrade request; the upgrade was requested and not confirmed", p.read.name)), nil
		}
		data["waited"] = true
		if p.read.facts.Provisioned && p.read.listed {
			data["nodes"] = end.nodes
		}
		return finish(res, runtime.MutationText("cluster", p.read.name, "upgraded to "+p.target), data), nil
	}
}

// retriedNodeListStatuses are the HTTP statuses of a Node List response that
// the wait retries: the gateway responds with them while the API server of the
// cluster restarts.
// Any other error ends the wait.
var retriedNodeListStatuses = []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}

// upgradeEnd is how the wait ended: the Nodes as read last, or a refused
// read.
type upgradeEnd struct {
	nodes   []nodeVersion
	refused bool
}

// waitDone waits until Rancher reports the upgrade done. A provisioned
// cluster is done when Rancher has seen the generation the update returned,
// its Updated condition is True and every Node the Plan listed runs the
// target; an imported cluster when its Upgraded condition is True and it
// runs the target or newer. The Updated condition False does not end the wait:
// the planner retries.
func (p upgradePlan) waitDone(ctx context.Context, generation int64) (upgradeEnd, error) {
	var (
		end upgradeEnd
		// nodeListErr is the error that ended the wait on a Node List response.
		nodeListErr error
		// nodesNotRead is the status of the last Node List of the wait while it
		// failed with a retried status; a Node List that succeeds clears it.
		nodesNotRead string
	)
	checkNodes := p.read.facts.Provisioned && p.read.listed
	recovery := p.recovery()
	failed := func(msg string) *clierr.Error {
		return committedWaitError(p.resourceKind(), p.objectName(), recovery, msg)
	}
	// abort ends the wait with the error of a read: its own code, and the
	// details that tell a caller the upgrade was requested.
	abort := func(err error) error {
		return engine.CommittedPartialSuccess(err, clierr.CommittedContextDetails{
			Committed: true, ResourceKind: p.resourceKind(), ResourceName: p.objectName(), Recovery: recovery.Field(),
		}).WithRetryable(false).WithHintCommand(recovery)
	}
	err := engine.WaitObject(ctx, p.client, engine.WaitParams{
		GVR:         p.gvr,
		Namespace:   p.objNS,
		Name:        p.objectName(),
		Timeout:     p.wait.Timeout,
		Interval:    clusterWaitInterval,
		SpacingFrom: engine.SpacingFromReturn,
		Observe: func(ctx context.Context, obj *unstructured.Unstructured, _ engine.WaitSource) (engine.WaitOutcome, error) {
			if !p.read.facts.Provisioned {
				status, _, _ := conditionOf(obj, "Upgraded")
				running, _, _ := unstructured.NestedString(obj.Object, "status", "version", "gitVersion")
				if status == "True" && clusterops.CompareVersions(running, p.target) >= 0 {
					return engine.WaitDone, nil
				}
				return engine.WaitContinue, nil
			}
			status, _, _ := conditionOf(obj, "Updated")
			observed, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", "observedGeneration")
			ready := status == "True" && numberOf(observed) >= generation
			if checkNodes {
				nodes, err := readNodes(ctx, p.clients, p.read.clusterID)
				if err != nil {
					switch code, answered := clierr.HTTPStatus(err); {
					case resources.ReadRefused(err):
						end.refused = true
						return engine.WaitDone, nil
					case ctx.Err() != nil:
						// The wait ended during the read: the loop reports it.
						return engine.WaitCheckAgain, nil
					case answered && slices.Contains(retriedNodeListStatuses, code):
						nodesNotRead = fmt.Sprintf("%d %s", code, http.StatusText(code))
						return engine.WaitCheckAgain, nil
					}
					nodeListErr = abort(err)
					return engine.WaitDone, nodeListErr
				}
				end.nodes = nodes
				nodesNotRead = ""
				ready = ready && nodesRunning(nodes, p.target) == len(nodes)
			}
			if ready {
				return engine.WaitDone, nil
			}
			return engine.WaitCheckAgain, nil
		},
		StopOn: resources.ReadRefused,
		RecordErr: func(ctx context.Context, err error) bool {
			_, answered := clierr.HTTPStatus(err)
			return ctx.Err() == nil || answered
		},
		OnAbort: func(err error) error {
			if resources.ReadRefused(err) {
				end.refused = true
				return nil
			}
			return abort(err)
		},
		OnCancel: func(context.Context, *unstructured.Unstructured, *clierr.Error) error {
			return failed(fmt.Sprintf("stopped waiting for cluster %s to run %s; the upgrade was requested and goes on", p.read.name, p.target))
		},
		OnTimeout: func(object *unstructured.Unstructured, _ *clierr.Error) error {
			return failed(fmt.Sprintf("timed out after %s waiting for cluster %s to run %s; the upgrade was requested and goes on; last state: %s; to wait again, run this command again with the same version: it changes nothing and waits",
				p.wait.TimeoutText, p.read.name, p.target, p.state(object, end.nodes, nodesNotRead)))
		},
	})
	if nodeListErr != nil && errors.Is(err, nodeListErr) {
		return upgradeEnd{}, err
	}
	if clierr.IsNotFound(err) {
		return upgradeEnd{}, failed(fmt.Sprintf("cluster %s was deleted during the upgrade", p.read.name))
	}
	return end, err
}

// state is the last state the wait saw, as the timeout error prints it: the
// message of the condition Rancher reports the upgrade in and, for a
// provisioned cluster whose Nodes were listed, how many run the target. When
// the last Node List of the wait failed with a retried status, notRead is that
// status and the state ends with it: the node count is then from an earlier
// read, or missing.
func (p upgradePlan) state(obj *unstructured.Unstructured, nodes []nodeVersion, notRead string) string {
	if obj == nil {
		return "no state seen yet"
	}
	condition := "Upgraded"
	if p.read.facts.Provisioned {
		condition = "Updated"
	}
	var parts []string
	if _, message, _ := conditionOf(obj, condition); message != "" {
		parts = append(parts, message)
	}
	if p.read.facts.Provisioned && p.read.listed && nodes != nil {
		parts = append(parts, fmt.Sprintf("%d/%d nodes at %s", nodesRunning(nodes, p.target), len(nodes), p.target))
	}
	state := "Rancher reported no message"
	if len(parts) > 0 {
		state = strings.Join(parts, ", ")
	}
	if notRead != "" {
		state += "; could not list the nodes: " + notRead
	}
	return state
}
