package fidelity

import (
	"net/http"

	"github.com/rancher/rancher-cli/internal/token"
)

// revocationProbes record the answers the revokes after a local delete read:
// an ext token deleting itself with its own bearer through the local
// cluster's proxy, unscoped and scoped to a cluster, that bearer answering
// 401 once its token is gone, a token deleted by another token of the same
// user (200) and a second delete with that bearer (404), and a standard
// user's delete of its own v3 token through the local proxy (403), the
// reason the CLI never deletes a v3 token. Each probe first captures the
// user id from a review and records with every captured value substituted. Every token a probe creates is
// deleted by the name or id its create recorded.
func revocationProbes() []Probe {
	const whoBody = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`
	reviewRules := []NormalizeRule{
		{Path: "status.userInfo.extra", Replace: "<extra>"},
		{Path: "status.userInfo.groups", KeepPrefix: "system:"},
	}
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
	bearer := func(name string) map[string]string {
		return map[string]string{"Authorization": "Bearer {" + name + "}"}
	}
	whoami := func(path string, id Identity) Observation {
		return Observation{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: path, Body: whoBody, Identity: id},
			Captures: map[string]Capture{"userid": {Path: "status.userInfo.username"}}}
	}
	createExt := func(name, body string, id Identity) Observation {
		return Observation{Name: "create-" + name, Request: RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: body, Identity: id},
			Captures: map[string]Capture{name: {Path: "status.bearerToken", ExtBearerName: true}, name + "bearer": {Path: "status.bearerToken"}}}
	}
	extPath := func(name string) string { return token.ExtTokensPath + "/{" + name + "}" }
	deleteWith := func(obs, name, bearerOf string) Observation {
		return Observation{Name: obs, Request: RequestSpec{Method: http.MethodDelete, Path: extPath(name), Identity: IdentityNone, Headers: bearer(bearerOf + "bearer")}}
	}
	const unscoped = `{"spec":{"description":"e2e-fid-revoke"}}`
	return []Probe{
		{
			Name: "revoke/unscoped-ext-token-deletes-itself", OracleOnly: true, SubstituteCaptures: true,
			Observations: []Observation{
				whoami(localWhoami, IdentityStandard),
				createExt("tok", unscoped, IdentityStandard),
				deleteWith("delete-with-its-own-bearer", "tok", "tok"),
				deleteWith("its-own-bearer-after-the-delete", "tok", "tok"),
			},
			Normalize: reviewRules,
			Teardown:  []Step{deleteFixture("ext token", extPath("tok"))},
		},
		{
			Name: "revoke/ext-token-deleted-by-another-token-of-its-user", OracleOnly: true, SubstituteCaptures: true,
			Observations: []Observation{
				whoami(localWhoami, IdentityStandard),
				createExt("target", unscoped, IdentityStandard),
				createExt("other", unscoped, IdentityStandard),
				deleteWith("delete-with-the-other-token", "target", "other"),
				deleteWith("second-delete-with-the-other-token", "target", "other"),
			},
			Normalize: reviewRules,
			Teardown: []Step{
				deleteFixture("target ext token", extPath("target")),
				deleteFixture("other ext token", extPath("other")),
			},
		},
		{
			Name: "revoke/standard-user-v3-self-delete-forbidden", OracleOnly: true, SubstituteCaptures: true,
			Observations: []Observation{
				whoami(localWhoami, IdentityStandard),
				{Name: "create-v3", Request: RequestSpec{Method: http.MethodPost, Path: "/v3/tokens", Body: `{"type":"token","description":"e2e-fid-revoke"}`, Identity: IdentityStandard},
					Captures: map[string]Capture{"v3id": {Path: "id"}, "v3bearer": {Path: "token"}}},
				{Name: "delete-with-its-own-bearer", Request: RequestSpec{Method: http.MethodDelete, Path: token.V3TokensPath + "/{v3id}", Identity: IdentityNone, Headers: bearer("v3bearer")}},
			},
			Normalize: append(append([]NormalizeRule{}, reviewRules...), v3TokenRules...),
			Teardown:  []Step{deleteFixture("v3 token", token.V3TokensPath+"/{v3id}")},
		},
		{
			// A project member's token scoped to the downstream deletes itself
			// through the local cluster's proxy, the path the CLI's revoke
			// uses.
			Name: "revoke-downstream/cluster-scoped-ext-token-deletes-itself", Downstream: true, SubstituteCaptures: true,
			Observations: []Observation{
				whoami(downstreamWhoami, IdentityMember),
				createExt("tok", `{"spec":{"description":"e2e-fid-revoke","clusterName":"{downstream}"}}`, IdentityMember),
				deleteWith("delete-with-its-own-bearer", "tok", "tok"),
				deleteWith("its-own-bearer-after-the-delete", "tok", "tok"),
			},
			Normalize: reviewRules,
			Teardown:  []Step{deleteFixture("scoped ext token", extPath("tok"))},
		},
	}
}
