package extres

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// passwordChangeRequestGVR names the ext.cattle.io PasswordChangeRequest
// resource: cluster-scoped and create-only - its own discovery advertises no
// get/list/delete verb, a command resource rather than a stored one - so
// unlike Token/Kubeconfig it carries no kinds.go row: a table row that could
// never support get/list/delete would only mislead the generic verb
// machinery into offering commands the server always rejects.
var passwordChangeRequestGVR = schema.GroupVersionResource{Group: "ext.cattle.io", Version: "", Resource: "passwordchangerequests"}

// passwordChangeRequestRedaction hides the password fields of the submission
// in the preview. The resource has no kind row to take them from.
var passwordChangeRequestRedaction = kinds.RowlessRedaction(passwordChangeRequestGVR,
	"the resource is create-only and has no kind row",
	[]string{"spec", "newPassword"}, []string{"spec", "currentPassword"})

type createPasswordChangeRequestCmd struct {
	clients engine.ClusterClientFactory
	src     engine.FileSource
}

// NewCreatePasswordChangeRequest builds `create password-change-request
// <user> --password-stdin` (mutating): submits an ext.cattle.io/v1
// PasswordChangeRequest, the password-change side of the user-password
// workflow `create user` also uses. The request completes synchronously -
// the server reports the change already applied in the create response,
// never a controller the caller must wait on.
func NewCreatePasswordChangeRequest(clients engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
	return &createPasswordChangeRequestCmd{clients: clients, src: src}
}

func (createPasswordChangeRequestCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create password-change-request",
		Usage: "Change a local user's password",
		Desc: "Submits an ext.cattle.io/v1 PasswordChangeRequest, which completes " +
			"synchronously. Requires a server that supports local user passwords " +
			"(the same support `rancher create user` needs).",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "user", Type: "string", Required: true, Desc: "user `id` (see: `rancher get users`)"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "password-stdin", Type: "bool", Desc: "read the new password from stdin (required; there is no --password flag because a flag value would be saved in shell history; the server enforces a minimum length, 12 by default)"},
		},
		Examples: []string{
			`echo -n "$NEWPASSWORD" | rancher create password-change-request u-abc123 --password-stdin --apply --yes`,
		},
		Destructive: "true",
		Auth:        true,
		Capability:  engine.UserPasswordWorkflowCapability(),
		Shape:       runtime.ShapeText,
		SecretInput: true,
	}
}

func (c createPasswordChangeRequestCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	userID := in.Args()[0]
	password, err := engine.ReadPasswordStdin(c.src, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "PasswordChangeRequest",
		"metadata":   map[string]any{},
		"spec": map[string]any{
			"userID":      userID,
			"newPassword": password,
		},
	}}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// The request resource is unnamed (create-only, no metadata.name):
	// PlanCreate's generic Effect-building derives DisplayName from the
	// dry-run response's name, which would be empty here - built by hand
	// instead, naming the target user rather than the (nonexistent) object.
	// The diff still comes from the same walk the planner uses.
	diff, err := engine.CreateDiff(passwordChangeRequestGVR, obj.Object, passwordChangeRequestRedaction)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if _, err := client.Create(ctx, passwordChangeRequestGVR, "", obj, true); err != nil {
		return safety.Effect{}, nil, engine.RefuseUnsupportedWorkflow(err,
			"the ext.cattle.io PasswordChangeRequest resource is absent",
			"upgrade the server to a version that supports them")
	}
	eff := safety.Effect{
		PreviewSource:     safety.PreviewServerDryRun,
		Action:            safety.ActionCreate,
		Target:            passwordChangeRequestGVR.Resource + "/" + userID,
		DisplayKind:       "password-change-request",
		DisplayName:       clierr.IDText(userID),
		Cluster:           engine.LocalCluster,
		Diff:              diff,
		ConflictDetection: true,
		Reversible:        true,
		Destructive:       true,
	}

	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, passwordChangeRequestGVR, "", obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		if !passwordUpdated(created) {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("the server did not confirm the password change for user %s", clierr.IDText(userID))).
				WithDetails(clierr.CommittedContextDetails{
					Committed: true, ResourceKind: "User", ResourceName: userID,
					Recovery: userLookupRecovery(userID),
				})
		}
		res := runtime.MutationText("password-change-request", clierr.IDText(userID), "completed")
		res.Kind = "PasswordChangeRequestCompleted"
		// Never the raw created object: its spec.newPassword field echoes
		// the plaintext value verbatim - Data is a hand-built, deliberately
		// password-free shape instead, the same posture
		// internal/engine/oidcclient's create takes with its Data map.
		res.Data = map[string]any{"userID": userID}
		return res, nil
	}
	return eff, exec, nil
}

// userLookupRecovery is the command that shows the user a password change was
// for. It names the id only when the id is a Rancher user id (a Kubernetes
// object name); any other typed value is shown through clierr.IDText, with
// the command that lists users and the place the exact id is: the field
// resourceName of the error's details in the JSON output.
func userLookupRecovery(userID string) string {
	if len(validation.IsDNS1123Subdomain(userID)) == 0 {
		return clierr.Rancher + " get user" + clierr.ShellQuoteArgs(" -o json", userID)
	}
	return "the user id " + clierr.ShellQuote(clierr.IDText(userID)) + " is not a Rancher user id, and the CLI prints no command for it; " +
		"the exact id is in the JSON output (-o json), field details.resourceName; find the user with: " + clierr.Rancher + " get users -o json"
}

// passwordUpdated reports whether created's status carries a True
// PasswordUpdated condition - the server's own synchronous confirmation,
// never inferred from the create call's bare success.
func passwordUpdated(created *unstructured.Unstructured) bool {
	updated := false
	engine.EachCondition(created.Object, func(typ, status, _, _ string) bool {
		if typ == "PasswordUpdated" && status == "True" {
			updated = true
			return false
		}
		return true
	})
	return updated
}
