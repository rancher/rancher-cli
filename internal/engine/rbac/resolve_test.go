package rbac

import (
	"context"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeClient fakes the user reads ResolveSubject performs, and the
// bindings list + create/delete recording grant/revoke use. Get errors can be
// forced per name via getErrs; List error via listErr (global) or
// listErrByGVR (per GVR resource). gets answers Get by "ns/name". When
// allowCreate is true, Create records the object instead of failing.
// When allowDelete is true, Delete records the call; deleteErrs forces
// an error on a real delete (dryRun == false) keyed by object name.
type fakeClient struct {
	users   []map[string]any // raw user objects, keyed into List and Get by metadata.name
	getErrs map[string]error
	listErr error
	lists   int

	listsByGVR   map[string]*unstructured.UnstructuredList // keyed by gvr.Resource
	listErrByGVR map[string]error                          // keyed by gvr.Resource; overrides listErr for that GVR
	gets         map[string]*unstructured.Unstructured     // keyed by "ns/name"

	created       *unstructured.Unstructured
	createdNS     string
	createDryRuns []bool
	allowCreate   bool

	allowDelete   bool
	deletedNames  []string
	deletedNS     []string
	deletedRVs    []string
	deleteDryRuns []bool
	deleteErrs    map[string]error // keyed by name; consulted only when dryRun == false
}

func userObj(id, display, username string, principals ...string) map[string]any {
	pids := make([]any, 0, len(principals))
	for _, p := range principals {
		pids = append(pids, p)
	}
	obj := map[string]any{
		"apiVersion":   "management.cattle.io/v3",
		"kind":         "User",
		"metadata":     map[string]any{"name": id},
		"username":     username,
		"principalIds": pids,
	}
	if display != "" {
		obj["displayName"] = display
	}
	return obj
}

func (f *fakeClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if err, ok := f.getErrs[name]; ok {
		return nil, err
	}
	if f.gets != nil {
		if obj, ok := f.gets[ns+"/"+name]; ok {
			return obj, nil
		}
	}
	for _, u := range f.users {
		if u["metadata"].(map[string]any)["name"] == name {
			return &unstructured.Unstructured{Object: u}, nil
		}
	}
	return nil, clierr.New(clierr.CodeNotFound, "not found: "+name)
}

func (f *fakeClient) List(_ context.Context, gvr schema.GroupVersionResource, _, _ string) (*unstructured.UnstructuredList, error) {
	f.lists++
	// Per-GVR error takes precedence over global listErr.
	if f.listErrByGVR != nil {
		if err, ok := f.listErrByGVR[gvr.Resource]; ok {
			return nil, err
		}
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Per-GVR data (bindings, projects, …).
	if f.listsByGVR != nil {
		if list, ok := f.listsByGVR[gvr.Resource]; ok {
			return list, nil
		}
	}
	// Default: return users (backwards compatibility for resolve tests).
	out := &unstructured.UnstructuredList{}
	for _, u := range f.users {
		out.Items = append(out.Items, unstructured.Unstructured{Object: u})
	}
	return out, nil
}

func (f *fakeClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if !f.allowCreate {
		return nil, fmt.Errorf("unexpected Create")
	}
	f.created, f.createdNS = obj, ns
	f.createDryRuns = append(f.createDryRuns, dryRun)
	return obj, nil
}
func (f *fakeClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("unexpected Update")
}
func (f *fakeClient) Delete(_ context.Context, _ schema.GroupVersionResource, ns, name, rv string, dryRun bool) error {
	if !f.allowDelete {
		return fmt.Errorf("unexpected Delete")
	}
	f.deletedNames = append(f.deletedNames, name)
	f.deletedNS = append(f.deletedNS, ns)
	f.deletedRVs = append(f.deletedRVs, rv)
	f.deleteDryRuns = append(f.deleteDryRuns, dryRun)
	if !dryRun {
		if err, ok := f.deleteErrs[name]; ok {
			return err
		}
	}
	return nil
}
func (f *fakeClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", fmt.Errorf("unexpected Logs")
}

type fakeFactory struct{ c engine.ClusterClient }

func (f fakeFactory) Cluster(context.Context, string) (engine.ClusterClient, error) { return f.c, nil }
func (f fakeFactory) ContextCluster() (string, error)                               { return "", nil }
func (f fakeFactory) ContextProject() (string, error)                               { panic("unused") }
func (f fakeFactory) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	if nameOrID == "" || nameOrID == "local" {
		return "local", nil
	}
	return nameOrID, nil
}

func TestResolveSubjectGroupPassthrough(t *testing.T) {
	got, err := ResolveSubject(context.Background(), fakeFactory{&fakeClient{}}, Subject{Kind: SubjectGroup, Principal: "okta_group://y"})
	require.NoError(t, err)
	assert.Equal(t, ResolvedSubject{Principal: "okta_group://y"}, got)
}

func TestResolveSubjectBareByIDNoList(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-abc", "Jane", "jane", "okta_user://x", "local://u-abc")}}
	got, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Bare: "u-abc"})
	require.NoError(t, err)
	assert.Equal(t, "u-abc", got.UserID)
	assert.Equal(t, "local://u-abc", got.Principal, "preferredPrincipal picks local://")
	assert.False(t, got.Provision)
	assert.Zero(t, c.lists, "exact id hit must not list")
}

func TestResolveSubjectBareByDisplayName(t *testing.T) {
	c := &fakeClient{users: []map[string]any{
		userObj("u-abc", "Jane Doe", "jane", "okta_user://x"),
		userObj("u-sys", "System account for Cluster c-x", "system-c-x", "system://c-x"),
	}}
	got, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Bare: "Jane Doe"})
	require.NoError(t, err)
	assert.Equal(t, "u-abc", got.UserID)
	assert.Equal(t, "okta_user://x", got.Principal)
}

func TestResolveSubjectBareSystemDisplayExcluded(t *testing.T) {
	c := &fakeClient{users: []map[string]any{
		userObj("u-sys", "Deploy Bot", "bot", "system://c-x"),
	}}
	_, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Bare: "Deploy Bot"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code, "system users must not resolve")
	assert.Empty(t, ce.Candidates, "system users must not appear as candidates")
}

func TestResolveSubjectSystemUserGetHitIsTerminal(t *testing.T) {
	// A successful Get that lands on a system user must be a terminal
	// validation error: no list fallback, no provisioning.
	sysPrincipal := "okta_user://sysalias"
	sysUsers := []map[string]any{
		userObj("u-sys", "System account for Cluster c-x", "system-c-x", "system://c-x"),
		userObj(ProvisionedUserID(sysPrincipal), "Sys Hash", "syshash", "system://c-y"),
	}
	tests := []struct {
		name string
		sub  Subject
	}{
		{name: "bare id", sub: Subject{Kind: SubjectUser, Bare: "u-sys"}},
		{name: "local principal", sub: Subject{Kind: SubjectUser, Principal: "local://u-sys"}},
		{name: "hash id hit", sub: Subject{Kind: SubjectUser, Principal: sysPrincipal}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{users: sysUsers}
			got, err := ResolveSubject(context.Background(), fakeFactory{c}, tt.sub)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.False(t, got.Provision, "system-user hit must never provision")
			assert.Zero(t, c.lists, "system-user hit must never fall back to a list")
		})
	}
}

func TestResolveSubjectBareAmbiguousDisplay(t *testing.T) {
	c := &fakeClient{users: []map[string]any{
		userObj("u-1", "Jane", "jane1", "okta_user://1"),
		userObj("u-2", "Jane", "jane2", "okta_user://2"),
	}}
	_, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Bare: "Jane"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Len(t, ce.Candidates, 2)
}

func TestResolveSubjectBareNotFoundNearMatchCandidates(t *testing.T) {
	users := []map[string]any{}
	for i := 0; i < 15; i++ {
		users = append(users, userObj(fmt.Sprintf("u-jan%02d", i), fmt.Sprintf("Jan %d", i), fmt.Sprintf("jan%d", i), fmt.Sprintf("okta_user://%d", i)))
	}
	users = append(users, userObj("u-zzz", "Zed", "zed", "okta_user://z"))
	c := &fakeClient{users: users}
	_, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Bare: "jan"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Len(t, ce.Candidates, 10, "near matches capped at 10")
	for _, cand := range ce.Candidates {
		assert.NotEqual(t, "u-zzz", cand.ID, "non-matching users excluded from candidates")
		assert.NotEmpty(t, cand.Name)
	}
}

func TestResolveSubjectBareCandidateNameFallsBackToUsername(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-abc", "", "jane", "okta_user://x")}}
	_, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Bare: "jan"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Candidates, 1)
	assert.Equal(t, "jane", ce.Candidates[0].Name)
}

func TestResolveSubjectLocalPrincipalStrips(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-abc", "Jane", "jane", "local://u-abc")}}
	got, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Principal: "local://u-abc"})
	require.NoError(t, err)
	assert.Equal(t, "u-abc", got.UserID)
	assert.Equal(t, "local://u-abc", got.Principal, "principal kept as given")
	assert.Zero(t, c.lists)
}

func TestResolveSubjectExternalPrincipalHashHit(t *testing.T) {
	principal := "okta_user://alice"
	id := ProvisionedUserID(principal)
	c := &fakeClient{users: []map[string]any{userObj(id, "Alice", "alice", principal)}}
	got, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Principal: principal})
	require.NoError(t, err)
	assert.Equal(t, id, got.UserID)
	assert.Equal(t, principal, got.Principal)
	assert.Zero(t, c.lists, "hash-id hit must not list")
}

func TestResolveSubjectExternalPrincipalListScanHit(t *testing.T) {
	// User exists under a non-hash id (e.g. created before EnsureUser, or
	// renamed) — the principalIds list scan must find it.
	c := &fakeClient{users: []map[string]any{userObj("user-vwq4j", "Admin", "admin", "okta_user://alice", "local://user-vwq4j")}}
	got, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Principal: "okta_user://alice"})
	require.NoError(t, err)
	assert.Equal(t, "user-vwq4j", got.UserID)
	assert.Equal(t, "okta_user://alice", got.Principal, "given principal, not preferredPrincipal")
	assert.Equal(t, 1, c.lists)
}

func TestResolveSubjectExternalPrincipalProvisions(t *testing.T) {
	c := &fakeClient{}
	got, err := ResolveSubject(context.Background(), fakeFactory{c}, Subject{Kind: SubjectUser, Principal: "okta_user://ghost"})
	require.NoError(t, err)
	assert.Equal(t, ResolvedSubject{Principal: "okta_user://ghost", Provision: true}, got)
}

func TestResolveSubjectErrorPassthrough(t *testing.T) {
	forbidden := clierr.New(clierr.CodeRBAC, "users is forbidden")
	tests := []struct {
		name string
		sub  Subject
		c    *fakeClient
	}{
		{name: "bare get forbidden", sub: Subject{Kind: SubjectUser, Bare: "u-abc"},
			c: &fakeClient{getErrs: map[string]error{"u-abc": forbidden}}},
		{name: "bare list forbidden", sub: Subject{Kind: SubjectUser, Bare: "jane"},
			c: &fakeClient{listErr: forbidden}},
		{name: "local get forbidden", sub: Subject{Kind: SubjectUser, Principal: "local://u-abc"},
			c: &fakeClient{getErrs: map[string]error{"u-abc": forbidden}}},
		{name: "hash get forbidden never provisions", sub: Subject{Kind: SubjectUser, Principal: "okta_user://alice"},
			c: &fakeClient{getErrs: map[string]error{ProvisionedUserID("okta_user://alice"): forbidden}}},
		{name: "principal list forbidden never provisions", sub: Subject{Kind: SubjectUser, Principal: "okta_user://alice"},
			c: &fakeClient{listErr: forbidden}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveSubject(context.Background(), fakeFactory{tt.c}, tt.sub)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeRBAC, ce.Code, "non-NOT_FOUND errors return unchanged")
			assert.False(t, got.Provision)
		})
	}
}
