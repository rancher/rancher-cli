package fidelity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"

	"github.com/rancher/rancher-cli/internal/token"
)

// v3TokenPath is the management.cattle.io/v3 Token collection path through
// the local proxy, with the trailing slash a name is appended to.
const v3TokenPath = token.V3TokensPath + "/"

// loginPath is the public login endpoint.
const loginPath = "/v1-public/login"

// loginRules scrub the login body: its credential and expiry vary per
// login; its id is a capture, so it records as the template; the keys are
// the fact.
var loginRules = []NormalizeRule{
	{Path: "token", Replace: "<secret>"},
	{Path: "expiresAt", Replace: "<ts>"},
}

// ownerLogin is the login request body of the local user ownerUserFixture
// creates, for the given response type.
func ownerLogin(responseType string) string {
	return `{"type":"localProvider","username":"{ownername}","password":"{ownerpw}","responseType":"` + responseType + `"}`
}

// ownerProbes record the facts the exec plugin's owner check rests on:
// every token create names its creator as the owner (an ext Token's
// spec.userID, unscoped and scoped to a cluster; a v3 Token's userId,
// unscoped and with a clusterId), the login body names no owner, and a
// SelfSubjectReview names the bearer's owner as status.userInfo.username
// and carries no uid - through the downstream with a cluster-scoped token,
// through local with an unscoped one - while local refuses a v3 token
// scoped to another cluster. Each probe first captures the user id from a
// review, and records with every captured value substituted, so a field
// equal to the user id records as {userid} (or {owneruid}): the golden
// states the equality, not the per-run id. Every token and user a probe
// creates is deleted by the id it recorded.
func ownerProbes() []Probe {
	const (
		whoBody  = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`
		v3Create = "/v3/tokens"
	)
	// A v3 Token as /v3/tokens answers it: the instance-chosen fields are
	// scrubbed; userId, clusterId, the kind fields and the flags stay.
	v3TokenRules := []NormalizeRule{
		{Path: "token", Replace: "<secret>"},
		{Path: "created", Replace: "<ts>"},
		{Path: "createdTS", Replace: "<ts>"},
		{Path: "expiresAt", Replace: "<ts>"},
		{Path: "lastUsedAt", Replace: "<ts>"},
		{Path: "lastUpdateTime", Replace: "<ts>"},
		{Path: "uuid", Replace: "<uid>"},
		{Path: "links", Replace: ""},
		{Path: "actions", Replace: ""},
		{Path: "annotations", Replace: ""},
	}
	reviewRules := []NormalizeRule{
		{Path: "status.userInfo.extra", Replace: "<extra>"},
		{Path: "status.userInfo.groups", KeepPrefix: "system:"},
	}
	bearer := func(name string) map[string]string {
		return map[string]string{"Authorization": "Bearer {" + name + "}"}
	}
	rules := func(sets ...[]NormalizeRule) []NormalizeRule {
		var out []NormalizeRule
		for _, s := range sets {
			out = append(out, s...)
		}
		return out
	}
	return []Probe{
		{
			// A standard user's unscoped creates, and a review with the
			// unscoped ext token through local.
			Name: "owner/local-creates-name-the-creator", OracleOnly: true, SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityStandard},
					Captures: map[string]Capture{"userid": {Path: "status.userInfo.username"}}},
				{Name: "ext-create-unscoped", Request: RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: `{"spec":{"description":"e2e-fid-owner"}}`, Identity: IdentityStandard},
					Captures: map[string]Capture{"extname": {Path: "status.bearerToken", ExtBearerName: true}, "extbearer": {Path: "status.bearerToken"}}},
				{Name: "v3-create-unscoped", Request: RequestSpec{Method: http.MethodPost, Path: v3Create, Body: `{"type":"token","description":"e2e-fid-owner"}`, Identity: IdentityStandard},
					Captures: map[string]Capture{"v3id": {Path: "id"}}},
				{Name: "review-with-the-ext-token", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityNone, Headers: bearer("extbearer")}, WaitForStatus: http.StatusCreated},
			},
			Normalize: rules(v3TokenRules, reviewRules),
			Teardown: []Step{
				deleteFixture("unscoped ext token", token.ExtTokensPath+"/{extname}"),
				deleteFixture("unscoped v3 token", v3TokenPath+"{v3id}"),
			},
		},
		{
			// The login body for json and kubeconfig, for a local user the
			// probe creates (a login needs a password).
			Name: "owner/login-body-names-no-owner", OracleOnly: true, SubstituteCaptures: true,
			Setup: []Step{ownerUserFixture(false)},
			Observations: []Observation{
				{Name: "login-json", Request: RequestSpec{Method: http.MethodPost, Path: loginPath, Body: ownerLogin("json"), Identity: IdentityNone},
					Captures: map[string]Capture{"jsonid": {Path: "id"}}},
				{Name: "login-kubeconfig", Request: RequestSpec{Method: http.MethodPost, Path: loginPath, Body: ownerLogin("kubeconfig"), Identity: IdentityNone},
					Captures: map[string]Capture{"kubeconfigid": {Path: "id"}}},
			},
			Normalize: loginRules,
			Teardown: []Step{
				deleteFixture("json login token", v3TokenPath+"{jsonid}"),
				deleteFixture("kubeconfig login token", v3TokenPath+"{kubeconfigid}"),
				deleteFixture("owner user", "/v3/users/{owneruid}"),
			},
		},
		{
			// A project member's creates scoped to the downstream, and
			// reviews with the scoped v3 token through the downstream and
			// through local.
			Name: "owner-downstream/scoped-creates-and-reviews", Downstream: true, SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityMember},
					Captures: map[string]Capture{"userid": {Path: "status.userInfo.username"}}},
				{Name: "ext-create-scoped", Request: RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath,
					Body: `{"spec":{"description":"e2e-fid-owner","clusterName":"{downstream}"}}`, Identity: IdentityMember},
					Captures: map[string]Capture{"extname": {Path: "status.bearerToken", ExtBearerName: true}}},
				{Name: "v3-create-scoped", Request: RequestSpec{Method: http.MethodPost, Path: v3Create,
					Body: `{"type":"token","description":"e2e-fid-owner","clusterId":"{downstream}"}`, Identity: IdentityMember},
					Captures: map[string]Capture{"v3id": {Path: "id"}, "v3bearer": {Path: "token"}}},
				{Name: "review-downstream-with-the-scoped-v3-token", Request: RequestSpec{Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityNone, Headers: bearer("v3bearer")}},
				{Name: "review-local-with-the-scoped-v3-token", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityNone, Headers: bearer("v3bearer")}},
			},
			Normalize: rules(v3TokenRules, reviewRules),
			Teardown: []Step{
				deleteFixture("scoped ext token", token.ExtTokensPath+"/{extname}"),
				deleteFixture("scoped v3 token", v3TokenPath+"{v3id}"),
			},
		},
		{
			// The kubeconfig_<downstream> login body, and a review through
			// the downstream with that login's token, for a local user the
			// probe creates with project-member on the downstream's Default
			// project.
			Name: "owner-downstream/cluster-login-and-review", Downstream: true, SubstituteCaptures: true,
			Setup: []Step{ownerUserFixture(true)},
			Observations: []Observation{
				{Name: "login-kubeconfig-cluster", Request: RequestSpec{Method: http.MethodPost, Path: loginPath, Body: ownerLogin("kubeconfig_{downstream}"), Identity: IdentityNone},
					Captures: map[string]Capture{"loginid": {Path: "id"}, "loginbearer": {Path: "token"}}},
				{Name: "review-downstream-with-the-login-token", Request: RequestSpec{Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityNone, Headers: bearer("loginbearer")}},
			},
			Normalize: rules(loginRules, reviewRules),
			Teardown: []Step{
				deleteFixture("cluster login token", v3TokenPath+"{loginid}"),
				deleteFixture("owner user", "/v3/users/{owneruid}"),
			},
		},
	}
}

// ownerUserFixture is localUserFixture with the baseline user GlobalRole.
func ownerUserFixture(member bool) Step {
	return localUserFixture("user", member)
}

// localUserFixture creates a local user for a probe that logs in: a name
// and a password new on every run, so no earlier run's user is looked up
// or removed, the given GlobalRole as its only global role, and, with member
// set, the project-member role on the downstream cluster's Default project.
// It records the user's name, password and id, and the id of the
// GlobalRoleBinding it created, as ownername, ownerpw, owneruid and ownergrb
// for the observations and the teardown, which deletes the user by its id;
// Rancher removes a deleted user's bindings.
func localUserFixture(globalRole string, member bool) Step {
	return Step{
		Name: "create owner user",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			var b [12]byte
			if _, err := rand.Read(b[:]); err != nil {
				return fmt.Errorf("generating the owner user's name and password: %w", err)
			}
			suffix := hex.EncodeToString(b[:])
			name, password := "e2e-fid-owner-"+suffix[:8], "e2e-fid-owner-pw-"+suffix
			created, status, err := adminJSON(ctx, tgt, http.MethodPost, "/v3/users",
				fmt.Sprintf(`{"type":"user","enabled":true,"mustChangePassword":false,"username":%q,"password":%q}`, name, password))
			if err != nil {
				return fmt.Errorf("creating the owner user: %w", err)
			}
			id, _ := created["id"].(string)
			if status != http.StatusCreated || id == "" {
				return fmt.Errorf("creating the owner user: status %d, want %d with an id", status, http.StatusCreated)
			}
			captured["ownername"], captured["ownerpw"], captured["owneruid"] = name, password, id
			binding, status, err := adminJSON(ctx, tgt, http.MethodPost, "/v3/globalrolebindings",
				fmt.Sprintf(`{"type":"globalRoleBinding","globalRoleId":%q,"userId":%q}`, globalRole, id))
			if err != nil {
				return fmt.Errorf("binding the %s role to the owner user: %w", globalRole, err)
			}
			bindingID, _ := binding["id"].(string)
			if status != http.StatusCreated || bindingID == "" {
				return fmt.Errorf("binding the %s role to the owner user: status %d, want %d with an id", globalRole, status, http.StatusCreated)
			}
			captured["ownergrb"] = bindingID
			if !member {
				return nil
			}
			downstream := captured["downstream"]
			projects, status, err := adminJSON(ctx, tgt, http.MethodGet, "/v3/projects?clusterId="+url.QueryEscape(downstream)+"&name=Default", "")
			if err != nil {
				return fmt.Errorf("looking up the downstream Default project: %w", err)
			}
			if status != http.StatusOK {
				return fmt.Errorf("looking up the downstream Default project: status %d", status)
			}
			data, _ := projects["data"].([]any)
			if len(data) != 1 {
				return fmt.Errorf("looking up the downstream Default project: %d matches", len(data))
			}
			project, _ := data[0].(map[string]any)
			projectID, _ := project["id"].(string)
			if projectID == "" {
				return fmt.Errorf("looking up the downstream Default project: entry without id")
			}
			return adminCreate(ctx, tgt, "binding project-member to the owner user", "/v3/projectroletemplatebindings",
				fmt.Sprintf(`{"type":"projectRoleTemplateBinding","projectId":%q,"roleTemplateId":"project-member","userId":%q}`, projectID, id))
		},
	}
}

// adminCreate posts one admin create and wants 201.
func adminCreate(ctx context.Context, tgt Target, what, path, body string) error {
	status, err := adminDo(ctx, tgt, http.MethodPost, path, body)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("%s: status %d, want %d", what, status, http.StatusCreated)
	}
	return nil
}
