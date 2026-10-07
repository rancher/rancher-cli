package fidelity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/token"
)

// clusterOwnerBearer is the capture clusterOwnerFixture records the
// cluster owner's login bearer under, which the executor sends for
// IdentityClusterOwner.
const clusterOwnerBearer = "clusterownerbearer"

// clusterOwnerReadyWait bounds the wait for a new cluster owner's roles to
// reach the local authorizer and steve's cache.
const clusterOwnerReadyWait = 25 * time.Second

// callerScopeProbes record the facts the CLI's caller-scoped cluster
// lookup rests on: the local cluster's proxy refuses a cluster owner the
// management clusters, the multi-cluster-management Feature and its own
// UserAttribute while it serves projects, which steve lists too; steve
// lists and serves the owner's cluster and its namespaces; a project member reads
// its cluster and project by id through steve and is refused local's
// Cluster and a project it cannot see; a member's cluster token serves its
// cluster, and a token create is refused for local, with a message naming
// the member, and for a cluster that does not exist; an administrator's
// steve Cluster carries every key of the Kubernetes one plus steve's own;
// a standard user's steve collection is empty with no count; and a
// standard user who created a cloud credential may not list the Secrets
// in cattle-global-data but may get its own.
func callerScopeProbes() []Probe {
	const (
		local        = "/k8s/clusters/local/apis/management.cattle.io/v3"
		steve        = "/v1/management.cattle.io"
		steveCluster = steve + ".clusters/{downstream}"
		whoBody      = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`
	)
	// A steve collection or object: the envelope and the instance-chosen
	// fields go, the ids and the count stay.
	steveRules := []NormalizeRule{
		{Path: "data._id", Replace: ""},
		{Path: "links", Replace: ""},
		{Path: "actions", Replace: ""},
		{Path: "pagination", Replace: ""},
		{Path: "sort", Replace: ""},
		{Path: "filters", Replace: ""},
		{Path: "createTypes", Replace: ""},
		{Path: "resourceType", Replace: ""},
		{Path: "revision", Replace: "<revision>"},
		{Path: "data.links", Replace: ""},
		{Path: "data.actions", Replace: ""},
		{Path: "data.metadata", Replace: ""},
		{Path: "data.spec", Replace: "<spec>"},
		{Path: "data.status", Replace: ""},
		{Path: "_id", Replace: ""},
		{Path: "metadata", Replace: ""},
		{Path: "spec", Replace: "<spec>"},
		{Path: "status", Replace: "<status>"},
		{Path: "message", Replace: "<message>"},
		{Path: "details", Replace: ""},
	}
	// A token create's refusal: the Status message stays, with the
	// member's user id substituted.
	tokenRules := append(slices.DeleteFunc(slices.Clone(steveRules), func(r NormalizeRule) bool { return r.Path == "message" }),
		NormalizeRule{Path: "status.userInfo.extra", Replace: "<extra>"},
		NormalizeRule{Path: "status.userInfo.groups", KeepPrefix: "system:"})
	// The steve and the Kubernetes Cluster object whole, keys kept: a value
	// a run, a host or a deployment chooses becomes a placeholder, so the
	// golden shows every key each object carries, the same on any server
	// whose downstream is an imported k3s cluster on the same Rancher
	// minor, and keeps only the values the readers branch on:
	// status.state, each condition's type and status, and
	// spec.localClusterAuthEndpoint.enabled. A condition's optional fields
	// are removed, and the keys of annotations, status.info,
	// status.requested and status.limits are not kept: controllers fill
	// them some time after a boot.
	clusterShapeRules := []NormalizeRule{
		{Path: "links", Replace: "<links>"},
		{Path: "actions", Replace: "<actions>"},
		{Path: "metadata.annotations", Replace: "<annotations>"},
		{Path: "metadata.labels.*", Replace: "<label>"},
		{Path: "metadata.finalizers", SortBy: "."},
		{Path: "metadata.generation", Replace: "<generation>"},
		{Path: "metadata.fields", Replace: "<fields>"},
		{Path: "metadata.relationships", Replace: "<relationships>"},
		{Path: "status.conditions", SortBy: "type"},
		{Path: "status.conditions.lastUpdateTime"},
		{Path: "status.conditions.lastTransitionTime"},
		{Path: "status.conditions.message"},
		{Path: "status.conditions.reason"},
	}
	for _, spec := range []string{"spec", "status.appliedSpec", "status.failedSpec"} {
		for _, key := range []string{"displayName", "description", "fleetWorkspaceName", "internal",
			"desiredAgentImage", "desiredAuthImage", "agentImageOverride", "agentEnvVars", "dockerRootDir",
			"defaultClusterRoleForProjectMembers", "defaultPodSecurityAdmissionConfigurationTemplateName",
			"enableNetworkPolicy", "windowsPreferedCluster", "clusterSecrets.*",
			"clusterAgentDeploymentCustomization", "fleetAgentDeploymentCustomization", "webhookDeploymentCustomization",
			"importedConfig.*", "k3sConfig.*", "rke2Config.*",
			"localClusterAuthEndpoint.fqdn", "localClusterAuthEndpoint.caCerts"} {
			clusterShapeRules = append(clusterShapeRules, NormalizeRule{Path: spec + "." + key, Replace: "<value>"})
		}
	}
	for _, key := range []string{"driver", "provider", "agentImage", "authImage", "appliedAgentEnvVars",
		"agentFeatures.*", "componentStatuses", "apiEndpoint", "serviceAccountTokenSecret", "caCert",
		"capacity.*", "allocatable.*", "requested", "limits", "version.*",
		"appliedEnableNetworkPolicy", "capabilities.*", "nodeVersion", "nodeCount", "linuxWorkerCount",
		"windowsWorkerCount", "istioEnabled", "certificatesExpiration", "currentCisRunName",
		"aksStatus", "eksStatus", "gkeStatus", "aliStatus",
		"appliedClusterAgentDeploymentCustomization", "appliedClusterAgentImagePullSecretsHash",
		"appliedWebhookDeploymentCustomization", "readyReconciling", "info"} {
		clusterShapeRules = append(clusterShapeRules, NormalizeRule{Path: "status." + key, Replace: "<value>"})
	}
	// A Secret a standard user created as a cloud credential: the data
	// keys stay, never a value; the annotations' values name the creator
	// and the creation.
	credentialRules := []NormalizeRule{
		{Path: "data.*", Replace: "<secret>"},
		{Path: "metadata.annotations.*", Replace: "<annotation>"},
		{Path: "status.userInfo.extra", Replace: "<extra>"},
		{Path: "status.userInfo.groups", KeepPrefix: "system:"},
	}
	extCreate := func(cluster string) string { return `{"spec":{"clusterName":"` + cluster + `"}}` }
	return []Probe{
		{
			Name: "access-downstream/owner-management-gate", Downstream: true, SubstituteCaptures: true,
			Setup: clusterOwnerFixture(),
			Observations: []Observation{
				{Name: "get-own-cluster-through-local", Request: RequestSpec{Method: http.MethodGet, Path: local + "/clusters/{downstream}", Identity: IdentityClusterOwner}},
				{Name: "list-clusters-through-local", Request: RequestSpec{Method: http.MethodGet, Path: local + "/clusters", Identity: IdentityClusterOwner}},
				{Name: "list-own-projects-through-local", Request: RequestSpec{Method: http.MethodGet, Path: local + "/namespaces/{downstream}/projects", Identity: IdentityClusterOwner}},
				{Name: "get-mcm-feature-through-local", Request: RequestSpec{Method: http.MethodGet, Path: local + "/features/multi-cluster-management", Identity: IdentityClusterOwner}},
				{Name: "get-own-userattribute-through-local", Request: RequestSpec{Method: http.MethodGet, Path: local + "/userattributes/{owneruid}", Identity: IdentityClusterOwner}},
				// The projects the local proxy serves, through steve: Rancher
				// generates the Default and System project names, so the ids
				// become one placeholder and the order does not matter. It is
				// the one steve collection here, so the id rule touches no
				// other observation.
				{Name: "own-cluster-projects-through-steve", Request: RequestSpec{Method: http.MethodGet, Path: steve + ".projects/{downstream}", Identity: IdentityClusterOwner}},
			},
			// A UserAttribute, if the oracle serves one, carries per-login
			// timestamps.
			Normalize: append(append([]NormalizeRule{probeFixtures("metadata.name", "p-"+fixturePrefix), {Path: "data.id", Replace: "<id>"}},
				steveRules...), NormalizeRule{Path: "items", Replace: "<items>"},
				NormalizeRule{Path: "LastRefresh", Replace: "<ts>"}, NormalizeRule{Path: "lastLogin", Replace: "<ts>"}),
			Teardown: clusterOwnerTeardown(),
		},
		{
			Name: "access-downstream/owner-steve", Downstream: true, SubstituteCaptures: true,
			Setup: clusterOwnerFixture(),
			Observations: []Observation{
				{Name: "clusters", Request: RequestSpec{Method: http.MethodGet, Path: steve + ".clusters", Identity: IdentityClusterOwner}},
				{Name: "own-cluster-by-id", Request: RequestSpec{Method: http.MethodGet, Path: steveCluster, Identity: IdentityClusterOwner}},
				{Name: "own-cluster-namespaces", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/{downstream}/v1/namespaces", Identity: IdentityClusterOwner}},
			},
			Normalize: append([]NormalizeRule{probeFixtures("id", fixturePrefix)}, steveRules...),
			Teardown:  clusterOwnerTeardown(),
		},
		{
			Name: "access-downstream/member-steve-by-id", Downstream: true, SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "projects", Request: RequestSpec{Method: http.MethodGet, Path: steve + ".projects", Identity: IdentityMember},
					Captures: map[string]Capture{"project": {Path: "data.0.metadata.name"}}},
				{Name: "own-cluster-by-id", Request: RequestSpec{Method: http.MethodGet, Path: steveCluster, Identity: IdentityMember}},
				{Name: "local-cluster-by-id", Request: RequestSpec{Method: http.MethodGet, Path: steve + ".clusters/local", Identity: IdentityMember}},
				{Name: "own-project-by-id", Request: RequestSpec{Method: http.MethodGet, Path: steve + ".projects/{downstream}/{project}", Identity: IdentityMember}},
				{Name: "missing-project-by-id", Request: RequestSpec{Method: http.MethodGet, Path: steve + ".projects/{downstream}/p-nosuchproject", Identity: IdentityMember}},
				{Name: "list-projects-through-local", Request: RequestSpec{Method: http.MethodGet, Path: local + "/namespaces/{downstream}/projects", Identity: IdentityMember}},
			},
			Normalize: steveRules,
		},
		{
			Name: "access-downstream/member-cluster-token", Downstream: true, SubstituteCaptures: true,
			Setup: []Step{memberClusterTokenFixture()},
			Observations: []Observation{
				{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityMember},
					Captures: map[string]Capture{"userid": {Path: "status.userInfo.username"}}},
				{Name: "downstream-read-with-the-cluster-token", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/{downstream}/api/v1/namespaces/default",
					Identity: IdentityNone, Headers: map[string]string{"Authorization": "Bearer {bearer}"}}},
				{Name: "create-for-local", Request: RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: extCreate("local"), Identity: IdentityMember}},
				{Name: "create-for-a-missing-cluster", Request: RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: extCreate("c-nosuchcluster"), Identity: IdentityMember}},
			},
			Normalize: tokenRules,
			Teardown:  []Step{deleteFixture("member cluster token", token.ExtTokensPath+"/{token}")},
		},
		{
			Name: "access-downstream/admin-steve-cluster-shape", Downstream: true, SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "steve", Request: RequestSpec{Method: http.MethodGet, Path: steveCluster, Identity: IdentityAdmin}},
				{Name: "local", Request: RequestSpec{Method: http.MethodGet, Path: local + "/clusters/{downstream}", Identity: IdentityAdmin}},
			},
			Normalize: clusterShapeRules,
		},
		{
			Name: "access-downstream/standard-steve-empty", Downstream: true,
			Observations: []Observation{
				{Name: "clusters", Request: RequestSpec{Method: http.MethodGet, Path: steve + ".clusters", Identity: IdentityStandard}},
			},
			Normalize: steveRules,
		},
		{
			Name: "access/standard-cloud-credential-by-name", OracleOnly: true, SubstituteCaptures: true,
			Setup: []Step{standardCloudCredentialFixture()},
			Observations: []Observation{
				{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityStandard},
					Captures: map[string]Capture{"userid": {Path: "status.userInfo.username"}}},
				{Name: "list-credential-secrets", Request: RequestSpec{Method: http.MethodGet, Path: cloudCredentialSecrets, Identity: IdentityStandard}},
				{Name: "get-own-credential-secret", Request: RequestSpec{Method: http.MethodGet, Path: cloudCredentialSecrets + "/{ccname}", Identity: IdentityStandard}},
			},
			Normalize: credentialRules,
			Teardown:  []Step{deleteFixture("standard user's cloud credential", "/v3/cloudcredentials/{ccid}")},
		},
	}
}

// fixturePrefix starts the name of every namespace a probe's setup
// creates, and of every project after its "p-" (the explain fixtures);
// Rancher and Kubernetes create no namespace or project named with it.
const fixturePrefix = "e2e-fid-"

// probeFixtures drops from a steve collection's data the items whose
// string at by starts with prefix, and makes count follow: a collection a
// probe reads after other probes ran records only what no probe creates,
// however far another probe's teardown has got.
func probeFixtures(by, prefix string) NormalizeRule {
	return NormalizeRule{Path: "data", DropBy: by, DropPrefix: prefix, CountKey: "count"}
}

// cloudCredentialSecrets is the local cluster's Secrets in the namespace
// Rancher stores cloud credentials in.
const cloudCredentialSecrets = "/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets"

// standardCloudCredentialReadyWait bounds the wait for the Role Rancher
// grants a cloud credential's creator on its Secret; with the create it
// fits inside the step's deadline.
const standardCloudCredentialReadyWait = 20 * time.Second

// standardCloudCredentialFixture creates a cloud credential as the
// standard user, records its id and its Secret's name as ccid and ccname,
// and waits, bounded, until the user may get that Secret: Rancher grants
// the creator get on it by name a moment after the create.
func standardCloudCredentialFixture() Step {
	return Step{
		Name: "create the standard user's cloud credential",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			bearer, err := tgt.Bearer(ctx, IdentityStandard)
			if err != nil {
				return fmt.Errorf("resolving standard bearer: %w", err)
			}
			body, status, err := bearerJSON(ctx, tgt, bearer, http.MethodPost, "/v3/cloudcredentials",
				`{"type":"cloudCredential","name":"e2e-fid-cc","amazonec2credentialConfig":{"accessKey":"e2e-fid-access","secretKey":"e2e-fid-secret"}}`)
			if err != nil {
				return fmt.Errorf("creating the standard user's cloud credential: %w", err)
			}
			id, _ := body["id"].(string)
			_, name, ok := strings.Cut(id, ":")
			if status != http.StatusCreated || !ok || name == "" {
				return fmt.Errorf("creating the standard user's cloud credential: status %d, want %d with a namespaced id", status, http.StatusCreated)
			}
			captured["ccid"], captured["ccname"] = id, name
			deadline := time.Now().Add(standardCloudCredentialReadyWait)
			for {
				if _, status, err := bearerJSON(ctx, tgt, bearer, http.MethodGet, cloudCredentialSecrets+"/"+name, ""); err == nil && status == http.StatusOK {
					return nil
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("the standard user may not get its cloud credential's Secret within %s", standardCloudCredentialReadyWait)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
		},
	}
}

// clusterOwnerFixture creates the cluster owner: a new local user with the
// baseline user GlobalRole (ownerUserFixture), the cluster-owner role on
// the downstream cluster, and a login, whose bearer and token id it
// records; then it waits, bounded, until the local authorizer lets the
// owner get its cluster and steve lists it, since roles reach both a
// moment after the binding is created.
func clusterOwnerFixture() []Step {
	return []Step{
		ownerUserFixture(false),
		{
			Name: "bind cluster-owner and sign in",
			Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
				if err := adminCreate(ctx, tgt, "binding cluster-owner to the owner user", "/v3/clusterroletemplatebindings",
					fmt.Sprintf(`{"type":"clusterRoleTemplateBinding","clusterId":%q,"roleTemplateId":"cluster-owner","userId":%q}`,
						captured["downstream"], captured["owneruid"])); err != nil {
					return err
				}
				body, status, err := bearerJSON(ctx, tgt, "", http.MethodPost, "/v1-public/login",
					fmt.Sprintf(`{"type":"localProvider","username":%q,"password":%q,"responseType":"json"}`, captured["ownername"], captured["ownerpw"]))
				if err != nil {
					return fmt.Errorf("signing in as the cluster owner: %w", err)
				}
				bearer, _ := body["token"].(string)
				id, _ := body["id"].(string)
				if status/100 != 2 || bearer == "" || id == "" {
					return fmt.Errorf("signing in as the cluster owner: status %d, want 2xx with a token and an id", status)
				}
				captured[clusterOwnerBearer], captured["clusterownerlogin"] = bearer, id
				return nil
			},
		},
		{
			Name: "wait for the cluster owner's roles",
			Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
				bearer, downstream := captured[clusterOwnerBearer], captured["downstream"]
				review := fmt.Sprintf(`{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"get","group":"management.cattle.io","resource":"clusters","name":%q}}}`, downstream)
				deadline := time.Now().Add(clusterOwnerReadyWait)
				for {
					allowed, listed := false, false
					if body, status, err := bearerJSON(ctx, tgt, bearer, http.MethodPost, "/k8s/clusters/local/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", review); err == nil && status == http.StatusCreated {
						st, _ := body["status"].(map[string]any)
						allowed, _ = st["allowed"].(bool)
					}
					if body, status, err := bearerJSON(ctx, tgt, bearer, http.MethodGet, "/v1/management.cattle.io.clusters", ""); err == nil && status == http.StatusOK {
						data, _ := body["data"].([]any)
						listed = slices.ContainsFunc(data, func(d any) bool {
							m, _ := d.(map[string]any)
							return m["id"] == downstream
						})
					}
					if allowed && listed {
						return nil
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("the cluster owner's roles did not reach the local authorizer (%t) and steve (%t) within %s", allowed, listed, clusterOwnerReadyWait)
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Second):
					}
				}
			},
		},
	}
}

// clusterOwnerTeardown deletes the cluster owner's login token and the
// user; Rancher removes a deleted user's bindings.
func clusterOwnerTeardown() []Step {
	return []Step{
		deleteFixture("cluster owner login token", token.V3TokensPath+"/{clusterownerlogin}"),
		deleteFixture("cluster owner user", "/v3/users/{owneruid}"),
	}
}

// memberClusterTokenFixture creates an ext token scoped to the downstream
// cluster with the member's bearer, and records its bearer and name as
// bearer and token.
func memberClusterTokenFixture() Step {
	return Step{
		Name: "create the member's cluster token",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			member, err := tgt.Bearer(ctx, IdentityMember)
			if err != nil {
				return fmt.Errorf("resolving member bearer: %w", err)
			}
			body, status, err := bearerJSON(ctx, tgt, member, http.MethodPost, token.ExtTokensPath,
				fmt.Sprintf(`{"spec":{"clusterName":%q}}`, captured["downstream"]))
			if err != nil {
				return fmt.Errorf("creating the member's cluster token: %w", err)
			}
			st, _ := body["status"].(map[string]any)
			bearer, _ := st["bearerToken"].(string)
			name, _, ok := strings.Cut(strings.TrimPrefix(bearer, "ext/"), ":")
			if status != http.StatusCreated || !ok || name == "" {
				return fmt.Errorf("creating the member's cluster token: status %d, want %d with an ext bearer", status, http.StatusCreated)
			}
			captured["bearer"], captured["token"] = bearer, name
			return nil
		},
	}
}

// bearerJSON sends one request with bearer ("" sends none) and decodes a
// JSON object answer; a body that is not one decodes to nil.
func bearerJSON(ctx context.Context, tgt Target, bearer, method, path, body string) (map[string]any, int, error) {
	build := func() (*http.Request, error) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, tgt.BaseURL()+path, rdr)
		if err != nil {
			return nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return req, nil
	}
	resp, err := doResilient(tgt.Client(), build)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		out = nil
	}
	return out, resp.StatusCode, nil
}
