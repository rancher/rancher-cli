package access

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

func chainObj(apiVersion, kind, ns, name string, labels, annotations map[string]string, fields map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		meta["labels"] = l
	}
	if len(annotations) > 0 {
		a := map[string]any{}
		for k, v := range annotations {
			a[k] = v
		}
		meta["annotations"] = a
	}
	obj := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	maps.Copy(obj, fields)
	return &unstructured.Unstructured{Object: obj}
}

func rbacRoleRef(kind, name string) map[string]any {
	return map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name}
}

func podsRule(verbs ...string) []any {
	vs := make([]any, 0, len(verbs))
	for _, v := range verbs {
		vs = append(vs, v)
	}
	return []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": vs}}
}

func explainQuestion() Question { return Question{Verb: "delete", Resource: "pods", Version: "v1"} }

// TestChainClassicRoleBindingToPRTB asserts the classic grammar: a
// RoleBinding labelled rtb-owner-updated resolves to the PRTB in the
// project's backing namespace, the RoleTemplate inheritance follows, the
// role node carries the covering rules and no source claim, and the
// chain is complete in exactly the requests the objects need.
func TestChainClassicRoleBindingToPRTB(t *testing.T) {
	t.Parallel()
	down := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"team-a/rb-1": chainObj("rbac.authorization.k8s.io/v1", "RoleBinding", "team-a", "rb-1",
			map[string]string{rtbOwnerLabel: "c-1-p-abc_prtb-1"}, nil,
			map[string]any{"roleRef": rbacRoleRef("ClusterRole", "project-member-promoted"), "subjects": []any{rbacSubject("User", "u-1", "")}}),
		"project-member-promoted": chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "project-member-promoted", nil, nil,
			map[string]any{"rules": podsRule("get", "list", "delete")}),
	}}
	mgmt := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"c-1-p-abc/prtb-1": chainObj("management.cattle.io/v3", "ProjectRoleTemplateBinding", "c-1-p-abc", "prtb-1", nil, nil,
			map[string]any{"userName": "u-1", "roleTemplateName": "project-member", "projectName": "c-1:p-abc"}),
		"project-member": chainObj("management.cattle.io/v3", "RoleTemplate", "", "project-member", nil, nil,
			map[string]any{"roleTemplateNames": []any{"edit"}}),
		"edit": chainObj("management.cattle.io/v3", "RoleTemplate", "", "edit", nil, nil, map[string]any{"external": true}),
	}}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, explainQuestion())
	root, state := w.explainAllowed(`RBAC: allowed by RoleBinding "rb-1/team-a" of ClusterRole "project-member-promoted" to User "u-1"`)
	require.NotNil(t, root)
	assert.Equal(t, chainComplete, state)
	assert.Empty(t, w.issues)
	assert.Equal(t, 5, w.requests, "binding, role, PRTB, two RoleTemplates")
	assert.Equal(t, "RoleBinding", root.Kind)
	assert.Equal(t, acceptedBindingNote, root.Note)
	require.Len(t, root.Children, 2)
	role, prtb := root.Children[0], root.Children[1]
	assert.Equal(t, "ClusterRole", role.Kind)
	assert.Equal(t, "rules covering the request: core/pods [get list delete]", role.Note)
	assert.Empty(t, role.Children, "the role makes no source claim under a verified parent")
	assert.Equal(t, "ProjectRoleTemplateBinding", prtb.Kind)
	assert.Equal(t, "c-1-p-abc", prtb.Namespace)
	assert.Equal(t, "project c-1:p-abc, user u-1, RoleTemplate project-member", prtb.Note)
	require.Len(t, prtb.Children, 1)
	assert.Equal(t, "RoleTemplate", prtb.Children[0].Kind)
	assert.Equal(t, "inherits RoleTemplate edit", prtb.Children[0].Note)
	require.Len(t, prtb.Children[0].Children, 1)
	assert.Equal(t, "external: binds the cluster's own ClusterRole edit", prtb.Children[0].Children[0].Note)
	lines := renderTree(*root, "[allowed]")
	assert.Equal(t, "[allowed] RoleBinding team-a/rb-1 - one binding the server accepted", lines[0])
	assert.Equal(t, "+-- ClusterRole project-member-promoted - rules covering the request: core/pods [get list delete]", lines[1])
	assert.Equal(t, "+-- ProjectRoleTemplateBinding c-1-p-abc/prtb-1 - project c-1:p-abc, user u-1, RoleTemplate project-member", lines[2])
	assert.Equal(t, "    +-- RoleTemplate project-member - inherits RoleTemplate edit", lines[3])
	assert.Equal(t, "        +-- RoleTemplate edit - external: binds the cluster's own ClusterRole edit", lines[4])
}

// TestChainAggregationSharedKeyNarrowsByProject asserts the aggregation
// grammar: the PRTB candidates are listed once, a key shared by every
// project's creator binding is narrowed to the project the bound role
// names, and a CRTB key with a truncated name still matches.
func TestChainAggregationSharedKeyNarrowsByProject(t *testing.T) {
	t.Parallel()
	crb := chainObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "crb-1",
		map[string]string{aggregationLabel: "true", aggregationKeyFor(ownerPRTB, "creator-project-owner"): "true",
			aggregationKeyFor(ownerCRTB, "c-1-fleet-default-owner-with-a-very-long-name"): "true"}, nil,
		map[string]any{"roleRef": rbacRoleRef("ClusterRole", "p-abc-namespaces-edit"), "subjects": []any{rbacSubject("User", "u-1", "")}})
	down := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"crb-1":                 crb,
		"p-abc-namespaces-edit": chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "p-abc-namespaces-edit", nil, nil, map[string]any{"rules": podsRule("*")}),
	}}
	prtb := func(ns, project string) unstructured.Unstructured {
		return *chainObj("management.cattle.io/v3", "ProjectRoleTemplateBinding", ns, "creator-project-owner", nil, nil,
			map[string]any{"userName": "u-1", "roleTemplateName": "project-owner", "projectName": project})
	}
	crtb := *chainObj("management.cattle.io/v3", "ClusterRoleTemplateBinding", "c-1", "c-1-fleet-default-owner-with-a-very-long-name", nil, nil,
		map[string]any{"userName": "u-1", "roleTemplateName": "cluster-owner"})
	mgmt := &fakeClient{
		Lists: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings||":    {Items: []unstructured.Unstructured{prtb("c-1-p-abc", "c-1:p-abc"), prtb("c-1-p-xyz", "c-1:p-xyz"), prtb("c-2-p-abc", "c-2:p-abc")}},
			"clusterroletemplatebindings|c-1|": {Items: []unstructured.Unstructured{crtb}},
		},
		Objects: map[string]*unstructured.Unstructured{
			"project-owner": chainObj("management.cattle.io/v3", "RoleTemplate", "", "project-owner", nil, nil, map[string]any{"roleTemplateNames": []any{"admin"}}),
			"admin":         chainObj("management.cattle.io/v3", "RoleTemplate", "", "admin", nil, nil, map[string]any{"external": true}),
			"cluster-owner": chainObj("management.cattle.io/v3", "RoleTemplate", "", "cluster-owner", nil, nil, nil),
		},
	}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, explainQuestion())
	root, state := w.explainAllowed(`RBAC: allowed by ClusterRoleBinding "crb-1" of ClusterRole "p-abc-namespaces-edit" to User "u-1"`)
	require.NotNil(t, root)
	assert.Equal(t, chainComplete, state, "%v", w.issues)
	// binding, role, one CRTB LIST, one PRTB LIST, three RoleTemplates.
	assert.Equal(t, 7, w.requests)
	require.Len(t, root.Children, 3)
	assert.Equal(t, "ClusterRole", root.Children[0].Kind)
	assert.Equal(t, "ClusterRoleTemplateBinding", root.Children[1].Kind)
	assert.Equal(t, "c-1-fleet-default-owner-with-a-very-long-name", root.Children[1].Name)
	assert.Equal(t, "ProjectRoleTemplateBinding", root.Children[2].Kind)
	assert.Equal(t, "c-1-p-abc", root.Children[2].Namespace, "narrowed to the project the role names")
}

// TestChainAggregationRuleEdge asserts an aggregating ClusterRole's
// selectors are followed with one LIST per selector to the roles they
// aggregate, each shown once, the aggregator itself excluded.
func TestChainAggregationRuleEdge(t *testing.T) {
	t.Parallel()
	agg := chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cluster-member-aggregator",
		map[string]string{"authz.cluster.cattle.io/clusterrole-owner": "cluster-member", aggregationLabel: "true", "management.cattle.io/aggregates": "cluster-member-aggregator"}, nil,
		map[string]any{"rules": podsRule("get"), "aggregationRule": map[string]any{"clusterRoleSelectors": []any{
			map[string]any{"matchLabels": map[string]any{"management.cattle.io/aggregates": "cluster-member"}},
		}}})
	member := *chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cluster-member",
		map[string]string{"management.cattle.io/aggregates": "cluster-member"}, nil, map[string]any{"rules": podsRule("get")})
	down := &fakeClient{
		Objects: map[string]*unstructured.Unstructured{
			"crb-1": chainObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "crb-1",
				map[string]string{aggregationLabel: "true", aggregationKeyFor(ownerCRTB, "crtb-1"): "true"}, nil,
				map[string]any{"roleRef": rbacRoleRef("ClusterRole", "cluster-member-aggregator"), "subjects": []any{rbacSubject("User", "u-1", "")}}),
			"cluster-member-aggregator": agg,
		},
		Lists: map[string]*unstructured.UnstructuredList{
			"clusterroles||management.cattle.io/aggregates=cluster-member": {Items: []unstructured.Unstructured{member, *agg}},
		},
	}
	crtb := *chainObj("management.cattle.io/v3", "ClusterRoleTemplateBinding", "c-1", "crtb-1", nil, nil, map[string]any{"userName": "u-1", "roleTemplateName": "cluster-member"})
	mgmt := &fakeClient{
		Lists:   map[string]*unstructured.UnstructuredList{"clusterroletemplatebindings|c-1|": {Items: []unstructured.Unstructured{crtb}}},
		Objects: map[string]*unstructured.Unstructured{"cluster-member": chainObj("management.cattle.io/v3", "RoleTemplate", "", "cluster-member", nil, nil, nil)},
	}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, Question{Verb: "get", Resource: "pods"})
	root, state := w.explainAllowed(`RBAC: allowed by ClusterRoleBinding "crb-1" of ClusterRole "cluster-member-aggregator" to User "u-1"`)
	require.NotNil(t, root)
	assert.Equal(t, chainComplete, state, "%v", w.issues)
	role := root.Children[0]
	assert.Equal(t, "cluster-member-aggregator", role.Name)
	require.Len(t, role.Children, 1)
	assert.Equal(t, "cluster-member", role.Children[0].Name)
	assert.Equal(t, "aggregated by cluster-member-aggregator (management.cattle.io/aggregates=cluster-member)", role.Children[0].Note)
	// binding, role, one selector LIST, the CRTB LIST, the RoleTemplate.
	assert.Equal(t, 5, w.requests)
}

// TestChainGlobalRoleByAnnotationsAndLabel asserts the GlobalRole path:
// a globaladmin binding's annotations lead to the GlobalRoleBinding and
// GlobalRole with its inherited RoleTemplates, and a management-cluster
// binding labelled grb-owner leads there too; a GlobalRoleBinding naming
// another subject is an unverified candidate.
func TestChainGlobalRoleByAnnotationsAndLabel(t *testing.T) {
	t.Parallel()
	mgmt := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"grb-1":         chainObj("management.cattle.io/v3", "GlobalRoleBinding", "", "grb-1", nil, nil, map[string]any{"userName": "user-a", "globalRoleName": "admin"}),
		"grb-other":     chainObj("management.cattle.io/v3", "GlobalRoleBinding", "", "grb-other", nil, nil, map[string]any{"userName": "user-b", "globalRoleName": "admin"}),
		"admin":         chainObj("management.cattle.io/v3", "GlobalRole", "", "admin", nil, nil, map[string]any{"inheritedClusterRoles": []any{"cluster-owner"}}),
		"cluster-owner": chainObj("management.cattle.io/v3", "RoleTemplate", "", "cluster-owner", nil, nil, nil),
	}}
	down := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"globaladmin-user-a": chainObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "globaladmin-user-a", nil,
			map[string]string{crbGlobalRoleAnnotation: "admin", crbGlobalRoleBindingAnnotation: "grb-1"},
			map[string]any{"roleRef": rbacRoleRef("ClusterRole", "cluster-admin"), "subjects": []any{rbacSubject("User", "user-a", "")}}),
		"cattle-globalrolebinding-grb-other": chainObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "cattle-globalrolebinding-grb-other",
			map[string]string{grbOwnerLabel: "grb-other"}, nil,
			map[string]any{"roleRef": rbacRoleRef("ClusterRole", "cattle-globalrole-admin"), "subjects": []any{rbacSubject("User", "user-b", "")}}),
		"cluster-admin":           chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cluster-admin", nil, nil, map[string]any{"rules": []any{map[string]any{"apiGroups": []any{"*"}, "resources": []any{"*"}, "verbs": []any{"*"}}}}),
		"cattle-globalrole-admin": chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cattle-globalrole-admin", map[string]string{grOwnerLabel: "admin"}, nil, map[string]any{"rules": podsRule("*")}),
	}}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "user-a", ID: "user-a"}, explainQuestion())
	root, state := w.explainAllowed(`RBAC: allowed by ClusterRoleBinding "globaladmin-user-a" of ClusterRole "cluster-admin" to User "user-a"`)
	require.NotNil(t, root)
	assert.Equal(t, chainComplete, state, "%v", w.issues)
	require.Len(t, root.Children, 2)
	grb := root.Children[1]
	assert.Equal(t, nodeGlobalRoleBind, grb.Kind)
	assert.Equal(t, "user user-a, GlobalRole admin", grb.Note)
	require.Len(t, grb.Children, 1)
	assert.Equal(t, "inheritedClusterRoles: RoleTemplate cluster-owner", grb.Children[0].Note)
	assert.Equal(t, 5, w.requests, "binding, role, GRB, GlobalRole, RoleTemplate")

	w2 := newChainWalker(t.Context(), down, mgmt, engine.LocalCluster, chainSubject{Kind: KindUser, Name: "user-a", ID: "user-a"}, explainQuestion())
	root2, state2 := w2.explainAllowed(`RBAC: allowed by ClusterRoleBinding "cattle-globalrolebinding-grb-other" of ClusterRole "cattle-globalrole-admin" to User "user-a"`)
	require.NotNil(t, root2)
	assert.Equal(t, chainPartial, state2)
	require.Len(t, w2.issues, 1)
	assert.Equal(t, codeUnverifiedCandidate, w2.issues[0].Code)
	assert.Equal(t, "the GlobalRoleBinding does not name the subject", w2.issues[0].Message)
}

// TestChainNonAdminFallsBackToHeldBindings asserts a caller refused the
// downstream RBAC objects still gets the binding and role nodes from the
// reason, three RBAC issues, a partial chain, and the held-bindings node
// from the management plane, filtered to the cluster.
func TestChainNonAdminFallsBackToHeldBindings(t *testing.T) {
	t.Parallel()
	down := &fakeClient{GetErr: clierr.New(clierr.CodeRBAC, `rolebindings.rbac.authorization.k8s.io "rb-1" is forbidden: User "u-1" cannot get resource "rolebindings"`)}
	prtb := *chainObj("management.cattle.io/v3", "ProjectRoleTemplateBinding", "c-1-p-abc", "prtb-1", nil, nil,
		map[string]any{"userName": "u-1", "roleTemplateName": "project-member", "projectName": "c-1:p-abc"})
	// The creator binding of two projects shares one name: the fallback
	// must resolve each by project and name, never the first by name.
	creatorA := *chainObj("management.cattle.io/v3", "ProjectRoleTemplateBinding", "c-1-p-abc", "creator-project-owner", nil, nil,
		map[string]any{"userName": "u-1", "roleTemplateName": "project-owner", "projectName": "c-1:p-abc"})
	creatorX := *chainObj("management.cattle.io/v3", "ProjectRoleTemplateBinding", "c-1-p-xyz", "creator-project-owner", nil, nil,
		map[string]any{"userName": "u-1", "roleTemplateName": "project-owner", "projectName": "c-1:p-xyz"})
	crtbHere := *chainObj("management.cattle.io/v3", "ClusterRoleTemplateBinding", "c-1", "crtb-1", nil, nil,
		map[string]any{"userName": "u-1", "roleTemplateName": "cluster-member"})
	crtbElsewhere := *chainObj("management.cattle.io/v3", "ClusterRoleTemplateBinding", "c-2", "crtb-2", nil, nil,
		map[string]any{"userName": "u-1", "roleTemplateName": "cluster-member"})
	mgmt := &fakeClient{
		Lists: map[string]*unstructured.UnstructuredList{
			"clusterroletemplatebindings||": {Items: []unstructured.Unstructured{crtbHere, crtbElsewhere}},
			"projectroletemplatebindings||": {Items: []unstructured.Unstructured{creatorX, prtb, creatorA}},
		},
		Objects: map[string]*unstructured.Unstructured{
			"c-1/crtb-1":     &crtbHere,
			"cluster-member": chainObj("management.cattle.io/v3", "RoleTemplate", "", "cluster-member", nil, nil, nil),
			"project-member": chainObj("management.cattle.io/v3", "RoleTemplate", "", "project-member", nil, nil, nil),
			"project-owner":  chainObj("management.cattle.io/v3", "RoleTemplate", "", "project-owner", nil, nil, nil),
		},
	}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, explainQuestion())
	root, state := w.explainAllowed(`RBAC: allowed by RoleBinding "rb-1/team-a" of ClusterRole "edit" to User "u-1"`)
	require.NotNil(t, root)
	assert.Equal(t, chainPartial, state)
	stages := []string{}
	for _, i := range w.issues {
		stages = append(stages, i.Stage+":"+i.Code)
	}
	assert.Equal(t, []string{"bindingRead:RBAC", "roleRead:RBAC", "labelTraversal:RBAC"}, stages)
	assert.Equal(t, "RoleBinding", root.Kind)
	assert.Equal(t, "team-a", root.Namespace)
	require.Len(t, root.Children, 2)
	assert.Equal(t, rulesUnreadableNote, root.Children[0].Note)
	held := root.Children[1]
	assert.Equal(t, nodeHeldBindings, held.Kind)
	assert.Equal(t, heldBindingsTitle, held.Note)
	require.Len(t, held.Children, 4, "the CRTB on c-2 is not this cluster's")
	assert.Equal(t, "ClusterRoleTemplateBinding", held.Children[0].Kind)
	assert.Equal(t, "crtb-1", held.Children[0].Name)
	got := []string{}
	for _, c := range held.Children[1:] {
		assert.Equal(t, "ProjectRoleTemplateBinding", c.Kind)
		got = append(got, c.Namespace+"/"+c.Name)
	}
	assert.Equal(t, []string{"c-1-p-abc/creator-project-owner", "c-1-p-abc/prtb-1", "c-1-p-xyz/creator-project-owner"}, got,
		"two creator bindings resolve to two projects")
}

// TestChainNoSourceIsFactOnly asserts an unlabelled binding is a
// no-source node with the fact-only sentence, a noSource issue, and the
// held-bindings fallback, and that a same-named RoleTemplate is never
// followed by name.
func TestChainNoSourceIsFactOnly(t *testing.T) {
	t.Parallel()
	down := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"grb-u-1-view": chainObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "grb-u-1-view", nil, nil,
			map[string]any{"roleRef": rbacRoleRef("ClusterRole", "cluster-member"), "subjects": []any{rbacSubject("User", "u-1", "")}}),
		"cluster-member": chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cluster-member", nil, nil, map[string]any{"rules": podsRule("get")}),
	}}
	mgmt := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"cluster-member": chainObj("management.cattle.io/v3", "RoleTemplate", "", "cluster-member", nil, nil, nil),
	}}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, Question{Verb: "get", Resource: "pods"})
	root, state := w.explainAllowed(`RBAC: allowed by ClusterRoleBinding "grb-u-1-view" of ClusterRole "cluster-member" to User "u-1"`)
	require.NotNil(t, root)
	assert.Equal(t, chainPartial, state)
	assert.Equal(t, noSourceNote, root.Note[len(acceptedBindingNote)+2:])
	require.Len(t, w.issues, 1)
	assert.Equal(t, codeNoSource, w.issues[0].Code)
	require.Len(t, root.Children, 2)
	assert.Empty(t, root.Children[0].Children, "cluster-member the ClusterRole is not followed to cluster-member the RoleTemplate by name")
	assert.Equal(t, nodeHeldBindings, root.Children[1].Kind)
}

// TestChainAggregationKeyWithoutCandidateIsUnverified asserts a binding
// whose aggregation label names a parent nothing matches gets the
// unverifiedCandidate issue alone, never a noSource claim: the node says
// no parent could be verified and the held-bindings fallback follows.
func TestChainAggregationKeyWithoutCandidateIsUnverified(t *testing.T) {
	t.Parallel()
	down := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"crb-1": chainObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "crb-1",
			map[string]string{aggregationLabel: "true", aggregationKeyFor(ownerPRTB, "creator-project-owner"): "true"}, nil,
			map[string]any{"roleRef": rbacRoleRef("ClusterRole", "p-abc-namespaces-edit"), "subjects": []any{rbacSubject("User", "u-1", "")}}),
		"p-abc-namespaces-edit": chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "p-abc-namespaces-edit", nil, nil, map[string]any{"rules": podsRule("*")}),
	}}
	mgmt := &fakeClient{Lists: map[string]*unstructured.UnstructuredList{
		"projectroletemplatebindings||":    {},
		"clusterroletemplatebindings|c-1|": {},
	}}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, explainQuestion())
	root, state := w.explainAllowed(`RBAC: allowed by ClusterRoleBinding "crb-1" of ClusterRole "p-abc-namespaces-edit" to User "u-1"`)
	require.NotNil(t, root)
	assert.Equal(t, chainPartial, state)
	assert.Equal(t, unverifiedNote, root.Note[len(acceptedBindingNote)+2:])
	require.Len(t, w.issues, 1, "%v", w.issues)
	assert.Equal(t, codeUnverifiedCandidate, w.issues[0].Code)
	assert.Contains(t, w.issues[0].Message, aggregationKeyFor(ownerPRTB, "creator-project-owner"))
	require.Len(t, root.Children, 2)
	assert.Equal(t, "ClusterRole", root.Children[0].Kind)
	assert.Equal(t, nodeHeldBindings, root.Children[1].Kind)
}

// TestChainMalformedClassicLabelIsUnverified asserts a classic owner label
// that names no <namespace>_<name> is an unverified back-reference, not a
// missing one: one unverifiedCandidate issue, the unverified note, and the
// held-bindings fallback.
func TestChainMalformedClassicLabelIsUnverified(t *testing.T) {
	t.Parallel()
	down := &fakeClient{Objects: map[string]*unstructured.Unstructured{
		"default/rb-1": chainObj("rbac.authorization.k8s.io/v1", "RoleBinding", "default", "rb-1",
			map[string]string{rtbOwnerLabel: "broken"}, nil,
			map[string]any{"roleRef": rbacRoleRef("ClusterRole", "edit"), "subjects": []any{rbacSubject("User", "u-1", "")}}),
		"edit": chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "edit", nil, nil, map[string]any{"rules": podsRule("*")}),
	}}
	mgmt := &fakeClient{Lists: map[string]*unstructured.UnstructuredList{
		"projectroletemplatebindings||":    {},
		"clusterroletemplatebindings|c-1|": {},
	}}
	w := newChainWalker(t.Context(), down, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, explainQuestion())
	root, state := w.explainAllowed(`RBAC: allowed by RoleBinding "rb-1/default" of ClusterRole "edit" to User "u-1"`)
	require.NotNil(t, root)
	assert.Equal(t, chainPartial, state)
	assert.Equal(t, unverifiedNote, root.Note[len(acceptedBindingNote)+2:])
	require.Len(t, w.issues, 1, "%v", w.issues)
	assert.Equal(t, codeUnverifiedCandidate, w.issues[0].Code)
	require.Len(t, root.Children, 2)
	assert.Equal(t, nodeHeldBindings, root.Children[1].Kind)
}

// TestChainBoundsCycleDiamondDepthNodes asserts the five bounded cases:
// a RoleTemplate inheriting itself, a two-template cycle, a diamond shown
// once, the depth limit, and the node limit, each with a deterministic
// child order, a typed issue, and a bounded request count.
func TestChainBoundsCycleDiamondDepthNodes(t *testing.T) {
	// NOT parallel: lowers the package-level bounds.
	rt := func(name string, inherits ...string) *unstructured.Unstructured {
		vals := make([]any, 0, len(inherits))
		for _, i := range inherits {
			vals = append(vals, i)
		}
		return chainObj("management.cattle.io/v3", "RoleTemplate", "", name, nil, nil, map[string]any{"roleTemplateNames": vals})
	}
	walkFrom := func(t *testing.T, mgmt *fakeClient, start string) (chainNode, *chainWalker) {
		t.Helper()
		w := newChainWalker(context.Background(), &fakeClient{}, mgmt, "c-1", chainSubject{Kind: KindUser, Name: "u-1"}, explainQuestion())
		return w.roleTemplateNode(start, 1), w
	}
	t.Run("self cycle", func(t *testing.T) {
		node, w := walkFrom(t, &fakeClient{Objects: map[string]*unstructured.Unstructured{"a": rt("a", "a")}}, "a")
		require.Len(t, node.Children, 1)
		assert.Equal(t, "cycle to RoleTemplate a", node.Children[0].Note)
		require.Len(t, w.issues, 1)
		assert.Equal(t, codeCycle, w.issues[0].Code)
		assert.Equal(t, 1, w.requests)
	})
	t.Run("two-node cycle", func(t *testing.T) {
		node, w := walkFrom(t, &fakeClient{Objects: map[string]*unstructured.Unstructured{"a": rt("a", "b"), "b": rt("b", "a")}}, "a")
		assert.Equal(t, "cycle to RoleTemplate a", node.Children[0].Children[0].Note)
		assert.Equal(t, codeCycle, w.issues[0].Code)
		assert.Equal(t, 2, w.requests)
	})
	t.Run("diamond", func(t *testing.T) {
		node, w := walkFrom(t, &fakeClient{Objects: map[string]*unstructured.Unstructured{"a": rt("a", "c", "b"), "b": rt("b", "d"), "c": rt("c", "d"), "d": rt("d")}}, "a")
		require.Len(t, node.Children, 2)
		assert.Equal(t, []string{"b", "c"}, []string{node.Children[0].Name, node.Children[1].Name}, "edges are sorted before they are followed")
		assert.Equal(t, "", node.Children[0].Children[0].Note)
		assert.Equal(t, "already shown", node.Children[1].Children[0].Note)
		assert.Empty(t, w.issues)
		assert.Equal(t, 4, w.requests, "d is fetched once")
	})
	t.Run("depth limit", func(t *testing.T) {
		old := maxChainDepth
		maxChainDepth = 3
		t.Cleanup(func() { maxChainDepth = old })
		node, w := walkFrom(t, &fakeClient{Objects: map[string]*unstructured.Unstructured{"a": rt("a", "b"), "b": rt("b", "c"), "c": rt("c", "d"), "d": rt("d", "e"), "e": rt("e")}}, "a")
		leaf := node.Children[0].Children[0].Children[0]
		assert.Equal(t, "d", leaf.Name)
		assert.Equal(t, "not followed: chain too deep", leaf.Note)
		require.Len(t, w.issues, 1)
		assert.Equal(t, codeLimitExceeded, w.issues[0].Code)
		assert.Equal(t, 3, w.requests)
	})
	t.Run("node limit", func(t *testing.T) {
		old := maxChainNodes
		maxChainNodes = 2
		t.Cleanup(func() { maxChainNodes = old })
		node, w := walkFrom(t, &fakeClient{Objects: map[string]*unstructured.Unstructured{"a": rt("a", "b", "c"), "b": rt("b"), "c": rt("c")}}, "a")
		require.Len(t, node.Children, 2)
		assert.Equal(t, "", node.Children[0].Note)
		assert.Equal(t, "not followed: too many objects", node.Children[1].Note)
		assert.Equal(t, codeLimitExceeded, w.issues[0].Code)
		assert.Equal(t, 2, w.requests)
	})
}

// TestChainDeniedListsBindingsNamingSubjectOrGroups asserts the denied
// tree lists the RoleBindings in the namespace and the
// ClusterRoleBindings naming the subject or one of its groups, sorted,
// each with its own chain, and that refused LISTs add the held-bindings
// fallback instead.
func TestChainDeniedListsBindingsNamingSubjectOrGroups(t *testing.T) {
	t.Parallel()
	rbMine := *chainObj("rbac.authorization.k8s.io/v1", "RoleBinding", "team-a", "rb-mine", nil, nil,
		map[string]any{"roleRef": rbacRoleRef("Role", "reader"), "subjects": []any{rbacSubject("User", "u-1", "")}})
	rbGroup := *chainObj("rbac.authorization.k8s.io/v1", "RoleBinding", "team-a", "rb-group", nil, nil,
		map[string]any{"roleRef": rbacRoleRef("ClusterRole", "view"), "subjects": []any{rbacSubject("Group", "okta_group://eng", "")}})
	rbOther := *chainObj("rbac.authorization.k8s.io/v1", "RoleBinding", "team-a", "rb-other", nil, nil,
		map[string]any{"roleRef": rbacRoleRef("ClusterRole", "view"), "subjects": []any{rbacSubject("User", "u-2", "")}})
	down := &fakeClient{
		Lists: map[string]*unstructured.UnstructuredList{
			"rolebindings|team-a|":  {Items: []unstructured.Unstructured{rbOther, rbMine, rbGroup}},
			"clusterrolebindings||": {},
		},
		Objects: map[string]*unstructured.Unstructured{
			"team-a/rb-mine":  &rbMine,
			"team-a/rb-group": &rbGroup,
			"team-a/reader":   chainObj("rbac.authorization.k8s.io/v1", "Role", "team-a", "reader", nil, nil, map[string]any{"rules": podsRule("get")}),
			"view":            chainObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "view", nil, nil, map[string]any{"rules": podsRule("get", "list")}),
		},
	}
	w := newChainWalker(t.Context(), down, &fakeClient{}, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1", Groups: []string{"okta_group://eng"}}, explainQuestion())
	root, state := w.explainDenied("team-a")
	assert.Equal(t, chainComplete, state, "plain bindings in the denied listing are facts, not issues: %v", w.issues)
	assert.Equal(t, nodeBindings, root.Kind)
	require.Len(t, root.Children, 2)
	assert.Equal(t, noSourceNote, root.Children[0].Note)
	assert.Equal(t, "rb-group", root.Children[0].Name)
	assert.Equal(t, "rb-mine", root.Children[1].Name)
	assert.Equal(t, "Role", root.Children[1].Children[0].Kind)
	assert.Equal(t, "no rule in this role covers the request as read; the server chose the binding", root.Children[1].Children[0].Note)

	refused := &fakeClient{ListErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	w2 := newChainWalker(t.Context(), refused, &fakeClient{}, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, explainQuestion())
	root2, state2 := w2.explainDenied("team-a")
	assert.Equal(t, chainPartial, state2)
	require.Len(t, root2.Children, 1)
	assert.Equal(t, nodeHeldBindings, root2.Children[0].Kind)
	assert.Len(t, w2.issues, 2, "one per refused LIST")
}
