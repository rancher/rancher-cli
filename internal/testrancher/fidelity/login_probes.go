package fidelity

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/rancher/rancher-cli/internal/token"
)

// loginProbes record what a login needs from a user's global role: after
// the v3 session token a local login creates, the CLI creates its derived
// ext Token with that session token as the bearer. A user whose only global
// role is user-base can create it; a user whose only global role is
// clusters-create, which carries no rule for ext tokens, is refused with a
// 403. Each probe records the user's one GlobalRoleBinding, so the golden
// names the role the fixture bound, and creates the ext Token with the body
// a login without a TTL sends. The setup waits until Rancher has applied
// the binding, and the probe fails unless the binding is the user's only
// GlobalRoleBinding both before the login and after the observations.
func loginProbes() []Probe {
	const (
		bindingPath = "/v3/globalrolebindings/{ownergrb}"
		reviewPath  = "/k8s/clusters/local/apis/authorization.k8s.io/v1/subjectaccessreviews"
		extBody     = `{"spec":{"description":"e2e-fid-login"}}`
		afterLogin  = "only one global role binding after the login"
		reviewBody  = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"{owneruid}","groups":["system:authenticated"],` +
			`"resourceAttributes":{"verb":"create","group":"%s","version":"%s","resource":"%s"}}}`
	)
	// A GlobalRoleBinding as /v3 answers it: the instance-chosen fields are
	// scrubbed, and so is the status Rancher's controllers write after the
	// API answered, except its update time and type; globalRoleId, userId
	// and the id stay.
	bindingRules := []NormalizeRule{
		{Path: "created", Replace: "<ts>"},
		{Path: "createdTS", Replace: "<ts>"},
		{Path: "creatorId", Replace: "<user-id>"},
		{Path: "uuid", Replace: "<uid>"},
		{Path: "ownerReferences.uid", Replace: "<uid>"},
		{Path: "links", Replace: ""},
		{Path: "actions", Replace: ""},
		{Path: "annotations", Replace: ""},
		{Path: "labels", Replace: ""},
		{Path: "status.localConditions", Replace: ""},
		{Path: "status.remoteConditions", Replace: ""},
		{Path: "status.observedGenerationLocal", Replace: ""},
		{Path: "status.observedGenerationRemote", Replace: ""},
		{Path: "status.summary", Replace: ""},
		{Path: "status.summaryLocal", Replace: ""},
		{Path: "status.summaryRemote", Replace: ""},
	}
	rules := slices.Concat(loginRules, bindingRules)
	loginBearer := map[string]string{"Authorization": "Bearer {loginbearer}"}
	login := Observation{Name: "login", Request: RequestSpec{Method: http.MethodPost, Path: loginPath, Body: ownerLogin("json"), Identity: IdentityNone},
		Captures: map[string]Capture{"loginid": {Path: "id"}, "loginbearer": {Path: "token"}}}
	binding := Observation{Name: "binding", Request: RequestSpec{Method: http.MethodGet, Path: bindingPath, Identity: IdentityAdmin}}
	extCreate := func(captures map[string]Capture) Observation {
		return Observation{Name: "ext-create-with-the-login-bearer", Request: RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: extBody, Identity: IdentityNone, Headers: loginBearer},
			Captures: captures}
	}
	setup := func(globalRole, group, version, resource string) []Step {
		return []Step{
			localUserFixture(globalRole, false),
			waitFixture("role grant live", http.MethodPost, reviewPath, fmt.Sprintf(reviewBody, group, version, resource), sarAllowed),
			waitFixture("binding reconciled", http.MethodGet, bindingPath, "", bindingReconciledLocally),
			onlyBindingStep("only one global role binding", globalRole),
		}
	}
	return []Probe{
		{
			// The login bearer of a user whose only global role is
			// user-base creates the ext token the CLI stores; the setup
			// waits until the user may create tokens in ext.cattle.io.
			Name: "login/user-base-user-creates-an-ext-token", OracleOnly: true, SubstituteCaptures: true,
			Setup: setup("user-base", "ext.cattle.io", "v1", "tokens"),
			Observations: []Observation{
				login,
				binding,
				extCreate(map[string]Capture{"extname": {Path: "status.bearerToken", ExtBearerName: true}}),
			},
			Normalize: rules,
			Teardown: []Step{
				onlyBindingStep(afterLogin, "user-base"),
				deleteFixture("ext token", token.ExtTokensPath+"/{extname}"),
				deleteFixture("login token", v3TokenPath+"{loginid}"),
				deleteFixture("owner user", "/v3/users/{owneruid}"),
			},
		},
		{
			// A user with clusters-create only has the rights clusters-create
			// grants, so the refusal shows the binding is applied and the
			// role has no rule for ext tokens. The create declares no
			// capture: a refusal has no token to capture, and deleting the
			// user removes any token a create would have made.
			Name: "login/clusters-create-user-cannot-create-an-ext-token", OracleOnly: true, SubstituteCaptures: true,
			Setup: setup("clusters-create", "management.cattle.io", "v3", "clusters"),
			Observations: []Observation{
				login,
				binding,
				extCreate(nil),
			},
			Normalize: rules,
			Teardown: []Step{
				onlyBindingStep(afterLogin, "clusters-create"),
				deleteFixture("login token", v3TokenPath+"{loginid}"),
				deleteFixture("owner user", "/v3/users/{owneruid}"),
			},
		},
	}
}

// bindingReconciledLocally reports whether Rancher's local controller has
// finished with a GlobalRoleBinding answered by /v3: its status carries the
// update time the recorded body keeps.
func bindingReconciledLocally(decoded map[string]any, status int) bool {
	st, _ := decoded["status"].(map[string]any)
	summary, _ := st["summaryLocal"].(string)
	return status == http.StatusOK && summary == "Completed"
}

// onlyBindingStep fails unless the fixture's user has exactly one
// GlobalRoleBinding, the one the fixture created, with the given role. It
// lists every binding as admin and filters on the client by the user's id,
// since Rancher ignores some list filters. Rancher's login adds the default
// GlobalRoleBindings (the roles marked newUserDefault) to a user it finds
// with the creator-role-bindings annotation; a user created through
// POST /v3/users lacks the annotation, so the login's EnsureUser returns
// the user without adding any binding. The step runs before the login and again after the observations,
// so a Rancher that stamps the annotation fails the probe instead of
// recording a role the user did not have alone.
func onlyBindingStep(name, globalRole string) Step {
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			list, status, err := adminJSON(ctx, tgt, http.MethodGet, "/v3/globalrolebindings?limit=-1", "")
			if err != nil {
				return fmt.Errorf("listing global role bindings: %w", err)
			}
			if status != http.StatusOK {
				return fmt.Errorf("listing global role bindings: status %d", status)
			}
			if page, _ := list["pagination"].(map[string]any); page["next"] != nil {
				return fmt.Errorf("listing global role bindings: the list is paged, the user's bindings cannot all be seen")
			}
			data, _ := list["data"].([]any)
			var found []string
			for _, item := range data {
				entry, _ := item.(map[string]any)
				if entry["userId"] != captured["owneruid"] {
					continue
				}
				id, _ := entry["id"].(string)
				role, _ := entry["globalRoleId"].(string)
				found = append(found, id+" ("+role+")")
				if id != captured["ownergrb"] || role != globalRole {
					return fmt.Errorf("the user has the binding %s (%s), want only %s (%s)", id, role, captured["ownergrb"], globalRole)
				}
			}
			if len(found) != 1 {
				return fmt.Errorf("the user has %d global role bindings %v, want exactly one", len(found), found)
			}
			return nil
		},
	}
}
