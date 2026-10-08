// Package user holds the curated composite for creating a local Rancher
// user: create user. The sanctioned public-API workflow is two resources,
// never Norman: a management.cattle.io/v3 User (no password field) and a
// same-named v1 Secret in cattle-local-user-passwords, whose
// stringData.password the rancher-webhook mutating admission webhook hashes
// server-side (pbkdf2sha3512) on write.
package user

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
// [--display-name] [--enabled] [--description] [--must-change-password]`
// (mutating): the composite that creates a local Rancher user through the
// sanctioned two-resource public-API workflow, in the oidc-client-create
// mold - both resources' server-side dry-run previews run at Plan time,
// through the ONE safety pipeline.
func NewCreate(clients engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
	return &createCmd{clients: clients, src: src}
}

func (createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create user",
		Usage: "Create a local user",
		Desc: "Creates the management.cattle.io/v3 User, then a same-named Secret in " +
			"cattle-local-user-passwords carrying the password - the server hashes it on " +
			"write (never the CLI). Requires the connected server's user-password workflow " +
			"(ext.cattle.io PasswordChangeRequest support is the declared marker).",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "username", Type: "string", Required: true, Desc: "local username (the server assigns the object's own id)"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "password-stdin", Type: "bool", Desc: "read the new password from stdin (required; there is no --password flag - a flag value would land in shell history; the server enforces a minimum length, 12 by default)"},
			{Name: "display-name", Type: "string", Desc: "display name (default: the username)"},
			{Name: "enabled", Type: "string", Enum: []string{"true", "false"}, Desc: "account enabled state (server default true)"},
			{Name: "description", Type: "string", Desc: "short `description` of the user"},
			// A bool suffices, unlike --enabled: the server default is false
			// and the field is omitted when false, so omitting the flag and
			// passing false produce the same User.
			{Name: "must-change-password", Type: "bool", Desc: "require the user to set a new password at their next Rancher UI login"},
		},
		Examples: []string{
			`echo -n "$PASSWORD" | rancher create user alice --password-stdin --apply --yes`,
		},
		Destructive: "true",
		Auth:        true,
		Capability:  engine.UserPasswordWorkflowCapability(),
		Shape:       runtime.ShapeText,
	}
}

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
	// PlanCreateObserved's own executor is discarded, same rationale as
	// oidc-client-create: this command's executor runs the Secret step too,
	// which the generic shape does not express. previewUser's dry-run
	// generateName expansion is a preview only - the real committed name is
	// not known until execute creates the User for real.
	eff, previewUser, _, err := engine.PlanCreateObserved(ctx, client, k.GVR, "", userObj, engine.LocalCluster, k.Name, "User", k.Sensitive)
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
	secretPreview := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": previewUser.GetName(), "namespace": secretNamespace},
		"stringData": map[string]any{"password": password},
	}}
	if _, _, err := engine.PlanCreate(ctx, client, secretGVR, secretNamespace, secretPreview, engine.LocalCluster, "secret", "Secret", true); err != nil {
		return safety.Effect{}, nil, engine.RefuseUnsupportedWorkflow(err,
			"the cattle-local-user-passwords password-secret namespace is absent",
			"upgrade the connected server to a version that carries this workflow, or restore the cattle-local-user-passwords namespace if it was removed")
	}
	// The name is never stated: previewUser.GetName() is a dry-run-only
	// placeholder that will not match the Secret's real committed name
	// (assigned only once the User create commits for real) - naming it
	// here would read as a promise the apply-time output then contradicts.
	eff.Warnings = append(eff.Warnings, fmt.Sprintf("also creates a v1/Secret in %s to hold the password", secretNamespace))

	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, client, k, username, displayName, userObj, password)
	}
	return eff, engine.RedactMutationResult(exec, k.RedactPaths, true), nil
}

// execute runs strictly in this order: User create -> Secret create. A
// Secret failure after the User committed returns a non-retryable error
// carrying CommittedContextDetails whose recovery creates the missing
// Secret directly - NOT `create password-change-request`, which requires
// an existing password Secret to change and 401s ("failed to get password
// secret") against a user that has none yet. `create resource -f -` is
// used instead of `create secret --from-literal` for the same reason
// `--password-stdin` exists on this
// command in the first place: --from-literal's value is a literal argv
// token, and the recovery must not reintroduce the password-in-shell-
// history/process-listing exposure this whole workflow exists to avoid.
func (c *createCmd) execute(ctx context.Context, client engine.ClusterClient, k kinds.Kind, username, displayName string, userObj *unstructured.Unstructured, password string) (runtime.Result, error) {
	createdUser, err := client.Create(ctx, k.GVR, "", userObj, false)
	if err != nil {
		return runtime.Result{}, err
	}
	name := createdUser.GetName()
	recovery := fmt.Sprintf(`the user %s was created without a password - set one without it touching argv or shell history: echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"cattle-local-user-passwords"},"stringData":{"password":"'"$PASSWORD"'"}}' | rancher create resource -f - --cluster local --apply --yes (a password containing a double quote or backslash must be JSON-escaped in the manifest)`, name, name)

	secretObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": secretNamespace},
		"stringData": map[string]any{"password": password},
	}}
	createdSecret, err := client.Create(ctx, secretGVR, secretNamespace, secretObj, false)
	if err != nil {
		return runtime.Result{}, engine.CommittedPartialSuccess(err, clierr.CommittedContextDetails{
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
		// data/stringData. The RedactMutationResult wrapper stays on the
		// executor as the backstop for anything a future edit adds here.
		"passwordSecret": createdSecret.GetName(),
	}
	res := runtime.MutationText("user", username, "created",
		"id: "+name,
		"password secret: "+createdSecret.GetName(),
	)
	res.Kind = "UserCreated"
	res.Data = data
	return res, nil
}
