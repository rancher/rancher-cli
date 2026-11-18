package resources

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FlagDeleteNamespaces is the `delete project` flag that also deletes the
// project's namespaces.
const FlagDeleteNamespaces = "delete-namespaces"

const (
	// creatorIDAnnotation marks a namespace Rancher deletes with its project;
	// Rancher keeps every namespace without it.
	creatorIDAnnotation = "field.cattle.io/creatorId"

	// projectRetries is how many times the project delete is retried after
	// a conflict that changed nothing but the project's used limit.
	projectRetries = 3

	namespaceKind = "namespace"
	projectKind   = "project"

	reasonKubernetes = "Kubernetes never deletes it"
	reasonLocal      = "Rancher never deletes it on the local cluster"

	warningDeleteNamespaces = `deleting this project and its namespaces deletes everything in the namespaces marked "will be deleted"; namespaces marked "not deleted" are kept; the project's role bindings and project-scoped secrets are deleted`
)

// DeleteProjectPartialFailure is the details payload of a `delete project`
// run that stopped after it started: every namespace and the project with
// what happened to each. When the server accepted the project's delete and
// the wait after it ended with an error, the committed details sit beside
// the objects; nil otherwise.
type DeleteProjectPartialFailure struct {
	Objects []safety.Object `json:"objects"`
	*clierr.CommittedContextDetails
}

// TextDetails lists each outcome with the names that have it, then the
// committed lines.
func (p DeleteProjectPartialFailure) TextDetails() []clierr.DetailLine {
	lines := runtime.ObjectDetailLines(p.Objects, "")
	if p.CommittedContextDetails != nil {
		lines = append(lines, p.CommittedContextDetails.TextDetails()...)
	}
	return lines
}

// MapText maps the message of every object, which can hold the text of the
// error a delete failed with and so a remedy command, and the recovery of
// the committed details. The names are the server's, the kind, outcome and
// code are the CLI's own words. The committed details are mapped on a copy.
func (p DeleteProjectPartialFailure) MapText(f func(string) string) clierr.TextDetails {
	objects := make([]safety.Object, len(p.Objects))
	for i, o := range p.Objects {
		o.Message = f(o.Message)
		objects[i] = o
	}
	p.Objects = objects
	if p.CommittedContextDetails != nil {
		if mapped, ok := p.CommittedContextDetails.MapText(f).(clierr.CommittedContextDetails); ok {
			p.CommittedContextDetails = &mapped
		}
	}
	return p
}

type deleteProjectCmd struct {
	k       kinds.Kind
	inner   deleteCmd
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewDeleteProject builds `delete project <id> --cluster <c>`: the generic
// delete plus a read of the project's namespaces. A plain run lists what
// Rancher deletes and keeps; --delete-namespaces deletes the namespaces
// itself, one at a time, before the project.
func NewDeleteProject(k kinds.Kind, clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	bare := k
	bare.DeleteCascadeWarning = ""
	return &deleteProjectCmd{k: k, inner: deleteCmd{k: bare, clients: clients}, clients: clients, roots: roots}
}

func (c *deleteProjectCmd) Describe() runtime.Descriptor {
	d := c.inner.Describe()
	d.Flags = append(d.Flags, runtime.ArgSpec{
		Name: FlagDeleteNamespaces,
		Type: "bool",
		Desc: "also delete the project's namespaces and everything in them",
	})
	return d
}

// namespaceRead is the project's namespaces as one run read them.
type namespaceRead struct {
	downstream engine.ClusterClient
	members    []unstructured.Unstructured
	// viaSteve is true when the namespace LIST was refused and steve's list
	// responded instead.
	viaSteve bool
	err      error
}

func (c *deleteProjectCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	w, err := ParseDeleteWait(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	local, ns, name, project, effectCluster, err := c.inner.fetch(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if w.mode == waitOn {
		if w, err = waitForBackingNamespace(ctx, local, project, w); err != nil {
			return safety.Effect{}, nil, err
		}
	}
	eff, innerExec, err := PlanFromFetched(ctx, local, c.inner.k, ns, name, project, effectCluster, w)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	rd := c.readNamespaces(ctx, effectCluster, name)
	if in.Bool(FlagDeleteNamespaces) {
		return c.planDeleteNamespaces(ctx, eff, rd, local, project, newDeleteTarget(c.inner.k, ns, name, project, effectCluster), w)
	}
	return c.planKeep(ctx, eff, innerExec, rd)
}

// backingNamespace is the namespace on the local cluster that holds the
// project's objects, as Rancher names it: status.backingNamespace, else the
// project id.
func backingNamespace(project *unstructured.Unstructured) string {
	if bn, _, _ := unstructured.NestedString(project.Object, "status", "backingNamespace"); bn != "" {
		return bn
	}
	return project.GetName()
}

// waitForBackingNamespace reads the project's backing namespace before the
// delete, on the local cluster where Rancher deletes it with the project:
// one the caller can read is waited for too; one the caller cannot read is
// not, with a warning; one that is not there needs no wait. Any other
// response fails the Plan before anything that changes the server is sent.
func waitForBackingNamespace(ctx context.Context, local engine.ClusterClient, project *unstructured.Unstructured, w DeleteWait) (DeleteWait, error) {
	nsKind, _ := kinds.Lookup(namespaceKind)
	name := backingNamespace(project)
	obj, err := local.Get(ctx, nsKind.GVR, "", name)
	switch {
	case err == nil:
		return w.AlsoWaitFor(deleteTarget{gvr: nsKind.GVR, name: name, uid: string(obj.GetUID()), kind: namespaceKind,
			recovery: clierr.NewCommand("get").Word(nsKind.Word()).Arg(name).FlagValue("--cluster", engine.LocalCluster), readBefore: true}), nil
	case readRefused(err):
		return w.NotWaitedFor(fmt.Sprintf("you cannot get namespace %s (the project's backing namespace on the local cluster); the command does not wait for it", name)), nil
	case clierr.IsNotFound(err):
		return w, nil
	}
	return w, err
}

// readNamespaces reads the project's member namespaces on the cluster the
// project belongs to.
func (c *deleteProjectCmd) readNamespaces(ctx context.Context, cluster, project string) namespaceRead {
	downstream, err := c.clients.Cluster(ctx, cluster)
	if err != nil {
		return namespaceRead{err: err}
	}
	items, viaSteve, err := steve.ProjectNamespaceObjects(ctx, downstream, steve.RootFactoryGetter(c.roots), cluster, project)
	if err != nil {
		return namespaceRead{downstream: downstream, err: err}
	}
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	return namespaceRead{downstream: downstream, members: items, viaSteve: viaSteve}
}

func (rd namespaceRead) terminating(ns *unstructured.Unstructured) bool {
	phase, _, _ := unstructured.NestedString(ns.Object, "status", "phase")
	return ns.GetDeletionTimestamp() != nil || phase == "Terminating"
}

// unknown reports whether the run cannot tell what Rancher does with ns:
// steve responded with the few fields it shows for a namespace the caller
// cannot get.
func (rd namespaceRead) unknown(ns *unstructured.Unstructured) bool {
	return rd.viaSteve && !steve.ReadableInFull(ns)
}

// planKeep is the run without the flag: the namespaces are listed with what
// Rancher will do to each; the CLI deletes none.
func (c *deleteProjectCmd) planKeep(ctx context.Context, eff safety.Effect, innerExec runtime.ExecuteFunc, rd namespaceRead) (safety.Effect, runtime.ExecuteFunc, error) {
	eff.Warnings = append(eff.Warnings, c.k.DeleteCascadeWarning)
	if rd.err != nil {
		eff.Warnings = append(eff.Warnings, fmt.Sprintf(
			"could not read the project's namespaces: %s; Rancher deletes those with the field.cattle.io/creatorId annotation and keeps the rest",
			clierr.From(rd.err).Message))
		return eff, c.withObjects(innerExec, nil), nil
	}
	objs := make([]safety.Object, 0, len(rd.members))
	var kept []string
	for i := range rd.members {
		ns := &rd.members[i]
		o := safety.Object{Kind: namespaceKind, Name: ns.GetName()}
		_, hasCreator := ns.GetAnnotations()[creatorIDAnnotation]
		switch {
		case rd.terminating(ns):
			o.Outcome = safety.OutcomeBeingDeleted
		case protectedReason(o.Name, eff.Cluster) != "":
			// Kubernetes and Rancher refuse to delete these, whatever
			// annotations they carry.
			o.Outcome = safety.OutcomeRancherKeeps
			kept = append(kept, o.Name)
		case rd.unknown(ns):
			o.Outcome = safety.OutcomeUnknown
		case hasCreator:
			o.Outcome = safety.OutcomeRancherDeletes
		default:
			o.Outcome = safety.OutcomeRancherKeeps
			kept = append(kept, o.Name)
		}
		objs = append(objs, o)
	}
	eff.Objects = objs
	if len(kept) > 0 && c.cannotSeeOutsideProjects(ctx, rd.downstream) {
		eff.Warnings = append(eff.Warnings, fmt.Sprintf(
			"you cannot get namespaces outside projects; after this delete you will not be able to get the namespaces Rancher keeps: %s",
			runtime.NameList(kept)))
	}
	return eff, c.withObjects(innerExec, objs), nil
}

// withObjects wraps the generic delete's executor so the result lists
// objs: the namespaces in text and the "objects" array in the data. The
// result keeps the wait's outcome and warnings; an error after the server
// accepted the delete keeps its committed details beside objs.
func (c *deleteProjectCmd) withObjects(exec runtime.ExecuteFunc, objs []safety.Object) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, withCommittedObjects(err, objs)
		}
		data, _ := res.Data.(map[string]any)
		name, _ := data["deleted"].(string)
		waited, _ := data["waited"].(bool)
		out := projectResult(c.k, name, objs, waited)
		out.Warnings = res.Warnings
		return out, nil
	}
}

// withCommittedObjects returns an error that carries committed details with
// objs beside them; any other error (a delete the server refused) is
// returned as it is.
func withCommittedObjects(err error, objs []safety.Object) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		return err
	}
	committed, ok := ce.Details.(clierr.CommittedContextDetails)
	if !ok {
		return err
	}
	if objs == nil {
		objs = []safety.Object{}
	}
	return engine.CommittedPartialSuccess(err, committed).WithDetails(DeleteProjectPartialFailure{Objects: objs, CommittedContextDetails: &committed})
}

// projectResult is the result of a project delete: the project's line, the
// namespace lines, and the objects in the data. waited says whether the
// command saw the project and its backing namespace deleted.
func projectResult(k kinds.Kind, name string, objs []safety.Object, waited bool) runtime.Result {
	if objs == nil {
		objs = []safety.Object{}
	}
	verb := "deletion started"
	if waited {
		verb = "deleted"
	}
	res := runtime.MutationText(k.Name, name, verb, runtime.ObjectLines(objs, projectKind)...)
	res.Kind = resultKind(k) + "Delete"
	res.Data = map[string]any{"deleted": name, "waited": waited, "objects": objs}
	return res
}

// cannotSeeOutsideProjects asks the cluster whether the caller may get
// namespaces with no name given: a grant by name, which is all a project
// role carries, does not allow that. The response is advisory: a failed
// request returns false, and the caller adds no warning.
func (c *deleteProjectCmd) cannotSeeOutsideProjects(ctx context.Context, downstream engine.ClusterClient) bool {
	gvk, ok := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
	if !ok {
		return false
	}
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind,
		"spec": map[string]any{"resourceAttributes": map[string]any{
			"verb": "get", "group": "", "resource": "namespaces",
		}},
	}}
	resp, err := downstream.PostReview(ctx, engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: review})
	if err != nil || resp.Status < 200 || resp.Status > 299 || resp.Object == nil {
		return false
	}
	allowed, found, _ := unstructured.NestedBool(resp.Object.Object, "status", "allowed")
	return found && !allowed
}

// protectedReason is why nobody may delete the namespace, or "".
func protectedReason(name, cluster string) string {
	switch name {
	case "default", "kube-system", "kube-public":
		return reasonKubernetes
	case "local", "fleet-local":
		if cluster == engine.LocalCluster {
			return reasonLocal
		}
	}
	return ""
}

// readFailure is the refusal for a failed namespace read.
func readFailure(err error, project, cluster string) error {
	ce := clierr.From(err)
	hint := deleteProjectHint(project, cluster)
	return clierr.New(ce.Code, fmt.Sprintf(
		"could not read the project's namespaces: %s; --delete-namespaces needs the full list before it deletes anything%s", ce.Message, deleteProjectHintNote(hint))).
		WithRetryable(ce.Retryable).
		WithHintCommand(hint).
		WithCause(err)
}

// deleteProjectHintNote says what the hint of a refusal does; a hint left out
// has nothing to say it about.
func deleteProjectHintNote(hint clierr.Command) string {
	if !hint.OK() {
		return ""
	}
	return "; the hint previews deleting the project without its namespaces: add --apply --yes after reading the preview"
}

func deleteProjectHint(project, cluster string) clierr.Command {
	return clierr.NewCommand("delete project").Arg(project).FlagValue("--cluster", cluster).Flag("--dry-run")
}

// planDeleteNamespaces is the run with --delete-namespaces: every namespace
// of the project that the CLI may delete is dry-run deleted with its own
// resourceVersion; the executor deletes them one at a time and the project
// after them. When w turns the wait on, it first waits until every namespace
// it deleted and every namespace of the project that was already being
// deleted is gone, and afterwards waits for the project and its backing
// namespace.
// projectTarget names the project for the namespace waits: their errors name
// it and give its get command.
func (c *deleteProjectCmd) planDeleteNamespaces(ctx context.Context, eff safety.Effect, rd namespaceRead, local engine.ClusterClient, project *unstructured.Unstructured, projectTarget deleteTarget, w DeleteWait) (safety.Effect, runtime.ExecuteFunc, error) {
	name, cluster := eff.DisplayName, eff.Cluster
	if rd.err != nil {
		return safety.Effect{}, nil, readFailure(rd.err, name, cluster)
	}
	var objs []safety.Object
	var toDelete []unstructured.Unstructured
	nsKind, _ := kinds.Lookup(namespaceKind)
	waits := map[string]deleteTarget{}
	for i := range rd.members {
		ns := &rd.members[i]
		nsName := ns.GetName()
		o := safety.Object{Kind: namespaceKind, Name: nsName}
		switch reason := protectedReason(nsName, cluster); {
		case rd.terminating(ns):
			o.Outcome = safety.OutcomeBeingDeleted
			waits[nsName] = namespaceTarget(nsKind, ns, projectTarget)
		case reason != "":
			o.Outcome, o.Message = safety.OutcomeNotDeleted, reason
		case rd.unknown(ns):
			hint := deleteProjectHint(name, cluster)
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"cannot read namespace %s in full; --delete-namespaces needs to read each namespace it deletes in full%s", nsName, deleteProjectHintNote(hint))).
				WithHintCommand(hint)
		default:
			o.Outcome = safety.OutcomeWillBeDeleted
			toDelete = append(toDelete, *ns)
			waits[nsName] = namespaceTarget(nsKind, ns, projectTarget)
		}
		objs = append(objs, o)
	}
	for i := range toDelete {
		nsName := toDelete[i].GetName()
		if err := rd.downstream.Delete(ctx, nsKind.GVR, "", nsName, toDelete[i].GetResourceVersion(), true); err != nil {
			ce := clierr.From(err)
			return safety.Effect{}, nil, clierr.New(ce.Code, fmt.Sprintf(
				"deleting namespace %s was refused: %s; nothing was deleted", nsName, ce.Message)).
				WithRetryable(ce.Retryable).WithCause(err)
		}
	}
	objs = append(objs, safety.Object{Kind: projectKind, Name: name, Outcome: safety.OutcomeWillBeDeleted})
	eff.Objects = objs
	eff.Warnings = append(eff.Warnings, warningDeleteNamespaces)
	return eff, c.execDeleteNamespaces(eff, namespaceRun{
		downstream: rd.downstream, local: local, ns: projectTarget.ns, project: project, toDelete: toDelete, waits: waits, w: w,
	}), nil
}

// namespaceRun is what the executor of a run with --delete-namespaces needs
// beyond the Effect: the client of the cluster the namespaces are on, the
// client of the local cluster, the project as the Plan read it and its
// metadata.namespace, the namespaces to delete, the wait for each namespace
// of the project that the command deletes or that is already being deleted
// (by namespace name), and how the run waits.
type namespaceRun struct {
	downstream, local engine.ClusterClient
	ns                string
	project           *unstructured.Unstructured
	toDelete          []unstructured.Unstructured
	waits             map[string]deleteTarget
	w                 DeleteWait
}

// namespaceTarget is the wait for a namespace of the project that the command
// read before it deletes the project. An error that ends the wait names the
// project, which the command does not delete, and its get command.
func namespaceTarget(nsKind kinds.Kind, ns *unstructured.Unstructured, project deleteTarget) deleteTarget {
	return deleteTarget{gvr: nsKind.GVR, name: ns.GetName(), uid: string(ns.GetUID()), kind: namespaceKind,
		recovery: project.recovery, readBefore: true, subjectKind: project.kind, subjectName: project.name}
}

// waitKey names an object on a cluster.
type waitKey struct {
	cluster string
	gvr     schema.GroupVersionResource
	ns      string
	name    string
}

// runWaits is what every wait of one project delete shares: the one time
// limit, which starts when the server accepts the first delete of the run,
// and the objects already waited for.
type runWaits struct {
	timeout  time.Duration
	deadline time.Time
	waited   map[waitKey]bool
}

// start sets the time limit unless it is set.
func (r *runWaits) start() {
	if r.deadline.IsZero() {
		r.deadline = time.Now().Add(r.timeout)
	}
}

// mark records t, an object on cluster, as waited for.
func (r *runWaits) mark(cluster string, t deleteTarget) {
	r.waited[waitKey{cluster, t.gvr, t.ns, t.name}] = true
}

// seen reports whether t, an object on cluster, was waited for.
func (r *runWaits) seen(cluster string, t deleteTarget) bool {
	return r.waited[waitKey{cluster, t.gvr, t.ns, t.name}]
}

// execDeleteNamespaces deletes the namespaces one at a time in name order,
// each with the resourceVersion it was read with, then, with the wait on,
// waits until each namespace it deleted and each namespace of the project
// that was being deleted is gone, then deletes the project and waits for it
// and its backing namespace. All the waits share one time limit. A failed
// namespace delete or a wait for a namespace that ends with an error stops
// the run and the project is not deleted; a wait that stops because the
// server refused a read gives a warning and the run goes on.
func (c *deleteProjectCmd) execDeleteNamespaces(eff safety.Effect, run namespaceRun) runtime.ExecuteFunc {
	rvByName := make(map[string]string, len(run.toDelete))
	for i := range run.toDelete {
		rvByName[run.toDelete[i].GetName()] = run.toDelete[i].GetResourceVersion()
	}
	return func(ctx context.Context) (runtime.Result, error) {
		objs := slices.Clone(eff.Objects)
		nsKind, _ := kinds.Lookup(namespaceKind)
		rw := &runWaits{timeout: run.w.timeout, waited: map[waitKey]bool{}}
		var failure error
		for i := range objs {
			o := &objs[i]
			if failure != nil {
				if o.Outcome == safety.OutcomeWillBeDeleted {
					o.Outcome = safety.OutcomeNotAttempted
				}
				continue
			}
			if o.Kind != namespaceKind || o.Outcome != safety.OutcomeWillBeDeleted {
				continue
			}
			err := run.downstream.Delete(ctx, nsKind.GVR, "", o.Name, rvByName[o.Name], false)
			switch {
			case err == nil:
				o.Outcome = safety.OutcomeDeletionStarted
				rw.start()
			case clierr.IsNotFound(err):
				o.Outcome = safety.OutcomeAlreadyGone
			default:
				ce := clierr.From(err)
				o.Outcome, o.Code, o.Message = safety.OutcomeFailed, ce.Code, ce.Message
				failure = ce
			}
		}
		if failure != nil {
			return runtime.Result{}, namespaceFailure(failure, objs)
		}
		end := deleteEnd{waited: run.w.mode == waitOn}
		if run.w.mode == waitOn {
			for i := range objs {
				o := &objs[i]
				if o.Kind != namespaceKind || (o.Outcome != safety.OutcomeDeletionStarted && o.Outcome != safety.OutcomeBeingDeleted) {
					continue
				}
				rw.start()
				nsEnd, err := waitDeleted(ctx, run.downstream, []deleteTarget{run.waits[o.Name]}, rw.deadline, run.w.timeoutText)
				if err != nil {
					return runtime.Result{}, namespaceWaitFailure(err, objs)
				}
				rw.mark(eff.Cluster, run.waits[o.Name])
				if nsEnd.waited && o.Outcome == safety.OutcomeDeletionStarted {
					o.Outcome = safety.OutcomeDeleted
				}
				end.waited = end.waited && nsEnd.waited
				end.warnings = append(end.warnings, nsEnd.warnings...)
			}
		}
		projectEnd, err := c.deleteProject(ctx, run.local, eff, run.ns, run.project, &objs[len(objs)-1], run.w, rw)
		if err != nil {
			return runtime.Result{}, afterProjectDelete(err, objs)
		}
		end.waited = end.waited && projectEnd.waited
		end.warnings = append(end.warnings, projectEnd.warnings...)
		if len(run.w.notWaited) > 0 {
			end.waited = false
			end.warnings = append(slices.Clip(run.w.notWaited), end.warnings...)
		}
		res := projectResult(c.k, eff.DisplayName, objs, end.waited)
		res.Warnings = end.warnings
		return res, nil
	}
}

// namespaceWaitFailure is the error for a wait for a namespace that ended
// with an error: the wait's error and code; its details name the project that
// was not deleted and list the outcome of every object.
func namespaceWaitFailure(err error, objs []safety.Object) error {
	ce := clierr.From(err)
	objs[len(objs)-1].Outcome = safety.OutcomeNotAttempted
	return withCommittedObjects(ce.WithMessage(ce.Message+"; the project was kept; run the same command again, which reads the project's current namespaces"), objs)
}

// afterProjectDelete attaches every object's outcome to an error of the
// project's delete or of the wait after it, keeping its code, message and
// committed details.
func afterProjectDelete(err error, objs []safety.Object) error {
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		if _, committed := ce.Details.(clierr.CommittedContextDetails); committed {
			return withCommittedObjects(err, objs)
		}
	}
	return withProjectOutcome(err, objs)
}

// namespaceFailure is the error for a namespace delete that failed: the
// failing call's code, every object's outcome in the details.
func namespaceFailure(failure error, objs []safety.Object) error {
	ce := clierr.From(failure)
	var started, notAttempted int
	var failed string
	for _, o := range objs {
		switch {
		case o.Kind != namespaceKind:
		case o.Outcome == safety.OutcomeDeletionStarted:
			started++
		case o.Outcome == safety.OutcomeNotAttempted:
			notAttempted++
		case o.Outcome == safety.OutcomeFailed:
			failed = o.Name
		}
	}
	return clierr.New(ce.Code, fmt.Sprintf(
		"delete project partially applied: deletion started for %d namespaces, failed on %s, %d not attempted; the project was kept; run the same command again, which reads the project's current namespaces",
		started, failed, notAttempted)).
		WithRetryable(ce.Retryable).
		WithDetails(DeleteProjectPartialFailure{Objects: objs})
}

// withProjectOutcome attaches every object's outcome to a project delete
// failure, keeping its code and message.
func withProjectOutcome(err error, objs []safety.Object) error {
	ce := clierr.From(err)
	return ce.WithDetails(DeleteProjectPartialFailure{Objects: objs})
}

// deleteProject deletes the project once its namespaces are deleted, then
// waits for it and for the other objects w carries (its backing namespace),
// when the wait is on: the project's outcome is `deleted` when the command saw
// the project gone. The namespace deletes can make Rancher update the project
// (its used quota), which moves its resourceVersion: a conflict is retried
// with the newer resourceVersion when nothing but the used quota and the
// status changed since the project was read, and is otherwise the named
// partial state. ns is the project's metadata.namespace, which is its cluster
// id; the wait reads the project there. item is the project's item in the
// result objects.
func (c *deleteProjectCmd) deleteProject(ctx context.Context, local engine.ClusterClient, eff safety.Effect, ns string, read *unstructured.Unstructured, item *safety.Object, w DeleteWait, rw *runWaits) (deleteEnd, error) {
	name, cluster, rv := eff.DisplayName, eff.Cluster, eff.ResourceVersion
	for attempt := 0; ; attempt++ {
		err := local.Delete(ctx, c.k.GVR, cluster, name, rv, false)
		if err == nil {
			item.Outcome = safety.OutcomeDeletionStarted
			rw.start()
			if w.mode != waitOn {
				return deleteEnd{}, nil
			}
			end, werr := waitDeleted(ctx, local, []deleteTarget{newDeleteTarget(c.inner.k, ns, name, read, cluster)}, rw.deadline, w.timeoutText)
			if werr != nil {
				return deleteEnd{}, werr
			}
			if end.waited {
				item.Outcome = safety.OutcomeDeleted
			}
			for _, t := range w.also {
				if rw.seen(engine.LocalCluster, t) {
					continue
				}
				more, werr := waitDeleted(ctx, local, []deleteTarget{t}, rw.deadline, w.timeoutText)
				if werr != nil {
					return deleteEnd{}, werr
				}
				end.waited = end.waited && more.waited
				end.warnings = append(end.warnings, more.warnings...)
			}
			return end, nil
		}
		if ce := clierr.From(err); ce.Code != clierr.CodeConflict || !ce.Retryable {
			return deleteEnd{}, c.projectFailed(item, err)
		}
		if attempt == projectRetries {
			return deleteEnd{}, c.projectChanged(item, err)
		}
		current, gerr := local.Get(ctx, c.k.GVR, cluster, name)
		if gerr != nil {
			return deleteEnd{}, c.projectFailed(item, gerr)
		}
		if projectSpecChanged(read, current) {
			return deleteEnd{}, c.projectChanged(item, err)
		}
		rv = current.GetResourceVersion()
	}
}

func (c *deleteProjectCmd) projectFailed(item *safety.Object, err error) error {
	ce := clierr.From(err)
	item.Outcome, item.Code, item.Message = safety.OutcomeFailed, ce.Code, ce.Message
	return ce
}

func (c *deleteProjectCmd) projectChanged(item *safety.Object, err error) error {
	ce := clierr.From(err)
	item.Outcome, item.Code, item.Message = safety.OutcomeFailed, ce.Code, ce.Message
	return clierr.New(clierr.CodeConflict, fmt.Sprintf(
		"project %s changed after it was read; its namespaces were deleted or are being deleted and the project was kept; run the same command again",
		item.Name)).WithCause(err)
}

// projectSpecChanged reports whether the project differs from the one the
// run read in anything but its status, its used quota and the bookkeeping
// the server keeps on every write.
func projectSpecChanged(before, after *unstructured.Unstructured) bool {
	strip := func(o *unstructured.Unstructured) map[string]any {
		m := o.DeepCopy().Object
		unstructured.RemoveNestedField(m, "status")
		unstructured.RemoveNestedField(m, "spec", "resourceQuota", "usedLimit")
		for _, f := range []string{"resourceVersion", "generation", "managedFields"} {
			unstructured.RemoveNestedField(m, "metadata", f)
		}
		return m
	}
	return !reflect.DeepEqual(strip(before), strip(after))
}

var _ runtime.MutatingCommand = (*deleteProjectCmd)(nil)
