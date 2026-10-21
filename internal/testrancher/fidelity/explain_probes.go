package fidelity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"strings"
)

const (
	mgmtAPI          = "/k8s/clusters/local/apis/management.cattle.io/v3"
	explainUser      = "e2e-fid-explain-user"
	explainProject   = "p-e2e-fid-explain"
	explainNamespace = "e2e-fid-explain"
	explainBackingNS = "{downstream}-" + explainProject
	explainPRTBName  = "e2e-fid-explain-prtb"
	explainCRTBName  = "e2e-fid-explain-crtb"
	downstreamRBAC   = "/k8s/clusters/{downstream}/apis/rbac.authorization.k8s.io/v1/"
	downstreamCore   = "/k8s/clusters/{downstream}/api/v1/"
	userBody         = `{"apiVersion":"management.cattle.io/v3","kind":"User","metadata":{"name":"` + explainUser + `"},"username":"e2e-fid-explain","displayName":"e2e-fid-explain","enabled":true,"principalIds":["local://` + explainUser + `"]}`
	grbBody          = `{"apiVersion":"management.cattle.io/v3","kind":"GlobalRoleBinding","metadata":{"name":"e2e-fid-explain-grb"},"userName":"` + explainUser + `","globalRoleName":"user"}`
	projectBody      = `{"apiVersion":"management.cattle.io/v3","kind":"Project","metadata":{"name":"` + explainProject + `","namespace":"{downstream}"},"spec":{"clusterName":"{downstream}","displayName":"e2e-fid-explain"}}`
	namespaceBody    = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"` + explainNamespace + `","annotations":{"field.cattle.io/projectId":"{downstream}:` + explainProject + `"}}}`
	prtbBody         = `{"apiVersion":"management.cattle.io/v3","kind":"ProjectRoleTemplateBinding","metadata":{"name":"` + explainPRTBName + `","namespace":"` + explainBackingNS + `"},"userName":"` + explainUser + `","roleTemplateName":"project-member","projectName":"{downstream}:` + explainProject + `"}`
	crtbBody         = `{"apiVersion":"management.cattle.io/v3","kind":"ClusterRoleTemplateBinding","metadata":{"name":"` + explainCRTBName + `","namespace":"{downstream}"},"userName":"` + explainUser + `","roleTemplateName":"cluster-member","clusterName":"{downstream}"}`
	// The plain Role grants a resource no RoleTemplate grants (a private
	// API group), so the authorizer can name only this binding for it;
	// with a shared grant it names whichever sufficient binding it
	// visits first, and the PRTB's bindings carry generated names.
	roleBody       = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"e2e-fid-explain-role","namespace":"` + explainNamespace + `"},"rules":[{"apiGroups":["e2e-fid.example"],"resources":["widgets"],"verbs":["get"]}]}`
	rbBody         = `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"e2e-fid-explain-rb","namespace":"` + explainNamespace + `"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"e2e-fid-explain-role"},"subjects":[{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":"` + explainUser + `"}]}`
	sarPodsNS      = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"` + explainUser + `","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"` + explainNamespace + `"}}}`
	sarNodes       = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"` + explainUser + `","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"nodes","group":"","version":"v1"}}}`
	sarWidgets     = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"` + explainUser + `","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"widgets","group":"e2e-fid.example","version":"v1","namespace":"` + explainNamespace + `"}}}`
	sarPodsDefault = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"` + explainUser + `","groups":["system:authenticated"],"resourceAttributes":{"verb":"get","resource":"pods","group":"","version":"v1","namespace":"default"}}}`
)

// explainSetupSteps creates the fixture chain through the management
// cluster's Kubernetes API with names the probe chooses (a Step cannot
// capture a generated name, so nothing generated is needed later),
// deleting leftovers first so a rerun on a kept pair starts clean, and
// waits for Rancher's controllers to put the grants on the downstream.
// The first step reads which label grammar the server writes on the
// bindings it creates; the waits and the observations ask for the labels
// of that grammar. explainTeardown removes it all. It returns a fresh
// slice on every call: two probes share it.
func explainSetupSteps() []Step {
	return []Step{
		detectBindingGrammarStep(),
		deleteUserNamed(explainUser),
		deleteFixture("stale project", mgmtAPI+"/namespaces/{downstream}/projects/"+explainProject),
		deleteFixture("stale namespace", downstreamCore+"namespaces/"+explainNamespace),
		// A deleted project's backing namespace on the management cluster
		// and the fixture namespace on the downstream linger while their
		// finalizers run; Rancher's project webhook refuses (500) a
		// project whose backing namespace still exists, so the setup
		// waits until the project, its backing namespace, and the
		// downstream namespace are all gone.
		waitFixture("stale project gone", http.MethodGet, mgmtAPI+"/namespaces/{downstream}/projects/"+explainProject, "",
			func(_ map[string]any, status int) bool { return status == http.StatusNotFound }),
		waitFixture("stale backing namespace gone", http.MethodGet, "/k8s/clusters/local/api/v1/namespaces/"+explainBackingNS, "",
			func(_ map[string]any, status int) bool { return status == http.StatusNotFound }),
		waitFixture("stale namespace gone", http.MethodGet, downstreamCore+"namespaces/"+explainNamespace, "",
			func(_ map[string]any, status int) bool { return status == http.StatusNotFound }),
		createFixture("user", mgmtAPI+"/users", userBody),
		createFixture("user global role binding", mgmtAPI+"/globalrolebindings", grbBody),
		createFixture("project", mgmtAPI+"/namespaces/{downstream}/projects", projectBody),
		waitFixture("backing namespace", http.MethodGet, "/k8s/clusters/local/api/v1/namespaces/"+explainBackingNS, "",
			func(_ map[string]any, status int) bool { return status == http.StatusOK }),
		createFixture("namespace", downstreamCore+"namespaces", namespaceBody),
		createFixture("prtb", mgmtAPI+"/namespaces/"+explainBackingNS+"/projectroletemplatebindings", prtbBody),
		createFixture("crtb", mgmtAPI+"/namespaces/{downstream}/clusterroletemplatebindings", crtbBody),
		// The probes read the two bindings back as objects; the recorded
		// bodies carry what Rancher's controllers write after the API
		// answered (the lifecycle annotations, the labels-updated labels of
		// the classic grammar, userPrincipalName), so the setup waits for
		// that decoration.
		waitFixtureCaptured("prtb reconciled", http.MethodGet, mgmtAPI+"/namespaces/"+explainBackingNS+"/projectroletemplatebindings/"+explainPRTBName, "",
			waitBindingReconciled("prtb")),
		waitFixtureCaptured("crtb reconciled", http.MethodGet, mgmtAPI+"/namespaces/{downstream}/clusterroletemplatebindings/"+explainCRTBName, "",
			waitBindingReconciled("crtb")),
		createFixture("plain role", downstreamRBAC+"namespaces/"+explainNamespace+"/roles", roleBody),
		createFixture("plain rolebinding", downstreamRBAC+"namespaces/"+explainNamespace+"/rolebindings", rbBody),
		waitFixture("prtb grant live", http.MethodPost, downstreamReview+"subjectaccessreviews", sarPodsNS, sarAllowed),
		waitFixture("crtb grant live", http.MethodPost, downstreamReview+"subjectaccessreviews", sarNodes, sarAllowed),
	}
}

// bindingGrammar is the set of labels Rancher writes on the RoleBindings
// and ClusterRoleBindings it creates for a ProjectRoleTemplateBinding or
// ClusterRoleTemplateBinding. The classic grammar is the default before
// Rancher v2.16; the aggregation grammar replaces it from v2.16 on, when
// the aggregated-roletemplates feature is on.
type bindingGrammar string

const (
	grammarClassic     bindingGrammar = "classic"
	grammarAggregation bindingGrammar = "aggregation"
)

// The captures the detection step stores. Their names start with
// unrecordedPrefix; no recorded body or text has a stored value rewritten
// into its placeholder. A stored selector is a complete string with the
// cluster id already in it; it never contains a placeholder.
const (
	grammarKey                         = unrecordedPrefix + "binding-grammar"
	selectorPRTBRoleBindings           = unrecordedPrefix + "prtb-rolebindings-selector"
	selectorPRTBClusterRoleBindings    = unrecordedPrefix + "prtb-clusterrolebindings-selector"
	selectorCRTBClusterRoleBindings    = unrecordedPrefix + "crtb-clusterrolebindings-selector"
	aggregatedRoleTemplatesFeaturePath = mgmtAPI + "/features/aggregated-roletemplates"
	aggregationLabelKey                = "management.cattle.io/roletemplate-aggregation"
)

// detectBindingGrammarStep reads the aggregated-roletemplates feature as
// the admin, decides which grammar Rancher writes on the bindings, and
// stores the grammar and the three label selectors the observations send
// (each with the cluster id resolved). It is a setup step: its answer is
// never recorded.
func detectBindingGrammarStep() Step {
	return Step{
		Name: "binding grammar",
		Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
			decoded, status, err := adminJSON(ctx, tgt, http.MethodGet, aggregatedRoleTemplatesFeaturePath, "")
			if err != nil {
				return fmt.Errorf("GET %s: %w", aggregatedRoleTemplatesFeaturePath, err)
			}
			if status != http.StatusOK {
				return fmt.Errorf("GET %s answered status %d; want 200", aggregatedRoleTemplatesFeaturePath, status)
			}
			if decoded == nil {
				return fmt.Errorf("GET %s answered a body that is not a JSON object; want the Feature object", aggregatedRoleTemplatesFeaturePath)
			}
			on, err := aggregatedRoleTemplatesEnabled(decoded)
			if err != nil {
				return fmt.Errorf("GET %s: %w", aggregatedRoleTemplatesFeaturePath, err)
			}
			grammar := grammarClassic
			if on {
				grammar = grammarAggregation
			}
			prtbRB, prtbCRB, crtbCRB := bindingSelectors(grammar)
			stored := map[string]string{
				grammarKey:                      string(grammar),
				selectorPRTBRoleBindings:        substitute(prtbRB, captured),
				selectorPRTBClusterRoleBindings: substitute(prtbCRB, captured),
				selectorCRTBClusterRoleBindings: substitute(crtbCRB, captured),
			}
			for key, value := range stored {
				if strings.Contains(value, "{") {
					return fmt.Errorf("stored value %s still holds a placeholder: %q", key, value)
				}
			}
			maps.Copy(captured, stored)
			return nil
		},
	}
}

// aggregatedRoleTemplatesEnabled reads a Feature object the way Rancher's
// IsEnabled check does, in this order: a Prime-only feature is off on a
// build that is not Prime, then status.lockedValue when it is a boolean,
// then spec.value when it is a boolean, then status.default. The first
// step does not apply here: aggregated-roletemplates is not a Prime
// feature. A field of any other type, or no boolean in the three fields,
// is an error; Rancher's own check has no such case.
func aggregatedRoleTemplatesEnabled(feature map[string]any) (bool, error) {
	fields := [][2]string{{"status", "lockedValue"}, {"spec", "value"}, {"status", "default"}}
	var value, found bool
	for _, field := range fields {
		parent, _ := feature[field[0]].(map[string]any)
		raw, present := parent[field[1]]
		if !present || raw == nil {
			continue
		}
		b, ok := raw.(bool)
		if !ok {
			return false, fmt.Errorf("%s.%s is %T; want a boolean or null", field[0], field[1], raw)
		}
		if !found {
			value, found = b, true
		}
	}
	if !found {
		return false, fmt.Errorf("no boolean in status.lockedValue, spec.value or status.default")
	}
	return value, nil
}

// storedGrammar reads the grammar the detection step stored.
func storedGrammar(captured map[string]string) (bindingGrammar, error) {
	switch g := bindingGrammar(captured[grammarKey]); g {
	case grammarClassic, grammarAggregation:
		return g, nil
	default:
		return "", fmt.Errorf("no binding grammar stored by the detection step (capture %s is %q)", grammarKey, captured[grammarKey])
	}
}

// bindingSelectors returns the label selectors of the three binding
// lists an explain probe reads, for a grammar: the PRTB's RoleBindings,
// the ClusterRoleBindings of the PRTB and the CRTB's ClusterRoleBindings.
// The classic ones hold the {downstream} placeholder. The aggregation
// ones are the selectors Rancher's own handlers list with: the owner
// label of the binding and the aggregation label, in sorted key order.
func bindingSelectors(grammar bindingGrammar) (prtbRoleBindings, prtbClusterRoleBindings, crtbClusterRoleBindings string) {
	if grammar == grammarAggregation {
		prtb := ownerLabelKey("prtb", explainPRTBName) + "=true," + aggregationLabelKey + "=true"
		crtb := ownerLabelKey("crtb", explainCRTBName) + "=true," + aggregationLabelKey + "=true"
		return prtb, prtb, crtb
	}
	return "authz.cluster.cattle.io/rtb-owner-updated=" + explainBackingNS + "_" + explainPRTBName,
		explainBackingNS + "_" + explainPRTBName + "=owner-user",
		"authz.cluster.cattle.io/rtb-owner-updated={downstream}_" + explainCRTBName
}

// ownerLabelKey is the label key Rancher puts on a binding for a
// ProjectRoleTemplateBinding (kind "prtb") or ClusterRoleTemplateBinding
// (kind "crtb") with this name in the aggregation grammar: the
// authz.cluster.cattle.io/<kind>-owner prefix and the name, joined and
// shortened by the same rule as wrangler's name.SafeConcatName.
func ownerLabelKey(kind, bindingName string) string {
	return safeConcatName("authz.cluster.cattle.io/"+kind+"-owner", bindingName)
}

// safeConcatName joins parts with "-" and, from 64 characters on, keeps
// the first 57 characters (56 when the character at index 56 is not a
// lowercase letter or digit) and appends "-" and the first five (six) hex
// digits of the SHA-256 of the full join.
func safeConcatName(parts ...string) string {
	full := strings.Join(parts, "-")
	if len(full) < 64 {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	digest := hex.EncodeToString(sum[:])
	if c := full[56]; 'a' <= c && c <= 'z' || '0' <= c && c <= '9' {
		return full[:57] + "-" + digest[:5]
	}
	return full[:56] + "-" + digest[:6]
}
