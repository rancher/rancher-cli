package resources

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// lookupClient answers every Get with miss and every List with objects, or
// with listErr, or never when block is set (it returns when its context ends).
type lookupClient struct {
	memClient
	miss    error
	objects []unstructured.Unstructured
	listErr error
	block   bool
	// ignoreCtx answers a List whose context has ended; nilList answers it
	// with no list and no error.
	ignoreCtx bool
	nilList   bool
	lists     int
}

func (c *lookupClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, c.miss
}

func (c *lookupClient) List(ctx context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.UnstructuredList, error) {
	c.lists++
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil && !c.ignoreCtx {
		return nil, err
	}
	if c.nilList {
		return nil, nil
	}
	if c.listErr != nil {
		return nil, c.listErr
	}
	return &unstructured.UnstructuredList{Items: c.objects}, nil
}

// projectListerFactory is a factory that also lists projects.
type projectListerFactory struct {
	recFactory
	projects []engine.ProjectRef
	err      error
	listed   []string
}

func (f *projectListerFactory) ListProjects(_ context.Context, clusterID string) ([]engine.ProjectRef, error) {
	f.listed = append(f.listed, clusterID)
	return f.projects, f.err
}

func handleUserObj(id, username, display string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":    map[string]any{"name": id},
		"username":    username,
		"displayName": display,
	}}
}

func globalRoleObj(id, display string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":    map[string]any{"name": id},
		"displayName": display,
	}}
}

const lookupPatch = `[{"op":"add","path":"/metadata/labels","value":{"a":"b"}}]`

// lookupVerbs runs one generated verb on k with typed as its argument and
// returns the verb's error. Each verb gets the same fake client and factory.
var lookupVerbs = map[string]func(t *testing.T, k kinds.Kind, f engine.ClusterClientFactory, in input) error{
	"get": func(t *testing.T, k kinds.Kind, f engine.ClusterClientFactory, in input) error {
		t.Helper()
		_, err := NewGet(k, f).(runtime.ReadCommand).Execute(t.Context(), in, nil)
		return err
	},
	"get wait": func(t *testing.T, k kinds.Kind, f engine.ClusterClientFactory, in input) error {
		t.Helper()
		in.b = map[string]bool{"wait": true}
		in.s = mergeStrings(in.s, map[string]string{"for": "state=active"})
		_, err := NewGet(k, f).(runtime.ReadCommand).Execute(t.Context(), in, nil)
		return err
	},
	"patch": func(t *testing.T, k kinds.Kind, f engine.ClusterClientFactory, in input) error {
		t.Helper()
		in.s = mergeStrings(in.s, map[string]string{"patch": lookupPatch})
		_, _, err := NewPatch(k, f, nil).(runtime.MutatingCommand).Plan(t.Context(), in)
		return err
	},
	"delete": func(t *testing.T, k kinds.Kind, f engine.ClusterClientFactory, in input) error {
		t.Helper()
		_, _, err := NewDelete(k, f).(runtime.MutatingCommand).Plan(t.Context(), in)
		return err
	},
}

func mergeStrings(a, b map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

func asClierr(t *testing.T, err error) *clierr.Error {
	t.Helper()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	return ce
}

// TestLookupNamesTheIDOnEachField asserts a miss on a typed value that is one
// field of one object says which field and which id, lists the object as a
// candidate, and hints the command that reads it, for every row and field.
func TestLookupNamesTheIDOnEachField(t *testing.T) {
	t.Parallel()
	rows := []struct {
		kind    string
		typed   string
		objects []unstructured.Unstructured
		message string
		id      string
		name    string
	}{
		{"user", "alice", []unstructured.Unstructured{handleUserObj("u-abc12", "alice", "Alice A"), handleUserObj("u-other", "bob", "Bob")},
			"user not found: alice; 'alice' is the username of user u-abc12", "u-abc12", "Alice A"},
		{"user", "Alice A", []unstructured.Unstructured{handleUserObj("u-abc12", "alice", "Alice A"), handleUserObj("u-other", "bob", "Bob")},
			"user not found: Alice A; 'Alice A' is the display name of user u-abc12", "u-abc12", "Alice A"},
		{"user", "carol", []unstructured.Unstructured{handleUserObj("u-c", "carol", "")},
			"user not found: carol; 'carol' is the username of user u-c", "u-c", "carol"},
		{"roletemplate", "Owner", []unstructured.Unstructured{roleTemplateObj("rt-owner", "cluster", "Owner"), roleTemplateObj("rt-m", "cluster", "Member")},
			"roletemplate not found: Owner; 'Owner' is the display name of roletemplate rt-owner", "rt-owner", "Owner"},
		{"globalrole", "Admin", []unstructured.Unstructured{globalRoleObj("admin", "Admin")},
			"globalrole not found: Admin; 'Admin' is the display name of globalrole admin", "admin", "Admin"},
		{"nodedriver", "Linode", []unstructured.Unstructured{nodeDriverObj("linode", "Linode", true, false)},
			"nodedriver not found: Linode; 'Linode' is the display name of nodedriver linode", "linode", "Linode"},
	}
	for _, r := range rows {
		k := mustKind(t, r.kind)
		for verb, run := range lookupVerbs {
			if verb == "delete" && !k.Deletable {
				continue
			}
			t.Run(r.kind+" "+r.typed+" "+verb, func(t *testing.T) {
				t.Parallel()
				client := &lookupClient{miss: kinds.NotFoundCurated(k, r.typed, ""), objects: r.objects}
				err := run(t, k, &recFactory{client: client}, input{args: []string{r.typed}})
				ce := asClierr(t, err)
				assert.Equal(t, clierr.CodeNotFound, ce.Code)
				assert.Equal(t, r.message, ce.Message)
				assert.Equal(t, clierr.Rancher+" get "+k.Name+" '"+r.id+"'", ce.Hint)
				assert.Equal(t, []clierr.Candidate{{ID: r.id, Name: r.name}}, ce.Candidates)
				assert.Equal(t, 1, client.lists)
			})
		}
	}
}

// TestLookupBothFieldsOfOneObjectJoinsTheLabels asserts a value that is both
// the username and the display name of one user is one match naming both.
func TestLookupBothFieldsOfOneObjectJoinsTheLabels(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "user")
	client := &lookupClient{miss: kinds.NotFoundCurated(k, "Bob", ""), objects: []unstructured.Unstructured{handleUserObj("u-1", "Bob", "Bob")}}
	err := lookupVerbs["get"](t, k, &recFactory{client: client}, input{args: []string{"Bob"}})
	ce := asClierr(t, err)
	assert.Equal(t, "user not found: Bob; 'Bob' is the username and display name of user u-1", ce.Message)
	assert.Equal(t, clierr.Rancher+" get user 'u-1'", ce.Hint)
	assert.Len(t, ce.Candidates, 1)
}

// TestLookupSeveralMatchesStayNotFoundWithCandidates asserts two objects with
// the typed value stay NOT_FOUND, list both sorted by id with their names, and
// keep the list hint, for one-field and two-field rows.
func TestLookupSeveralMatchesStayNotFoundWithCandidates(t *testing.T) {
	t.Parallel()
	user := mustKind(t, "user")
	client := &lookupClient{miss: kinds.NotFoundCurated(user, "Bob", ""), objects: []unstructured.Unstructured{
		handleUserObj("u-2", "robert", "Bob"), handleUserObj("u-1", "Bob", ""), handleUserObj("u-3", "carol", "Carol"),
	}}
	err := lookupVerbs["get"](t, user, &recFactory{client: client}, input{args: []string{"Bob"}})
	ce := asClierr(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, 4, ce.ExitCode())
	assert.Equal(t, "user not found: Bob; 2 users have that username or display name", ce.Message)
	assert.Equal(t, clierr.Rancher+" get users", ce.Hint)
	assert.Equal(t, []clierr.Candidate{{ID: "u-1", Name: "Bob"}, {ID: "u-2", Name: "Bob"}}, ce.Candidates)

	rt := mustKind(t, "roletemplate")
	rtClient := &lookupClient{miss: kinds.NotFoundCurated(rt, "Owner", ""), objects: []unstructured.Unstructured{
		roleTemplateObj("rt-b", "project", "Owner"), roleTemplateObj("rt-a", "cluster", "Owner"),
	}}
	err = lookupVerbs["patch"](t, rt, &recFactory{client: rtClient}, input{args: []string{"Owner"}})
	ce = asClierr(t, err)
	assert.Equal(t, "roletemplate not found: Owner; 2 roletemplates have that display name", ce.Message)
	assert.Equal(t, clierr.Rancher+" get roletemplates", ce.Hint)
	assert.Equal(t, []clierr.Candidate{{ID: "rt-a", Name: "Owner"}, {ID: "rt-b", Name: "Owner"}}, ce.Candidates)
}

// TestLookupMatchIsExactAndCaseSensitive asserts a value that differs in case
// or by a substring does not match.
func TestLookupMatchIsExactAndCaseSensitive(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "user")
	miss := kinds.NotFoundCurated(k, "alice", "")
	client := &lookupClient{miss: miss, objects: []unstructured.Unstructured{handleUserObj("u-1", "Alice", "alice smith"), handleUserObj("u-2", "alice2", "")}}
	err := lookupVerbs["get"](t, k, &recFactory{client: client}, input{args: []string{"alice"}})
	assert.Same(t, miss, err)
}

// TestLookupDegradesToTheOriginalError asserts a lookup with no match, an
// empty list, a failed LIST (403) and a cancelled context all return the very
// error the Get returned.
func TestLookupDegradesToTheOriginalError(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "user")
	cases := map[string]*lookupClient{
		"none":  {objects: []unstructured.Unstructured{handleUserObj("u-1", "bob", "Bob")}},
		"403":   {listErr: clierr.New(clierr.CodeRBAC, "the server refused list users")},
		"empty": {},
	}
	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			miss := kinds.NotFoundCurated(k, "alice", "")
			client.miss = miss
			err := lookupVerbs["get"](t, k, &recFactory{client: client}, input{args: []string{"alice"}})
			assert.Same(t, miss, err)
			assert.Equal(t, "user not found: alice", asClierr(t, err).Message)
			assert.Equal(t, clierr.Rancher+" get users", asClierr(t, err).Hint)
			assert.Empty(t, asClierr(t, err).Candidates)
		})
	}

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		miss := kinds.NotFoundCurated(k, "alice", "")
		client := &lookupClient{miss: miss, objects: []unstructured.Unstructured{handleUserObj("u-1", "alice", "")}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		got := lookupHandle(ctx, &recFactory{client: client}, client, k, "", "alice", miss)
		assert.Same(t, miss, got)
	})
}

// TestLookupTimeoutReturnsTheOriginalError asserts a LIST that never answers
// ends at the lookup timeout and the Get's error stands. The timeout is a
// package variable; the test is serial and restores it.
func TestLookupTimeoutReturnsTheOriginalError(t *testing.T) {
	old := handleLookupTimeout
	handleLookupTimeout = 20 * time.Millisecond
	t.Cleanup(func() { handleLookupTimeout = old })

	k := mustKind(t, "user")
	miss := kinds.NotFoundCurated(k, "alice", "")
	client := &lookupClient{miss: miss, block: true}
	start := time.Now()
	err := lookupVerbs["get"](t, k, &recFactory{client: client}, input{args: []string{"alice"}})
	assert.Same(t, miss, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1, client.lists)
}

// TestLookupLeavesOtherErrorsAlone asserts a kind without a handle, a Get
// error that is not a miss, and an RBAC refusal on a kind that is not a
// project make no lookup request.
func TestLookupLeavesOtherErrorsAlone(t *testing.T) {
	t.Parallel()
	user := mustKind(t, "user")
	forbidden := clierr.New(clierr.CodeRBAC, "the server refused get users 'alice'")
	generic := clierr.New(clierr.CodeGeneric, "boom")
	for name, miss := range map[string]error{"rbac": forbidden, "generic": generic} {
		client := &lookupClient{miss: miss, objects: []unstructured.Unstructured{handleUserObj("u-1", "alice", "")}}
		err := lookupVerbs["get"](t, user, &recFactory{client: client}, input{args: []string{"alice"}})
		assert.Same(t, miss, err, name)
		assert.Zero(t, client.lists, name)
	}

	binding := mustKind(t, "globalrolebinding")
	require.Nil(t, binding.Handle)
	miss := kinds.NotFoundCurated(binding, "alice", "")
	client := &lookupClient{miss: miss, objects: []unstructured.Unstructured{handleUserObj("u-1", "alice", "")}}
	err := lookupVerbs["get"](t, binding, &recFactory{client: client}, input{args: []string{"alice"}})
	assert.Same(t, miss, err)
	assert.Zero(t, client.lists)
}

// TestLookupProjectNamesTheIDWithTheCluster asserts a project miss on a
// display name lists the caller's projects on the typed cluster through the
// factory, names the id and puts the cluster in the hint, and never reads a
// LIST through the verb's own client.
func TestLookupProjectNamesTheIDWithTheCluster(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "project")
	for _, verb := range []string{"get", "patch", "delete"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			client := &lookupClient{miss: kinds.NotFoundCurated(k, "Web Team", "")}
			f := &projectListerFactory{
				client:   client,
				projects: []engine.ProjectRef{{ID: "p-abc12", DisplayName: "Web Team"}, {ID: "p-zzz", DisplayName: "Data"}},
			}
			err := lookupVerbs[verb](t, k, f, input{args: []string{"Web Team"}, s: map[string]string{"cluster": "c-x"}})
			ce := asClierr(t, err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, "project not found: Web Team; 'Web Team' is the display name of project p-abc12", ce.Message)
			assert.Equal(t, clierr.Rancher+" get project 'p-abc12' --cluster 'c-x'", ce.Hint)
			assert.Equal(t, []clierr.Candidate{{ID: "p-abc12", Name: "Web Team"}}, ce.Candidates)
			assert.Equal(t, []string{"c-x"}, f.listed)
			assert.Zero(t, client.lists)
		})
	}
}

// TestLookupProjectSeveralNotFoundKeepsTheListHint asserts two projects with
// the typed display name stay NOT_FOUND, with their ids as candidates and the
// hint the miss already had.
func TestLookupProjectSeveralNotFoundKeepsTheListHint(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "project")
	miss := kinds.NotFoundCurated(k, "Web", "")
	f := &projectListerFactory{
		client:   &lookupClient{miss: miss},
		projects: []engine.ProjectRef{{ID: "p-2", DisplayName: "Web"}, {ID: "p-1", DisplayName: "Web"}},
	}
	err := lookupVerbs["get"](t, k, f, input{args: []string{"Web"}, s: map[string]string{"cluster": "c-x"}})
	ce := asClierr(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: Web; 2 projects have that display name", ce.Message)
	assert.Equal(t, clierr.Rancher+" get projects", ce.Hint)
	assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "Web"}, {ID: "p-2", Name: "Web"}}, ce.Candidates)
}

// TestLookupProjectForbiddenKeepsRBACAndAddsTheID asserts a project member's
// refusal of a Get by display name keeps its code, exit status and message,
// and adds the id, the candidates and the cluster-scoped hints, whether one or
// several projects match; with no match the refusal is returned itself.
func TestLookupProjectForbiddenKeepsRBACAndAddsTheID(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "project")
	const refusal = `the server refused get projects.management.cattle.io 'Web Team' in c-x`

	forbidden := func() *clierr.Error { return clierr.New(clierr.CodeRBAC, refusal) }
	run := func(t *testing.T, verb string, projects []engine.ProjectRef, miss error) (*projectListerFactory, error) {
		t.Helper()
		f := &projectListerFactory{client: &lookupClient{miss: miss}, projects: projects}
		return f, lookupVerbs[verb](t, k, f, input{args: []string{"Web Team"}, s: map[string]string{"cluster": "c-x"}})
	}

	t.Run("one match", func(t *testing.T) {
		t.Parallel()
		for _, verb := range []string{"get", "patch", "delete"} {
			_, err := run(t, verb, []engine.ProjectRef{{ID: "p-abc12", DisplayName: "Web Team"}}, forbidden())
			ce := asClierr(t, err)
			assert.Equal(t, clierr.CodeRBAC, ce.Code, verb)
			assert.Equal(t, 6, ce.ExitCode(), verb)
			assert.Equal(t, refusal+"; 'Web Team' is the display name of project p-abc12", ce.Message, verb)
			assert.Equal(t, clierr.Rancher+" get project 'p-abc12' --cluster 'c-x'", ce.Hint, verb)
			assert.Equal(t, []clierr.Candidate{{ID: "p-abc12", Name: "Web Team"}}, ce.Candidates, verb)
		}
	})
	t.Run("several", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, "get", []engine.ProjectRef{{ID: "p-2", DisplayName: "Web Team"}, {ID: "p-1", DisplayName: "Web Team"}}, forbidden())
		ce := asClierr(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, refusal+"; 2 projects have that display name", ce.Message)
		assert.Equal(t, clierr.Rancher+" access projects --cluster 'c-x'", ce.Hint)
		assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "Web Team"}, {ID: "p-2", Name: "Web Team"}}, ce.Candidates)
	})
	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		miss := forbidden()
		f, err := run(t, "get", []engine.ProjectRef{{ID: "p-9", DisplayName: "Data"}}, miss)
		assert.Same(t, miss, err)
		assert.Equal(t, []string{"c-x"}, f.listed)
	})
	t.Run("envelope", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, "get", []engine.ProjectRef{{ID: "p-abc12", DisplayName: "Web Team"}}, forbidden())
		raw, merr := json.Marshal(asClierr(t, err))
		require.NoError(t, merr)
		var env struct {
			Code       string             `json:"code"`
			Message    string             `json:"message"`
			Hint       string             `json:"hint"`
			Candidates []clierr.Candidate `json:"candidates"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		assert.Equal(t, "RBAC", env.Code)
		assert.Equal(t, refusal+"; 'Web Team' is the display name of project p-abc12", env.Message)
		assert.Equal(t, clierr.Rancher+" get project 'p-abc12' --cluster 'c-x'", env.Hint)
		assert.Equal(t, []clierr.Candidate{{ID: "p-abc12", Name: "Web Team"}}, env.Candidates)
	})
}

// TestLookupProjectDegrades asserts a factory without a project lister, a
// lister that fails, and a get that waits through a refusal all leave the
// Get's error as it was.
func TestLookupProjectDegrades(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "project")
	in := input{args: []string{"Web Team"}, s: map[string]string{"cluster": "c-x"}}

	miss := kinds.NotFoundCurated(k, "Web Team", "")
	client := &lookupClient{miss: miss}
	err := lookupVerbs["get"](t, k, &recFactory{client: client}, in)
	assert.Same(t, miss, err, "a factory without a lister")
	assert.Zero(t, client.lists)

	miss = kinds.NotFoundCurated(k, "Web Team", "")
	f := &projectListerFactory{client: &lookupClient{miss: miss}, err: errors.New("steve is down")}
	assert.Same(t, miss, lookupVerbs["delete"](t, k, f, in), "a failed lister")

	forbidden := clierr.New(clierr.CodeRBAC, "the server refused get projects")
	f = &projectListerFactory{
		client:   &lookupClient{miss: forbidden},
		projects: []engine.ProjectRef{{ID: "p-abc12", DisplayName: "Web Team"}},
	}
	waitIn := input{args: []string{"Web Team"}, s: map[string]string{"cluster": "c-x"}}
	rc := NewGet(k, f).(runtime.ReadCommand)
	waitIn.b = map[string]bool{"wait": true}
	waitIn.s["for"] = "state=active"
	waitIn.s["timeout"] = "50ms"
	_, werr := rc.Execute(t.Context(), waitIn, nil)
	require.Error(t, werr)
	assert.NotContains(t, werr.Error(), "p-abc12")
	assert.Empty(t, f.listed, "a wait polls through a refusal and looks nothing up")
}

// TestHandleRowsDeclareTheIDArgument asserts every row with a handle names its
// positional id on get, patch and delete, with the row's description, and
// that the cluster keeps its name with only a description.
func TestHandleRowsDeclareTheIDArgument(t *testing.T) {
	t.Parallel()
	count := 0
	for _, k := range kinds.All() {
		if k.Handle == nil {
			continue
		}
		count++
		descs := map[string]runtime.Descriptor{
			"get":   NewGet(k, nil).Describe(),
			"patch": NewPatch(k, nil, nil).Describe(),
		}
		if k.Deletable {
			descs["delete"] = NewDelete(k, nil).Describe()
		}
		for verb, d := range descs {
			require.Len(t, d.Args, 1, k.Name+" "+verb)
			assert.Equal(t, runtime.ArgSpec{Name: "id", Type: "string", Required: verb != "get", Desc: k.ArgDesc}, d.Args[0], k.Name+" "+verb)
		}
	}
	assert.Equal(t, 5, count)

	cluster := mustKind(t, "cluster")
	for _, d := range []runtime.Descriptor{NewGet(cluster, nil).Describe(), NewPatch(cluster, nil, nil).Describe()} {
		assert.Equal(t, "name", d.Args[0].Name, d.Name)
		assert.Equal(t, "cluster id or display name", d.Args[0].Desc, d.Name)
	}
	pod := mustKind(t, "pod")
	assert.Equal(t, runtime.ArgSpec{Name: "name", Type: "string"}, NewGet(pod, nil).Describe().Args[0])
}

// TestHandleRowRefusalsNameTheID asserts the refusals that name the positional
// say id on a row with a handle and keep name on a row without one.
func TestHandleRowRefusalsNameTheID(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"user", "project"} {
		k := mustKind(t, name)
		in := input{s: map[string]string{"cluster": "c-x", "patch": lookupPatch}}
		_, _, err := NewDelete(k, &recFactory{client: &memClient{}}).(runtime.MutatingCommand).Plan(t.Context(), in)
		require.ErrorContains(t, err, "argument <id> is required", name)
		_, _, err = NewPatch(k, &recFactory{client: &memClient{}}, nil).(runtime.MutatingCommand).Plan(t.Context(), in)
		require.ErrorContains(t, err, "argument <id> is required", name)
	}
	crtb := mustKind(t, "clusterroletemplatebinding")
	_, _, err := NewDelete(crtb, &recFactory{client: &memClient{}}).(runtime.MutatingCommand).Plan(t.Context(), input{})
	require.ErrorContains(t, err, "argument <name> is required")

	user := mustKind(t, "user")
	_, err = NewGet(user, &recFactory{client: &memClient{}}).(runtime.ReadCommand).Execute(t.Context(), input{b: map[string]bool{"wait": true}}, nil)
	require.ErrorContains(t, err, "--wait requires the id argument")
	pod := mustKind(t, "pod")
	_, err = NewGet(pod, &recFactory{client: &memClient{}}).(runtime.ReadCommand).Execute(t.Context(), input{b: map[string]bool{"wait": true}}, nil)
	require.ErrorContains(t, err, "--wait requires the name argument")

	project := mustKind(t, "project")
	_, err = NewGet(project, &recFactory{client: &memClient{}}).(runtime.ReadCommand).Execute(t.Context(), input{args: []string{"x"}}, nil)
	require.ErrorContains(t, err, "--cluster is required when you give the id of one project")
	_, err = NewGet(crtb, &recFactory{client: &memClient{}}).(runtime.ReadCommand).Execute(t.Context(), input{args: []string{"x"}}, nil)
	require.ErrorContains(t, err, "--cluster is required when you give the name of one clusterroletemplatebinding")
}

// TestLookupIgnoresAnAnswerAfterTheContextEnds asserts a list that arrives
// after the context ended is not used: the Get's error stands.
func TestLookupIgnoresAnAnswerAfterTheContextEnds(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "user")
	miss := kinds.NotFoundCurated(k, "alice", "")
	client := &lookupClient{miss: miss, ignoreCtx: true, objects: []unstructured.Unstructured{handleUserObj("u-1", "alice", "")}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := lookupHandle(ctx, &recFactory{client: client}, client, k, "", "alice", miss)
	assert.Same(t, miss, got)
	assert.Equal(t, 1, client.lists)
}

// TestLookupProjectWithoutAClusterIDReadsNothing asserts a project miss with no
// resolved cluster id lists no projects and keeps the Get's error.
func TestLookupProjectWithoutAClusterIDReadsNothing(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "project")
	miss := kinds.NotFoundCurated(k, "Web Team", "")
	f := &projectListerFactory{client: &lookupClient{miss: miss}, projects: []engine.ProjectRef{{ID: "p-1", DisplayName: "Web Team"}}}
	got := lookupHandle(t.Context(), f, f.client, k, "", "Web Team", miss)
	assert.Same(t, miss, got)
	assert.Empty(t, f.listed)
}

// TestLookupCountsEachIDOnce asserts an id that a list returns twice, or a
// lister returns twice, is one match.
func TestLookupCountsEachIDOnce(t *testing.T) {
	t.Parallel()
	user := mustKind(t, "user")
	client := &lookupClient{miss: kinds.NotFoundCurated(user, "alice", ""), objects: []unstructured.Unstructured{
		handleUserObj("u-1", "alice", "Alice"), handleUserObj("u-1", "alice", "Alice"),
	}}
	ce := asClierr(t, lookupVerbs["get"](t, user, &recFactory{client: client}, input{args: []string{"alice"}}))
	assert.Equal(t, "user not found: alice; 'alice' is the username of user u-1", ce.Message)
	assert.Len(t, ce.Candidates, 1)

	project := mustKind(t, "project")
	f := &projectListerFactory{
		client:   &lookupClient{miss: kinds.NotFoundCurated(project, "Web", "")},
		projects: []engine.ProjectRef{{ID: "p-1", DisplayName: "Web"}, {ID: "p-1", DisplayName: "Web"}},
	}
	ce = asClierr(t, lookupVerbs["get"](t, project, f, input{args: []string{"Web"}, s: map[string]string{"cluster": "c-x"}}))
	assert.Equal(t, "project not found: Web; 'Web' is the display name of project p-1", ce.Message)
	assert.Len(t, ce.Candidates, 1)
}

// TestLookupNilListKeepsTheError asserts a list call that returns neither a
// list nor an error leaves the Get's error as it was.
func TestLookupNilListKeepsTheError(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "user")
	miss := kinds.NotFoundCurated(k, "alice", "")
	client := &lookupClient{miss: miss, nilList: true}
	assert.Same(t, miss, lookupVerbs["get"](t, k, &recFactory{client: client}, input{args: []string{"alice"}}))
}

// TestLookupLeavesTheInputErrorUnchanged asserts a lookup that adds text, a
// hint and candidates builds a new error: the error it was given keeps its
// message, hint and candidates, and the new error keeps the same cause.
func TestLookupLeavesTheInputErrorUnchanged(t *testing.T) {
	t.Parallel()
	cause := errors.New("the server answered 404")
	user := mustKind(t, "user")
	project := mustKind(t, "project")
	userObjects := []unstructured.Unstructured{handleUserObj("u-1", "bob", "Bob"), handleUserObj("u-2", "robert", "Bob")}
	projects := []engine.ProjectRef{{ID: "p-1", DisplayName: "Web"}, {ID: "p-2", DisplayName: "Web"}}

	cases := map[string]struct {
		k       kinds.Kind
		typed   string
		in      *clierr.Error
		ns      string
		wantErr string
	}{
		"one user":      {user, "bob", clierr.New(clierr.CodeNotFound, "user not found: bob").WithHint("list with: x").WithCause(cause), "", "user not found: bob; 'bob' is the username of user u-1"},
		"several users": {user, "Bob", clierr.New(clierr.CodeNotFound, "user not found: Bob").WithHint("list with: x").WithCause(cause), "", "user not found: Bob; 2 users have that username or display name"},
		"project":       {project, "Web", clierr.New(clierr.CodeRBAC, "refused").WithHint("see x").WithCause(cause), "c-x", "refused; 2 projects have that display name"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			message, hint := c.in.Message, c.in.Hint
			client := &lookupClient{objects: userObjects}
			f := &projectListerFactory{client: client, projects: projects}
			out := lookupHandle(t.Context(), f, client, c.k, c.ns, c.typed, c.in)
			require.NotSame(t, c.in, out)
			assert.Equal(t, c.wantErr, asClierr(t, out).Message)
			assert.Equal(t, message, c.in.Message)
			assert.Equal(t, hint, c.in.Hint)
			assert.Empty(t, c.in.Candidates)
			assert.Same(t, cause, errors.Unwrap(out))
		})
	}
}
