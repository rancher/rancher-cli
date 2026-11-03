// Package user holds the curated composite for creating a local Rancher
// user: create user. The sanctioned public-API workflow is two resources,
// never Norman: a management.cattle.io/v3 User (no password field) and a
// same-named v1 Secret in cattle-local-user-passwords, whose
// stringData.password the rancher-webhook mutating admission webhook hashes
// server-side (pbkdf2sha3512) on write. The user is then bound to global
// roles, as the Rancher UI binds them, with one management.cattle.io/v3
// GlobalRoleBinding per role: without a global role the user cannot create a
// token and cannot log in.
package user

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	usercomplete "github.com/rancher/rancher-cli/internal/engine/user/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// secretGVR/secretNamespace name the backing password Secret every local
// user's credential lives in - a same-named v1 Secret in
// cattle-local-user-passwords, owned (via an ownerReference the webhook adds
// itself) by the User it belongs to.
var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

const secretNamespace = "cattle-local-user-passwords"

// userKind returns the user kind row. A missing/renamed row is a programmer
// error, not a runtime condition to recover from - mirrors
// internal/engine/oidcclient's oidcClientKind panic and its rationale.
func userKind() kinds.Kind {
	k, ok := kinds.Lookup("user")
	if !ok {
		panic("user kind row not found in kinds.go - renamed or removed without updating internal/engine/user")
	}
	return k
}

type createCmd struct {
	clients engine.ClusterClientFactory
	src     engine.FileSource
}

// NewCreate builds `create user <username> --password-stdin
// [--display-name] [--enabled] [--description] [--must-change-password]
// [--global-role]` (mutating): the composite that creates a local Rancher
// user through the sanctioned two-resource public-API workflow, then binds
// its global roles, in the oidc-client-create mold - every resource's
// server-side dry-run preview runs at Plan time, through the ONE safety
// pipeline.
func NewCreate(clients engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
	return &createCmd{clients: clients, src: src}
}

func (createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create user",
		Usage: "Create a local user and bind its global roles",
		Desc: "Creates the management.cattle.io/v3 User, then a same-named Secret in " +
			"cattle-local-user-passwords that holds the password - the server hashes it on " +
			"write (never the CLI) - then a management.cattle.io/v3 GlobalRoleBinding for each " +
			"global role Rancher gives new users by default (the ones with newUserDefault set), " +
			"or for each --global-role. A user with no global role cannot create a token and cannot log in. " +
			"Requires a server that supports local user passwords " +
			"(it serves the ext.cattle.io PasswordChangeRequest resource).",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "username", Type: "string", Required: true, Desc: "local username (the server assigns the user id)"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "password-stdin", Type: "bool", Desc: "read the new password from stdin (required; there is no --password flag because a flag value would be saved in shell history; the server enforces a minimum length, 12 by default)"},
			{Name: "display-name", Type: "string", Desc: "display name (default: the username)"},
			{Name: "enabled", Type: "string", Enum: []string{"true", "false"}, Desc: "account enabled state (server default true)"},
			{Name: "description", Type: "string", Desc: "short `description` of the user"},
			// A bool suffices, unlike --enabled: the server default is false
			// and the field is omitted when false, so omitting the flag and
			// passing false produce the same User.
			{Name: "must-change-password", Type: "bool", Desc: "require the user to set a new password at their next Rancher UI login"},
			{Name: "global-role", Type: "stringSlice", Desc: "global role `id` to bind (repeatable); default: the roles the server marks newUserDefault; list them with: `rancher get globalroles`"},
		},
		Examples: []string{
			`echo -n "$PASSWORD" | rancher create user alice --password-stdin --apply --yes`,
			`echo -n "$PASSWORD" | rancher create user bob --password-stdin --global-role user-base --apply --yes`,
		},
		Destructive: "true",
		Auth:        true,
		Capability:  engine.UserPasswordWorkflowCapability(),
		Shape:       runtime.ShapeText,
		SecretInput: true,
	}
}

// LiveSource completes the --global-role value with the ids of the global
// roles that Rancher accepts in a new binding.
func (createCmd) LiveSource() completion.Source { return usercomplete.GlobalRoleSource() }

func (c *createCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	username := in.Args()[0]
	password, err := engine.ReadPasswordStdin(c.src, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	// An omitted --display-name defaults to the username; the pipeline
	// refuses a typed empty one.
	displayName := in.String("display-name")
	if displayName == "" {
		displayName = username
	}

	userObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":  "management.cattle.io/v3",
		"kind":        "User",
		"metadata":    map[string]any{"generateName": "u-"},
		"username":    username,
		"displayName": displayName,
	}}
	switch in.String("enabled") {
	case "true":
		userObj.Object["enabled"] = true
	case "false":
		userObj.Object["enabled"] = false
	}
	if desc := in.String("description"); desc != "" {
		userObj.Object["description"] = desc
	}
	if in.Bool("must-change-password") {
		userObj.Object["mustChangePassword"] = true
	}

	k := userKind()
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	roles, err := resolveRoles(ctx, client, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// PlanCreateObserved's own executor is discarded, same rationale as
	// oidc-client-create: this command's executor runs the Secret step too,
	// which the generic shape does not express. previewUser's dry-run
	// generateName expansion is a preview only - the real committed name is
	// not known until execute creates the User for real.
	eff, previewUser, _, err := engine.PlanCreateObserved(ctx, client, k.GVR, "", userObj, k.Redaction(), engine.LocalCluster, k.Name, "User", k.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	// The enforcing net: prove THIS server accepts the Secret create before
	// anything commits - on a server without the workflow, the namespace
	// does not exist and this dry-run fails, which the declared-signal
	// capability gate (Run, before Plan) should already have caught; this
	// is the backstop for the rare case that signal itself was unreadable.
	// previewUser's name is a placeholder only: a dry-run never persists, so
	// there is no collision risk, and only the namespace/webhook's
	// acceptance of the shape matters here.
	secretKind, ok := kinds.Lookup("secret")
	if !ok {
		return safety.Effect{}, nil, clierr.New(clierr.CodeGeneric, "internal error: no kind named secret")
	}
	secretPreview := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": previewUser.GetName(), "namespace": secretNamespace},
		"stringData": map[string]any{"password": password},
	}}
	if _, _, err := engine.PlanCreate(ctx, client, secretGVR, secretNamespace, secretPreview, secretKind.Redaction(), engine.LocalCluster, "secret", "Secret", true); err != nil {
		return safety.Effect{}, nil, engine.RefuseUnsupportedWorkflow(err,
			"the cattle-local-user-passwords namespace is absent",
			"upgrade the server to a version that supports them, or restore the cattle-local-user-passwords namespace if it was removed")
	}
	// The name is never stated: previewUser.GetName() is a dry-run-only
	// placeholder that will not match the Secret's real committed name
	// (assigned only once the User create commits for real) - naming it
	// here would read as a promise the apply-time output then contradicts.
	eff.Warnings = append(eff.Warnings, fmt.Sprintf("also creates a v1/Secret in %s to hold the password", secretNamespace))

	// Each binding is previewed with the preview user's name: the webhook
	// checks the requester may bind the role (an escalation check) and does
	// not look the user up. The real writes never use that name.
	bk := mustKind("globalrolebinding")
	for _, role := range roles.ids {
		if _, _, err := engine.PlanCreate(ctx, client, bk.GVR, "", bindingObject(role, previewUser.GetName()), bk.Redaction(), engine.LocalCluster, bk.Name, bk.K8sKind, bk.Sensitive); err != nil {
			return safety.Effect{}, nil, refuseBinding(role, err)
		}
		eff.Warnings = append(eff.Warnings, "also creates a management.cattle.io/v3 GlobalRoleBinding for global role "+role)
	}
	var noRoleWarning string
	switch {
	case len(roles.ids) == 0:
		noRoleWarning = noDefaultWarning(username)
	case roles.noTokenRole:
		noRoleWarning = noTokenRoleWarning(username)
	}
	if noRoleWarning != "" {
		eff.Warnings = append(eff.Warnings, noRoleWarning)
	}

	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, client, k, username, displayName, userObj, password, roles.ids)
	}
	return eff, exec, nil
}

// execute runs strictly in this order: User create -> Secret create ->
// one GlobalRoleBinding create per role, the roles Plan resolved and
// previewed (execute never reads a GlobalRole again). A Secret failure after
// the User committed returns a non-retryable error
// carrying CommittedContextDetails whose recovery creates the missing
// Secret directly - NOT `create password-change-request`, which requires
// an existing password Secret to change and 401s ("failed to get password
// secret") against a user that has none yet. `create resource -f -` is
// used instead of `create secret --from-literal` for the same reason
// `--password-stdin` exists on this
// command in the first place: --from-literal's value is a literal argv
// token, and the recovery must not reintroduce the password-in-shell-
// history/process-listing exposure this whole workflow exists to avoid.
// It also stops before any binding is attempted and adds a grant clause for
// each role not bound. A binding failure does not stop the remaining
// bindings: every role is attempted, then one error built from the first
// failure names each role not bound. The user is never deleted.
func (c *createCmd) execute(ctx context.Context, client engine.ClusterClient, k kinds.Kind, username, displayName string, userObj *unstructured.Unstructured, password string, roles []string) (runtime.Result, error) {
	createdUser, err := client.Create(ctx, k.GVR, "", userObj, false)
	if err != nil {
		return runtime.Result{}, err
	}
	name := createdUser.GetName()
	recovery := fmt.Sprintf("the user %s was created without a password; the CLI prints no command for an id it cannot quote", clierr.ShellQuote(name))
	if len(validation.IsDNS1123Subdomain(name)) == 0 {
		recovery = fmt.Sprintf(`the user %s was created without a password; to set one without putting it on the command line or in shell history, set PASSWORD in your shell (a password containing a double quote or backslash must be JSON-escaped in the manifest) and add --apply after reading the preview of: echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"cattle-local-user-passwords"},"stringData":{"password":"'"$PASSWORD"'"}}' | `, name, name) +
			clierr.Rancher + " create resource -f - --cluster local --dry-run"
	}

	secretObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": secretNamespace},
		"stringData": map[string]any{"password": password},
	}}
	createdSecret, err := client.Create(ctx, secretGVR, secretNamespace, secretObj, false)
	if err != nil {
		if len(roles) > 0 {
			recovery += "; " + strings.Join(notBoundClauses(name, roles), "; ")
		}
		return runtime.Result{}, engine.CommittedPartialSuccess(err, clierr.CommittedContextDetails{
			Committed: true, ResourceKind: "User", ResourceName: name, Recovery: recovery,
		})
	}

	bk := mustKind("globalrolebinding")
	bindings := make([]map[string]any, 0, len(roles))
	var notBound []string
	var firstErr error
	for _, role := range roles {
		created, err := client.Create(ctx, bk.GVR, "", bindingObject(role, name), false)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			notBound = append(notBound, role)
			continue
		}
		bindings = append(bindings, map[string]any{"name": created.GetName(), "globalRoleName": role})
	}
	if firstErr != nil {
		recovery := fmt.Sprintf("the user %s was created; %s", clierr.ShellQuote(name), strings.Join(notBoundClauses(name, notBound), "; "))
		return runtime.Result{}, engine.CommittedPartialSuccess(firstErr, clierr.CommittedContextDetails{
			Committed: true, ResourceKind: "User", ResourceName: name, Recovery: recovery,
		})
	}

	enabled := true
	if v, ok := createdUser.Object["enabled"].(bool); ok {
		enabled = v
	}
	userData := map[string]any{
		"name":        name,
		"username":    username,
		"displayName": displayName,
		"enabled":     enabled,
	}
	// The two optional fields are reported when the created User carries
	// them, so the result shows what the server stored.
	if desc, _ := createdUser.Object["description"].(string); desc != "" {
		userData["description"] = desc
	}
	if must, _ := createdUser.Object["mustChangePassword"].(bool); must {
		userData["mustChangePassword"] = true
	}
	data := map[string]any{
		"user": userData,
		// The server's create response echoes the hashed payload
		// (data/salt); none of it belongs in output, so the map is built
		// from identity fields only and never copies the Secret's
		// data/stringData.
		"passwordSecret":     createdSecret.GetName(),
		"globalRoleBindings": bindings,
	}
	lines := []string{"id: " + name, "password Secret: " + createdSecret.GetName()}
	for _, b := range bindings {
		lines = append(lines, fmt.Sprintf("global role: %s (binding %s)", b["globalRoleName"], b["name"]))
	}
	res := runtime.MutationText("user", username, "created", lines...)
	res.Kind = "UserCreated"
	res.Data = data
	return res, nil
}
