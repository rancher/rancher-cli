package fidelity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

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

// All returns the probe set: bearer-validation and RBAC-denial
// auth probes, facade-owned ext-token mutation probes, and kube-proxy
// mutation-pipeline probes.
func All() []Probe {
	probes := []Probe{}
	probes = append(probes, authProbes()...)
	probes = append(probes, tokProbes()...)
	probes = append(probes, mutProbes()...)
	probes = append(probes, capabilityProbes()...)
	probes = append(probes, userPasswordWorkflowProbes()...)
	probes = append(probes, accessDownstreamProbes()...)
	probes = append(probes, memberProbes()...)
	probes = append(probes, projectLookupProbes()...)
	probes = append(probes, directEndpointProbes()...)
	probes = append(probes, accessProbes()...)
	probes = append(probes, discoveryProbes()...)
	probes = append(probes, ownerProbes()...)
	probes = append(probes, loginProbes()...)
	probes = append(probes, callerScopeProbes()...)
	probes = append(probes, revocationProbes()...)
	probes = append(probes, projectNamespaceProbes()...)
	probes = append(probes, serverTableProbes()...)
	probes = append(probes, serverFactsProbes()...)
	return probes
}

// discoveryProbes records the discovery shapes the provider's
// ServerResources depends on: the aggregated document the downstream
// serves (both roots), the local cluster's answer to the aggregated header
// through the proxy (406 on Rancher v2.15.0 and v2.15.1, the aggregated
// document from v2.15.2), and a dead aggregated API group's legacy
// discovery document (503, text/plain), which is what makes the provider
// report a partial answer. The dead group is an APIService pointing at a
// service that does not exist, created and removed around the observation;
// the aggregated Stale mark for it appears only after the API server's
// availability controller runs, so that shape is asserted by unit tests,
// not a probe.
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
			// Rancher v2.15.0 and v2.15.1 redirected the local cluster's /apis
			// to /apis/, which answers the aggregated-only header with 406;
			// v2.15.2 serves the aggregated document there.
			Name: "discovery/aggregated-on-local", OracleOnly: true,
			Observations: []Observation{
				{Name: "apis", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis", Headers: map[string]string{"Accept": aggregatedAccept}, Identity: IdentityAdmin}},
			},
			Normalize: []NormalizeRule{{Path: "items", Replace: "<scrubbed>"}, {Path: "metadata", Replace: "<scrubbed>"}},
		},
		{
			Name: "discovery/dead-group-legacy-document", Downstream: true,
			Setup: []Step{
				deleteFixture("stale dead APIService", deadAPIService+"/v1beta1.dead.example"),
				createFixture("dead APIService", deadAPIService, deadBody),
				// The aggregator registers the group's route after the
				// APIService is accepted; until then the document is a 404,
				// not the 503 the dead backend answers with.
				waitFixture("dead group routed", http.MethodGet, deadGroupDoc, "",
					func(_ map[string]any, status int) bool { return status == http.StatusServiceUnavailable }),
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
// whatever Rancher E2E_FIDELITY_URL names, with E2E_FIDELITY_DOWNSTREAM set to a
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
// downstreamReview is the downstream cluster's authorization review prefix.
const (
	downstreamWhoami = "/k8s/clusters/{downstream}/apis/authentication.k8s.io/v1/selfsubjectreviews"
	localWhoami      = "/k8s/clusters/local/apis/authentication.k8s.io/v1/selfsubjectreviews"
	downstreamReview = "/k8s/clusters/{downstream}/apis/authorization.k8s.io/v1/"
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
		// An owner's uid is as instance-bound as the object's own; the
		// owner's kind and name are the fact worth keeping.
		{Path: "metadata.ownerReferences.uid", Replace: "<uid>"},
	}
}

func accessDownstreamProbes() []Probe {
	const (
		whoBody         = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`
		ssrrBody        = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","spec":{"namespace":"default"}}`
		ssarBody        = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"list","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		sarAnonBody     = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"system:anonymous","groups":["system:unauthenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
		nonexistentUser = "u-nonexistent99"
		nonexistentSA   = "system:serviceaccount:default:nonexistent-sa"
		defaultSA       = "system:serviceaccount:default:default"

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

	explainTeardown := func() []Step {
		return slices.Concat(
			[]Step{
				deleteFixture("crtb", mgmtAPI+"/namespaces/{downstream}/clusterroletemplatebindings/"+explainCRTBName),
				deleteFixture("prtb", mgmtAPI+"/namespaces/"+explainBackingNS+"/projectroletemplatebindings/"+explainPRTBName),
				deleteFixture("namespace", downstreamCore+"namespaces/"+explainNamespace),
			},
			deleteProjectFixture(explainProject),
			[]Step{
				deleteFixture("user global role binding", mgmtAPI+"/globalrolebindings/e2e-fid-explain-grb"),
				deleteUserNamed(explainUser),
			},
		)
	}
	explainFixtureChain := Probe{
		// What access explain reads on a downstream behind a PRTB and a
		// CRTB: the authorizer allows the grants (their reasons name
		// bindings with generated names, so they are scrubbed and the
		// bindings are read back as the labelled lists, sorted by role), the
		// RoleBindings and project-derived ClusterRoleBindings keyed by the
		// PRTB, the ClusterRoleBinding keyed by the CRTB, the PRTB and CRTB
		// objects on the management plane, and the admin's globaladmin
		// binding with its GlobalRole annotations. Captures are substituted
		// back so the cluster id in label keys and values records as
		// {downstream}.
		Name:               "access-downstream/explain-fixture-chain",
		Downstream:         true,
		SubstituteCaptures: true,
		Setup:              explainSetupSteps(),
		Observations: []Observation{
			{Name: "reason-prtb-grant", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarPodsNS, Identity: IdentityAdmin}},
			{Name: "reason-crtb-grant", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarNodes, Identity: IdentityAdmin}},
			{Name: "prtb-rolebindings", Request: RequestSpec{Method: http.MethodGet, Path: downstreamRBAC + "namespaces/" + explainNamespace + "/rolebindings",
				Query: url.Values{"labelSelector": {"{" + selectorPRTBRoleBindings + "}"}}, Identity: IdentityAdmin}},
			{Name: "prtb-clusterrolebindings", Request: RequestSpec{Method: http.MethodGet, Path: downstreamRBAC + "clusterrolebindings",
				Query: url.Values{"labelSelector": {"{" + selectorPRTBClusterRoleBindings + "}"}}, Identity: IdentityAdmin}},
			{Name: "crtb-clusterrolebindings", Request: RequestSpec{Method: http.MethodGet, Path: downstreamRBAC + "clusterrolebindings",
				Query: url.Values{"labelSelector": {"{" + selectorCRTBClusterRoleBindings + "}"}}, Identity: IdentityAdmin}},
			{Name: "clusterrole-project-member", Request: RequestSpec{Method: http.MethodGet, Path: downstreamRBAC + "clusterroles/project-member", Identity: IdentityAdmin}},
			{Name: "prtb-object", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/management.cattle.io.projectroletemplatebindings/" + explainBackingNS + "/" + explainPRTBName, Identity: IdentityAdmin}},
			{Name: "crtb-object", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/management.cattle.io.clusterroletemplatebindings/{downstream}/" + explainCRTBName, Identity: IdentityAdmin}},
			{Name: "admin-whoami", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityAdmin},
				Captures: map[string]Capture{"adminuid": {Path: "status.userInfo.username"}}},
			{Name: "globaladmin-binding", Request: RequestSpec{Method: http.MethodGet, Path: downstreamRBAC + "clusterrolebindings/globaladmin-{adminuid}", Identity: IdentityAdmin},
				Captures: map[string]Capture{"grb": {Path: "metadata.annotations.[authz.cluster.cattle.io/globalrolebinding]"}}},
		},
		// SAR answers keep status.allowed and lose the generated binding
		// name in the reason; management objects lose their controller
		// status; the admin's GlobalRoleBinding name is captured from the
		// globaladmin binding's own annotation (captures resolve before
		// substitution), so it records as {grb} and the clusterrole-owner
		// annotation stays.
		Normalize: append(append([]NormalizeRule{
			{Path: "status.reason", Replace: "<reason>"},
			{Path: "items", SortBy: "roleRef.name"},
			{Path: "items.metadata.name", Replace: "<name>"},
			{Path: "items.metadata.uid", Replace: "<uid>"},
			{Path: "items.metadata.resourceVersion", Replace: "<rv>"},
			{Path: "items.metadata.creationTimestamp", Replace: "<time>"},
			{Path: "items.metadata.managedFields", Replace: ""},
			{Path: "links", Replace: ""},
			{Path: "metadata.relationships", Replace: ""},
			{Path: "metadata.state", Replace: ""},
			{Path: "metadata.finalizers", Replace: ""},
			{Path: "data", Replace: "<list>"},
			{Path: "actions", Replace: ""},
			{Path: "pagination", Replace: ""},
			{Path: "sort", Replace: ""},
			{Path: "filters", Replace: ""},
			{Path: "createTypes", Replace: ""},
			{Path: "resourceType", Replace: ""},
			{Path: "revision", Replace: ""},
			{Path: "status.localConditions", Replace: ""},
			{Path: "status.remoteConditions", Replace: ""},
			{Path: "status.summary", Replace: ""},
			{Path: "status.summaryLocal", Replace: ""},
			{Path: "status.summaryRemote", Replace: ""},
			{Path: "status.observedGenerationLocal", Replace: ""},
			{Path: "status.observedGenerationRemote", Replace: ""},
			{Path: "id", Replace: "<id>"},
			{Path: "metadata.fields", Replace: ""},
			{Path: "metadata.generation", Replace: ""},
			{Path: "rules", Replace: "<rules>"},
		}, bindingMetadataRules()...), whoamiPrivacyRules()...),
		Teardown: explainTeardown(),
	}
	explainFixtureReasonGrammar := Probe{
		// The RoleBinding form of the authorizer's reason, recorded
		// verbatim: a plain Role and RoleBinding with fixed names are the
		// only grant of get on widgets.e2e-fid.example for the fixture user,
		// so the sentence carries no generated name; and a denial elsewhere
		// carries no reason.
		Name:               "access-downstream/explain-fixture-reason-grammar",
		Downstream:         true,
		SubstituteCaptures: true,
		Setup:              explainSetupSteps(),
		Observations: []Observation{
			{Name: "reason-rolebinding-of-role", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarWidgets, Identity: IdentityAdmin}},
			{Name: "denied-elsewhere", Request: RequestSpec{Method: http.MethodPost, Path: downstreamReview + "subjectaccessreviews", Body: sarPodsDefault, Identity: IdentityAdmin}},
		},
		Teardown: explainTeardown(),
	}

	return []Probe{
		callerWithoutAccess, impersonationRefused, subjectWithoutAccess, nonexistentUserProbe,
		nonexistentServiceAccount, serviceAccountIdentity, impersonateGroupDropped, rulesReviewComplete,
		sarWithoutRight, versionFieldAccepted, whoCanReasonAndBinding,
		explainFixtureChain, explainFixtureReasonGrammar,
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
		ssarListSettings       = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"list","resource":"settings","group":"management.cattle.io","version":"v3"}}}`
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
		},
		{
			// The GlobalRole back-references on the management cluster: the
			// globaladmin binding's annotations name the GlobalRoleBinding
			// (captured there, since the name is generated), the
			// cattle-globalrolebinding-<grb> ClusterRoleBinding carries the
			// grb-owner label, and the ClusterRole it binds carries the
			// gr-owner label.
			Name: "access/explain-local-globalrole-labels", OracleOnly: true, SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "admin-whoami", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityAdmin},
					Captures: map[string]Capture{"adminuid": {Path: "status.userInfo.username"}}},
				{Name: "globaladmin-binding", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/globaladmin-{adminuid}", Identity: IdentityAdmin},
					Captures: map[string]Capture{"grb": {Path: "metadata.annotations.[authz.cluster.cattle.io/globalrolebinding]"}}},
				{Name: "admin-ssar-settings", Request: RequestSpec{Method: http.MethodPost, Path: reviews + "selfsubjectaccessreviews", Body: ssarListSettings, Identity: IdentityAdmin}},
				// The admin holds two sufficient bindings on local (globaladmin-<user>
				// and cattle-globalrolebinding-<grb>); the authorizer names either,
				// so the reason is scrubbed and both bindings are read by name.
				{Name: "grb-binding", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/cattle-globalrolebinding-{grb}", Identity: IdentityAdmin}},
				{Name: "grb-clusterrole", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis/rbac.authorization.k8s.io/v1/clusterroles/cattle-globalrole-admin", Identity: IdentityAdmin}},
			},
			Normalize: append(append([]NormalizeRule{
				{Path: "status.reason", Replace: "<reason>"},
				{Path: "data", Replace: "<list>"},
				{Path: "links", Replace: ""},
				{Path: "metadata.relationships", Replace: ""},
				{Path: "metadata.state", Replace: ""},
				{Path: "metadata.finalizers", Replace: ""},
				{Path: "actions", Replace: ""},
				{Path: "pagination", Replace: ""},
				{Path: "sort", Replace: ""},
				{Path: "filters", Replace: ""},
				{Path: "createTypes", Replace: ""},
				{Path: "resourceType", Replace: ""},
				{Path: "revision", Replace: ""},
				{Path: "rules", Replace: "<rules>"},
			}, bindingMetadataRules()...), whoamiPrivacyRules()...),
		},
		{
			// The management objects the chain GETs, in the shapes it reads:
			// a RoleTemplate with roleTemplateNames and external, a
			// GlobalRole with inheritedClusterRoles (null on the built-in
			// admin), and the cr-name annotation that is informational only.
			Name: "access/explain-management-shapes", OracleOnly: true,
			Observations: []Observation{
				{Name: "roletemplate-project-member", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/management.cattle.io.roletemplates/project-member", Identity: IdentityAdmin}},
				{Name: "roletemplate-edit", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/management.cattle.io.roletemplates/edit", Identity: IdentityAdmin}},
				{Name: "globalrole-admin", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/management.cattle.io.globalroles/admin", Identity: IdentityAdmin}},
				{Name: "globalrole-user", Request: RequestSpec{Method: http.MethodGet, Path: "/v1/management.cattle.io.globalroles/user", Identity: IdentityAdmin}},
			},
			Normalize: append([]NormalizeRule{
				{Path: "links", Replace: ""},
				{Path: "metadata.relationships", Replace: ""},
				{Path: "metadata.state", Replace: ""},
				{Path: "metadata.finalizers", Replace: ""},
				{Path: "id", Replace: "<id>"},
				{Path: "status", Replace: "<status>"},
				{Path: "rules", Replace: "<rules>"},
				{Path: "metadata.fields", Replace: ""},
				{Path: "metadata.generation", Replace: ""},
				{Path: "metadata.annotations", Replace: "<annotations>"},
			}, bindingMetadataRules()...),
		},
	}
}

// directEndpointProbes records the management shapes `rancher kubeconfig`
// reads for a cluster's Authorized Cluster Endpoint: the management
// Cluster's spec.localClusterAuthEndpoint and status.caCert, the
// multi-cluster-management Feature, and the management Nodes of the
// downstream (spec.controlPlane, spec.requestedHostname, the address types
// and the Ready condition). Every instance-chosen value (names, addresses,
// certificates, timestamps, capacities, versions) is normalized with its
// presence kept, so a golden diff means the shape the command depends on
// moved.
func directEndpointProbes() []Probe {
	drop := func(paths ...string) []NormalizeRule {
		rules := make([]NormalizeRule, 0, len(paths))
		for _, p := range paths {
			rules = append(rules, NormalizeRule{Path: p, Replace: ""})
		}
		return rules
	}
	metaRules := append(bindingMetadataRules(), drop("metadata.annotations", "metadata.labels", "metadata.finalizers", "metadata.generation")...)
	return []Probe{
		{
			Name: "access-downstream/direct-endpoint-shapes", Downstream: true, SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "management-cluster", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis/management.cattle.io/v3/clusters/{downstream}", Identity: IdentityAdmin}},
				{Name: "mcm-feature", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis/management.cattle.io/v3/features/multi-cluster-management", Identity: IdentityAdmin}},
				{Name: "management-nodes", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/{downstream}/nodes", Identity: IdentityAdmin}},
			},
			Normalize: append(append(append(metaRules,
				// The cluster: the endpoint block and the CA's presence stay,
				// every other spec and status field is instance detail.
				drop("spec.agentImageOverride", "spec.clusterSecrets", "spec.description", "spec.desiredAgentImage", "spec.desiredAuthImage",
					"spec.enableNetworkPolicy", "spec.fleetWorkspaceName", "spec.importedConfig", "spec.internal", "spec.k3sConfig", "spec.rke2Config",
					"spec.windowsPreferedCluster", "spec.defaultPodSecurityAdmissionConfigurationTemplateName",
					"status.agentFeatures", "status.agentImage", "status.aksStatus", "status.aliStatus", "status.allocatable", "status.apiEndpoint",
					"status.appliedAgentEnvVars", "status.appliedEnableNetworkPolicy", "status.appliedSpec", "status.authImage", "status.capabilities",
					"status.capacity", "status.conditions", "status.driver", "status.eksStatus", "status.gkeStatus", "status.info", "status.limits",
					"status.nodeCount", "status.provider", "status.requested", "status.serviceAccountTokenSecret", "status.version",
					"status.description", "status.dynamic", "status.lockedValue")...),
				[]NormalizeRule{
					{Path: "spec.displayName", Replace: "<name>"},
					{Path: "status.caCert", Replace: "<cert>"},
					{Path: "spec.localClusterAuthEndpoint.fqdn", Replace: "<fqdn>"},
					{Path: "spec.localClusterAuthEndpoint.caCerts", Replace: "<cert>"},
					// The node list: names and addresses normalized, the
					// roles, the address types and the Ready condition kept.
					{Path: "items.metadata.name", Replace: "<name>"},
					{Path: "items.metadata.uid", Replace: "<uid>"},
					{Path: "items.metadata.resourceVersion", Replace: "<rv>"},
					{Path: "items.metadata.creationTimestamp", Replace: "<time>"},
					{Path: "items.spec.requestedHostname", Replace: "<hostname>"},
					{Path: "items.spec.displayName", Replace: "<name>"},
					{Path: "items.status.internalNodeStatus.addresses.address", Replace: "<address>"},
					{Path: "items.status.nodeAnnotations", Replace: "<annotations>"},
					{Path: "items.status.nodeLabels", Replace: "<labels>"},
					{Path: "items.status.nodeName", Replace: "<name>"},
					{Path: "items.status.internalNodeStatus.conditions.lastHeartbeatTime", Replace: "<time>"},
					{Path: "items.status.internalNodeStatus.conditions.lastTransitionTime", Replace: "<time>"},
					{Path: "items.status.internalNodeStatus.conditions.message", Replace: "<message>"},
					{Path: "items.status.internalNodeStatus.conditions.reason", Replace: "<reason>"},
				}...),
				drop("items.metadata.annotations", "items.metadata.labels", "items.metadata.finalizers", "items.metadata.generation", "items.metadata.managedFields",
					"items.metadata.generateName", "items.spec.customConfig", "items.spec.desiredNodeTaints", "items.spec.internalNodeSpec", "items.spec.metadataUpdate",
					"items.status.conditions", "items.status.limits", "items.status.requested",
					"items.status.internalNodeStatus.allocatable", "items.status.internalNodeStatus.capacity", "items.status.internalNodeStatus.daemonEndpoints",
					"items.status.internalNodeStatus.features", "items.status.internalNodeStatus.nodeInfo", "items.status.internalNodeStatus.runtimeHandlers")...),
		},
	}
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
	revoked.Observations[0].WaitForStatus = http.StatusUnauthorized
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
		// token's own generated name vary, and the principal's display and login
		// names are fixture names that differ between targets; scrub them to the
		// shape that matters.
		Normalize: []NormalizeRule{
			{Path: "metadata.name", Replace: "<name>"},
			{Path: "metadata.labels.*", Replace: "<label>"},
			{Path: "spec.userID", Replace: "<user-id>"},
			{Path: "spec.userPrincipal.name", Replace: "<principal>"},
			{Path: "spec.userPrincipal.displayName", Replace: "<display-name>"},
			{Path: "spec.userPrincipal.loginName", Replace: "<login-name>"},
		},
		Teardown: []Step{deleteFixture("created standard token", token.ExtTokensPath+"/{tokname}")},
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
	// status.bearerToken - the same mechanism OracleTarget.revokedBearer
	// uses, and one that holds whatever metadata the response carries.
	create := Observation{
		Name:     "create",
		Request:  RequestSpec{Method: http.MethodPost, Path: token.ExtTokensPath, Body: `{"spec":{}}`, Identity: IdentityAdmin},
		Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
	}
	namedPath := token.ExtTokensPath + "/{tokname}"
	revokeLeftover := deleteFixture("created token", namedPath)

	// The create/get/revoke Token bodies carry the acting admin's per-install
	// user id (spec.userID, the cattle.io/user-id label, and the userPrincipal
	// name "local://<id>"), which differs across fresh boots, and the
	// principal's display and login names, which differ between targets; scrub
	// them exactly as auth/standard-create does for its own user. metadata.labels.* stays
	// probe-level on purpose - as a default rule it would blank meaningful
	// labels on other kinds (e.g. the mut/* Secrets).
	tokenIdentity := func() []NormalizeRule {
		return []NormalizeRule{
			{Path: "metadata.name", Replace: "<name>"},
			{Path: "metadata.labels.*", Replace: "<label>"},
			{Path: "spec.userID", Replace: "<user-id>"},
			{Path: "spec.userPrincipal.name", Replace: "<principal>"},
			{Path: "spec.userPrincipal.displayName", Replace: "<display-name>"},
			{Path: "spec.userPrincipal.loginName", Replace: "<login-name>"},
		}
	}

	createShape := Probe{
		Name: "tok/create-shape",
		Observations: []Observation{
			create,
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}, WaitForStatus: http.StatusOK},
		},
		Normalize: tokenIdentity(),
		Teardown:  []Step{revokeLeftover},
	}

	dryrunDelete := Probe{
		Name: "tok/dryrun-delete",
		Observations: []Observation{
			create,
			{
				Name:    "dry-run-delete",
				Request: RequestSpec{Method: http.MethodDelete, Path: namedPath, Body: `{"dryRun":["All"]}`, Identity: IdentityAdmin},
			},
			{Name: "still-there", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}, WaitForStatus: http.StatusOK},
		},
		Normalize: tokenIdentity(),
		Teardown:  []Step{revokeLeftover},
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
		FacadeDivergence: "the facade answers 409 Conflict with no body, as Rancher v2.16 answers 409 (rancher/rancher#57111); Rancher 2.14 and 2.15, including this oracle, answer 500 InternalError with a Status",
		Teardown:         []Step{revokeLeftover},
	}

	revoke := Probe{
		Name: "tok/revoke",
		Observations: []Observation{
			create,
			{Name: "revoke", Request: RequestSpec{Method: http.MethodDelete, Path: namedPath, Identity: IdentityAdmin}},
			{Name: "gone", Request: RequestSpec{Method: http.MethodGet, Path: namedPath, Identity: IdentityAdmin}, WaitForStatus: http.StatusNotFound},
		},
		// The 404 "gone" Status names the deleted token in details.name and in
		// the message; both carry the generated token name.
		Normalize: append(tokenIdentity(),
			NormalizeRule{Path: "details.name", Replace: "<name>"},
			NormalizeRule{Path: "message", Replace: "<message>"},
		),
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

// adminJSON sends one admin-authenticated request and decodes a JSON
// body; a non-JSON body decodes to nil with the status still returned.
func adminJSON(ctx context.Context, tgt Target, method, path, body string) (map[string]any, int, error) {
	tok, err := tgt.Bearer(ctx, IdentityAdmin)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving admin bearer: %w", err)
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
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return decoded, resp.StatusCode, nil
}

// waitFixture polls an admin request every three seconds for up to two
// minutes until ready reports true on its decoded body and status:
// Rancher's controllers create the objects a fixture depends on (a
// project's backing namespace, the downstream bindings behind a CRTB or
// PRTB) after the API answered.
func waitFixture(name, method, pathTemplate, body string, ready func(decoded map[string]any, status int) bool) Step {
	return waitFixtureCaptured(name, method, pathTemplate, body,
		func(decoded map[string]any, status int, _ map[string]string) (bool, error) {
			return ready(decoded, status), nil
		})
}

// waitFixtureCaptured is waitFixture for a condition that depends on a
// value an earlier setup step stored in the probe's captures: ready also
// receives them, and an error from it fails the step at once.
func waitFixtureCaptured(name, method, pathTemplate, body string, ready func(decoded map[string]any, status int, captured map[string]string) (bool, error)) Step {
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			path := substitute(pathTemplate, captured)
			reqBody := substitute(body, captured)
			deadline := time.Now().Add(2 * time.Minute)
			for {
				decoded, status, err := adminJSON(ctx, tgt, method, path, reqBody)
				if err != nil {
					return err
				}
				done, err := ready(decoded, status, captured)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				if done {
					return nil
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("%s: not ready after 2 minutes (last status %d)", name, status)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(3 * time.Second):
				}
			}
		},
	}
}

// deleteUserNamed removes a fixture User by its metadata.name (the user
// id) and waits until it is gone, so a rerun on a kept pair starts clean
// and Rancher has cascaded the user's bindings.
func deleteUserNamed(id string) Step {
	path := "/k8s/clusters/local/apis/management.cattle.io/v3/users/" + id
	return Step{
		Name: "delete user " + id,
		Do: func(ctx context.Context, tgt Target, _ map[string]string) error {
			if status, err := adminDo(ctx, tgt, http.MethodDelete, path, ""); err != nil || (status >= 300 && status != http.StatusNotFound) {
				return fmt.Errorf("deleting %s: status %d, %w", id, status, err)
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				status, err := adminDo(ctx, tgt, http.MethodGet, path, "")
				if err != nil {
					return err
				}
				if status == http.StatusNotFound {
					return nil
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("user %s still present after delete", id)
				}
				time.Sleep(time.Second)
			}
		},
	}
}

// bindingReconciled reports whether a ProjectRoleTemplateBinding or
// ClusterRoleTemplateBinding read back from the management API carries
// what Rancher's controllers write to it after creation, every field
// the recorded golden holds. kind is "prtb" or "crtb", the token both
// controller names carry. The conditions, by grammar:
//
//   - both: status 200, the mgmt-auth controller's lifecycle annotation,
//     the cluster sync controller's lifecycle annotation (its key ends in
//     the cluster id), the resolved userPrincipalName and, on a
//     ClusterRoleTemplateBinding, status.lastUpdateTime (the classic
//     mgmt-auth controller and the aggregation handler both stamp it);
//   - classic: also both labels-updated labels, which only the classic
//     handlers write;
//   - aggregation: nothing more; the aggregation handlers never write the
//     labels-updated labels.
func bindingReconciled(decoded map[string]any, status int, kind string, grammar bindingGrammar) bool {
	if status != http.StatusOK {
		return false
	}
	if kind == "crtb" {
		st, _ := decoded["status"].(map[string]any)
		if updated, _ := st["lastUpdateTime"].(string); updated == "" {
			return false
		}
	}
	md, _ := decoded["metadata"].(map[string]any)
	annotations, _ := md["annotations"].(map[string]any)
	labels, _ := md["labels"].(map[string]any)
	if _, ok := annotations["lifecycle.cattle.io/create.mgmt-auth-"+kind+"-controller"]; !ok {
		return false
	}
	synced := false
	for key := range annotations {
		if strings.HasPrefix(key, "lifecycle.cattle.io/create.cluster-"+kind+"-sync_") {
			synced = true
		}
	}
	if !synced {
		return false
	}
	if grammar == grammarClassic {
		for _, key := range []string{"auth.management.cattle.io/crb-rb-labels-updated", "authz.cluster.cattle.io/crb-rb-labels-updated"} {
			if _, ok := labels[key]; !ok {
				return false
			}
		}
	}
	principal, _ := decoded["userPrincipalName"].(string)
	return principal != ""
}

// waitBindingReconciled is the ready function of a wait step for
// bindingReconciled: it reads the grammar the detection step stored and
// fails when none is stored, never assuming one.
func waitBindingReconciled(kind string) func(decoded map[string]any, status int, captured map[string]string) (bool, error) {
	return func(decoded map[string]any, status int, captured map[string]string) (bool, error) {
		grammar, err := storedGrammar(captured)
		if err != nil {
			return false, err
		}
		return bindingReconciled(decoded, status, kind, grammar), nil
	}
}

// sarAllowed reports whether a decoded SubjectAccessReview answer allowed.
func sarAllowed(decoded map[string]any, status int) bool {
	st, _ := decoded["status"].(map[string]any)
	allowed, _ := st["allowed"].(bool)
	return status == http.StatusCreated && allowed
}

// fixtureRetryAttempts and fixtureRetryPause bound the retry of a fixture
// create on the downstream cluster. Rancher proxies those requests through
// a tunnel to the downstream agent and answers 5xx until the agent
// reconnects. Five attempts with four pauses fit inside defaultStepTimeout.
// Tests shorten the pause and must not run in parallel.
const fixtureRetryAttempts = 5

var fixtureRetryPause = 3 * time.Second

// retriableFixtureCreate reports whether a 5xx answer to a create of body at
// path may be retried: the path goes through the proxy of the probe's
// downstream cluster and the body names its object. Such a create either
// makes the object or answers 409, which counts as done, so a retry cannot
// make a second object. A body that is not JSON, has no metadata.name or
// sets metadata.generateName is never retried.
func retriableFixtureCreate(path, body string, captured map[string]string) bool {
	id := captured["downstream"]
	if id == "" || !strings.HasPrefix(path, "/k8s/clusters/"+id+"/") {
		return false
	}
	var obj struct {
		Metadata struct {
			Name         string `json:"name"`
			GenerateName string `json:"generateName"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(body), &obj) != nil {
		return false
	}
	return obj.Metadata.Name != "" && obj.Metadata.GenerateName == ""
}

// createFixture POSTs body to pathTemplate (captures resolved) as admin.
// A 2xx or a 409 (the fixture already exists) counts as done; anything
// else is surfaced, since an observation that depends on the fixture
// would otherwise record the wrong shape. A 5xx answer to a create that
// retriableFixtureCreate accepts is retried up to fixtureRetryAttempts
// times; a transport error or any other status ends the step at once.
func createFixture(name, pathTemplate, body string) Step {
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			path := substitute(pathTemplate, captured)
			body := substitute(body, captured)
			if strings.Contains(path, "{") || strings.Contains(body, "{downstream}") {
				return fmt.Errorf("setup %s: capture in %q unresolved", name, pathTemplate)
			}
			attempts := 1
			if retriableFixtureCreate(path, body, captured) {
				attempts = fixtureRetryAttempts
			}
			for n := 1; ; n++ {
				status, err := adminDo(ctx, tgt, http.MethodPost, path, body)
				if err != nil {
					return fmt.Errorf("setup %s: %w", name, err)
				}
				if status == http.StatusConflict || (status >= 200 && status < 300) {
					return nil
				}
				if status < 500 || n >= attempts {
					if n > 1 {
						return fmt.Errorf("setup %s: POST %s answered %d after %d attempts", name, path, status, n)
					}
					return fmt.Errorf("setup %s: POST %s answered %d", name, path, status)
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("setup %s: POST %s answered %d, retry ended: %w", name, path, status, ctx.Err())
				case <-time.After(fixtureRetryPause):
				}
			}
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

// deleteProjectFixture returns the steps that delete the project named
// project from the management cluster, then wait until the project and its
// backing namespace on the local cluster both answer 404. Rancher removes a
// project in the background after the delete answers; a probe that runs
// next in the same cluster must not start before the project and its
// backing namespace are gone.
func deleteProjectFixture(project string) []Step {
	gone := func(_ map[string]any, status int) bool { return status == http.StatusNotFound }
	projectPath := mgmtAPI + "/namespaces/{downstream}/projects/" + project
	return []Step{
		deleteFixture("project "+project, projectPath),
		waitFixture("project "+project+" gone", http.MethodGet, projectPath, "", gone),
		waitFixture("backing namespace of project "+project+" gone", http.MethodGet, "/k8s/clusters/local/api/v1/namespaces/{downstream}-"+project, "", gone),
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

// memberProbes record what a project member sees, for access clusters: the
// steve cluster collection answers with exactly the member's cluster, the
// proxy admits the member to the downstream, and the management-plane reads
// the command's GRANTED BY side needs are refused (a bodyless 403 for the
// Kubernetes-style cluster LIST and GET; a Status for the cluster-namespace
// and all-namespace ClusterRoleTemplateBinding LISTs, the all-namespace
// ProjectRoleTemplateBinding LIST, the GlobalRoleBinding and GlobalRole
// LISTs, and the downstream namespace LIST), which is what makes the
// command's bindingsDiscovery unavailable for a member. The member identity is a local user with
// one project-member ProjectRoleTemplateBinding on the downstream's Default
// project.
func memberProbes() []Probe {
	const (
		steveClusters        = "/v1/management.cattle.io.clusters"
		mgmtClusters         = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
		mgmtCRTBs            = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/{downstream}/clusterroletemplatebindings"
		mgmtAllCRTBs         = "/k8s/clusters/local/apis/management.cattle.io/v3/clusterroletemplatebindings"
		mgmtAllPRTBs         = "/k8s/clusters/local/apis/management.cattle.io/v3/projectroletemplatebindings"
		mgmtGRBs             = "/k8s/clusters/local/apis/management.cattle.io/v3/globalrolebindings"
		mgmtGlobalRoles      = "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"
		mgmtUsers            = "/k8s/clusters/local/apis/management.cattle.io/v3/users"
		mgmtUserAttributes   = "/k8s/clusters/local/apis/management.cattle.io/v3/userattributes"
		downstreamNamespaces = "/k8s/clusters/{downstream}/api/v1/namespaces"
		steveProjects        = "/v1/management.cattle.io.projects"
		steveNamespaces      = "/k8s/clusters/{downstream}/v1/namespaces"
		downstreamSSAR       = "/k8s/clusters/{downstream}/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
		getNamespaceBody     = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"get","resource":"namespaces","version":"v1","name":"%[1]s","namespace":"%[1]s"}}}`
		whoBody              = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`
	)
	// Steve stashes the pre-transform id under _id only when an item goes
	// through its transform a second time, which depends on whether the
	// SQL cache served it; the field is dropped rather than recorded.
	steveEnvelopeRules := []NormalizeRule{
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
	}
	statusRules := []NormalizeRule{
		{Path: "message", Replace: "<message>"},
		{Path: "metadata", Replace: ""},
		{Path: "details", Replace: ""},
	}
	return []Probe{
		{
			Name:               "access-downstream/member-steve-clusters",
			Downstream:         true,
			SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "clusters", Request: RequestSpec{Method: http.MethodGet, Path: steveClusters, Identity: IdentityMember}},
			},
			Normalize: steveEnvelopeRules,
		},
		{
			Name:       "access-downstream/member-proxy-whoami",
			Downstream: true,
			Observations: []Observation{
				{Name: "whoami", Request: RequestSpec{Method: http.MethodPost, Path: downstreamWhoami, Body: whoBody, Identity: IdentityMember}},
			},
			Normalize: append(whoamiPrivacyRules(), NormalizeRule{Path: "status.userInfo.username", Replace: "<user-id>"}),
		},
		{
			Name:               "access-downstream/member-management-refusals",
			Downstream:         true,
			SubstituteCaptures: true,
			Observations: []Observation{
				// The member's own id, for the two self reads below: a user
				// may read its own User but not its own UserAttribute, and
				// that refusal is the one that turns groupAliases unavailable.
				{Name: "whoami-local", Request: RequestSpec{Method: http.MethodPost, Path: localWhoami, Body: whoBody, Identity: IdentityMember},
					Captures: map[string]Capture{"member": {Path: "status.userInfo.username"}}},
				{Name: "get-own-user", Request: RequestSpec{Method: http.MethodGet, Path: mgmtUsers + "/{member}", Identity: IdentityMember}},
				{Name: "get-own-userattribute", Request: RequestSpec{Method: http.MethodGet, Path: mgmtUserAttributes + "/{member}", Identity: IdentityMember}},
				{Name: "list-clusters", Request: RequestSpec{Method: http.MethodGet, Path: mgmtClusters, Identity: IdentityMember}},
				{Name: "get-own-cluster", Request: RequestSpec{Method: http.MethodGet, Path: mgmtClusters + "/{downstream}", Identity: IdentityMember}},
				{Name: "list-crtbs", Request: RequestSpec{Method: http.MethodGet, Path: mgmtCRTBs, Identity: IdentityMember}},
				{Name: "list-all-crtbs", Request: RequestSpec{Method: http.MethodGet, Path: mgmtAllCRTBs, Identity: IdentityMember}},
				{Name: "list-all-prtbs", Request: RequestSpec{Method: http.MethodGet, Path: mgmtAllPRTBs, Identity: IdentityMember}},
				{Name: "list-grbs", Request: RequestSpec{Method: http.MethodGet, Path: mgmtGRBs, Identity: IdentityMember}},
				{Name: "list-globalroles", Request: RequestSpec{Method: http.MethodGet, Path: mgmtGlobalRoles, Identity: IdentityMember}},
				{Name: "list-downstream-namespaces", Request: RequestSpec{Method: http.MethodGet, Path: downstreamNamespaces, Identity: IdentityMember}},
			},
			Normalize: append(whoamiPrivacyRules(), append(statusRules,
				// The own-User read carries a condition timestamp.
				NormalizeRule{Path: "status.conditions.lastUpdateTime", Replace: "<time>"})...),
		},
		{
			// What access projects reads for a member: the steve project
			// collection (exactly the member's project), the downstream's
			// steve namespace collection (exactly that project's namespaces,
			// labeled and annotated with the project id) where the
			// Kubernetes LIST is refused, and the per-namespace review the
			// command asks: get
			// the namespace, allowed for the project's own namespace and
			// denied for another project's.
			Name:               "access-downstream/member-steve-projects-and-namespaces",
			Downstream:         true,
			SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "projects", Request: RequestSpec{Method: http.MethodGet, Path: steveProjects, Identity: IdentityMember},
					Captures: map[string]Capture{"project": {Path: "data.0.metadata.name"}}},
				{Name: "namespaces", Request: RequestSpec{Method: http.MethodGet, Path: steveNamespaces, Identity: IdentityMember}},
				{Name: "kube-namespaces", Request: RequestSpec{Method: http.MethodGet, Path: downstreamNamespaces, Identity: IdentityMember}},
				{Name: "get-own-namespace", Request: RequestSpec{Method: http.MethodPost, Path: downstreamSSAR, Body: fmt.Sprintf(getNamespaceBody, "default"), Identity: IdentityMember}},
				{Name: "get-other-namespace", Request: RequestSpec{Method: http.MethodPost, Path: downstreamSSAR, Body: fmt.Sprintf(getNamespaceBody, "kube-system"), Identity: IdentityMember}},
			},
			Normalize: append(statusRules,
				NormalizeRule{Path: "links", Replace: ""},
				NormalizeRule{Path: "actions", Replace: ""},
				NormalizeRule{Path: "pagination", Replace: ""},
				NormalizeRule{Path: "sort", Replace: ""},
				NormalizeRule{Path: "filters", Replace: ""},
				NormalizeRule{Path: "createTypes", Replace: ""},
				NormalizeRule{Path: "resourceType", Replace: ""},
				NormalizeRule{Path: "revision", Replace: "<revision>"},
				NormalizeRule{Path: "data._id", Replace: ""},
				NormalizeRule{Path: "data.links", Replace: ""},
				NormalizeRule{Path: "data.actions", Replace: ""},
				NormalizeRule{Path: "data.status", Replace: ""},
				// The namespace item keeps its name, its labels and its
				// field.cattle.io/projectId annotation (the project
				// mapping); the rest of its metadata is instance data. The
				// annotations rule also reaches the Project item in the same
				// answer.
				NormalizeRule{Path: "data.metadata.annotations", KeepKeys: []string{"field.cattle.io/projectId"}},
				NormalizeRule{Path: "data.metadata.managedFields", Replace: ""},
				NormalizeRule{Path: "data.metadata.fields", Replace: ""},
				NormalizeRule{Path: "data.metadata.creationTimestamp", Replace: ""},
				NormalizeRule{Path: "data.metadata.resourceVersion", Replace: ""},
				NormalizeRule{Path: "data.metadata.uid", Replace: ""},
				NormalizeRule{Path: "data.metadata.relationships", Replace: ""},
				NormalizeRule{Path: "data.metadata.state", Replace: ""},
				NormalizeRule{Path: "data.metadata.generation", Replace: ""},
				// The project item: display name kept, the rest of spec is
				// instance data; the allowed review's reason names a
				// generated ClusterRoleBinding.
				NormalizeRule{Path: "data.spec.resourceQuota", Replace: ""},
				NormalizeRule{Path: "data.spec.namespaceDefaultResourceQuota", Replace: ""},
				NormalizeRule{Path: "data.spec.containerDefaultResourceLimit", Replace: ""},
				NormalizeRule{Path: "data.spec.description", Replace: ""},
				NormalizeRule{Path: "status.reason", Replace: "<reason>"},
			),
		},
	}
}
