package fidelity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	localCore   = "/k8s/clusters/local/api/v1/"
	localRBAC   = "/k8s/clusters/local/apis/rbac.authorization.k8s.io/v1/"
	localReview = "/k8s/clusters/local/apis/authorization.k8s.io/v1/"
	localCRDs   = "/k8s/clusters/local/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
	// dwg stands for the delete of an object the caller cannot get: the
	// namespaces, roles, bindings, ConfigMap, Pod and custom resource a probe
	// creates have names that start with e2e-fid-dwg-. The
	// CustomResourceDefinition is deletionprobes.<group>, and its group starts
	// with e2e-fid-.
	dwgConfigMap     = "e2e-fid-dwg-cm"
	dwgPod           = "e2e-fid-dwg-pod"
	dwgResource      = "e2e-fid-dwg-resource"
	dwgRole          = "e2e-fid-dwg-delete-only"
	dwgCRDPlural     = "deletionprobes"
	dwgDryRunBody    = `{"dryRun":["All"]}`
	dwgOtherUID      = "00000000-0000-0000-0000-000000000000"
	dwgMismatchUID   = `{"preconditions":{"uid":"` + dwgOtherUID + `"}}`
	dwgNamespace     = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"{ns}"}}`
	dwgConfigMapBody = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + dwgConfigMap + `"},"data":{"k":"v"}}`
	// The scheduling gate keeps the Pod unscheduled: no node pulls its
	// image or runs it. The security contexts meet the restricted Pod
	// Security level: the create request passes whatever level the namespace
	// enforces.
	dwgPodBody = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + dwgPod + `"},"spec":{"schedulingGates":[{"name":"e2e-fid.example/hold"}],` +
		`"securityContext":{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}},` +
		`"containers":[{"name":"hold","image":"e2e-fid.invalid/hold:none","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}`
	dwgCRDBody = `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"` + dwgCRDPlural + `.{crdgroup}"},` +
		`"spec":{"group":"{crdgroup}","scope":"Namespaced","names":{"plural":"` + dwgCRDPlural + `","singular":"deletionprobe","kind":"DeletionProbe","listKind":"DeletionProbeList"},` +
		`"versions":[{"name":"v1","served":true,"storage":true,"schema":{"openAPIV3Schema":{"type":"object","x-kubernetes-preserve-unknown-fields":true}}}]}}`
	dwgResourceBody = `{"apiVersion":"{crdgroup}/v1","kind":"DeletionProbe","metadata":{"name":"` + dwgResource + `"},"spec":{"k":"v"}}`
	dwgRoleBody     = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"` + dwgRole + `"},"rules":[{"apiGroups":[""],"resources":["configmaps"],"verbs":["delete"]}]}`
	dwgBindingBody  = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"` + dwgRole + `"},` +
		`"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"` + dwgRole + `"},"subjects":[{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":"{userid}"}]}`
	dwgDeleteReview = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"{userid}","groups":["system:authenticated"],` +
		`"resourceAttributes":{"verb":"delete","resource":"configmaps","group":"","version":"v1","namespace":"{ns}","name":"` + dwgConfigMap + `"}}}`
	// The cluster role and its binding take the name of the namespace, new
	// on every run: a run never finds one that an earlier run left behind.
	dwgClusterRole        = "{ns}-delete-only"
	dwgClusterRoleBody    = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"` + dwgClusterRole + `"},"rules":[{"apiGroups":[""],"resources":["namespaces"],"resourceNames":["{ns}"],"verbs":["delete"]}]}`
	dwgClusterBindingBody = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"` + dwgClusterRole + `"},` +
		`"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"` + dwgClusterRole + `"},"subjects":[{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":"{userid}"}]}`
	dwgNamespaceReview = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"{userid}","groups":["system:authenticated"],` +
		`"resourceAttributes":{"verb":"delete","resource":"namespaces","group":"","version":"v1","name":"{ns}"}}}`
	// The cluster role the cluster-scope probe sends its GET for has no rules;
	// the delete-only cluster role and its binding take that cluster role's
	// name with a suffix. The name is new on every run.
	dwgTargetRoleBody       = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"{role}"},"rules":[]}`
	dwgTargetDeleteOnly     = "{role}-delete-only"
	dwgTargetDeleteOnlyBody = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"` + dwgTargetDeleteOnly + `"},"rules":[{"apiGroups":["rbac.authorization.k8s.io"],"resources":["clusterroles"],"resourceNames":["{role}"],"verbs":["delete"]}]}`
	dwgTargetBindingBody    = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"` + dwgTargetDeleteOnly + `"},` +
		`"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"` + dwgTargetDeleteOnly + `"},"subjects":[{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":"{userid}"}]}`
	dwgTargetRoleReview = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"{userid}","groups":["system:authenticated"],` +
		`"resourceAttributes":{"verb":"delete","resource":"clusterroles","group":"rbac.authorization.k8s.io","version":"v1","name":"{role}"}}}`
	dwgGroupRoleBody = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"` + dwgRole + `"},"rules":[{"apiGroups":["{crdgroup}"],"resources":["` + dwgCRDPlural + `"],"verbs":["delete"]}]}`
	dwgGroupReview   = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"{userid}","groups":["system:authenticated"],` +
		`"resourceAttributes":{"verb":"delete","resource":"` + dwgCRDPlural + `","group":"{crdgroup}","version":"v1","namespace":"{ns}","name":"` + dwgResource + `"}}}`
)

// deleteCannotGetProbes records the facts the delete of an object the caller
// cannot get depends on, through the proxy. The response to a DELETE with
// dryRun All and no precondition carries the uid of the object; it is
// recorded for a ConfigMap, a Pod, a custom resource and a namespace. The 409
// of a DELETE whose uid precondition is not the object's is recorded for a
// ConfigMap and a namespace. The 403 of a GET by a caller whose only role
// grants delete is recorded for a ConfigMap in a namespace, through the local
// cluster and through the downstream cluster, each with that caller's dry-run
// DELETE and DELETE with a uid that is not the object's; for a cluster role,
// which names the cluster scope; for a namespace, which the response names as
// the namespace of the request; and for a custom resource in an API group the
// probe creates, which names the group. Every namespace, cluster role and API
// group a probe creates has a name new on every run, and the teardown deletes
// it by the name the setup recorded. Captures are substituted: the object's
// uid records as its capture name wherever a body carries it (details.uid, the
// 409 message), and so do the namespace and cluster role names and the user id
// in the 403 message.
func deleteCannotGetProbes() []Probe {
	configMapPath := localCore + "namespaces/{ns}/configmaps/" + dwgConfigMap
	namespacePath := localCore + "namespaces/{ns}"
	crdCollection := "/k8s/clusters/local/apis/{crdgroup}/v1/namespaces/{ns}/" + dwgCRDPlural
	dryRun := func(path string, rules ...NormalizeRule) Observation {
		return Observation{Name: "dry-run-delete", Request: RequestSpec{Method: http.MethodDelete, Path: path, Body: dwgDryRunBody, Identity: IdentityAdmin}, Normalize: rules}
	}
	mismatch := func(path string) Observation {
		return Observation{Name: "delete-with-a-different-uid", Request: RequestSpec{Method: http.MethodDelete, Path: path, Body: dwgMismatchUID, Identity: IdentityAdmin}}
	}
	deleteNamespace := deleteFixture("namespace", namespacePath)
	return []Probe{
		{
			Name: "delete/configmap-dry-run-and-uid-mismatch", OracleOnly: true, SubstituteCaptures: true,
			Setup: []Step{
				randomNameStep("ns", fixturePrefix+"dwg-", ""),
				createFixture("namespace", localCore+"namespaces", dwgNamespace),
				createRecordingUID("configmap", localCore+"namespaces/{ns}/configmaps", dwgConfigMapBody, "cmuid"),
			},
			Observations: []Observation{dryRun(configMapPath), mismatch(configMapPath)},
			Teardown:     []Step{deleteNamespace},
		},
		{
			// The response is the Pod. The API server fills in its spec and status
			// and the CLI does not read them: the golden replaces both.
			Name: "delete/pod-dry-run", OracleOnly: true, SubstituteCaptures: true,
			Setup: []Step{
				randomNameStep("ns", fixturePrefix+"dwg-", ""),
				createFixture("namespace", localCore+"namespaces", dwgNamespace),
				createRecordingUID("pod", localCore+"namespaces/{ns}/pods", dwgPodBody, "poduid"),
			},
			Observations: []Observation{dryRun(localCore+"namespaces/{ns}/pods/"+dwgPod,
				NormalizeRule{Path: "spec", Replace: "<spec>"},
				NormalizeRule{Path: "status", Replace: "<status>"},
				NormalizeRule{Path: "metadata.deletionTimestamp", Replace: "<ts>"},
			)},
			Teardown: []Step{deleteNamespace},
		},
		{
			// A custom resource of a CustomResourceDefinition the probe
			// creates in an API group new on every run; the group records
			// as {crdgroup}.
			Name: "delete/custom-resource-dry-run", OracleOnly: true, SubstituteCaptures: true,
			Setup: []Step{
				randomNameStep("ns", fixturePrefix+"dwg-", ""),
				randomNameStep("crdgroup", fixturePrefix, ".example"),
				createFixture("namespace", localCore+"namespaces", dwgNamespace),
				createFixture("custom resource definition", localCRDs, dwgCRDBody),
				// The API server serves the new resource a moment after the
				// definition is accepted; until then the collection is a 404.
				waitFixture("custom resource served", http.MethodGet, crdCollection, "",
					func(_ map[string]any, status int) bool { return status == http.StatusOK }),
				createRecordingUID("custom resource", crdCollection, dwgResourceBody, "cruid"),
			},
			Observations: []Observation{dryRun(crdCollection + "/" + dwgResource)},
			Teardown: []Step{
				deleteFixture("custom resource definition", localCRDs+"/"+dwgCRDPlural+".{crdgroup}"),
				deleteNamespace,
			},
		},
		{
			// The response is the Namespace. Rancher's controllers add
			// annotations, labels and finalizers some time after a create:
			// the golden keeps only the metadata fields the API server sets.
			Name: "delete/namespace-dry-run-and-uid-mismatch", OracleOnly: true, SubstituteCaptures: true,
			Setup: []Step{
				randomNameStep("ns", fixturePrefix+"dwg-", ""),
				createRecordingUID("namespace", localCore+"namespaces", dwgNamespace, "nsuid"),
			},
			Observations: []Observation{
				dryRun(namespacePath,
					NormalizeRule{Path: "metadata", KeepKeys: []string{"name", "uid", "resourceVersion", "creationTimestamp", "deletionTimestamp"}},
					NormalizeRule{Path: "metadata.deletionTimestamp", Replace: "<ts>"},
				),
				mismatch(namespacePath),
			},
			Teardown: []Step{deleteNamespace},
		},
		deleteOnlyGetProbe("delete/get-refused-for-a-delete-only-role", false, IdentityStandard,
			localCore, localRBAC, localReview),
		deleteOnlyGetProbe("delete-downstream/get-refused-for-a-delete-only-role", true, IdentityMember,
			downstreamCore, downstreamRBAC, downstreamReview),
		deleteOnlyClusterScopeProbe(),
		deleteOnlyNamespaceProbe(),
		deleteOnlyNamedGroupProbe(),
	}
}

// deleteOnlyClusterScopeProbe is the probe of a GET of a cluster role by a
// caller whose delete-only cluster role grants delete on that cluster role and
// nothing else: the authorizer's refusal names the cluster scope in place of a
// namespace.
func deleteOnlyClusterScopeProbe() Probe {
	return Probe{
		Name: "delete/get-refused-at-the-cluster-scope", OracleOnly: true, SubstituteCaptures: true,
		Setup: []Step{
			randomNameStep("role", fixturePrefix+"dwg-", ""),
			callerNameStep(IdentityStandard, "userid"),
			createFixture("cluster role", localRBAC+"clusterroles", dwgTargetRoleBody),
			createFixture("delete-only cluster role", localRBAC+"clusterroles", dwgTargetDeleteOnlyBody),
			createFixture("delete-only cluster role binding", localRBAC+"clusterrolebindings", dwgTargetBindingBody),
			waitFixture("delete allowed", http.MethodPost, localReview+"subjectaccessreviews", dwgTargetRoleReview, sarAllowed),
		},
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: localRBAC + "clusterroles/{role}", Identity: IdentityStandard}},
		},
		Teardown: []Step{
			deleteFixture("delete-only cluster role binding", localRBAC+"clusterrolebindings/"+dwgTargetDeleteOnly),
			deleteFixture("delete-only cluster role", localRBAC+"clusterroles/"+dwgTargetDeleteOnly),
			deleteFixture("cluster role", localRBAC+"clusterroles/{role}"),
		},
	}
}

// deleteOnlyNamespaceProbe is the probe of a GET of a namespace by a caller
// whose cluster role grants delete on that namespace and nothing else: the
// API server reads the name in /namespaces/<name> as the namespace of the
// request, and the authorizer's refusal names that namespace in place of the
// cluster scope.
func deleteOnlyNamespaceProbe() Probe {
	return Probe{
		Name: "delete/get-refused-for-a-namespace", OracleOnly: true, SubstituteCaptures: true,
		Setup: []Step{
			randomNameStep("ns", fixturePrefix+"dwg-", ""),
			callerNameStep(IdentityStandard, "userid"),
			createFixture("namespace", localCore+"namespaces", dwgNamespace),
			createFixture("delete-only cluster role", localRBAC+"clusterroles", dwgClusterRoleBody),
			createFixture("delete-only cluster role binding", localRBAC+"clusterrolebindings", dwgClusterBindingBody),
			waitFixture("delete allowed", http.MethodPost, localReview+"subjectaccessreviews", dwgNamespaceReview, sarAllowed),
		},
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: localCore + "namespaces/{ns}", Identity: IdentityStandard}},
		},
		Teardown: []Step{
			deleteFixture("cluster role binding", localRBAC+"clusterrolebindings/"+dwgClusterRole),
			deleteFixture("cluster role", localRBAC+"clusterroles/"+dwgClusterRole),
			deleteFixture("namespace", localCore+"namespaces/{ns}"),
		},
	}
}

// deleteOnlyNamedGroupProbe is the probe of a GET of a custom resource by a
// caller whose role grants delete on the custom resources of an API group
// new on every run and nothing else: the authorizer's refusal and the
// Status details name that group, which the response for a core resource
// leaves out.
func deleteOnlyNamedGroupProbe() Probe {
	crdCollection := "/k8s/clusters/local/apis/{crdgroup}/v1/namespaces/{ns}/" + dwgCRDPlural
	return Probe{
		Name: "delete/get-refused-in-a-named-group", OracleOnly: true, SubstituteCaptures: true,
		Setup: []Step{
			randomNameStep("ns", fixturePrefix+"dwg-", ""),
			randomNameStep("crdgroup", fixturePrefix, ".example"),
			callerNameStep(IdentityStandard, "userid"),
			createFixture("namespace", localCore+"namespaces", dwgNamespace),
			createFixture("custom resource definition", localCRDs, dwgCRDBody),
			waitFixture("custom resource served", http.MethodGet, crdCollection, "",
				func(_ map[string]any, status int) bool { return status == http.StatusOK }),
			createFixture("custom resource", crdCollection, dwgResourceBody),
			createFixture("delete-only role", localRBAC+"namespaces/{ns}/roles", dwgGroupRoleBody),
			createFixture("delete-only role binding", localRBAC+"namespaces/{ns}/rolebindings", dwgBindingBody),
			waitFixture("delete allowed", http.MethodPost, localReview+"subjectaccessreviews", dwgGroupReview, sarAllowed),
		},
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: crdCollection + "/" + dwgResource, Identity: IdentityStandard}},
		},
		Teardown: []Step{
			deleteFixture("custom resource definition", localCRDs+"/"+dwgCRDPlural+".{crdgroup}"),
			deleteFixture("namespace", localCore+"namespaces/{ns}"),
		},
	}
}

// deleteOnlyGetProbe is the probe of the requests the caller sends to delete a
// ConfigMap in a namespace new on every run, where a plain Role and
// RoleBinding grant the caller delete on configmaps and nothing else. core,
// rbac and review are the cluster's core API, RBAC API and authorization
// review prefixes. The setup waits until the authorizer allows the delete,
// because the binding reaches it a moment after the create request; the
// observations then record, as the caller, the authorizer's refusal of the
// get, the response to a DELETE with dryRun All and no precondition, which
// carries the uid of the ConfigMap, and the 409 of a DELETE whose uid
// precondition does not match it. Neither DELETE deletes the ConfigMap. A
// downstream caller is the member, whose project-member role on the Default
// project lets it through the proxy and grants nothing in this namespace,
// which belongs to no project.
func deleteOnlyGetProbe(name string, downstream bool, caller Identity, core, rbac, review string) Probe {
	configMapPath := core + "namespaces/{ns}/configmaps/" + dwgConfigMap
	return Probe{
		Name: name, OracleOnly: !downstream, Downstream: downstream, SubstituteCaptures: true,
		Setup: []Step{
			randomNameStep("ns", fixturePrefix+"dwg-", ""),
			callerNameStep(caller, "userid"),
			createFixture("namespace", core+"namespaces", dwgNamespace),
			createRecordingUID("configmap", core+"namespaces/{ns}/configmaps", dwgConfigMapBody, "cmuid"),
			createFixture("delete-only role", rbac+"namespaces/{ns}/roles", dwgRoleBody),
			createFixture("delete-only role binding", rbac+"namespaces/{ns}/rolebindings", dwgBindingBody),
			waitFixture("delete allowed", http.MethodPost, review+"subjectaccessreviews", dwgDeleteReview, sarAllowed),
		},
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: configMapPath, Identity: caller}},
			{Name: "dry-run-delete", Request: RequestSpec{Method: http.MethodDelete, Path: configMapPath, Body: dwgDryRunBody, Identity: caller}},
			{Name: "delete-with-a-different-uid", Request: RequestSpec{Method: http.MethodDelete, Path: configMapPath, Body: dwgMismatchUID, Identity: caller}},
		},
		Teardown: []Step{deleteFixture("namespace", core+"namespaces/{ns}")},
	}
}

// randomNameStep records under key a name new on every run: prefix, eight
// hex digits, then suffix. A run then never finds an object that an earlier
// run left behind, and its teardown deletes only what it named.
func randomNameStep(key, prefix, suffix string) Step {
	return Step{
		Name: "name the " + key,
		Do: func(_ context.Context, _ Target, captured map[string]string) error {
			var b [4]byte
			if _, err := rand.Read(b[:]); err != nil {
				return fmt.Errorf("generating the %s name: %w", key, err)
			}
			captured[key] = prefix + hex.EncodeToString(b[:]) + suffix
			return nil
		},
	}
}

// callerNameStep records under key the user id the local cluster's
// SelfSubjectReview returns for caller (status.userInfo.username). Rancher's
// proxy impersonates the caller as that user, and a RoleBinding names it as
// its subject.
func callerNameStep(caller Identity, key string) Step {
	return Step{
		Name: "read the " + string(caller) + " user id",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			bearer, err := tgt.Bearer(ctx, caller)
			if err != nil {
				return fmt.Errorf("resolving %s bearer: %w", caller, err)
			}
			body, status, err := bearerJSON(ctx, tgt, bearer, http.MethodPost, localWhoami,
				`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)
			if err != nil {
				return fmt.Errorf("sending the SelfSubjectReview as %s: %w", caller, err)
			}
			st, _ := body["status"].(map[string]any)
			info, _ := st["userInfo"].(map[string]any)
			username, _ := info["username"].(string)
			if status != http.StatusCreated || username == "" {
				return fmt.Errorf("the SelfSubjectReview as %s returned status %d, want %d with a user id", caller, status, http.StatusCreated)
			}
			captured[key] = username
			return nil
		},
	}
}

// unresolvedPlaceholder matches a {name} placeholder left in a request
// body; a JSON object opens with `{"`, never with a letter.
var unresolvedPlaceholder = regexp.MustCompile(`\{[a-z][a-z0-9]*\}`)

// createRecordingUID POSTs body to the collection at pathTemplate (captures
// resolved) as admin, wants 201, and records the created object's
// metadata.uid under key.
func createRecordingUID(name, pathTemplate, body, key string) Step {
	return Step{
		Name: "create " + name,
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			path, reqBody := substitute(pathTemplate, captured), substitute(body, captured)
			if strings.Contains(path, "{") || unresolvedPlaceholder.MatchString(reqBody) {
				return fmt.Errorf("setup %s: capture in %q unresolved", name, pathTemplate)
			}
			created, status, err := adminJSON(ctx, tgt, http.MethodPost, path, reqBody)
			if err != nil {
				return fmt.Errorf("setup %s: %w", name, err)
			}
			md, _ := created["metadata"].(map[string]any)
			uid, _ := md["uid"].(string)
			if status != http.StatusCreated || uid == "" {
				return fmt.Errorf("setup %s: POST %s returned %d, want %d with a uid", name, path, status, http.StatusCreated)
			}
			captured[key] = uid
			return nil
		},
	}
}
