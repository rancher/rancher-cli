// day2ops.go has the pieces the commands that request an operation of
// Rancher on a provisioned cluster share: the reads and checks of the Plan,
// the next generation of the request, the Effect, the waiting run and the
// wait on the RKEControlPlane. A waiting run is a run of a command while the
// operation it requests is already in progress: it changes nothing and waits.
// Rancher calls etcd snapshots, restores and rotations day 2 operations; the
// identifiers say day2. An operation is requested by writing its field
// of spec.rkeConfig on the provisioning Cluster; Rancher copies the request
// into the status of the RKEControlPlane (the status copy) and reports the
// operation's phase beside it.
package provisioning

import (
	"context"
	"fmt"
	"maps"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// phaseFinished and phaseFailed are the phases that end an operation with
	// a phase field.
	phaseFinished = "Finished"
	phaseFailed   = "Failed"

	// notStartedPhase stands in a message for the phase of a request Rancher
	// has not started.
	notStartedPhase = "requested, not started"
)

// day2Operation describes one operation Rancher runs on a provisioned cluster
// and the command that requests it.
type day2Operation struct {
	// kind is the operation as the refusals name it.
	kind clusterops.OperationKind
	// field is the field of spec.rkeConfig that requests the operation and
	// the field of the RKEControlPlane's status that copies the request.
	field string
	// phaseField is the field of the RKEControlPlane's status that has the
	// phase.
	phaseField string
	// doneAt is the phase that says Rancher finished the operation.
	doneAt string
	// requested and done are the verbs of the result text: "etcd snapshot
	// requested" and "etcd snapshot taken".
	requested, done string
}

// day2Target is what a Plan read about the cluster before it builds the
// request.
type day2Target struct {
	client engine.ClusterClient
	// roots is for the list of etcd snapshots through Steve, as the caller,
	// after the Kubernetes API responds with 403.
	roots engine.RootClientFactory
	prov  *unstructured.Unstructured
	ns    string
	name  string
	// clusterID is the id of the management Cluster, "" when Rancher has not
	// linked one yet.
	clusterID string
	// controlPlane is nil when the RKEControlPlane could not be read.
	controlPlane *unstructured.Unstructured
	// waiting is true when the operation the command requests is already in
	// progress.
	waiting bool
	// running is the operation in progress that a waiting run waits for, as
	// RunningOperations reports it.
	running clusterops.Operation
	// phase is the phase Rancher reports for the operation in progress.
	phase string
	// notes are the warnings of the reads that failed, in the order the Plan
	// made them.
	notes []string
}

// noClusterError is the refusal for a name with no provisioning Cluster in
// the namespace.
func noClusterError(name, ns string) *clierr.Error {
	list := clierr.NewCommand("get provisioning-clusters")
	if ns != clusterNamespace {
		list = list.FlagValue("--namespace", ns)
	}
	return clierr.New(clierr.CodeNotFound,
		fmt.Sprintf("cluster %s has no provisioning cluster in namespace %s; an admin or a user with the Create Clusters role can list them", name, ns)).
		WithHintCommand(list)
}

// noControlPlaneError is the refusal for a provisioned cluster whose
// RKEControlPlane does not exist yet.
func noControlPlaneError(name string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("cluster %s has no RKEControlPlane yet; Rancher creates it while it provisions the cluster; try again later", name)).WithRetryable(true)
}

// notReadyError is the refusal for a cluster the Rancher UI does not offer
// the operation on: its management Cluster is not connected or ready.
func notReadyError(name, condType, status, message string) *clierr.Error {
	state := "it reports no Connected or Ready condition"
	if condType != "" {
		state = fmt.Sprintf("condition %s is %s", condType, status)
		if message != "" {
			state += ": " + message
		}
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("cluster %s is not ready (%s); the Rancher UI offers etcd snapshots, etcd snapshot restores, certificate rotations and encryption key rotations only on a ready cluster", name, state))
}

// day2Match says which running operation of the command's own kind a run
// waits for. The zero value matches any operation of the kind; a restore
// matches only a running restore of the same etcd snapshot and, when the run
// types a restore mode, the same mode.
type day2Match struct {
	// snapshot is the name of the etcd snapshot object a restore names.
	snapshot string
	// mode is the restore mode typed, "" when none was.
	mode string
}

// matches reports whether a running operation is the request of this run.
func (m day2Match) matches(kind clusterops.OperationKind, running clusterops.Operation) bool {
	if running.Kind != kind {
		return false
	}
	if kind != clusterops.OperationRestore {
		return true
	}
	if name, _ := running.Request["name"].(string); name != m.snapshot {
		return false
	}
	return m.mode == "" || runningRestoreMode(running) == m.mode
}

// runningRestoreMode is the restore mode of a running restore as Rancher reads
// it: an empty restoreRKEConfig is none.
func runningRestoreMode(running clusterops.Operation) string {
	if mode, _ := running.Request["restoreRKEConfig"].(string); mode != "" {
		return mode
	}
	return clusterops.RestoreModeNone
}

// readDay2Target reads the provisioning Cluster, the management Cluster and
// the RKEControlPlane in the order of the checks and refuses what the Rancher
// UI and Rancher's one operation at a time refuse. It returns the target of a
// request, or of a waiting run when the operation is already in progress. A
// restore of mode kubernetesVersion or all that is running makes Rancher write
// the snapshot's Kubernetes version into the provisioning Cluster before the
// planner records it as applied; a Kubernetes upgrade is then reported beside
// the restore, and a waiting run for that restore does not refuse it.
func readDay2Target(ctx context.Context, clients engine.ClusterClientFactory, roots engine.RootClientFactory, in runtime.Input, op day2Operation, match day2Match) (*day2Target, error) {
	client, prov, ns, name, err := fetchClusterFamilyTarget(ctx, clients, in, "update")
	if err != nil {
		if clierr.IsNotFound(err) {
			return nil, noClusterError(in.Args()[0], resolveClusterNamespace(in)).WithCause(err)
		}
		return nil, err
	}
	id, _, _ := unstructured.NestedString(prov.Object, "status", "clusterName")
	t := &day2Target{client: client, roots: roots, prov: prov, ns: ns, name: name, clusterID: id}

	if !clusterops.HasRKEConfig(prov.Object) {
		if id == "" {
			return nil, unlinkedClusterError(name)
		}
		mgmt, err := resources.ManagementClusterThroughSteve(ctx, roots, id)
		if err != nil {
			return nil, err
		}
		return nil, classifyNoRKEConfigFor(op.kind, name, id, mgmt.Object, kindRegistered)
	}

	cp, err := fetchControlPlane(ctx, client, ns, name)
	switch {
	case err == nil:
		t.controlPlane = cp
	case clierr.IsNotFound(err):
		return nil, noControlPlaneError(name)
	default:
		t.notes = append(t.notes, fmt.Sprintf("cannot read the RKEControlPlane of cluster %s (%s); this command cannot see whether an operation is in progress, and if one is, Rancher runs the %s after it ends", name, clierr.From(err).Code, op.kind))
	}

	if cp != nil {
		for _, running := range clusterops.RunningOperations(prov.Object, cp.Object) {
			if match.matches(op.kind, running) {
				t.waiting, t.phase, t.running = true, running.Phase, running
				continue
			}
			if t.waiting && op.kind == clusterops.OperationRestore && running.Kind == clusterops.OperationUpgrade && restoresVersion(t.running) {
				continue
			}
			return nil, runningOperationError(name, ns, running)
		}
	}
	if t.waiting {
		return t, nil
	}

	if id == "" {
		return nil, unlinkedClusterError(name)
	}
	mgmt, err := resources.ManagementClusterThroughSteve(ctx, roots, id)
	if err != nil {
		t.notes = append(t.notes, fmt.Sprintf("cannot read the management cluster of cluster %s (%s); the check that the cluster is ready was skipped", name, clierr.From(err).Code))
		return t, nil
	}
	if ready, condType, status, message := clusterops.Ready(mgmt.Object); !ready {
		return nil, notReadyError(name, condType, status, message)
	}
	return t, nil
}

// restoresVersion reports whether a running restore has the mode
// kubernetesVersion or all, which write the snapshot's Kubernetes version.
func restoresVersion(running clusterops.Operation) bool {
	mode := runningRestoreMode(running)
	return mode == clusterops.RestoreModeKubernetesVersion || mode == clusterops.RestoreModeAll
}

// nextGeneration is the generation to write: one more than the higher of the
// request's generation on the provisioning Cluster and the one in the status
// copy of the RKEControlPlane, an absent one counting as 0. With no readable
// RKEControlPlane it is the request's plus one.
func (t *day2Target) nextGeneration(op day2Operation) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(t.prov.Object, "spec", "rkeConfig", op.field, "generation")
	generation := numberOf(v)
	if t.controlPlane != nil {
		v, _, _ = unstructured.NestedFieldNoCopy(t.controlPlane.Object, "status", op.field, "generation")
		generation = max(generation, numberOf(v))
	}
	return generation + 1
}

// requestedGeneration is the generation of the request on the provisioning
// Cluster, the one a waiting run waits for.
func (t *day2Target) requestedGeneration(op day2Operation) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(t.prov.Object, "spec", "rkeConfig", op.field, "generation")
	return numberOf(v)
}

// day2Plan is a Plan's result for one run of a command.
type day2Plan struct {
	op     day2Operation
	target *day2Target
	wait   clusterWait
	// generation is the generation written, or in a waiting run the one read.
	generation int64
	// snapshot and mode are the etcd snapshot object a restore names and the
	// restore mode the run typed, "" when none was; both are empty for the
	// other operations.
	snapshot, mode string
	// data are the keys a restore adds to the result's data, and done the text
	// of the finished operation; set by the Plan, empty for the save.
	data map[string]any
	done string
}

// buildEffect makes the Effect of the request: the provisioning Cluster as
// read with the field of spec.rkeConfig set to the next generation and the
// request's other fields, or, in a waiting run, as read. warnings come after
// the two every operation carries; the target's notes come last. The executor
// is wrapped to report and wait.
func (p *day2Plan) buildEffect(ctx context.Context, warnings []string, fields map[string]any) (safety.Effect, runtime.ExecuteFunc, error) {
	t := p.target
	after := t.prov.DeepCopy()
	if !t.waiting {
		p.generation = t.nextGeneration(p.op)
		request := map[string]any{"generation": p.generation}
		maps.Copy(request, fields)
		if err := unstructured.SetNestedField(after.Object, request, "spec", "rkeConfig", p.op.field); err != nil {
			return safety.Effect{}, nil, err
		}
	} else {
		p.generation = t.requestedGeneration(p.op)
	}
	after.SetResourceVersion(t.prov.GetResourceVersion())

	diff := engine.DiffObjects(t.prov.Object, after.Object)
	eff, exec, err := engine.PlanUpdate(ctx, t.client, provisioningClusterGVR, t.ns, after, provisioningClusterRedaction(), engine.LocalCluster, provisioningClusterDisplayKind, "Cluster", diff, provisioningClusterSensitive())
	if err != nil {
		return eff, exec, err
	}
	eff.Reversible = false
	eff.Warnings = append(eff.Warnings,
		fmt.Sprintf("the server-side dry-run checks the request; it does not run the %s", p.op.kind),
		"Rancher runs one operation at a time on a cluster; while one is in progress, Rancher does not apply an upgrade or a configuration change")
	eff.Warnings = append(eff.Warnings, warnings...)
	eff.Warnings = append(eff.Warnings, t.notes...)
	return eff, p.wrap(exec), nil
}

// reportAfterDone adds to the result what a command reports once Rancher ended
// the operation: the etcd snapshot lists the etcd snapshot objects of the
// cluster. It returns the warnings.
func (p *day2Plan) reportAfterDone(ctx context.Context, data map[string]any) []string {
	if p.op.kind == clusterops.OperationSnapshot {
		return listClusterSnapshots(ctx, p.target, data)
	}
	return nil
}

// waitingRunWarning is the warning of a run while the operation is already in
// progress.
func (p *day2Plan) waitingRunWarning() string {
	phase := p.target.phase
	if phase == "" {
		phase = notStartedPhase
	}
	article := operationArticle(p.op.kind)
	tail := "the command waits for it"
	if !p.wait.On {
		tail = "the command does not wait for it"
	}
	return fmt.Sprintf("%s %s of cluster %s is in progress (phase %s): nothing changes; %s", article, p.op.kind, p.target.name, phase, tail)
}

// recovery is the command that previews the same request again.
func (p *day2Plan) recovery() clierr.Command {
	switch p.op.kind {
	case clusterops.OperationSnapshot:
		return saveEtcdSnapshotRecovery(p.target.name, p.target.ns)
	case clusterops.OperationRestore:
		return restoreEtcdSnapshotRecovery(p.target.name, p.target.ns, p.snapshot, p.mode)
	}
	panic("day2 operation " + string(p.op.kind) + " has no recovery command")
}

// failed makes a committed error of the wait: the request was accepted.
func (p *day2Plan) failed(recovery clierr.Command, msg string) *clierr.Error {
	return committedWaitError("provisioning-cluster", p.target.name, recovery, msg)
}

// day2End is how the wait ended.
type day2End struct {
	phase   string
	refused bool
}

// wrap makes the executor that sends the update, then reports it or waits
// until Rancher reports the operation done.
func (p *day2Plan) wrap(exec runtime.ExecuteFunc) runtime.ExecuteFunc {
	t := p.target
	return func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		data := map[string]any{
			"cluster":    t.name,
			"namespace":  t.ns,
			"field":      "spec.rkeConfig." + p.op.field,
			"generation": p.generation,
			"waited":     false,
			"written":    !t.waiting,
		}
		if t.clusterID != "" {
			data["clusterId"] = t.clusterID
		}
		maps.Copy(data, p.data)
		set := func(text runtime.Result, warnings ...string) runtime.Result {
			res.Text, res.StyledText, res.Data = text.Text, text.StyledText, data
			res.Warnings = append(res.Warnings, warnings...)
			return res
		}
		requested := runtime.MutationText("cluster", t.name, p.op.requested)
		if t.waiting {
			requested = runtime.MutationText("cluster", t.name, fmt.Sprintf("changed nothing; the %s already requested is in progress", p.op.kind))
		}
		if !p.wait.On {
			return set(requested), nil
		}
		if t.controlPlane == nil {
			return set(requested, fmt.Sprintf("cannot read the RKEControlPlane of cluster %s; the %s was requested and this CLI cannot wait for it", t.name, p.op.kind)), nil
		}

		end, werr := p.waitDone(ctx)
		if werr != nil {
			return runtime.Result{}, werr
		}
		if end.refused {
			return set(requested, fmt.Sprintf("your access to the RKEControlPlane of cluster %s ended after the %s request; the %s was requested and not confirmed", t.name, p.op.kind, p.op.kind)), nil
		}
		data["waited"] = true
		data["phase"] = end.phase
		warnings := p.reportAfterDone(ctx, data)
		doneText := p.op.done
		if p.done != "" {
			doneText = p.done
		}
		done := runtime.MutationText("cluster", t.name, doneText)
		if t.waiting {
			first := fmt.Sprintf("cluster/%s changed nothing; waiting for the %s already requested", t.name, p.op.kind)
			text := clierr.Plain(first + "\n").Append(clierr.Plain(done.Text).TrimSuffix("\n")).AppendString("\n")
			done = runtime.Result{Text: text.Marked(), StyledText: runtime.StyleEveryLineBoldOf(text)}
		}
		return set(done, warnings...), nil
	}
}

// waitDone waits until Rancher reports the operation done: the status copy of
// the request equals the generation written and the phase is the operation's
// done phase. A phase Failed with that copy ends the wait with an error. The
// phase of an earlier generation does not end the wait. A status copy of a
// later generation than the one written means another write replaced the
// request: Rancher runs the later one and may never run this one. The wait then
// ends with an error at once.
func (p *day2Plan) waitDone(ctx context.Context) (day2End, error) {
	t := p.target
	var end day2End
	recovery := p.recovery()
	subject := fmt.Sprintf("%s of cluster %s", p.op.kind, t.name)
	failed := func(msg string) *clierr.Error { return p.failed(recovery, msg) }
	err := engine.WaitObject(ctx, t.client, engine.WaitParams{
		GVR:         rkeControlPlaneGVR,
		Namespace:   t.ns,
		Name:        t.name,
		Timeout:     p.wait.Timeout,
		Interval:    clusterWaitInterval,
		SpacingFrom: engine.SpacingFromReturn,
		Observe: func(_ context.Context, obj *unstructured.Unstructured, _ engine.WaitSource) (engine.WaitOutcome, error) {
			end.phase, _, _ = unstructured.NestedString(obj.Object, "status", p.op.phaseField)
			request, found, _ := unstructured.NestedMap(obj.Object, "status", p.op.field)
			if !found {
				return engine.WaitContinue, nil
			}
			generation, _, _ := unstructured.NestedFieldNoCopy(request, "generation")
			seen := numberOf(generation)
			if seen > p.generation {
				return engine.WaitContinue, failed(fmt.Sprintf("%s: Rancher reports generation %d of the request, later than generation %d, which this run waits for; last state: %s; the %s this run asked for may not have been taken",
					subject, seen, p.generation, p.lastState(obj), p.op.kind))
			}
			if seen != p.generation {
				return engine.WaitContinue, nil
			}
			switch end.phase {
			case p.op.doneAt:
				return engine.WaitDone, nil
			case phaseFailed:
				reason, message := readyCondition(obj)
				text := subject + " failed: "
				switch {
				case reason != "" && message != "":
					text += reason + ": " + message
				case reason != "":
					text += reason + ": Rancher reported no message"
				case message != "":
					text += message
				default:
					text += "Rancher reported no message"
				}
				return engine.WaitContinue, failed(text)
			}
			return engine.WaitContinue, nil
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
			return engine.CommittedPartialSuccess(err, clierr.CommittedContextDetails{
				Committed: true, ResourceKind: "provisioning-cluster", ResourceName: t.name, Recovery: recovery.Field(),
			}).WithRetryable(false).WithHintCommand(recovery)
		},
		OnCancel: func(context.Context, *unstructured.Unstructured, *clierr.Error) error {
			return failed(fmt.Sprintf("stopped waiting for the %s; it was requested and goes on", subject))
		},
		OnTimeout: func(object *unstructured.Unstructured, _ *clierr.Error) error {
			if p.op.kind == clusterops.OperationRestore {
				return failed(fmt.Sprintf("timed out after %s waiting for the %s of cluster %s; it was requested and goes on; last state: %s; %s",
					p.wait.TimeoutText, p.op.kind, t.name, p.lastState(object), restoreInProgressAdvice))
			}
			return failed(fmt.Sprintf("timed out after %s waiting for the %s of cluster %s; it was requested and goes on; last state: %s; to wait again, run the same command: while the %s is in progress it changes nothing and waits; once the %s has ended it requests a new one, and --dry-run previews that request",
				p.wait.TimeoutText, p.op.kind, t.name, p.lastState(object), p.op.kind, p.op.kind))
		},
	})
	if clierr.IsNotFound(err) {
		removed := clierr.NewCommand("get provisioning-clusters")
		if t.ns != clusterNamespace {
			removed = removed.FlagValue("--namespace", t.ns)
		}
		return day2End{}, p.failed(removed, fmt.Sprintf("cluster %s was deleted during the wait", t.name))
	}
	return end, err
}

// lastState is the last state the wait saw: the phase and the message of the
// RKEControlPlane's Ready condition.
func (p *day2Plan) lastState(obj *unstructured.Unstructured) string {
	if obj == nil {
		return "no state seen yet"
	}
	phase, _, _ := unstructured.NestedString(obj.Object, p.statusPath()...)
	if phase == "" {
		phase = notStartedPhase
	}
	if _, message := readyCondition(obj); message != "" {
		return phase + ", " + message
	}
	return phase
}

func (p *day2Plan) statusPath() []string { return []string{"status", p.op.phaseField} }

// readyCondition is the reason and the message of the RKEControlPlane's
// Ready condition, empty when it has none.
func readyCondition(obj *unstructured.Unstructured) (reason, message string) {
	engine.EachCondition(obj.Object, func(typ, _, r, m string) bool {
		if typ != "Ready" {
			return true
		}
		reason, message = r, m
		return false
	})
	return reason, message
}
