package user

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// bindingGenerateName is the prefix Rancher's own code and its UI give
	// the GlobalRoleBindings they create for a new user.
	bindingGenerateName = "grb-"
	// candidateLimit is how many global roles a NOT_FOUND error lists.
	candidateLimit = 10
)

// mustKind returns the kind row for key. A missing row is a programmer
// error, as for userKind.
func mustKind(key string) kinds.Kind {
	k, ok := kinds.Lookup(key)
	if !ok {
		panic("kind row " + key + " not found in kinds.go - renamed or removed without updating internal/engine/user")
	}
	return k
}

// roleSet is the set of global roles create user binds, resolved once at
// Plan. The writes at execute follow exactly this set.
type roleSet struct {
	// ids are the role ids, sorted and without duplicates.
	ids []string
	// noTokenRole is true when roles were typed and none of them allows
	// creating tokens.
	noTokenRole bool
}

// resolveRoles returns the global roles to bind: the ones typed with
// --global-role, each read once so an unknown id fails before anything is
// previewed, or else every GlobalRole the server marks newUserDefault.
func resolveRoles(ctx context.Context, client engine.ClusterClient, in runtime.Input) (roleSet, error) {
	k := mustKind("globalrole")
	if typed := in.Strings("global-role"); len(typed) > 0 {
		ids := slices.Clone(typed)
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				return roleSet{}, clierr.New(clierr.CodeValidation, "--global-role needs a global role id")
			}
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		rs := roleSet{ids: ids, noTokenRole: true}
		for _, id := range ids {
			obj, err := checkGlobalRole(ctx, client, k, id)
			if err != nil {
				return roleSet{}, err
			}
			if allowsCreatingTokens(obj) {
				rs.noTokenRole = false
			}
		}
		return rs, nil
	}

	list, err := client.List(ctx, k.GVR, "", "")
	if err != nil {
		if ce := clierr.From(err); ce.Code == clierr.CodeRBAC {
			return roleSet{}, clierr.New(clierr.CodeRBAC, fmt.Sprintf(
				"cannot list global roles to find the defaults for new users: %s; pass --global-role with the global roles to bind", ce.Message))
		}
		return roleSet{}, err
	}
	var ids []string
	for i := range list.Items {
		if isDefault, _, _ := unstructured.NestedBool(list.Items[i].Object, "newUserDefault"); isDefault {
			ids = append(ids, list.Items[i].GetName())
		}
	}
	slices.Sort(ids)
	return roleSet{ids: ids}, nil
}

// checkGlobalRole follows rbac.checkRole for global scope: it reads the
// global role id. A role that does not exist is NOT_FOUND with the nearest
// roles as candidates. The candidates are chosen from the global roles
// Rancher accepts in a new binding, those that inherit no locked role
// template and no role template that does not exist; the role templates are
// listed for that only when some global role names an inherited one, and
// when that list fails the candidates are the global roles that name none. A
// role that exists is returned as it is, whatever it inherits: the server
// refuses it at the preview. A role that cannot be read for another reason
// returns a nil object and no error: the server enforces the binding, and an
// unreadable role must not block an operator who may create it.
// Cancellation always propagates, also during the role template list.
func checkGlobalRole(ctx context.Context, client engine.ClusterClient, k kinds.Kind, id string) (*unstructured.Unstructured, error) {
	obj, err := client.Get(ctx, k.GVR, "", id)
	if err == nil {
		return obj, nil
	}
	if !clierr.IsNotFound(err) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	e := clierr.New(clierr.CodeNotFound, "role not found: "+id).
		WithHint(clierr.Rancher + " get globalroles")
	list, lerr := client.List(ctx, k.GVR, "", "")
	if lerr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, e
	}
	roles := make([]map[string]any, len(list.Items))
	for i := range list.Items {
		roles[i] = list.Items[i].Object
	}
	bindable, berr := kinds.BindableGlobalRoles(roles, func() ([]map[string]any, error) {
		return listRoleTemplateObjects(ctx, client)
	})
	if berr != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var near, all []clierr.Candidate
	for _, role := range bindable {
		item := unstructured.Unstructured{Object: role}
		display, _, _ := unstructured.NestedString(role, "displayName")
		c := clierr.Candidate{ID: item.GetName(), Name: display}
		all = append(all, c)
		if strings.Contains(strings.ToLower(c.ID+" "+c.Name), strings.ToLower(id)) {
			near = append(near, c)
		}
	}
	if len(near) == 0 {
		near = all
	}
	return nil, e.WithCandidatesCut(near, candidateLimit)
}

// listRoleTemplateObjects lists every roletemplate, whatever its context:
// Rancher reads the roletemplates a globalrole inherits by name alone.
func listRoleTemplateObjects(ctx context.Context, client engine.ClusterClient) ([]map[string]any, error) {
	list, err := client.List(ctx, mustKind("roletemplate").GVR, "", "")
	if err != nil {
		return nil, err
	}
	objs := make([]map[string]any, len(list.Items))
	for i := range list.Items {
		objs[i] = list.Items[i].Object
	}
	return objs, nil
}

// allowsCreatingTokens reports whether one of the global role's own rules
// grants create on tokens in the ext.cattle.io group (a wildcard counts).
// Only rules count: they are the only rules a GlobalRole applies to the
// local cluster, where tokens.ext.cattle.io is served. A role that could not
// be read (nil) counts as allowing it.
func allowsCreatingTokens(role *unstructured.Unstructured) bool {
	if role == nil {
		return true
	}
	rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if holds(rule["apiGroups"], "ext.cattle.io") && holds(rule["resources"], "tokens") && holds(rule["verbs"], "create") {
			return true
		}
	}
	return false
}

// holds reports whether the list of strings v holds want or the wildcard.
func holds(v any, want string) bool {
	items, _ := v.([]any)
	for _, item := range items {
		if s, _ := item.(string); s == want || s == "*" {
			return true
		}
	}
	return false
}

// bindingObject is the GlobalRoleBinding of a user to a global role, in the
// shape Rancher's own code and the Rancher UI create for a new user.
func bindingObject(role, userName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":     "management.cattle.io/v3",
		"kind":           mustKind("globalrolebinding").K8sKind,
		"metadata":       map[string]any{"generateName": bindingGenerateName},
		"globalRoleName": role,
		"userName":       userName,
	}}
}

// refuseBinding words a binding dry-run the server refused as a permission
// error. Any other error passes through unchanged.
func refuseBinding(role string, err error) error {
	ce := clierr.From(err)
	if ce.Code != clierr.CodeRBAC {
		return err
	}
	return clierr.New(clierr.CodeRBAC, fmt.Sprintf(
		"you may not bind global role %s to a new user: %s; pass --global-role with a global role you may bind, or ask an administrator to grant one after creating the user",
		role, ce.Message))
}

// grantPreview is the grant command that previews binding the global role to
// the user, a username or an id.
func grantPreview(subject, role string) string {
	return clierr.Rancher + " grant" + clierr.ShellQuoteArgs(" --global --role "+clierr.ShellQuote(role)+" --dry-run", subject)
}

// grantUserPreview is the grant command that previews binding the global role
// user, the built-in role that allows creating tokens, to the username.
func grantUserPreview(username string) string {
	return clierr.Rancher + " grant" + clierr.ShellQuoteArgs(" --global --role user --dry-run", username)
}

// notBoundClauses words, for each role that was not bound, how to grant it
// to the committed user id. An id that is not an object name gets prose only:
// the CLI prints no command for an id it cannot quote.
func notBoundClauses(id string, roles []string) []string {
	quotable := len(validation.IsDNS1123Subdomain(id)) == 0
	clauses := make([]string, 0, len(roles))
	for _, role := range roles {
		clause := "global role " + role + " was not bound"
		if quotable {
			clause += "; preview the grant with: " + grantPreview(id, role)
		}
		clauses = append(clauses, clause)
	}
	return clauses
}

// noDefaultWarning is the preview note for an empty role set: the server
// marks no default role and the new user would have no role to log in with.
func noDefaultWarning(username string) string {
	return "the server marks no global role as a default for new users; the user cannot log in until granted one; preview the grant with: " +
		grantUserPreview(username)
}

// noTokenRoleWarning is the preview note for typed roles none of which
// allows creating tokens.
func noTokenRoleWarning(username string) string {
	return "none of these global roles allows creating tokens; the user cannot log in with rancher-cli until granted one; preview the grant with: " +
		grantUserPreview(username)
}
