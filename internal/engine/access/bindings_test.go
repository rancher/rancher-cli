package access

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func mgmtObj(kind, ns, name string, fields map[string]any) *unstructured.Unstructured {
	obj := map[string]any{"apiVersion": "management.cattle.io/v3", "kind": kind,
		"metadata": map[string]any{"name": name}}
	if ns != "" {
		obj["metadata"].(map[string]any)["namespace"] = ns
	}
	maps.Copy(obj, fields)
	return &unstructured.Unstructured{Object: obj}
}

func listOf(items ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	for _, it := range items {
		l.Items = append(l.Items, *it)
	}
	return l
}

// provenanceFixture is a management cluster with alice (u-alice, principal
// github_user://1, cached group github_org://eng) holding one CRTB on c-1,
// one PRTB on c-1's project p-a, one GRB to a GlobalRole with inherited
// RoleTemplates, and one GRB to admin; bob's bindings are noise.
func provenanceFixture() *fakeClient {
	return &fakeClient{
		Objects: map[string]*unstructured.Unstructured{
			"u-alice": mgmtObj("User", "", "u-alice", map[string]any{"username": "alice", "displayName": "Alice",
				"principalIds": []any{"local://u-alice", "github_user://1"}}),
			"u-alice-attr": mgmtObj("UserAttribute", "", "u-alice", map[string]any{
				"GroupPrincipals": map[string]any{"github": map[string]any{"Items": []any{
					map[string]any{"metadata": map[string]any{"name": "github_org://eng"}, "displayName": "eng"}}}}}),
		},
		Lists: map[string]*unstructured.UnstructuredList{
			"users||": listOf(
				mgmtObj("User", "", "u-alice", map[string]any{"username": "alice", "displayName": "Alice",
					"principalIds": []any{"local://u-alice", "github_user://1"}}),
				mgmtObj("User", "", "u-bob", map[string]any{"username": "bob", "principalIds": []any{"local://u-bob"}}),
			),
			"clusterroletemplatebindings||": listOf(
				mgmtObj("ClusterRoleTemplateBinding", "c-1", "crtb-alice", map[string]any{"userName": "u-alice", "roleTemplateName": "cluster-member", "clusterName": "c-1"}),
				mgmtObj("ClusterRoleTemplateBinding", "c-2", "crtb-eng", map[string]any{"groupPrincipalName": "github_org://eng", "roleTemplateName": "cluster-owner", "clusterName": "c-2"}),
				mgmtObj("ClusterRoleTemplateBinding", "c-1", "crtb-bob", map[string]any{"userName": "u-bob", "roleTemplateName": "cluster-owner", "clusterName": "c-1"}),
			),
			"projectroletemplatebindings||": listOf(
				mgmtObj("ProjectRoleTemplateBinding", "c-1-p-a", "prtb-alice", map[string]any{"userPrincipalName": "github_user://1", "roleTemplateName": "project-member", "projectName": "c-1:p-a"}),
			),
			"globalrolebindings||": listOf(
				mgmtObj("GlobalRoleBinding", "", "grb-alice-reader", map[string]any{"userName": "u-alice", "globalRoleName": "fleet-reader"}),
				mgmtObj("GlobalRoleBinding", "", "grb-alice-user", map[string]any{"userName": "u-alice", "globalRoleName": "user"}),
				mgmtObj("GlobalRoleBinding", "", "grb-eng-admin", map[string]any{"groupPrincipalName": "github_org://eng", "globalRoleName": "admin"}),
			),
			"globalroles||": listOf(
				mgmtObj("GlobalRole", "", "fleet-reader", map[string]any{"inheritedClusterRoles": []any{"cluster-member", "fleet-view"}}),
				mgmtObj("GlobalRole", "", "user", map[string]any{}),
				mgmtObj("GlobalRole", "", "admin", map[string]any{"builtin": true}),
			),
		},
	}
}

func TestReadProvenanceMatchesEveryAliasAndFiltersGlobalRoles(t *testing.T) {
	t.Parallel()
	p, err := readProvenanceOver(t.Context(), provenanceFixture(), provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	assert.Equal(t, "u-alice", p.Subject.ID)
	assert.Equal(t, "Alice", p.Subject.DisplayName)
	assert.Equal(t, []string{"github_org://eng"}, p.Groups)
	assert.Equal(t, "complete", p.Discovery.Status)
	assert.Empty(t, p.Discovery.Issues)
	assert.Equal(t, "available", p.GroupAliases.Status)
	require.NotNil(t, p.GroupAliases.Count)
	assert.Equal(t, 1, *p.GroupAliases.Count)
	assert.False(t, p.GroupAliases.Complete)

	var names []string
	for _, b := range p.Bindings {
		names = append(names, b.Kind+"/"+b.Name)
	}
	assert.Equal(t, []string{
		"ClusterRoleTemplateBinding/crtb-alice", "ClusterRoleTemplateBinding/crtb-eng",
		"GlobalRoleBinding/grb-alice-reader", "GlobalRoleBinding/grb-eng-admin",
		"ProjectRoleTemplateBinding/prtb-alice",
	}, names, "the user GlobalRole reaches no cluster and is not provenance")

	byName := map[string]bindingRecord{}
	for _, b := range p.Bindings {
		byName[b.Name] = b
	}
	assert.Equal(t, []viaEntry{{Kind: "user", Value: "u-alice"}}, byName["crtb-alice"].Via)
	assert.Equal(t, []viaEntry{{Kind: "group", Value: "github_org://eng"}}, byName["crtb-eng"].Via)
	assert.Equal(t, []viaEntry{{Kind: "principal", Value: "github_user://1"}}, byName["prtb-alice"].Via)
	assert.Equal(t, &projectRef{Cluster: "c-1", Project: "p-a"}, byName["prtb-alice"].Project)
	assert.Equal(t, "c-1", byName["prtb-alice"].Cluster)
	assert.Equal(t, []string{"cluster-member", "fleet-view"}, byName["grb-alice-reader"].InheritedRoleTemplates)
	assert.True(t, byName["grb-eng-admin"].AllClusters)

	c1 := bindingsFor(p.Bindings, "c-1")
	assert.Len(t, c1, 4, "c-1: its CRTB, its project's PRTB, both GRBs")
	assert.Equal(t, []grant{
		{Kind: "GlobalRole", Name: "admin"},
		{Kind: "RoleTemplate", Name: "cluster-member"}, {Kind: "RoleTemplate", Name: "fleet-view"}, {Kind: "RoleTemplate", Name: "project-member"},
	}, grantsFor(c1, p.Discovery))
	c3 := bindingsFor(p.Bindings, "c-3")
	assert.Len(t, c3, 2, "a cluster with no direct binding still carries the cluster-wide GlobalRoleBindings")
}

func TestReadProvenanceResolvedSubjectSkipsTheUserRead(t *testing.T) {
	t.Parallel()
	fc := provenanceFixture()
	delete(fc.Objects, "u-alice")
	p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice", ID: "u-alice"},
		PrincipalIDs: []string{"github_user://1"}, Resolved: true})
	require.NoError(t, err)
	assert.Equal(t, "complete", p.Discovery.Status)
	assert.Len(t, p.Bindings, 5)
}

func TestReadProvenanceUserNotFoundIsAnObservedFact(t *testing.T) {
	t.Parallel()
	fc := provenanceFixture()
	delete(fc.Objects, "u-alice")
	p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	assert.Equal(t, "complete", p.Discovery.Status)
	assert.Empty(t, p.PrincipalIDs)
	names := []string{}
	for _, b := range p.Bindings {
		names = append(names, b.Name)
	}
	assert.NotContains(t, names, "prtb-alice", "the principal alias is unknown without the User")
	assert.Contains(t, names, "crtb-alice", "the username still matches")
}

func TestReadProvenanceEachReadFailureIsAStageIssue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		key    string // Objects key or Lists key to break
		isList bool
		stage  string
		status string
	}{
		{"user 503", "u-alice", false, stageUser, "partial"},
		{"userattribute 403", "u-alice-attr", false, stageUserAttribute, "partial"},
		{"crtb 403", "clusterroletemplatebindings||", true, stageCRTB, "partial"},
		{"prtb 403", "projectroletemplatebindings||", true, stagePRTB, "partial"},
		{"grb 503", "globalrolebindings||", true, stageGRB, "partial"},
		{"globalrole 503", "globalroles||", true, stageGlobalRole, "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fc := provenanceFixture()
			denied := clierr.New(clierr.CodeRBAC, tc.name+" refused")
			if tc.isList {
				fc.ListErrFor = map[string]error{tc.key: denied}
			} else {
				fc.GetErrFor = map[string]error{tc.key: denied}
			}
			p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
			require.NoError(t, err)
			assert.Equal(t, tc.status, p.Discovery.Status)
			require.Len(t, p.Discovery.Issues, 1)
			assert.Equal(t, tc.stage, p.Discovery.Issues[0].Stage)
			assert.Equal(t, clierr.CodeRBAC, p.Discovery.Issues[0].Issue.Code)
			assert.Nil(t, grantsFor(bindingsFor(p.Bindings, "c-1"), p.Discovery), "grants are null unless provenance is complete")
			if tc.stage == stageGlobalRole {
				for _, b := range p.Bindings {
					if b.Kind == bindingGRB {
						assert.Nil(t, b.InheritedRoleTemplates)
					}
				}
			}
			if tc.stage == stageUserAttribute {
				assert.Equal(t, "unavailable", p.GroupAliases.Status)
				assert.Nil(t, p.GroupAliases.Count)
			}
		})
	}
}

func TestReadProvenanceAllBindingReadsFailingIsUnavailable(t *testing.T) {
	t.Parallel()
	fc := provenanceFixture()
	denied := clierr.New(clierr.CodeRBAC, "forbidden")
	fc.ListErrFor = map[string]error{"clusterroletemplatebindings||": denied, "projectroletemplatebindings||": denied, "globalrolebindings||": denied}
	p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	assert.Equal(t, "unavailable", p.Discovery.Status)
	assert.Len(t, p.Discovery.Issues, 3)
	assert.Equal(t, []string{stageCRTB, stagePRTB, stageGRB}, []string{p.Discovery.Issues[0].Stage, p.Discovery.Issues[1].Stage, p.Discovery.Issues[2].Stage})
	assert.Empty(t, p.Bindings)
}

func TestReadProvenanceExpiredCredentialAndCancellationAreTerminal(t *testing.T) {
	t.Parallel()
	fc := provenanceFixture()
	fc.ListErrFor = map[string]error{"clusterroletemplatebindings||": clierr.New(clierr.CodeAuth, "401")}
	_, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAuth, ce.Code)

	ctx, cancel := context.WithCancel(t.Context())
	fc2 := provenanceFixture()
	fc2.Before = func(context.Context) error { cancel(); return ctx.Err() }
	_, err = readProvenanceOver(ctx, fc2, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.Error(t, err)
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestReadProvenanceGlobalRoleReadFailsKeepsTheBindingUnresolved(t *testing.T) {
	t.Parallel()
	fc := provenanceFixture()
	fc.ListErrFor = map[string]error{"globalroles||": clierr.Retryable(clierr.CodeGeneric, "503")}
	p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	var grbs []bindingRecord
	for _, b := range p.Bindings {
		if b.Kind == bindingGRB {
			grbs = append(grbs, b)
		}
	}
	require.Len(t, grbs, 3, "every matched GlobalRoleBinding is kept, its reach unknown, the user role included")
	for _, g := range grbs {
		assert.Nil(t, g.InheritedRoleTemplates)
	}
}

func TestReadProvenanceGroupAliasesStates(t *testing.T) {
	t.Parallel()
	t.Run("404 is an observed empty cache", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		delete(fc.Objects, "u-alice-attr")
		p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
		require.NoError(t, err)
		assert.Equal(t, "available", p.GroupAliases.Status)
		require.NotNil(t, p.GroupAliases.Count)
		assert.Equal(t, 0, *p.GroupAliases.Count)
		assert.Equal(t, "complete", p.Discovery.Status)
	})
	t.Run("an attribute without groups is an empty cache", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		fc.Objects["u-alice-attr"] = mgmtObj("UserAttribute", "", "u-alice", map[string]any{})
		p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
		require.NoError(t, err)
		assert.Equal(t, "available", p.GroupAliases.Status)
		assert.Equal(t, 0, *p.GroupAliases.Count)
		assert.Empty(t, p.Groups)
	})
	t.Run("503 is unavailable and a staged issue", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		fc.GetErrFor = map[string]error{"u-alice-attr": clierr.Retryable(clierr.CodeGeneric, "503")}
		p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
		require.NoError(t, err)
		assert.Equal(t, "unavailable", p.GroupAliases.Status)
		assert.Nil(t, p.GroupAliases.Count)
		assert.Equal(t, "partial", p.Discovery.Status)
		assert.Equal(t, stageUserAttribute, p.Discovery.Issues[0].Stage)
	})
}

func TestReadProvenanceOrderIsStableUnderShuffledInput(t *testing.T) {
	t.Parallel()
	forward := provenanceFixture()
	shuffled := provenanceFixture()
	for _, key := range []string{"clusterroletemplatebindings||", "globalrolebindings||", "globalroles||"} {
		slices.Reverse(shuffled.Lists[key].Items)
	}
	a, err := readProvenanceOver(t.Context(), forward, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	b, err := readProvenanceOver(t.Context(), shuffled, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	assert.Equal(t, a.Bindings, b.Bindings)
}

func TestReadProvenanceDeduplicatesRepeatedBindings(t *testing.T) {
	t.Parallel()
	t.Run("an agreeing repeat is one record", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		l := fc.Lists["clusterroletemplatebindings||"]
		l.Items = append(l.Items, l.Items[0])
		p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
		require.NoError(t, err)
		n := 0
		for _, b := range p.Bindings {
			if b.Name == "crtb-alice" {
				n++
			}
		}
		assert.Equal(t, 1, n)
	})
	t.Run("copies matching different aliases merge their via entries", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		l := fc.Lists["clusterroletemplatebindings||"]
		byUser := mgmtObj("ClusterRoleTemplateBinding", "c-1", "crtb-both", map[string]any{"userName": "u-alice", "roleTemplateName": "cluster-member", "clusterName": "c-1"})
		byPrincipal := mgmtObj("ClusterRoleTemplateBinding", "c-1", "crtb-both", map[string]any{"userPrincipalName": "github_user://1", "roleTemplateName": "cluster-member", "clusterName": "c-1"})
		l.Items = append(l.Items, *byUser, *byPrincipal)
		p, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
		require.NoError(t, err)
		found := false
		for _, b := range p.Bindings {
			if b.Name == "crtb-both" {
				found = true
				assert.Equal(t, []viaEntry{{Kind: "user", Value: "u-alice"}, {Kind: "principal", Value: "github_user://1"}}, b.Via)
			}
		}
		assert.True(t, found)
	})
	t.Run("a conflicting repeat is a changed collection", func(t *testing.T) {
		t.Parallel()
		fc := provenanceFixture()
		l := fc.Lists["clusterroletemplatebindings||"]
		l.Items = append(l.Items, *mgmtObj("ClusterRoleTemplateBinding", "c-1", "crtb-alice", map[string]any{"userName": "u-alice", "roleTemplateName": "cluster-owner", "clusterName": "c-1"}))
		_, err := readProvenanceOver(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.True(t, ce.Retryable)
		require.ErrorContains(t, err, "twice with different fields")
	})
}

func TestReadProvenanceInheritedRoleTemplatesNullVersusEmpty(t *testing.T) {
	t.Parallel()
	decode := func(t *testing.T, p provenance) []map[string]any {
		t.Helper()
		raw, err := json.Marshal(p.Bindings)
		require.NoError(t, err)
		var decoded []map[string]any
		require.NoError(t, json.Unmarshal(raw, &decoded))
		return decoded
	}
	p, err := readProvenanceOver(t.Context(), provenanceFixture(), provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	for _, d := range decode(t, p) {
		if d["name"] == "grb-eng-admin" {
			assert.Equal(t, []any{}, d["inheritedRoleTemplates"], "a GlobalRole read fine that inherits nothing is an empty list")
		}
	}
	fc2 := provenanceFixture()
	fc2.ListErrFor = map[string]error{"globalroles||": clierr.Retryable(clierr.CodeGeneric, "503")}
	p, err = readProvenanceOver(t.Context(), fc2, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	for _, d := range decode(t, p) {
		if d["kind"] == bindingGRB {
			v, present := d["inheritedRoleTemplates"]
			assert.True(t, present, "the field is present as null, never omitted")
			assert.Nil(t, v)
		}
	}
}
