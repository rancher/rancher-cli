package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The flags both catalog action commands take.
const (
	flagNoHooks = "no-hooks"
)

// noAppNote is the install result's warning on what the follow-up commands
// show when the run fails before Helm stores the release.
const noAppNote = "a chart that fails before Helm stores the release leaves no app; the operation then shows the failure"

// unchangedAppNote is the upgrade result's warning on what the follow-up
// commands show when Helm refuses the upgrade before it stores a revision.
const unchangedAppNote = "an upgrade Helm refuses before it stores a revision leaves the app unchanged; the operation then shows the failure"

// The warnings the catalog action previews carry. An install or upgrade
// preview says the server has no dry-run for its action; an uninstall
// preview says the CLI does not send the action's dry-run option.
const (
	warnServerReads          = "this preview comes from server reads; the server has no dry-run for this action"
	warnServerReadsUninstall = "this preview comes from server reads; the CLI does not send the uninstall action's dry-run option"
	warnNoWatch              = "nothing after the request checks for a concurrent change to the release"
	warnPullSecretsInstall   = "Rancher may create, change or delete image pull secrets for this release; the preview cannot list them"
	warnPullSecretsUninstall = "Rancher may delete image pull secrets for this release; the preview cannot list them"
)

// identityWarning names the identity Helm runs as.
func identityWarning(identity string) string {
	return "Rancher runs Helm in a pod as " + identity + "; the chart's own writes are checked only when Helm runs them"
}

// callerIdentity is how the warnings name the caller.
const callerIdentity = "you"

// pullSecretsWarningFloor is the first Rancher minor whose previews carry
// warnPullSecretsInstall or warnPullSecretsUninstall.
const pullSecretsWarningFloor = "2.15"

// pullSecretsWarningApplies reports whether the previews carry
// warnPullSecretsInstall or warnPullSecretsUninstall: the server is v2.15
// or newer, or its version cannot be read.
func pullSecretsWarningApplies(ctx context.Context, clients engine.ClusterClientFactory) bool {
	versions, ok := clients.(engine.ServerVersionProvider)
	if !ok {
		return true
	}
	raw, err := versions.ServerVersion(ctx)
	if err != nil {
		return true
	}
	below, known := config.VersionBelow(raw, pullSecretsWarningFloor)
	return !known || !below
}

// kind returns the row of a catalog kind; the rows are part of the binary.
func kind(key string) kinds.Kind {
	k, ok := kinds.ByKey(key)
	if !ok {
		panic("catalog: no kind row " + key)
	}
	return k
}

// operationRecent is how long after its creation an Operation still counts
// as possibly running: Rancher removes a run's access one hour after the
// operation starts.
const operationRecent = time.Hour

// runningOperation finds an Operation in ns for release that may still be
// running: created less than an hour ago, and with Reconciling=True, no
// Reconciling condition, or status.observedGeneration behind
// metadata.generation. It returns its name, or "". ok is false when the
// caller cannot list operations in ns; any other read failure is the error.
func runningOperation(ctx context.Context, client engine.ClusterClient, ns, release string, now time.Time) (name string, ok bool, err error) {
	list, err := client.List(ctx, kind("operation").GVR, ns, "")
	if err != nil {
		if forbidden(err) {
			return "", false, nil
		}
		return "", true, err
	}
	for i := range list.Items {
		op := &list.Items[i]
		if r, _, _ := unstructured.NestedString(op.Object, "status", "releaseName"); r != release {
			continue
		}
		if created := op.GetCreationTimestamp(); !created.IsZero() && now.Sub(created.Time) >= operationRecent {
			continue
		}
		if operationMayRun(op) {
			return op.GetName(), true, nil
		}
	}
	return "", true, nil
}

// operationMayRun reports whether op may still be running.
func operationMayRun(op *unstructured.Unstructured) bool {
	observed, _, _ := unstructured.NestedInt64(op.Object, "status", "observedGeneration")
	if observed < op.GetGeneration() {
		return true
	}
	conds, _, _ := unstructured.NestedSlice(op.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Reconciling" {
			return m["status"] == "True"
		}
	}
	return true
}

// operationRunning is the refusal while an Operation for the release may
// be running.
func operationRunning(t actionTarget, op string) error {
	return clierr.New(clierr.CodeConflict, fmt.Sprintf(
		"operation %s for release %s in namespace %s may still be running; Rancher runs a new one for each request",
		clierr.ShellQuote(op), clierr.ShellQuote(t.release), clierr.ShellQuote(t.namespace))).
		WithHintCommand(clierr.NewCommand("get operation").Arg(op).FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace))
}

// operationsNotChecked is the warning when the caller cannot list
// operations.
func operationsNotChecked(t actionTarget) string {
	return fmt.Sprintf("you cannot list operations in namespace %s; a running operation for release %s was not checked",
		clierr.ShellQuote(t.namespace), clierr.ShellQuote(t.release))
}

// changedBeforeRequest is Execute's refusal when the state the preview read
// changed before the request.
func changedBeforeRequest(what string) error {
	return clierr.New(clierr.CodeConflict, what+" since the preview; run the command again to see a new preview").
		WithRetryable(true)
}

// recheckOperations is Execute's re-read of the operations: none may be
// running by the preview's rule.
func recheckOperations(ctx context.Context, client engine.ClusterClient, t actionTarget, now time.Time) error {
	op, ok, err := runningOperation(ctx, client, t.namespace, t.release, now)
	if err != nil {
		return err
	}
	if ok && op != "" {
		return changedBeforeRequest(fmt.Sprintf("operation %s for release %s started", clierr.ShellQuote(op), clierr.ShellQuote(t.release)))
	}
	return nil
}

// canGetOperations asks the cluster whether the caller may get operations
// in ns. A failed request is no, with reviewed false.
func canGetOperations(ctx context.Context, client engine.ClusterClient, ns string) (allowed, reviewed bool) {
	gvk, ok := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
	if !ok {
		return false, false
	}
	gvr := kind("operation").GVR
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind,
		"spec": map[string]any{"resourceAttributes": map[string]any{
			"verb": "get", "group": gvr.Group, "resource": gvr.Resource, "namespace": ns,
		}},
	}}
	resp, err := client.PostReview(ctx, engine.ReviewRequest{Kind: engine.ReviewSelfSubjectAccess, Object: review})
	if err != nil || resp.Status < 200 || resp.Status > 299 || resp.Object == nil {
		return false, false
	}
	allowed, _, _ = unstructured.NestedBool(resp.Object.Object, "status", "allowed")
	return allowed, true
}

// serviceAccountIdentity is the identity Helm runs as for an install from
// a ClusterRepo: its service account when spec.serviceAccount and
// spec.serviceAccountNamespace are both set and the namespace holds no ":",
// else the caller ("").
func serviceAccountIdentity(repo *unstructured.Unstructured) string {
	sa, _, _ := unstructured.NestedString(repo.Object, "spec", "serviceAccount")
	saNS, _, _ := unstructured.NestedString(repo.Object, "spec", "serviceAccountNamespace")
	if sa == "" || saNS == "" || strings.Contains(saNS, ":") {
		return ""
	}
	return "system:serviceaccount:" + saNS + ":" + sa
}

// identityOf reads who Helm runs as and, when that is not the caller,
// whether the caller can still read the operation. The warning names the
// identity; reviewed is false when the access review request failed.
func identityOf(ctx context.Context, client engine.ClusterClient, sa, ns string) (warning string, canReadOperation, reviewed bool) {
	if sa == "" {
		return identityWarning(callerIdentity), true, true
	}
	canRead, reviewed := canGetOperations(ctx, client, ns)
	return identityWarning(sa), canRead, reviewed
}

// result is the payload of a catalog action: the operation Rancher created
// and the commands that follow it. RequestID is set for an install and an
// upgrade. Status and Revision are set after a wait: the App's
// spec.info.status at the end (uninstalled when the App is gone) and, for an
// install or an upgrade, its spec.version.
type result struct {
	Release            string   `json:"release"`
	Namespace          string   `json:"namespace"`
	OperationName      string   `json:"operationName"`
	OperationNamespace string   `json:"operationNamespace"`
	RequestID          string   `json:"requestID,omitempty"`
	Status             string   `json:"status,omitempty"`
	Revision           int64    `json:"revision,omitempty"`
	FollowUp           []string `json:"followUp"`
}

// actionResult builds the result of an accepted request: the text names
// the operation, and the follow-up commands read the operation (when the
// caller can) and the App. The App of an install exists once Helm stores
// the release and then holds Helm's error when the run fails; a chart that
// fails before that leaves no App, and only the operation shows the
// failure, which an install's result warns about. An upgrade Helm refuses
// before it stores a revision leaves the App as it was, which an upgrade's
// result warns about.
func actionResult(verb string, t actionTarget, a actionAnswer, canReadOperation bool) runtime.Result {
	warnings := []string{warnNoWatch}
	switch verb {
	case "install":
		warnings = append(warnings, noAppNote)
	case "upgrade":
		warnings = append(warnings, unchangedAppNote)
	}
	var commands []clierr.Command
	if canReadOperation {
		commands = append(commands, clierr.NewCommand("get operation").Arg(a.OperationName).
			FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", a.OperationNamespace).Flag("--wait"))
	}
	commands = append(commands, t.getApp())
	follow, followText := followUp(commands)
	var text strings.Builder
	fmt.Fprintf(&text, "app %s: Rancher accepted the %s request; operation %s in namespace %s runs Helm\n",
		t.release, verb, a.OperationName, a.OperationNamespace)
	text.WriteString(followText)
	return runtime.Result{
		Kind: "AppOperation",
		Data: result{
			Release: t.release, Namespace: t.namespace,
			OperationName: a.OperationName, OperationNamespace: a.OperationNamespace,
			RequestID: t.requestID, FollowUp: follow,
		},
		Text:     text.String(),
		Warnings: warnings,
	}
}

// followUp is the commands of a result that can be printed, as the result's
// followUp list (empty, never nil), and their lines of text. A command left out
// is missing from both, and one last line carries clierr.LeftOutNote.
func followUp(commands []clierr.Command) ([]string, string) {
	list := []string{}
	var text strings.Builder
	left := false
	for _, c := range commands {
		if !c.OK() {
			left = true
			continue
		}
		list = append(list, c.Field())
		text.WriteString(clierr.Text(clierr.Lead("follow it with: "), c) + "\n")
	}
	if left {
		text.WriteString(clierr.LeftOutNote + "\n")
	}
	return list, text.String()
}

// waitedResult builds the result of a request whose wait ended with
// success: the text names the end, and the one follow-up command reads the
// App when it still exists.
func waitedResult(verb string, t actionTarget, a actionAnswer, end waitEnd) runtime.Result {
	commands := []clierr.Command{t.getApp()}
	line := fmt.Sprintf("app %s: uninstalled\n", t.release)
	switch verb {
	case "install":
		line = fmt.Sprintf("app %s: installed (revision %d, status %s)\n", t.release, end.revision, end.status)
	case "upgrade":
		line = fmt.Sprintf("app %s: upgraded (revision %d, status %s)\n", t.release, end.revision, end.status)
	}
	// An App that is gone has nothing left for get app to show.
	if end.gone {
		commands = nil
	}
	follow, followText := followUp(commands)
	var text strings.Builder
	text.WriteString(line)
	text.WriteString(followText)
	return runtime.Result{
		Kind: "AppOperation",
		Data: result{
			Release: t.release, Namespace: t.namespace,
			OperationName: a.OperationName, OperationNamespace: a.OperationNamespace,
			RequestID: t.requestID, Status: end.status, Revision: end.revision, FollowUp: follow,
		},
		Text:     text.String(),
		Warnings: end.warnings,
	}
}

// forbidden reports whether err is the server's refusal of the caller.
func forbidden(err error) bool {
	if code, ok := clierr.HTTPStatus(err); ok && code == http.StatusForbidden {
		return true
	}
	return clierr.From(err).Code == clierr.CodeRBAC
}

// notFound reports whether err is the server's NOT_FOUND.
func notFound(err error) bool {
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		return ce.Code == clierr.CodeNotFound
	}
	code, ok := clierr.HTTPStatus(err)
	return ok && code == http.StatusNotFound
}
