package fidelity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
)

// crtCollectionPath is the Steve collection the bearer-validation auth probes
// read. It is testrancher's exported constant (fidelity imports its parent),
// so there is one copy on the test side.
const crtCollectionPath = testrancher.CRTCollectionPath

// localSecretsPath is the kube-proxy path prefix for core/v1 Secrets in the
// local cluster - the exact URL shape client-go's dynamic client produces for
// internal/client/provider's Create/Update/Delete calls.
const localSecretsPath = "/k8s/clusters/local/api/v1/namespaces/default/secrets"

// All returns the first-slice probe set: bearer-validation and RBAC-denial
// auth probes, facade-owned ext-token mutation probes, and kube-proxy
// mutation-pipeline probes. Each family's oracle target is documented in the
// spec ("which layer it oracles").
func All() []Probe {
	probes := []Probe{}
	probes = append(probes, authProbes()...)
	probes = append(probes, tokProbes()...)
	probes = append(probes, mutProbes()...)
	probes = append(probes, capabilityProbes()...)
	probes = append(probes, userPasswordWorkflowProbes()...)
	probes = append(probes, accessDownstreamProbes()...)
	probes = append(probes, accessProbes()...)
	probes = append(probes, discoveryProbes()...)
	return probes
}

// discoveryProbes records the discovery shapes the provider's
// ServerResources depends on: the aggregated document the downstream
// serves (both roots), the local cluster's refusal to serve it through
// the proxy (a legacy list whatever the Accept header says), and a dead
// aggregated API group's legacy discovery document (503, text/plain),
// which is what makes client-go report a partial answer. The dead group
// is an APIService pointing at a service that does not exist, created
// and removed around the observation; the aggregated Stale mark for it
// appears only after the API server's availability controller runs, so
// that shape is a recorded spike fact asserted by unit tests, not a probe.
func discoveryProbes() []Probe {
	const (
		aggregatedAccept = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList," +
			"application/json;g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList"
		deadAPIService = "/k8s/clusters/{downstream}/apis/apiregistration.k8s.io/v1/apiservices"
		deadGroupDoc   = "/k8s/clusters/{downstream}/apis/dead.example/v1beta1"
	)
	deadBody := `{"apiVersion":"apiregistration.k8s.io/v1","kind":"APIService","metadata":{"name":"v1beta1.dead.example"},` +
		`"spec":{"group":"dead.example","version":"v1beta1","groupPriorityMinimum":1000,"versionPriority":15,"insecureSkipTLSVerify":true,` +
		`"service":{"name":"e2e-fid-nonexistent","namespace":"default"}}}`
	return []Probe{
		{
			Name: "discovery/aggregated-downstream", Downstream: true,
			Observations: []Observation{
				{Name: "apis", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/{downstream}/apis", Headers: map[string]string{"Accept": aggregatedAccept}, Identity: IdentityAdmin}},
				{Name: "api", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/{downstream}/api", Headers: map[string]string{"Accept": aggregatedAccept}, Identity: IdentityAdmin}},
			},
			Normalize: []NormalizeRule{{Path: "items", Replace: "<scrubbed>"}, {Path: "metadata", Replace: "<scrubbed>"}},
		},
		{
			Name: "discovery/aggregated-not-served-on-local", OracleOnly: true,
			Observations: []Observation{
				{Name: "apis", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis", Headers: map[string]string{"Accept": aggregatedAccept}, Identity: IdentityAdmin}},
			},
			Normalize: []NormalizeRule{{Path: "groups", Replace: "<scrubbed>"}},
		},
		{
			Name: "discovery/dead-group-legacy-document", Downstream: true,
			Setup: []Step{
				deleteFixture("stale dead APIService", deadAPIService+"/v1beta1.dead.example"),
				createFixture("dead APIService", deadAPIService, deadBody),
			},
			Observations: []Observation{
				{Name: "group-version-document", Request: RequestSpec{Method: http.MethodGet, Path: deadGroupDoc, Identity: IdentityAdmin}},
			},
			Teardown: []Step{deleteFixture("dead APIService", deadAPIService+"/v1beta1.dead.example")},
		},
	}
}

// accessDownstreamProbes asserts the proxy behaviors the access suite relies
// on that only appear on a downstream cluster: the local route drops
// Impersonate-User and Impersonate-Group, so cluster-access refusal,
// impersonation refusal, and a service account's own identity are
// unrecordable against the local-only pinned oracle. Recorded against
// whatever Rancher E2E_REAL_URL names, with E2E_REAL_DOWNSTREAM set to a
// downstream cluster it has.
//
// Unlike the pinned Docker oracle, this target is a real, named instance:
// its admin caller has a real identity, real group memberships, and a real
// login history. A downstream golden holds status codes, Content-Types, and
// the strings Kubernetes or Rancher fix in code (error shapes, field names,
// literal messages for names this suite itself chose, like
// "u-nonexistent99"); every value the instance chose - user and token ids,
// identity-provider principals, e-mail addresses, group names, credential
// ids, timestamps, and cluster ids embedded inside messages - is
// normalized, never recorded verbatim. The standard user is provisioned
// fresh per target, so its username is normalized wherever it appears; the
// admin identity is stable across runs but still holds real personal data,
// so its username and its userInfo.extra and groups are normalized the
// same way.
// downstreamWhoami and localWhoami are the SelfSubjectReview endpoints the
// access-downstream family posts to; probes_test.go checks every observation
// against these same constants to require the whoami privacy rules below.
const (
	downstreamWhoami = "/k8s/clusters/{downstream}/apis/authentication.k8s.io/v1/selfsubjectreviews"
	localWhoami      = "/k8s/clusters/local/apis/authentication.k8s.io/v1/selfsubjectreviews"
)

// whoamiPrivacyRules normalizes the two userInfo fields every whoami
// response carries that the instance, not Kubernetes or Rancher, chose:
// extra (identity-provider principals with e-mail addresses, the live
// token id, username aliases) is replaced whole, and groups keeps only the
// system:-prefixed elements a fact like "system:masters is absent" or a
// service account's own group set depends on. Returns a fresh slice per
// call so callers can append probe-specific rules without aliasing.
func whoamiPrivacyRules() []NormalizeRule {
	return []NormalizeRule{
		{Path: "status.userInfo.extra", Replace: "<extra>"},
		{Path: "status.userInfo.groups", KeepPrefix: "system:"},
		{Path: "status.userInfo.uid", Replace: "<uid>"},
	}
}

// bindingMetadataRules scrubs the fields of a recorded RBAC binding that
// the instance, not Kubernetes, chose. Returns a fresh slice per call.
func bindingMetadataRules() []NormalizeRule {
	return []NormalizeRule{
		{Path: "metadata.uid", Replace: "<uid>"},
		{Path: "metadata.resourceVersion", Replace: "<rv>"},
		{Path: "metadata.creationTimestamp", Replace: "<time>"},
		{Path: "metadata.managedFields", Replace: ""},
	}
}

func accessDownstreamProbes() []Probe {
	const (
		downstreamReview = "/k8s/clusters/{downstream}/apis/authorization.k8s.io/v1/"
		whoBody          = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`
		ssrrBody         = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","spec":{"namespace":"default"}}`
		ssarBody         = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"list","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		sarAnonBody      = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:anonymous","groups":["system:unauthenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		nonexistentUser  = "u-nonexistent99"
		nonexistentSA    = "system:serviceaccount:default:nonexistent-sa"
		defaultSA        = "system:serviceaccount:default:default"

		sarSchedulerDefault    = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:kube-scheduler","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		sarSchedulerKubeSystem = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:kube-scheduler","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"kube-system"}}}`
		sarMasters             = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"groups":["system:masters"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		clusterAdminBinding    = "/k8s/clusters/{downstream}/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/cluster-admin"
	)

	callerWithoutAccess := Probe{
		Name:       "access-downstream/caller-without-cluster-access",
		Downstream: true,
		Observations: []Observation{
			{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityStandard}},
		},
		Normalize: append(whoamiPrivacyRules(), NormalizeRule{Path: "Message", Replace: "<message>"}),
	}

	impersonationRefused := Probe{
		Name:       "access-downstream/impersonation-refused",
		Downstream: true,
		Observations: []Observation{
			{
				Name:     "admin-whoami",
				Request:  RequestSpec{Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityAdmin},
				Captures: map[string]Capture{"adminUser": {Path: "status.userInfo.username"}},
			},
			{
				Name: "impersonate",
				Request: RequestSpec{
					Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityStandard,
					Headers: map[string]string{"Impersonate-User": "{adminUser}"},
				},
			},
		},
		Normalize: append(whoamiPrivacyRules(), NormalizeRule{Path: "status.userInfo.username", Replace: "<user-id>"}),
	}

	subjectWithoutAccess := Probe{
		Name:       "access-downstream/subject-without-cluster-access",
		Downstream: true,
		Observations: []Observation{
			{
				Name:     "standard-whoami-local",
				Request:  RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityStandard},
				Captures: map[string]Capture{"stdUser": {Path: "status.userInfo.username"}},
			},
			{
				Name: "impersonate",
				Request: RequestSpec{
					Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityAdmin,
					Headers: map[string]string{"Impersonate-User": "{stdUser}"},
				},
			},
		},
		Normalize: append(whoamiPrivacyRules(),
			NormalizeRule{Path: "status.userInfo.username", Replace: "<user-id>"},
			NormalizeRule{Path: "Message", Replace: "<message>"},
		),
	}

	nonexistentUserProbe := Probe{
		Name:       "access-downstream/nonexistent-user",
		Downstream: true,
		Observations: []Observation{
			{
				Name: "impersonate",
				Request: RequestSpec{
					Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityAdmin,
					Headers: map[string]string{"Impersonate-User": nonexistentUser},
				},
			},
		},
		Normalize: append(whoamiPrivacyRules(), NormalizeRule{Path: "Message", Replace: "<message>"}),
	}

	nonexistentServiceAccount := Probe{
		Name:       "access-downstream/nonexistent-service-account",
		Downstream: true,
		Observations: []Observation{
			{
				Name: "impersonate",
				Request: RequestSpec{
					Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityAdmin,
					Headers: map[string]string{"Impersonate-User": nonexistentSA},
				},
			},
		},
		Normalize: whoamiPrivacyRules(),
	}

	serviceAccountIdentity := Probe{
		Name:       "access-downstream/service-account-identity",
		Downstream: true,
		Observations: []Observation{
			{
				Name: "impersonate",
				Request: RequestSpec{
					Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityAdmin,
					Headers: map[string]string{"Impersonate-User": defaultSA},
				},
			},
		},
		Normalize: whoamiPrivacyRules(),
	}

	impersonateGroupDropped := Probe{
		Name:       "access-downstream/impersonate-group-dropped",
		Downstream: true,
		Observations: []Observation{
			{
				Name: "whoami",
				Request: RequestSpec{
					Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityAdmin,
					Headers: map[string]string{"Impersonate-Group": "system:masters"},
				},
			},
		},
		Normalize: append(whoamiPrivacyRules(), NormalizeRule{Path: "status.userInfo.username", Replace: "<user-id>"}),
	}

	rulesReviewComplete := Probe{
		Name:       "access-downstream/rules-review-complete",
		Downstream: true,
		Observations: []Observation{
			{Name: "ssrr", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "selfsubjectrulesreviews", Body: ssrrBody, Identity: IdentityAdmin}},
		},
		Normalize: []NormalizeRule{
			{Path: "status.resourceRules", Replace: "<scrubbed>"},
			{Path: "status.nonResourceRules", Replace: "<scrubbed>"},
		},
	}

	sarWithoutRight := Probe{
		Name:       "access-downstream/sar-without-right",
		Downstream: true,
		Observations: []Observation{
			{
				Name: "sar",
				Request: RequestSpec{
					Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarAnonBody, Identity: IdentityAdmin,
					Headers: map[string]string{"Impersonate-User": defaultSA},
				},
			},
		},
		Normalize: []NormalizeRule{
			{Path: "message", Replace: "<message>"},
			{Path: "details.*", Replace: "<scrubbed>"},
		},
	}

	versionFieldAccepted := Probe{
		Name:       "access-downstream/version-field-accepted",
		Downstream: true,
		Observations: []Observation{
			{Name: "ssar", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "selfsubjectaccessreviews", Body: ssarBody, Identity: IdentityAdmin}},
		},
		Normalize: []NormalizeRule{{Path: "status.reason", Replace: "<reason>"}},
	}

	whoCanReasonAndBinding := Probe{
		// The same facts on a downstream cluster through the proxy: the
		// authorizer's reason grammar for a ClusterRoleBinding grant,
		// identical across namespaces, the privileged group allowed with no
		// reason, and the binding shape the candidates are read from.
		Name:       "access-downstream/who-can-reason-and-binding-shape",
		Downstream: true,
		Observations: []Observation{
			{Name: "scheduler-default", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarSchedulerDefault, Identity: IdentityAdmin}},
			{Name: "scheduler-kube-system", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarSchedulerKubeSystem, Identity: IdentityAdmin}},
			{Name: "masters-no-reason", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarMasters, Identity: IdentityAdmin}},
			{Name: "cluster-admin-binding", Request: RequestSpec{Method: http.MethodGet, Path: clusterAdminBinding, Identity: IdentityAdmin}},
		},
		Normalize: bindingMetadataRules(),
	}

	return []Probe{
		callerWithoutAccess, impersonationRefused, subjectWithoutAccess, nonexistentUserProbe,
		nonexistentServiceAccount, serviceAccountIdentity, impersonateGroupDropped, rulesReviewComplete,
		sarWithoutRight, versionFieldAccepted, whoCanReasonAndBinding,
	}
}

// accessProbes records what the access commands rely on from the
// authorization and authentication review endpoints through the proxy on
// the management cluster. OracleOnly: the facade has no review endpoints;
// the e2e shim models them for the scenario suite. The downstream half of
// the suite's facts (the impersonation refusal, the cluster-access refusal,
// a service account the cluster does not have) needs a cluster behind
// /k8s/clusters/<id>, which this oracle does not have.
func accessProbes() []Probe {
	const (
		reviews  = "/k8s/clusters/local/apis/authorization.k8s.io/v1/"
		whoami   = "/k8s/clusters/local/apis/authentication.k8s.io/v1/selfsubjectreviews"
		ssrrBody = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","spec":{"namespace":"default"}}`
		ssarBody = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"list","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		sarAnon  = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:anonymous","groups":["system:unauthenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		whoBody  = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`

		sarSchedulerDefault    = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:kube-scheduler","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		sarSchedulerKubeSystem = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:kube-scheduler","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"kube-system"}}}`
		sarControllerManager   = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:kube-controller-manager","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		sarMasters             = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"groups":["system:masters"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		sarUnauthHealthz       = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"groups":["system:unauthenticated"],"nonResourceAttributes":{"verb":"get","path":"/healthz"}}}`
		clusterAdminBinding    = "/k8s/clusters/local/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/cluster-admin"
	)
	// The username, the principal, and the token id are all created per
	// run (the standard identity is a fresh user each time): the facts
	// here are the group set and the extra keys the proxy attaches, not
	// which user was asked.
	userField := []NormalizeRule{
		{Path: "status.userInfo.username", Replace: "<user-id>"},
		{Path: "status.userInfo.uid", Replace: ""},
		{Path: "status.userInfo.extra.principalid", Replace: "<principal>"},
		{Path: "status.userInfo.extra.requesttokenid", Replace: "<token-id>"},
	}
	return []Probe{
		{
			// The standard user's identity through the proxy: the
			// impersonating service account presents the Rancher user id
			// and Rancher's group set (system:authenticated and
			// system:cattle:authenticated), never the SA's own groups.
			Name: "access/whoami-through-proxy", OracleOnly: true,
			Observations: []Observation{{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: whoami, Body: whoBody, Identity: IdentityStandard}}},
			Normalize:    userField,
		},
		{
			// Impersonate-User on the local route is dropped, not refused:
			// the answer is 201 with the caller's own identity, where a
			// honored header for a nonexistent user would be a refusal.
			Name: "access/impersonate-user-dropped-on-local", OracleOnly: true,
			Observations: []Observation{{Name: "whoami-with-header", Request: RequestSpec{Method: http.MethodPost, Path: whoami, Body: whoBody,
				Headers: map[string]string{"Impersonate-User": "u-nonexistent"}, Identity: IdentityAdmin}}},
			Normalize: userField,
		},
		{
			// A rules review on a stock management cluster answers
			// complete, and reports the standard user's non-resource rules
			// as Rancher's fixed set for that role. The resource rules are
			// scrubbed: their ORDER varies run to run (the aggregation
			// follows binding creation) and one of them names the
			// per-run user id, so the recorded fact is incomplete: false
			// plus the non-resource set. The CLI sorts and merges the
			// resource rules itself before rendering them.
			Name: "access/rules-review-standard-user-default", OracleOnly: true,
			Observations: []Observation{{Name: "ssrr", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "selfsubjectrulesreviews", Body: ssrrBody, Identity: IdentityStandard}}},
			// Both rule lists are aggregated from the subject's ClusterRoles in
			// an order the server does not fix: two runs of the same oracle
			// image returned the same non-resource rules swapped.
			Normalize: []NormalizeRule{{Path: "status.resourceRules", Replace: "<unordered>"}, {Path: "status.nonResourceRules", Replace: "<unordered>"}},
		},
		{
			// spec.resourceAttributes.version is accepted; a negative
			// answer carries no reason; a positive one names the binding.
			Name: "access/self-access-review-with-version", OracleOnly: true,
			Observations: []Observation{
				{Name: "standard", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "selfsubjectaccessreviews", Body: ssarBody, Identity: IdentityStandard}},
				{Name: "admin", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "selfsubjectaccessreviews", Body: ssarBody, Identity: IdentityAdmin}},
			},
			// The admin's reason names its boot-generated globaladmin
			// binding; the fact recorded is that a reason is present and
			// the answer allowed, so the text is replaced.
			Normalize: []NormalizeRule{{Path: "status.reason", Replace: "<reason>"}},
		},
		{
			// A SubjectAccessReview needs the create right on the
			// management cluster: the standard user is refused with a
			// Kubernetes Status body, the admin is answered;
			// system:anonymous with system:unauthenticated is denied
			// without a reason.
			Name: "access/subject-access-review-right-and-anonymous", OracleOnly: true,
			Observations: []Observation{
				{Name: "standard-refused", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "subjectaccessreviews", Body: sarAnon, Identity: IdentityStandard}},
				{Name: "admin-anonymous", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "subjectaccessreviews", Body: sarAnon, Identity: IdentityAdmin}},
			},
			Normalize: []NormalizeRule{{Path: "message", Replace: "<message>"}, {Path: "details.*", Replace: "<scrubbed>"}},
		},
		{
			// The SubjectAccessReview answers who-can classifies, about
			// subjects Kubernetes' bootstrap policy fixes: the reason
			// grammar for a ClusterRoleBinding grant (system:kube-scheduler,
			// identical in two namespaces - the collapse evidence), a denial
			// without a reason (system:kube-controller-manager on pods), the
			// privileged group allowed with NO reason (system:masters, so
			// the fixed row never collapses), and a non-resource grant to
			// system:unauthenticated.
			Name: "access/subject-access-review-reason-grammar", OracleOnly: true,
			Observations: []Observation{
				{Name: "scheduler-default", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "subjectaccessreviews", Body: sarSchedulerDefault, Identity: IdentityAdmin}},
				{Name: "scheduler-kube-system", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "subjectaccessreviews", Body: sarSchedulerKubeSystem, Identity: IdentityAdmin}},
				{Name: "controller-manager-denied", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "subjectaccessreviews", Body: sarControllerManager, Identity: IdentityAdmin}},
				{Name: "masters-no-reason", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "subjectaccessreviews", Body: sarMasters, Identity: IdentityAdmin}},
				{Name: "unauthenticated-healthz", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "subjectaccessreviews", Body: sarUnauthHealthz, Identity: IdentityAdmin}},
			},
		},
		{
			// The binding object shape who-can reads its candidates from:
			// subjects[] carrying kind, apiGroup, and name. The bootstrap
			// cluster-admin binding is the one whose subject Kubernetes
			// fixes; the instance-chosen metadata is scrubbed.
			Name: "access/bootstrap-clusterrolebinding-shape", OracleOnly: true,
			Observations: []Observation{{Name: "cluster-admin", Request: RequestSpec{Method: http.MethodGet, Path: clusterAdminBinding, Identity: IdentityAdmin}}},
			Normalize:    bindingMetadataRules(),
		}}
}

// userPasswordWorkflowProbes asserts the facts `create user`/`create
// password-change-request`'s capability gate depends on: the ext.cattle.io
// discovery documents the checker parses (group-level preferred version,
// then the resource list naming passwordchangerequests) - read by a
// standard, non-admin identity, since the discovery is readable regardless
// of the caller's RBAC and the checker relies on that - and the
// cattle-local-user-passwords namespace the password Secret is written
// into. A golden diff here on a future server version means this workflow's
// wire shape changed and the capability gate/create-user composite need a
// fresh look, not that the fidelity suite itself broke.
func userPasswordWorkflowProbes() []Probe {
	return []Probe{
		{
			Name:       "user-password-workflow/discovery-and-namespace",
			OracleOnly: true,
			Observations: []Observation{
				{Name: "ext-group", Request: RequestSpec{Method: http.MethodGet, Path: "/apis/ext.cattle.io", Identity: IdentityStandard}},
				{Name: "ext-v1-resources", Request: RequestSpec{Method: http.MethodGet, Path: "/apis/ext.cattle.io/v1", Identity: IdentityStandard}},
				{Name: "password-namespace", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/api/v1/namespaces/cattle-local-user-passwords", Identity: IdentityAdmin}},
			},
			// The namespace's annotations/labels carry the boot's generated
			// "local" project id (e.g. field.cattle.io/projectId:
			// "local:p-<random>"), which is server-generated per boot and
			// not stable across boots. What this probe asserts is the
			// namespace's existence/phase, not its incidental
			// project-scoping metadata, so both maps are scrubbed whole
			// rather than fighting the per-boot id with a placeholder (the
			// dotted NormalizeRule path syntax cannot address a key that
			// itself contains dots, e.g. "field.cattle.io/projectId").
			Normalize: []NormalizeRule{
				{Path: "metadata.annotations.*", Replace: "<scrubbed>"},
				{Path: "metadata.labels.*", Replace: "<scrubbed>"},
			},
		},
	}
}

// capabilityProbes asserts the two wire reads the Prime capability detector's
// truth table decides on: the unauthenticated edition string and a named
// Feature's effective-value and prime-classification fields. oidc-provider
// is the one live case known to be un-tiered pre-v2.16 (status.prime false
// on both editions) - a golden diff here means the server started
// classifying it, which changes what outcome the detector produces.
func capabilityProbes() []Probe {
	return []Probe{
		{
			Name:       "capability/edition-and-oidc-provider-classification",
			OracleOnly: true,
			Observations: []Observation{
				{Name: "edition", Request: RequestSpec{Method: http.MethodGet, Path: "/rancherversion", Identity: IdentityNone}},
				{
					Name:    "oidc-provider-feature",
					Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis/management.cattle.io/v3/features/oidc-provider", Identity: IdentityAdmin},
				},
			},
		},
	}
}

func authProbes() []Probe {
	crtGet := func(name string, id Identity) Probe {
		return Probe{
			Name: name,
			Observations: []Observation{
				{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: crtCollectionPath, Identity: id}},
			},
		}
	}
	// Same delta for all three bearer-rejection identities: the composite
	// facade's bearer check rejects the read with a bodyless 401, where the
	// real server answers 401 application/json with a Norman error envelope.
	const bearer401Divergence = "facade bearer-check returns a bodyless 401 with an empty Content-Type; the oracle returns 401 application/json with a Norman error body {message,status,type}"
	noToken := crtGet("auth/no-token", IdentityNone)
	noToken.FacadeDivergence = bearer401Divergence
	garbage := crtGet("auth/garbage-token", IdentityGarbage)
	garbage.FacadeDivergence = bearer401Divergence
	revoked := crtGet("auth/revoked-token", IdentityRevoked)
	revoked.FacadeDivergence = bearer401Divergence

	standardCreate := Probe{
		Name: "auth/standard-create",
		Observations: []Observation{
			{
				Name:     "create",
				Request:  RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: `{"spec":{}}`, Identity: IdentityStandard},
				Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
			},
		},
		// The standard user is provisioned fresh each run, so its id (embedded
		// in the token name, the user-id label, and the userPrincipal) and the
		// token's own generated name vary; scrub them to the shape that matters.
		Normalize: []NormalizeRule{
			{Path: "metadata.name", Replace: "<name>"},
			{Path: "metadata.labels.*", Replace: "<label>"},
			{Path: "spec.userID", Replace: "<user-id>"},
			{Path: "spec.userPrincipal.name", Replace: "<principal>"},
		},
		FacadeDivergence: "facade create returns a Token carrying only {apiVersion,kind,metadata:{name,resourceVersion},status:{bearerToken,expiresAt}}; the oracle returns the full ext Token with metadata (labels, generateName, uid), spec (ttl, userID, userPrincipal), and status (current/expired/lastUpdateTime)",
		Teardown:         []Step{deleteFixture("created standard token", token.ExtTokensPath+"/{tokname}")},
	}

	forbiddenRead := Probe{
		Name:       "auth/forbidden-read",
		OracleOnly: true,
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/secrets", Identity: IdentityStandard}},
		},
		// The Steve collection carries a top-level revision (its list
		// resourceVersion) that advances between runs, plus links.self and
		// createTypes.secret which embed the harness bind address; every value
		// under links/createTypes is a URL, so scrub them so a re-record
		// against a different address does not gratuitously diverge.
		Normalize: []NormalizeRule{
			{Path: "revision", Replace: "<rev>"},
			{Path: "links.*", Replace: "<url>"},
			{Path: "createTypes.*", Replace: "<url>"},
		},
	}
	forbiddenWrite := Probe{
		Name:       "auth/forbidden-write",
		OracleOnly: true,
		Observations: []Observation{
			{
				Name: "post",
				Request: RequestSpec{
					Method:   http.MethodPost,
					Path:     "/v1/management.cattle.io.globalroles",
					Body:     `{"apiVersion":"management.cattle.io/v3","kind":"GlobalRole","metadata":{"name":"e2e-fid-forbidden"}}`,
					Identity: IdentityStandard,
				},
			},
		},
	}
	return []Probe{noToken, garbage, revoked, standardCreate, forbiddenRead, forbiddenWrite}
}

func tokProbes() []Probe {
	// The created name is derived from the ext-shaped bearer under
	// status.bearerToken - the same mechanism RealTarget.revokedBearer
	// uses, and one that holds whatever metadata the response carries.
	create := Observation{
		Name:     "create",
		Request:  RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: `{"spec":{}}`, Identity: IdentityAdmin},
		Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
	}
	namedPath := token.ExtTokensPath + "/{tokname}"
	revokeLeftover := deleteFixture("created token", namedPath)

	// The facade's ext-token store returns a sparse Token; every full-Token
	// observation therefore diverges on shape (missing metadata labels/uid,
	// spec.userPrincipal, and status.hash/current/expired/lastUpdateTime, plus
	// an empty spec.clusterName the facade emits on GET).
	const tokenShapeDivergence = "facade returns a Token carrying {metadata.name/rv, status:{bearerToken,expiresAt}} on create and {metadata.name/rv, spec.clusterName, status.expiresAt} on GET; the oracle returns the full ext Token with metadata labels/generateName/uid, spec ttl/userID/userPrincipal, and status hash/current/expired/lastUpdateTime"

	// The create/get/revoke Token bodies carry the acting admin's per-install
	// user id (spec.userID, the cattle.io/user-id label, and the userPrincipal
	// name "local://<id>"), which differs across fresh boots; scrub it exactly
	// as auth/standard-create does for its own user. metadata.labels.* stays
	// probe-level on purpose - as a default rule it would blank meaningful
	// labels on other kinds (e.g. the mut/* Secrets).
	tokenIdentity := func() []NormalizeRule {
		return []NormalizeRule{
			{Path: "metadata.name", Replace: "<name>"},
			{Path: "metadata.labels.*", Replace: "<label>"},
			{Path: "spec.userID", Replace: "<user-id>"},
			{Path: "spec.userPrincipal.name", Replace: "<principal>"},
		}
	}

	createShape := Probe{
		Name: "tok/create-shape",
		Observations: []Observation{
			create,
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		Normalize:        tokenIdentity(),
		FacadeDivergence: tokenShapeDivergence,
		Teardown:         []Step{revokeLeftover},
	}

	dryrunDelete := Probe{
		Name: "tok/dryrun-delete",
		Observations: []Observation{
			create,
			{
				Name:    "dry-run-delete",
				Request: RequestSpec{Method: http.MethodDelete, Path: namedPath, Body: `{"dryRun":["All"]}`, Identity: IdentityAdmin},
			},
			{Name: "still-there", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		Normalize:        tokenIdentity(),
		FacadeDivergence: tokenShapeDivergence,
		Teardown:         []Step{revokeLeftover},
	}

	stalePrecondition := Probe{
		Name: "tok/stale-precondition-delete",
		Observations: []Observation{
			create,
			{
				Name: "stale-delete",
				Request: RequestSpec{
					Method:   http.MethodDelete,
					Path:     namedPath,
					Body:     `{"preconditions":{"resourceVersion":"0-stale"}}`,
					Identity: IdentityAdmin,
				},
			},
		},
		// The 500 Status embeds the generated token name and its live
		// resourceVersion in the top-level message and in details.causes.
		Normalize: append(tokenIdentity(),
			NormalizeRule{Path: "message", Replace: "<message>"},
			NormalizeRule{Path: "details.causes.message", Replace: "<message>"},
		),
		FacadeDivergence: "facade returns 409 Conflict with no body where the 2.14.3 oracle returns 500 InternalError with a k8s Status for a stale preconditions.resourceVersion delete; the create observation also diverges on Token shape (minimal vs full).",
		Teardown:         []Step{revokeLeftover},
	}

	revoke := Probe{
		Name: "tok/revoke",
		Observations: []Observation{
			create,
			{Name: "revoke", Request: RequestSpec{Method: http.MethodDelete, Path: namedPath, Identity: IdentityAdmin}},
			{Name: "gone", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}},
		},
		// The 404 "gone" Status names the deleted token in details.name and in
		// the message; both carry the generated token name.
		Normalize: append(tokenIdentity(),
			NormalizeRule{Path: "details.name", Replace: "<name>"},
			NormalizeRule{Path: "message", Replace: "<message>"},
		),
		FacadeDivergence: "facade create and revoke return a sparse Token; the oracle returns the full ext Token (metadata labels/generateName/uid, spec userPrincipal, status hash/current/expired/lastUpdateTime). The 404 gone observation conforms.",
	}
	return []Probe{createShape, dryrunDelete, stalePrecondition, revoke}
}

func mutProbes() []Probe {
	secretBody := func(name string) string {
		return fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":%q},"stringData":{"k":"v"}}`, name)
	}
	secretUpdate := func(name, rv, label string) string {
		return fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":%q,"resourceVersion":%q,"labels":{"step":%q}},"stringData":{"k":"v"}}`, name, rv, label)
	}
	getRV := func(name string) Observation {
		return Observation{
			Name:     "get",
			Request:  RequestSpec{Method: http.MethodGet, Path: localSecretsPath + "/" + name, Identity: IdentityAdmin},
			Captures: map[string]Capture{"rv1": {Path: "metadata.resourceVersion"}},
		}
	}
	dryRunAll := url.Values{"dryRun": []string{"All"}}

	dryrunCreate := Probe{
		Name:  "mut/dryrun-create",
		Setup: []Step{deleteFixture("stale dry-run fixture", localSecretsPath+"/e2e-fid-dryrun")},
		Observations: []Observation{
			{
				Name:    "dry-run-create",
				Request: RequestSpec{Method: http.MethodPost, Path: localSecretsPath, Query: dryRunAll, Body: secretBody("e2e-fid-dryrun"), Identity: IdentityAdmin},
			},
			{Name: "not-persisted", Request: RequestSpec{Method: http.MethodGet, Path: localSecretsPath + "/e2e-fid-dryrun", Identity: IdentityAdmin}},
		},
		Teardown: []Step{deleteFixture("dry-run leftover", localSecretsPath+"/e2e-fid-dryrun")},
	}

	dryrunInvalid := Probe{
		Name: "mut/dryrun-invalid",
		Observations: []Observation{
			{
				Name: "dry-run-invalid",
				Request: RequestSpec{
					Method: http.MethodPost, Path: localSecretsPath, Query: dryRunAll,
					Body:     `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"e2e-fid-Bad_Name!"}}`,
					Identity: IdentityAdmin,
				},
			},
		},
	}

	staleUpdate := Probe{
		Name:  "mut/stale-rv-update",
		Setup: []Step{createSecretFixture("e2e-fid-rv", secretBody("e2e-fid-rv"))},
		Observations: []Observation{
			getRV("e2e-fid-rv"),
			{
				Name:    "bump",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-rv", Body: secretUpdate("e2e-fid-rv", "{rv1}", "bump"), Identity: IdentityAdmin},
			},
			{
				Name:    "stale-update",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-rv", Body: secretUpdate("e2e-fid-rv", "{rv1}", "stale"), Identity: IdentityAdmin},
			},
		},
		Teardown: []Step{deleteFixture("rv fixture", localSecretsPath+"/e2e-fid-rv")},
	}

	staleDelete := Probe{
		Name:  "mut/rv-precondition-delete",
		Setup: []Step{createSecretFixture("e2e-fid-del", secretBody("e2e-fid-del"))},
		Observations: []Observation{
			getRV("e2e-fid-del"),
			{
				Name:    "bump",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-del", Body: secretUpdate("e2e-fid-del", "{rv1}", "bump"), Identity: IdentityAdmin},
			},
			{
				Name:    "stale-delete",
				Request: RequestSpec{Method: http.MethodDelete, Path: localSecretsPath + "/e2e-fid-del", Body: `{"preconditions":{"resourceVersion":"{rv1}"}}`, Identity: IdentityAdmin},
			},
		},
		// The 409 Status message quotes both the precondition and the record
		// resourceVersion, which advance between runs.
		Normalize: []NormalizeRule{{Path: "message", Replace: "<message>"}},
		Teardown:  []Step{deleteFixture("del fixture", localSecretsPath+"/e2e-fid-del")},
	}

	// The dry-run PUT and the real PUT send the byte-identical body - the
	// CLI safety invariant is that execution applies exactly the object the
	// preview rendered, so the probe must not vary it between the two.
	planned := secretUpdate("e2e-fid-seq", "{rv1}", "planned")
	dryrunThenApply := Probe{
		Name:  "mut/dryrun-then-apply",
		Setup: []Step{createSecretFixture("e2e-fid-seq", secretBody("e2e-fid-seq"))},
		Observations: []Observation{
			getRV("e2e-fid-seq"),
			{
				Name:    "dry-run-update",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-seq", Query: dryRunAll, Body: planned, Identity: IdentityAdmin},
			},
			{
				Name:    "apply",
				Request: RequestSpec{Method: http.MethodPut, Path: localSecretsPath + "/e2e-fid-seq", Body: planned, Identity: IdentityAdmin},
			},
		},
		Teardown: []Step{deleteFixture("seq fixture", localSecretsPath+"/e2e-fid-seq")},
	}

	return []Probe{dryrunCreate, dryrunInvalid, staleUpdate, staleDelete, dryrunThenApply}
}

// adminDo sends one admin-authenticated request for fixture steps and
// returns the status code. Transport and bearer failures are real errors -
// steps surface them, never swallow them.
func adminDo(ctx context.Context, tgt Target, method, path, body string) (int, error) {
	tok, err := tgt.Bearer(ctx, IdentityAdmin)
	if err != nil {
		return 0, fmt.Errorf("resolving admin bearer: %w", err)
	}
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
		req.Header.Set("Authorization", "Bearer "+tok)
		return req, nil
	}
	resp, err := doResilient(tgt.Client(), build)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// createFixture POSTs body to pathTemplate (captures resolved) as admin.
// A 2xx or a 409 (the fixture already exists) counts as done; anything
// else is surfaced, since an observation that depends on the fixture
// would otherwise record the wrong shape.
func createFixture(name, pathTemplate, body string) Step {
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			path := substitute(pathTemplate, captured)
			if strings.Contains(path, "{") {
				return fmt.Errorf("setup %s: capture in %q unresolved", name, pathTemplate)
			}
			status, err := adminDo(ctx, tgt, http.MethodPost, path, body)
			if err != nil {
				return fmt.Errorf("setup %s: %w", name, err)
			}
			if status == http.StatusConflict || (status >= 200 && status < 300) {
				return nil
			}
			return fmt.Errorf("setup %s: POST %s answered %d", name, path, status)
		},
	}
}

// deleteFixture DELETEs pathTemplate (captures resolved) as admin. Only an
// explicit 404 or a 2xx counts as clean; auth and transport failures are
// surfaced. An unresolved template is surfaced too - the fixture may have
// materialized without its capture resolving, and a silent skip would leak
// it on a kept server.
func deleteFixture(name, pathTemplate string) Step {
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			path := substitute(pathTemplate, captured)
			if strings.Contains(path, "{") {
				return fmt.Errorf("cleanup %s: capture in %q unresolved; a fixture may have leaked", name, pathTemplate)
			}
			status, err := adminDo(ctx, tgt, http.MethodDelete, path, "")
			if err != nil {
				return fmt.Errorf("cleanup %s: %w", name, err)
			}
			if status == http.StatusNotFound || (status >= 200 && status < 300) {
				return nil
			}
			return fmt.Errorf("cleanup %s: unexpected status %d", name, status)
		},
	}
}

// createSecretFixture is idempotent delete-before-create: an interrupted
// prior run's leftover is removed first, so a kept-container rerun never
// trips AlreadyExists.
func createSecretFixture(name, body string) Step {
	path := localSecretsPath + "/" + name
	return Step{
		Name: "create " + name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			if err := deleteFixture("stale "+name, path).Do(ctx, tgt, captured); err != nil {
				return err
			}
			status, err := adminDo(ctx, tgt, http.MethodPost, localSecretsPath, body)
			if err != nil {
				return fmt.Errorf("creating fixture %s: %w", name, err)
			}
			if status != http.StatusCreated {
				return fmt.Errorf("creating fixture %s: status %d, want %d", name, status, http.StatusCreated)
			}
			return nil
		},
	}
}
