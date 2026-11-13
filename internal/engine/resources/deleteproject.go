package resources

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// DeleteProjectPartialFailure is the details payload of a `delete project
// --delete-namespaces` run that stopped after it started: every namespace
// and the project with what happened to each.
type DeleteProjectPartialFailure struct {
	Objects []safety.Object `json:"objects"`
}

// TextDetails lists each outcome with the names that have it.
func (p DeleteProjectPartialFailure) TextDetails() []clierr.DetailLine {
	return runtime.ObjectDetailLines(p.Objects, "")
}

// MapText maps the message of every object, which can hold the text of the
// error a delete failed with and so a remedy command. The names are the
// server's, the kind, outcome and code are the CLI's own words.
func (p DeleteProjectPartialFailure) MapText(f func(string) string) clierr.TextDetails {
	objects := make([]safety.Object, len(p.Objects))
	for i, o := range p.Objects {
		o.Message = f(o.Message)
		objects[i] = o
	}
	p.Objects = objects
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
	// answered.
	viaSteve bool
	err      error
}

func (c *deleteProjectCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	local, ns, name, project, effectCluster, err := c.inner.fetch(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff, innerExec, err := PlanFromFetched(ctx, local, c.inner.k, ns, name, project, effectCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	rd := c.readNamespaces(ctx, effectCluster, name)
	if in.Bool(FlagDeleteNamespaces) {
		return c.planDeleteNamespaces(ctx, eff, rd, local, project)
	}
	return c.planKeep(ctx, eff, innerExec, rd)
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
// steve answered with the few fields it shows for a namespace the caller
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
// objs: the namespaces in text and the "objects" array in the data.
func (c *deleteProjectCmd) withObjects(exec runtime.ExecuteFunc, objs []safety.Object) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		data, _ := res.Data.(map[string]any)
		name, _ := data["deleted"].(string)
		return projectResult(c.k, name, objs), nil
	}
}

// projectResult is the result of a project delete: the project's line, the
// namespace lines, and the objects in the data.
func projectResult(k kinds.Kind, name string, objs []safety.Object) runtime.Result {
	if objs == nil {
		objs = []safety.Object{}
	}
	res := runtime.MutationText(k.Name, name, "deleted", runtime.ObjectLines(objs, projectKind)...)
	res.Kind = resultKind(k) + "Delete"
	res.Data = map[string]any{"deleted": name, "objects": objs}
	return res
}

// cannotSeeOutsideProjects asks the cluster whether the caller may get
// namespaces with no name given: a grant by name, which is all a project
// role carries, does not answer yes. The answer is advisory: a failed
// request is no.
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

// planDeleteNamespaces is the run with --delete-namespaces: every member
// namespace the CLI may delete is dry-run deleted with its own
// resourceVersion; the executor deletes them one at a time, then the
// project.
func (c *deleteProjectCmd) planDeleteNamespaces(ctx context.Context, eff safety.Effect, rd namespaceRead, local engine.ClusterClient, project *unstructured.Unstructured) (safety.Effect, runtime.ExecuteFunc, error) {
	name, cluster := eff.DisplayName, eff.Cluster
	if rd.err != nil {
		return safety.Effect{}, nil, readFailure(rd.err, name, cluster)
	}
	var objs []safety.Object
	var toDelete []unstructured.Unstructured
	for i := range rd.members {
		ns := &rd.members[i]
		nsName := ns.GetName()
		o := safety.Object{Kind: namespaceKind, Name: nsName}
		switch reason := protectedReason(nsName, cluster); {
		case rd.terminating(ns):
			o.Outcome = safety.OutcomeBeingDeleted
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
		}
		objs = append(objs, o)
	}
	nsKind, _ := kinds.Lookup(namespaceKind)
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
	return eff, c.execDeleteNamespaces(eff, rd.downstream, local, project, toDelete), nil
}

// execDeleteNamespaces deletes the namespaces one at a time in name order,
// each with the resourceVersion it was read with, then the project. A
// failed namespace delete stops the run and keeps the project.
func (c *deleteProjectCmd) execDeleteNamespaces(eff safety.Effect, downstream, local engine.ClusterClient, project *unstructured.Unstructured, toDelete []unstructured.Unstructured) runtime.ExecuteFunc {
	rvByName := make(map[string]string, len(toDelete))
	for i := range toDelete {
		rvByName[toDelete[i].GetName()] = toDelete[i].GetResourceVersion()
	}
	return func(ctx context.Context) (runtime.Result, error) {
		objs := slices.Clone(eff.Objects)
		nsKind, _ := kinds.Lookup(namespaceKind)
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
			err := downstream.Delete(ctx, nsKind.GVR, "", o.Name, rvByName[o.Name], false)
			switch {
			case err == nil:
				o.Outcome = safety.OutcomeDeletionStarted
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
		if err := c.deleteProject(ctx, local, eff, project, &objs[len(objs)-1]); err != nil {
			return runtime.Result{}, withProjectOutcome(err, objs)
		}
		return projectResult(c.k, eff.DisplayName, objs), nil
	}
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
		"delete project partially applied: deletion started for %d namespaces, failed on %s, %d not attempted; the project was kept; re-run the same command; it reads the project's namespaces again",
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

// deleteProject deletes the project once its namespaces are deleted. The
// namespace deletes can make Rancher update the project (its used quota),
// which moves its resourceVersion: a conflict is retried with the newer
// resourceVersion when nothing but the used quota and the status changed
// since the project was read, and is otherwise the named partial state.
// item is the project's entry in the result objects.
func (c *deleteProjectCmd) deleteProject(ctx context.Context, local engine.ClusterClient, eff safety.Effect, read *unstructured.Unstructured, item *safety.Object) error {
	name, cluster, rv := eff.DisplayName, eff.Cluster, eff.ResourceVersion
	for attempt := 0; ; attempt++ {
		err := local.Delete(ctx, c.k.GVR, cluster, name, rv, false)
		if err == nil {
			item.Outcome = safety.OutcomeDeletionStarted
			return nil
		}
		if ce := clierr.From(err); ce.Code != clierr.CodeConflict || !ce.Retryable {
			return c.projectFailed(item, err)
		}
		if attempt == projectRetries {
			return c.projectChanged(item, err)
		}
		current, gerr := local.Get(ctx, c.k.GVR, cluster, name)
		if gerr != nil {
			return c.projectFailed(item, gerr)
		}
		if projectSpecChanged(read, current) {
			return c.projectChanged(item, err)
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
		"project %s changed after it was read; its namespaces are being deleted and the project was kept; re-run the same command",
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
