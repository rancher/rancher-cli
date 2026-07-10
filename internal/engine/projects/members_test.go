package projects

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// memberClient fakes the pieces add/remove-member exercise: List (projects,
// users, prtbs, keyed by GVR resource), Get (the project object, for
// BackingNamespace), Create (recorded), and Delete (recorded per call).
type memberClient struct {
	projects *unstructured.UnstructuredList
	users    *unstructured.UnstructuredList
	prtbs    *unstructured.UnstructuredList
	// gets answers Get by "namespace/name" key.
	gets map[string]*unstructured.Unstructured

	created       *unstructured.Unstructured
	createdNS     string
	createDryRuns []bool

	deletedNames  []string
	deletedNS     []string
	deletedRVs    []string
	deleteDryRuns []bool
}

func (m *memberClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if obj, ok := m.gets[ns+"/"+name]; ok {
		return obj, nil
	}
	return nil, clierr.New(clierr.CodeNotFound, "not found: "+name)
}

func (m *memberClient) List(_ context.Context, gvr schema.GroupVersionResource, _, _ string) (*unstructured.UnstructuredList, error) {
	switch gvr.Resource {
	case "projects":
		if m.projects != nil {
			return m.projects, nil
		}
	case "users":
		if m.users != nil {
			return m.users, nil
		}
	case "projectroletemplatebindings":
		if m.prtbs != nil {
			return m.prtbs, nil
		}
	}
	return &unstructured.UnstructuredList{}, nil
}

func (m *memberClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	m.created, m.createdNS = obj, ns
	m.createDryRuns = append(m.createDryRuns, dryRun)
	return obj, nil
}

func (m *memberClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (m *memberClient) Delete(_ context.Context, _ schema.GroupVersionResource, ns, name, rv string, dryRun bool) error {
	m.deletedNames = append(m.deletedNames, name)
	m.deletedNS = append(m.deletedNS, ns)
	m.deletedRVs = append(m.deletedRVs, rv)
	m.deleteDryRuns = append(m.deleteDryRuns, dryRun)
	return nil
}

func (m *memberClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

func userObj(id, display string, principalIDs ...string) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{
		"metadata":    map[string]any{"name": id},
		"displayName": display,
	}}
	if len(principalIDs) > 0 {
		ids := make([]any, len(principalIDs))
		for i, p := range principalIDs {
			ids[i] = p
		}
		_ = unstructured.SetNestedSlice(obj.Object, ids, "principalIds")
	}
	return obj
}

func prtbObj(name, userName, groupPrincipalName string) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name, "resourceVersion": name + "-rv"}}}
	if userName != "" {
		_ = unstructured.SetNestedField(obj.Object, userName, "userName")
	}
	if groupPrincipalName != "" {
		_ = unstructured.SetNestedField(obj.Object, groupPrincipalName, "groupPrincipalName")
	}
	return obj
}

// memberIn is a runtime.Input fake supporting bool flags, which the shared
// `in` type (projects_test.go) hardcodes to false — add/remove-member need a
// working --group flag.
type memberIn struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (i memberIn) String(k string) string { return i.s[k] }
func (i memberIn) Bool(k string) bool     { return i.b[k] }
func (i memberIn) Args() []string         { return i.a }

func planMember(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "member commands must be MutatingCommand")
	return mc.Plan(context.Background(), input)
}

func TestAddMemberUserPrincipalSetsBothFields(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "local://u-1")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	eff, exec, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-member"}, a: []string{"alice"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionCreate, eff.Action)

	assert.Equal(t, "p-1", client.createdNS, "no status.backingNamespace falls back to the project id")
	un, _, _ := unstructured.NestedString(client.created.Object, "userName")
	assert.Equal(t, "u-1", un)
	upn, _, _ := unstructured.NestedString(client.created.Object, "userPrincipalName")
	assert.Equal(t, "local://u-1", upn)
	pn, _, _ := unstructured.NestedString(client.created.Object, "projectName")
	assert.Equal(t, "c-x:p-1", pn)
	rtn, _, _ := unstructured.NestedString(client.created.Object, "roleTemplateName")
	assert.Equal(t, "project-member", rtn)
	_, found, _ := unstructured.NestedString(client.created.Object, "groupPrincipalName")
	assert.False(t, found, "user principal path never sets groupPrincipalName")
}

func TestAddMemberUsesBackingNamespaceWhenSet(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "local://u-1")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "p-backing-ns")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-owner"}, a: []string{"alice"}})
	require.NoError(t, err)
	assert.Equal(t, "p-backing-ns", client.createdNS)
}

func TestAddMemberEmptyPrincipalIDsIsValidation(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-2", "bob")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "read-only"}, a: []string{"bob"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Hint, "--group")
}

func TestAddMemberAmbiguousDisplayNameIsAmbiguousSelection(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-1", "alice", "local://u-1"),
			userObj("u-2", "alice", "local://u-2"),
		}},
		gets: map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-member"}, a: []string{"alice"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
}

func TestAddMemberGroupPassthrough(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{
		s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-member"},
		b: map[string]bool{"group": true},
		a: []string{"activedirectory_group://CN=Admins"},
	})
	require.NoError(t, err)
	gp, _, _ := unstructured.NestedString(client.created.Object, "groupPrincipalName")
	assert.Equal(t, "activedirectory_group://CN=Admins", gp)
	_, found, _ := unstructured.NestedString(client.created.Object, "userName")
	assert.False(t, found, "--group path never sets userName")
	_, found, _ = unstructured.NestedString(client.created.Object, "userPrincipalName")
	assert.False(t, found, "--group path never sets userPrincipalName")
}

func TestRemoveMemberNoMatchIsNotFound(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "local://u-1")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewRemoveMember(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"alice"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestRemoveMemberSingleMatchDeletes(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "local://u-1")}},
		prtbs:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{prtbObj("prtb-1", "u-1", "")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewRemoveMember(stubFactory{c: client})

	eff, exec, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"alice"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.True(t, eff.Destructive)
	assert.Equal(t, "projectroletemplatebindings/prtb-1", eff.Target)
	assert.Equal(t, "prtb-1-rv", eff.ResourceVersion)
	assert.Equal(t, []bool{true}, client.deleteDryRuns, "Plan dry-runs the admission preview")
	assert.Equal(t, []string{"prtb-1-rv"}, client.deletedRVs, "the dry-run preview carries the object's real resourceVersion, not empty")

	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"prtb-1", "prtb-1"}, client.deletedNames, "dry-run preview, then the real delete")
	assert.Equal(t, []string{"prtb-1-rv", "prtb-1-rv"}, client.deletedRVs, "the real delete also carries the precondition — a concurrent modification must surface as a conflict")
	deleted, _ := res.Data.(map[string]any)["deleted"].([]string)
	assert.Equal(t, []string{"prtb-1"}, deleted)
}

func TestRemoveMemberMultiMatchIsOneEffect(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "local://u-1")}},
		prtbs: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			prtbObj("prtb-owner", "u-1", ""),
			prtbObj("prtb-member", "u-1", ""),
		}},
		gets: map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewRemoveMember(stubFactory{c: client})

	eff, exec, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x"}, a: []string{"alice"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, "projectroletemplatebindings (2) for alice", eff.Target)
	assert.False(t, eff.ConflictDetection, "no single resourceVersion precondition applies across multiple objects")

	res, err := exec(context.Background())
	require.NoError(t, err)
	deleted, _ := res.Data.(map[string]any)["deleted"].([]string)
	assert.ElementsMatch(t, []string{"prtb-owner", "prtb-member"}, deleted)
}

func TestRemoveMemberGroupPassthrough(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		prtbs:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{prtbObj("prtb-g", "", "activedirectory_group://CN=Admins")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewRemoveMember(stubFactory{c: client})

	eff, _, err := planMember(t, cmd, memberIn{
		s: map[string]string{"project": "p-1", "cluster": "c-x"},
		b: map[string]bool{"group": true},
		a: []string{"activedirectory_group://CN=Admins"},
	})
	require.NoError(t, err)
	assert.Equal(t, "projectroletemplatebindings/prtb-g", eff.Target)
}

// TestAddMemberIsDestructive pins the confirmation gate: projectroletemplatebinding
// is a Sensitive kind, so add-member must need --yes exactly like the
// generic create path (resources/create.go) — a create Effect isn't
// automatically non-destructive just because engine.PlanCreate defaults it
// to false.
func TestAddMemberIsDestructive(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "local://u-1")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	eff, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-member"}, a: []string{"alice"}})
	require.NoError(t, err)
	assert.True(t, eff.Destructive)
}

// TestAddMemberDryRunTargetIsPrefixNotConcreteName pins that the previewed
// Target never implies a name the server will actually assign: obj uses
// generateName, so a real server's dry-run response carries a throwaway
// random suffix that Execute's separate create call will never reproduce.
func TestAddMemberDryRunTargetIsPrefixNotConcreteName(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "local://u-1")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	eff, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-member"}, a: []string{"alice"}})
	require.NoError(t, err)
	assert.Equal(t, "projectroletemplatebindings/project-member-*", eff.Target)
}

func TestAddMemberPrefersLocalPrincipal(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "activedirectory_user://CN=alice", "local://u-1")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-member"}, a: []string{"alice"}})
	require.NoError(t, err)
	upn, _, _ := unstructured.NestedString(client.created.Object, "userPrincipalName")
	assert.Equal(t, "local://u-1", upn, "prefers the local:// principal over principalIDs[0]")
}

func TestAddMemberFallsBackToFirstPrincipalWhenNoLocal(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		users:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{userObj("u-1", "alice", "activedirectory_user://CN=alice", "github_user://alice")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewAddMember(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{s: map[string]string{"project": "p-1", "cluster": "c-x", "role": "project-member"}, a: []string{"alice"}})
	require.NoError(t, err)
	upn, _, _ := unstructured.NestedString(client.created.Object, "userPrincipalName")
	assert.Equal(t, "activedirectory_user://CN=alice", upn, "no local:// entry falls back to principalIDs[0]")
}

var _ engine.ClusterClient = (*memberClient)(nil)
