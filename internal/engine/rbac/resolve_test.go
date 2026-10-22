package rbac

import (
	"context"
	"errors"
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

	createFn func(gvr schema.GroupVersionResource) (*unstructured.Unstructured, error)

	createCalls   int
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

func (f *fakeClient) Create(_ context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	f.createCalls++
	if f.createFn != nil {
		return f.createFn(gvr)
	}
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

type fakeFactory struct {
	c        engine.ClusterClient
	cErr     error
	ctxAware bool
}

func (f fakeFactory) Cluster(ctx context.Context, _ string) (engine.ClusterClient, error) {
	if f.ctxAware && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.cErr != nil {
		return nil, f.cErr
	}
	return f.c, nil
}
func (f fakeFactory) ContextCluster() (string, error) { return "", nil }
func (f fakeFactory) ContextProject() (string, error) { return "", nil }
func (f fakeFactory) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	if nameOrID == "" || nameOrID == "local" {
		return "local", nil
	}
	return nameOrID, nil
}

func TestResolveSubjectGroupPassthrough(t *testing.T) {
	got, err := ResolveSubject(t.Context(), fakeFactory{c: &fakeClient{}}, Subject{Kind: SubjectGroup, Principal: "okta_group://y"})
	require.NoError(t, err)
	assert.Equal(t, ResolvedSubject{Principal: "okta_group://y"}, got)
}

func TestResolveSubjectBareByIDNoList(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-abc", "Jane", "jane", "okta_user://x", "local://u-abc")}}
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "u-abc"})
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
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "Jane Doe"})
	require.NoError(t, err)
	assert.Equal(t, "u-abc", got.UserID)
	assert.Equal(t, "okta_user://x", got.Principal)
}

func TestResolveSubjectBareSystemDisplayExcluded(t *testing.T) {
	c := &fakeClient{users: []map[string]any{
		userObj("u-sys", "Deploy Bot", "bot", "system://c-x"),
	}}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "Deploy Bot"})
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
			got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, tt.sub)
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
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "Jane"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Len(t, ce.Candidates, 2)
}

func TestResolveSubjectBareNotFoundNearMatchCandidates(t *testing.T) {
	users := []map[string]any{}
	for i := range 15 {
		users = append(users, userObj(fmt.Sprintf("u-jan%02d", i), fmt.Sprintf("Jan %d", i), fmt.Sprintf("jan%d", i), fmt.Sprintf("okta_user://%d", i)))
	}
	users = append(users, userObj("u-zzz", "Zed", "zed", "okta_user://z"))
	c := &fakeClient{users: users}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "jan"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Len(t, ce.Candidates, CandidateLimit, "the first 10 of the near matches")
	assert.Equal(t, 15, ce.CandidatesTotal, "every near match is counted")
	for _, cand := range ce.Candidates {
		assert.NotEqual(t, "u-zzz", cand.ID, "non-matching users excluded from candidates")
		assert.NotEmpty(t, cand.Name)
	}
}

// TestResolveSubjectBareNotFoundNearMatchesAtTheLimitHaveNoTotal asserts a
// near-match list of exactly 10 is shown whole with no total.
func TestResolveSubjectBareNotFoundNearMatchesAtTheLimitHaveNoTotal(t *testing.T) {
	t.Parallel()
	var users []map[string]any
	for i := range CandidateLimit {
		users = append(users, userObj(fmt.Sprintf("u-jan%02d", i), fmt.Sprintf("Jan %d", i), fmt.Sprintf("jan%d", i), fmt.Sprintf("okta_user://%d", i)))
	}
	users = append(users, userObj("u-zzz", "Zed", "zed", "okta_user://z"))
	c := &fakeClient{users: users}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "jan"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a *clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Len(t, ce.Candidates, CandidateLimit)
	assert.Zero(t, ce.CandidatesTotal)
}

func TestResolveSubjectBareCandidateNameFallsBackToUsername(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-abc", "", "jane", "okta_user://x")}}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "jan"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Candidates, 1)
	assert.Equal(t, "jane", ce.Candidates[0].Name)
}

func TestResolveSubjectLocalPrincipalStrips(t *testing.T) {
	c := &fakeClient{users: []map[string]any{userObj("u-abc", "Jane", "jane", "local://u-abc")}}
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Principal: "local://u-abc"})
	require.NoError(t, err)
	assert.Equal(t, "u-abc", got.UserID)
	assert.Equal(t, "local://u-abc", got.Principal, "principal kept as given")
	assert.Zero(t, c.lists)
}

func TestResolveSubjectExternalPrincipalHashHit(t *testing.T) {
	// Happy path: hash-named user still owns the principal.
	// Resolution must succeed. The ownership scan is always run for external
	// principals, so c.lists is 1.
	principal := "okta_user://alice"
	id := ProvisionedUserID(principal)
	c := &fakeClient{users: []map[string]any{userObj(id, "Alice", "alice", principal)}}
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Principal: principal})
	require.NoError(t, err)
	assert.Equal(t, id, got.UserID)
	assert.Equal(t, principal, got.Principal)
	assert.Equal(t, 1, c.lists, "ownership scan always runs for external principals")
}

func TestResolveSubjectExternalPrincipalHashHitOccupiedConflict(t *testing.T) {
	// A user was provisioned as hash(okta_user://alice). Its principal has
	// since been replaced with okta_user://renamed and no other user owns
	// okta_user://alice. EnsureUser would derive the same id and get
	// AlreadyExists - provisioning must be blocked with CodeConflict, not
	// silently entered.
	requestedPrincipal := "okta_user://alice"
	id := ProvisionedUserID(requestedPrincipal)
	c := &fakeClient{users: []map[string]any{
		userObj(id, "Alice", "alice", "okta_user://renamed"),
	}}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Principal: requestedPrincipal})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, id, "conflict message must name the occupied user id")
	assert.Contains(t, ce.Message, requestedPrincipal)
}

func TestResolveSubjectExternalPrincipalHashHitAmbiguousOwners(t *testing.T) {
	// Hash-named owner carries the principal AND a second user also carries it.
	// Even though the hash-id Get succeeds and ownership is verified,
	// the ownership scan must find both and return AMBIGUOUS_SELECTION.
	// Tested in both list orders (hash user first, other user first).
	principal := "okta_user://shared"
	hashID := ProvisionedUserID(principal)
	hashUser := userObj(hashID, "Hash", "hash", principal)
	otherUser := userObj("u-other", "Other", "other", principal)
	tests := []struct {
		name  string
		users []map[string]any
	}{
		{name: "hash first", users: []map[string]any{hashUser, otherUser}},
		{name: "other first", users: []map[string]any{otherUser, hashUser}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &fakeClient{users: tt.users}
			_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Principal: principal})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
			assert.Len(t, ce.Candidates, 2)
			assert.Empty(t, ce.Hint, "candidates already carry the selection guidance")
		})
	}
}

func TestResolveSubjectExternalPrincipalAmbiguousOwners(t *testing.T) {
	// Two non-system users share the same principal (malformed cluster);
	// neither has the hash id - forces the fallback-scan-only path.
	// Resolution must return AMBIGUOUS_SELECTION regardless of list order.
	principal := "okta_user://shared"
	user1 := userObj("u-aaa", "Alice", "alice", principal)
	user2 := userObj("u-bbb", "Bob", "bob", principal)
	tests := []struct {
		name  string
		users []map[string]any
	}{
		{name: "alice first", users: []map[string]any{user1, user2}},
		{name: "bob first", users: []map[string]any{user2, user1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &fakeClient{users: tt.users}
			_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Principal: principal})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
			assert.Len(t, ce.Candidates, 2)
			assert.Empty(t, ce.Hint, "candidates already carry the selection guidance")
		})
	}
}

func TestResolveSubjectExternalPrincipalListScanHit(t *testing.T) {
	// User exists under a non-hash id (e.g. created before EnsureUser, or
	// renamed) - the principalIds list scan must find it.
	c := &fakeClient{users: []map[string]any{userObj("user-vwq4j", "Admin", "admin", "okta_user://alice", "local://user-vwq4j")}}
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Principal: "okta_user://alice"})
	require.NoError(t, err)
	assert.Equal(t, "user-vwq4j", got.UserID)
	assert.Equal(t, "okta_user://alice", got.Principal, "given principal, not preferredPrincipal")
	assert.Equal(t, 1, c.lists)
}

func TestResolveSubjectExternalPrincipalProvisions(t *testing.T) {
	c := &fakeClient{}
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Principal: "okta_user://ghost"})
	require.NoError(t, err)
	assert.Equal(t, ResolvedSubject{Principal: "okta_user://ghost", Provision: true}, got)
}

func TestResolveSubjectBareByUsername(t *testing.T) {
	// Motivating case: user has no displayName, only a username.
	c := &fakeClient{users: []map[string]any{
		userObj("u-adm", "", "adm-user", "local://u-adm"),
	}}
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "adm-user"})
	require.NoError(t, err)
	assert.Equal(t, "u-adm", got.UserID)
	assert.Equal(t, "local://u-adm", got.Principal)
}

func TestResolveSubjectBareUsernameDisplayCollision(t *testing.T) {
	// User A has displayName "dev"; user B has username "dev" - input "dev" is ambiguous.
	c := &fakeClient{users: []map[string]any{
		userObj("u-aaa", "dev", "aaa", "local://u-aaa"),
		userObj("u-bbb", "", "dev", "local://u-bbb"),
	}}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "dev"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Len(t, ce.Candidates, 2)
	assert.Empty(t, ce.Hint, "candidates already carry the selection guidance")
}

func TestResolveSubjectBareSameUserBothFields(t *testing.T) {
	// One user whose displayName and username are both "ops" - matches once, not ambiguous.
	c := &fakeClient{users: []map[string]any{
		userObj("u-ops", "ops", "ops", "local://u-ops"),
	}}
	got, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "ops"})
	require.NoError(t, err)
	assert.Equal(t, "u-ops", got.UserID)
	assert.Equal(t, "local://u-ops", got.Principal)
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
			got, err := ResolveSubject(t.Context(), fakeFactory{c: tt.c}, tt.sub)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeRBAC, ce.Code, "non-NOT_FOUND errors return unchanged")
			assert.False(t, got.Provision)
		})
	}
}

// TestResolveSubjectNoNearMatchFallsBackToPool mirrors
// TestGrantNoNearMatchRoleFallsBackToPool: a subject typo that shares no
// substring with any seeded user falls back to the whole pool so the
// NOT_FOUND always names valid options.
func TestResolveSubjectNoNearMatchFallsBackToPool(t *testing.T) {
	t.Parallel()
	// "zz-no-such-user" shares no substring with "alice", "bob", or "carol".
	c := &fakeClient{users: []map[string]any{
		userObj("u-alice", "Alice", "alice", "local://u-alice"),
		userObj("u-bob", "Bob", "bob", "local://u-bob"),
		userObj("u-carol", "Carol", "carol", "local://u-carol"),
	}}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: c}, Subject{Kind: SubjectUser, Bare: "zz-no-such-user"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "zz-no-such-user")
	// No substring overlap with any user → fallback to the full pool.
	require.NotEmpty(t, ce.Candidates, "zero near-matches must fall back to the user pool")
	ids := make([]string, 0, len(ce.Candidates))
	for _, cand := range ce.Candidates {
		ids = append(ids, cand.ID)
	}
	assert.Contains(t, ids, "u-alice")
	assert.Contains(t, ids, "u-bob")
	assert.Contains(t, ids, "u-carol")
}

func (*fakeClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*fakeClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

func (f fakeFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

// TestResolveSubjectBareNotFoundWholePoolIsCutInListOrder asserts a name that
// near-matches no user lists the first 10 users in list order and counts the
// whole pool.
func TestResolveSubjectBareNotFoundWholePoolIsCutInListOrder(t *testing.T) {
	t.Parallel()
	var users []map[string]any
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("u-%02d", i)
		users = append(users, userObj(id, "", "member", "local://"+id))
	}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: &fakeClient{users: users}},
		Subject{Kind: SubjectUser, Bare: "lx-no-such-user"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a *clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	var ids []string
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	assert.Equal(t, []string{"u-01", "u-02", "u-03", "u-04", "u-05", "u-06", "u-07", "u-08", "u-09", "u-10"}, ids)
	assert.Equal(t, 12, ce.CandidatesTotal)
}

// TestResolveSubjectBareNotFoundWholePoolAtTheLimitHasNoTotal asserts a pool
// of exactly 10 users is shown whole with no total.
func TestResolveSubjectBareNotFoundWholePoolAtTheLimitHasNoTotal(t *testing.T) {
	t.Parallel()
	var users []map[string]any
	for i := range CandidateLimit {
		id := fmt.Sprintf("u-%02d", i)
		users = append(users, userObj(id, "", "member", "local://"+id))
	}
	_, err := ResolveSubject(t.Context(), fakeFactory{c: &fakeClient{users: users}},
		Subject{Kind: SubjectUser, Bare: "lx-no-such-user"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a *clierr.Error, got %v", err)
	assert.Len(t, ce.Candidates, CandidateLimit)
	assert.Zero(t, ce.CandidatesTotal)
}

// TestSubjectCandidatesReturnsTheFullList asserts SubjectCandidates cuts
// nothing: every near match, or the whole pool when none matches.
func TestSubjectCandidatesReturnsTheFullList(t *testing.T) {
	t.Parallel()
	var users []*unstructured.Unstructured
	for i := 1; i <= 15; i++ {
		id := fmt.Sprintf("u-%02d", i)
		users = append(users, &unstructured.Unstructured{Object: userObj(id, "", "member", "local://"+id)})
	}
	assert.Len(t, SubjectCandidates(users, "u-"), 15, "every near match")
	assert.Len(t, SubjectCandidates(users, "lx-no-such-user"), 15, "the whole pool")
	assert.Len(t, SubjectCandidates(users, "u-1"), 6, "a partial near match")
}
