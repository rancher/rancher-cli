package provisioning

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeClient is a minimal engine.ClusterClient fake covering only Get/List
// (the two verbs this package's resolver and read command use), mirroring
// the convention in internal/engine/projects' listClient and
// internal/engine/inspect's fakeClient.
type fakeClient struct {
	objects map[string]*unstructured.Unstructured // keyed by secret name
	getErr  error
	list    *unstructured.UnstructuredList
	listErr error
	mu      sync.Mutex
	gets    []string // the name of every Get
	lists   int
}

func (f *fakeClient) Get(_ context.Context, _ schema.GroupVersionResource, _ string, name string) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	f.gets = append(f.gets, name)
	f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if obj, ok := f.objects[name]; ok {
		return obj, nil
	}
	// Return the realistic curated clierr shape the dynamic client produces on
	// a 404 for the secrets GVR - this is the shape the fast-path fall-through
	// fix must treat as NOT_FOUND and route to the list-based resolution path.
	return nil, clierr.New(clierr.CodeNotFound, "secret not found: "+name).
		WithHint("list with: rancher get secrets")
}

func (f *fakeClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	f.mu.Lock()
	f.lists++
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.list == nil {
		return &unstructured.UnstructuredList{}, nil
	}
	return f.list, nil
}

func (f *fakeClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (f *fakeClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (f *fakeClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (f *fakeClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

var _ engine.ClusterClient = (*fakeClient)(nil)

// fakeFactory hands back a fixed ClusterClient for any cluster id.
type fakeFactory struct {
	c engine.ClusterClient
	// clusterErr, when set, makes Cluster return this error instead of c -
	// the injectable-error seam that lets a test assert
	// clients.Cluster()'s precedence over a simultaneous release-fetch
	// failure (versions.go's fetchVersionsDetailed).
	clusterErr error
}

func (f fakeFactory) Cluster(context.Context, string) (engine.ClusterClient, error) {
	if f.clusterErr != nil {
		return nil, f.clusterErr
	}
	return f.c, nil
}
func (f fakeFactory) ContextCluster() (string, error)                            { return "", nil }
func (f fakeFactory) ContextProject() (string, error)                            { return "", nil }
func (f fakeFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

var _ engine.ClusterClientFactory = (*fakeFactory)(nil)

// in is a minimal runtime.Input fake: this package's read command only ever
// consults Args().
type in struct {
	args []string
}

func (i in) String(string) string    { return "" }
func (i in) Bool(string) bool        { return false }
func (i in) Strings(string) []string { return nil }
func (i in) IsSet(string) bool       { return false }
func (i in) Args() []string          { return i.args }

var _ runtime.Input = in{}

// secretObj builds a fixture cattle-global-data Secret: metadata.name,
// optional annotations, optional data (key/value - values may be any type,
// including a []byte sentinel for the hygiene test, since production code
// only ever reads data KEY NAMES).
func secretObj(name string, annotations map[string]string, data map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "creationTimestamp": "2024-01-01T00:00:00Z"}
	if len(annotations) > 0 {
		ann := map[string]any{}
		for k, v := range annotations {
			ann[k] = v
		}
		meta["annotations"] = ann
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": meta}}
	if len(data) > 0 {
		obj.Object["data"] = data
	}
	return obj
}

// listOf is a list holding objs.
func listOf(objs ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	for _, o := range objs {
		l.Items = append(l.Items, *o)
	}
	return l
}

// TestResolveCloudCredentialByExactNamespacedID asserts the namespaced id
// form resolves from the one List, with no Get.
func TestResolveCloudCredentialByExactNamespacedID(t *testing.T) {
	t.Parallel()
	obj := secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil)
	client := &fakeClient{list: listOf(obj)}
	factory := fakeFactory{c: client}

	ref, warning, err := ResolveCloudCredential(t.Context(), factory, "cattle-global-data:cc-abc12")
	require.NoError(t, err)
	assert.Equal(t, "cattle-global-data:cc-abc12", ref.ID)
	assert.Equal(t, "cc-abc12", ref.SecretName)
	assert.Equal(t, "AWS Prod", ref.DisplayName)
	assert.Empty(t, warning)
	assert.Equal(t, 1, client.lists)
	assert.Empty(t, client.gets)
}

// TestResolveCloudCredentialByBareID asserts an id-typed value pays one
// List, where it once paid one GET, and sends no Get.
func TestResolveCloudCredentialByBareID(t *testing.T) {
	t.Parallel()
	obj := secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil)
	client := &fakeClient{list: listOf(obj)}
	factory := fakeFactory{c: client}

	ref, _, err := ResolveCloudCredential(t.Context(), factory, "cc-abc12")
	require.NoError(t, err)
	assert.Equal(t, "cattle-global-data:cc-abc12", ref.ID, "bare cc- ids normalize to the namespaced form")
	assert.Equal(t, 1, client.lists)
	assert.Empty(t, client.gets)
}

// TestResolveCloudCredentialIDWinsWithAWarning asserts an id that is also
// another credential's display name resolves to the id, with the warning
// naming the other credential, for the flag and for the positional.
func TestResolveCloudCredentialIDWinsWithAWarning(t *testing.T) {
	t.Parallel()
	abc := secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil)
	// The id is also readable by a GET, so a lookup that read it by id
	// first would resolve it without the warning.
	client := &fakeClient{objects: map[string]*unstructured.Unstructured{"cc-abc12": abc}, list: listOf(
		abc, secretObj("cc-zzz99", map[string]string{displayNameAnnotation: "cc-abc12"}, nil),
	)}
	factory := fakeFactory{c: client}
	ref, warning, err := ResolveCloudCredential(t.Context(), factory, "cc-abc12")
	require.NoError(t, err)
	assert.Equal(t, "cc-abc12", ref.SecretName)
	assert.Equal(t, "--cloud-credential 'cc-abc12' is the id of cloud credential 'cc-abc12'; cloud credential 'cc-zzz99' also has the display name 'cc-abc12' - pass --cloud-credential 'cc-zzz99' for that one", warning)

	cmd, ok := NewGetCloudCredential(factory).(runtime.ReadCommand)
	require.True(t, ok)
	res, err := cmd.Execute(t.Context(), in{args: []string{"cc-abc12"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"'cc-abc12' is the id of cloud credential 'cc-abc12'; cloud credential 'cc-zzz99' also has the display name 'cc-abc12' - pass 'cc-zzz99' for that one"}, res.Warnings)
}

// TestResolveCloudCredentialMissResolvesThroughOneGet asserts a credential
// the List did not carry yet resolves through one GET by id, a Secret that
// GET returns that is not a credential stays NOT_FOUND, any other GET
// failure is returned, and a value that cannot be a Secret name sends no
// GET.
func TestResolveCloudCredentialMissResolvesThroughOneGet(t *testing.T) {
	t.Parallel()
	fresh := secretObj("cc-fresh1", map[string]string{displayNameAnnotation: "New"}, nil)
	client := &fakeClient{objects: map[string]*unstructured.Unstructured{"cc-fresh1": fresh}}
	ref, _, err := ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cc-fresh1")
	require.NoError(t, err)
	assert.Equal(t, "cattle-global-data:cc-fresh1", ref.ID)
	assert.Equal(t, []string{"cc-fresh1"}, client.gets)

	plain := secretObj("db-password", nil, map[string]any{"password": []byte("x")})
	client = &fakeClient{objects: map[string]*unstructured.Unstructured{"db-password": plain}}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "db-password")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cloud credential not found: db-password", ce.Message)
	assert.Equal(t, []string{"db-password"}, client.gets)

	client = &fakeClient{getErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cc-fresh1")
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code, "a GET failure other than NOT_FOUND is returned")

	client = &fakeClient{}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "AWS Prod")
	require.Error(t, err)
	assert.Empty(t, client.gets, "a value that cannot be a Secret name sends no GET")
}

func TestResolveCloudCredentialByDisplayName(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil),
		*secretObj("cc-xyz99", map[string]string{displayNameAnnotation: "GCP Dev"}, nil),
	}}
	client := &fakeClient{list: list}
	factory := fakeFactory{c: client}

	ref, _, err := ResolveCloudCredential(t.Context(), factory, "AWS Prod")
	require.NoError(t, err)
	assert.Equal(t, "cattle-global-data:cc-abc12", ref.ID)
}

func TestResolveCloudCredentialDisplayNameBackfillFromObjectName(t *testing.T) {
	t.Parallel()
	obj := secretObj("cc-noannotation", nil, nil)
	client := &fakeClient{list: listOf(obj)}
	factory := fakeFactory{c: client}

	ref, _, err := ResolveCloudCredential(t.Context(), factory, "cc-noannotation")
	require.NoError(t, err)
	assert.Equal(t, "cc-noannotation", ref.DisplayName, "display name backfills from the object name when the annotation is absent")
}

func TestResolveCloudCredentialNotFound(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil),
	}}
	client := &fakeClient{list: list}
	factory := fakeFactory{c: client}

	_, _, err := ResolveCloudCredential(t.Context(), factory, "unknown")
	require.Error(t, err)
	var cerr *clierr.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, clierr.CodeNotFound, cerr.Code)
	require.Len(t, cerr.Candidates, 1)
	assert.Equal(t, "cc-abc12", cerr.Candidates[0].ID)
	assert.Equal(t, "AWS Prod", cerr.Candidates[0].Name)
}

// TestResolveCloudCredentialMissGetNotFoundKeepsTheDomainError asserts
// that when the GET by id after a List miss returns the curated "secret
// not found" shape (the dynamic client curates 404s by literal kind name),
// the resolver returns the domain-specific "cloud credential not found"
// shape - NOT "secret not found". Candidates come from the list.
func TestResolveCloudCredentialMissGetNotFoundKeepsTheDomainError(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil),
	}}
	// objects is empty: the GET by id of "cc-missing" returns the curated
	// clierr NOT_FOUND shape.
	client := &fakeClient{list: list}
	factory := fakeFactory{c: client}

	_, _, err := ResolveCloudCredential(t.Context(), factory, "cc-missing")
	require.Error(t, err)
	var cerr *clierr.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, clierr.CodeNotFound, cerr.Code)
	assert.Contains(t, cerr.Message, "cloud credential", "a miss must yield the domain error, not 'secret not found'")
	assert.NotContains(t, cerr.Message, "secret", "a miss must not leak the Secret kind implementation detail")
	require.NotEmpty(t, cerr.Candidates, "a miss must yield candidates from the list")
	assert.Equal(t, []string{"cc-missing"}, client.gets, "one GET by id")
	assert.Equal(t, "rancher get cloud-credential", cerr.Hint)
}

// TestResolveCloudCredentialLegacyNoPrefixResolvesByBareAndNamespacedID asserts
// that a legacy secret with no cc- prefix (credential-shaped only by
// its *credentialConfig- data key) must resolve via BOTH its bare name and
// its "cattle-global-data:<name>" namespaced form. Before the fix, the
// namespaced form fell through the List path and matched against the
// unstripped nameOrID, which never equals a bare object id - NOT_FOUND.
func TestResolveCloudCredentialLegacyNoPrefixResolvesByBareAndNamespacedID(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*secretObj("my-legacy-cred", nil, map[string]any{"amazonec2credentialConfig-accessKey": []byte("dummy")}),
	}}
	client := &fakeClient{list: list}
	factory := fakeFactory{c: client}

	bare, _, err := ResolveCloudCredential(t.Context(), factory, "my-legacy-cred")
	require.NoError(t, err)
	assert.Equal(t, "cattle-global-data:my-legacy-cred", bare.ID)

	namespaced, _, err := ResolveCloudCredential(t.Context(), factory, "cattle-global-data:my-legacy-cred")
	require.NoError(t, err)
	assert.Equal(t, "cattle-global-data:my-legacy-cred", namespaced.ID)

	assert.Equal(t, bare, namespaced)
}

func TestResolveCloudCredentialAmbiguousDisplayName(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*secretObj("cc-aaa11", map[string]string{displayNameAnnotation: "Shared"}, nil),
		*secretObj("cc-bbb22", map[string]string{displayNameAnnotation: "Shared"}, nil),
	}}
	client := &fakeClient{list: list}
	factory := fakeFactory{c: client}

	_, _, err := ResolveCloudCredential(t.Context(), factory, "Shared")
	require.Error(t, err)
	var cerr *clierr.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, clierr.CodeAmbiguousSelection, cerr.Code)
	require.Len(t, cerr.Candidates, 2)
	assert.Contains(t, cerr.Candidates[0].ID, "cc-", "candidates must name the exact-ID form")
	assert.Empty(t, cerr.Hint, "candidates already carry the selection guidance")
}

// TestResolveCloudCredentialAmbiguousCandidatesNarrowed asserts that
// when the display name collides, candidates must contain ONLY the
// credentials sharing that display name, not every credential of the query.
func TestResolveCloudCredentialAmbiguousCandidatesNarrowed(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*secretObj("cc-aaa11", map[string]string{displayNameAnnotation: "Shared"}, nil),
		*secretObj("cc-bbb22", map[string]string{displayNameAnnotation: "Shared"}, nil),
		*secretObj("cc-ccc33", map[string]string{displayNameAnnotation: "Other"}, nil),
	}}
	client := &fakeClient{list: list}
	factory := fakeFactory{c: client}

	_, _, err := ResolveCloudCredential(t.Context(), factory, "Shared")
	require.Error(t, err)
	var cerr *clierr.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, clierr.CodeAmbiguousSelection, cerr.Code)
	require.Len(t, cerr.Candidates, 2, "candidates must be the two colliding credentials, not the unrelated one")
	ids := []string{cerr.Candidates[0].ID, cerr.Candidates[1].ID}
	assert.Contains(t, ids, "cc-aaa11")
	assert.Contains(t, ids, "cc-bbb22")
	assert.NotContains(t, ids, "cc-ccc33")
}

func TestResolveCloudCredentialDriverFromDataKeyPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		data   map[string]any
		driver string
	}{
		{"amazonec2", map[string]any{"amazonec2credentialConfig-accessKey": []byte("dummy")}, "amazonec2"},
		{"vmwarevsphere", map[string]any{"vmwarevspherecredentialConfig-password": []byte("dummy")}, "vmwarevsphere"},
		{"noCredentialConfigKey", map[string]any{"foo": []byte("dummy")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := secretObj("cc-drv1", nil, tc.data)
			client := &fakeClient{list: listOf(obj)}
			factory := fakeFactory{c: client}

			ref, _, err := ResolveCloudCredential(t.Context(), factory, "cc-drv1")
			require.NoError(t, err)
			assert.Equal(t, tc.driver, ref.Driver)
		})
	}
}

// TestAgeFromRendersBlankForAbsentTimestamp asserts the house deviation from
// kubectl's own "<unknown>" placeholder: absent metadata.creationTimestamp
// renders blank, never a placeholder.
func TestAgeFromRendersBlankForAbsentTimestamp(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "cc-x"}}}
	assert.Empty(t, ageFrom(obj))
}

// TestAgeFromUsesHumanDurationFromNowSeam asserts the aligned formatter
// (duration.HumanDuration, not ShortHumanDuration) through the now() seam
// shared with the kind-table renderer's AGE column.
func TestAgeFromUsesHumanDurationFromNowSeam(t *testing.T) {
	// NOT parallel: mutates the package-level now var.
	restore := now
	now = func() time.Time { return time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC) }
	defer func() { now = restore }()

	obj := secretObj("cc-x", nil, nil) // creationTimestamp "2024-01-01T00:00:00Z"
	assert.Equal(t, "120m", ageFrom(obj))
}

func TestNewGetCloudCredentialDescribe(t *testing.T) {
	t.Parallel()
	d := NewGetCloudCredential(fakeFactory{}).Describe()
	assert.Equal(t, "get cloud-credential", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)
}

func TestGetCloudCredentialListsOnlyCredentialShapedSecrets(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, map[string]any{"amazonec2credentialConfig-accessKey": []byte("x")}),
		*secretObj("cc-xyz99", nil, nil),
		*secretObj("unrelated-secret", nil, map[string]any{"password": []byte("x")}),
	}}
	client := &fakeClient{list: list}
	factory := fakeFactory{c: client}
	cmd, ok := NewGetCloudCredential(factory).(runtime.ReadCommand)
	require.True(t, ok)

	result, err := cmd.Execute(t.Context(), in{}, nil)
	require.NoError(t, err)
	rows, ok := result.Data.(cloudCredRows)
	require.True(t, ok)
	require.Len(t, rows, 2, "unrelated-secret is neither cc-prefixed nor credentialConfig-keyed")
}

func TestGetCloudCredentialByNameResolvesOne(t *testing.T) {
	t.Parallel()
	obj := secretObj("cc-abc12", map[string]string{displayNameAnnotation: "AWS Prod"}, nil)
	client := &fakeClient{list: listOf(obj)}
	factory := fakeFactory{c: client}
	cmd, ok := NewGetCloudCredential(factory).(runtime.ReadCommand)
	require.True(t, ok)

	result, err := cmd.Execute(t.Context(), in{args: []string{"cc-abc12"}}, nil)
	require.NoError(t, err)
	rows, ok := result.Data.(cloudCredRows)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, "cc-abc12", rows[0].SecretName)
}

// TestResolveCloudCredentialNeverLeaksSecretValue is the non-negotiable
// hygiene test: the sentinel secret VALUE must never surface
// in any returned struct, Result, or error string produced by either the
// resolver or the read command, however rendered.
func TestResolveCloudCredentialNeverLeaksSecretValue(t *testing.T) {
	t.Parallel()
	const sentinel = "SUPERSECRET"
	obj := secretObj("cc-secret1", map[string]string{displayNameAnnotation: "Prod"}, map[string]any{
		"amazonec2credentialConfig-accessKey": []byte(sentinel),
	})
	client := &fakeClient{
		objects: map[string]*unstructured.Unstructured{"cc-secret1": obj},
		list:    &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*obj}},
	}
	factory := fakeFactory{c: client}

	ref, _, err := ResolveCloudCredential(t.Context(), factory, "cc-secret1")
	require.NoError(t, err)
	assert.NotContains(t, fmt.Sprintf("%v", ref), sentinel)
	assert.NotContains(t, fmt.Sprintf("%+v", ref), sentinel)

	cmd, ok := NewGetCloudCredential(factory).(runtime.ReadCommand)
	require.True(t, ok)

	listResult, err := cmd.Execute(t.Context(), in{}, nil)
	require.NoError(t, err)
	assert.NotContains(t, fmt.Sprintf("%v", listResult), sentinel)
	assert.NotContains(t, fmt.Sprintf("%+v", listResult), sentinel)

	singleResult, err := cmd.Execute(t.Context(), in{args: []string{"cc-secret1"}}, nil)
	require.NoError(t, err)
	assert.NotContains(t, fmt.Sprintf("%v", singleResult), sentinel)
	assert.NotContains(t, fmt.Sprintf("%+v", singleResult), sentinel)

	// An ambiguous/not-found error path also builds candidates from these
	// same secrets - assert its rendering is clean too.
	_, _, err = ResolveCloudCredential(t.Context(), factory, "no-such-credential")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinel)
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

// TestResolveCloudCredentialListRefusedFallsBackToOneGet asserts that a
// List refused with RBAC, for a value that is a Secret name, resolves
// through one GET by id with no warning, for the flag and the positional;
// a GET 404 is the cloud credential NOT_FOUND with no candidates; a GET
// refusal is returned as the GET's error; and a value that cannot be a
// Secret name, or a List failure other than RBAC, returns the List's error
// with no GET.
func TestResolveCloudCredentialListRefusedFallsBackToOneGet(t *testing.T) {
	t.Parallel()
	listErr := clierr.New(clierr.CodeRBAC, "secrets is forbidden: cannot list resource \"secrets\"")
	own := secretObj("cc-own12", map[string]string{displayNameAnnotation: "Mine"}, nil)

	client := &fakeClient{listErr: listErr, objects: map[string]*unstructured.Unstructured{"cc-own12": own}}
	ref, warning, err := ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cattle-global-data:cc-own12")
	require.NoError(t, err)
	assert.Equal(t, CloudCredRef{ID: "cattle-global-data:cc-own12", SecretName: "cc-own12", DisplayName: "Mine"}, ref)
	assert.Empty(t, warning)
	assert.Equal(t, []string{"cc-own12"}, client.gets)

	client = &fakeClient{listErr: listErr, objects: map[string]*unstructured.Unstructured{"cc-own12": own}}
	cmd, ok := NewGetCloudCredential(fakeFactory{c: client}).(runtime.ReadCommand)
	require.True(t, ok)
	result, err := cmd.Execute(t.Context(), in{args: []string{"cc-own12"}}, nil)
	require.NoError(t, err)
	rows, ok := result.Data.(cloudCredRows)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, "cc-own12", rows[0].SecretName)
	assert.Empty(t, result.Warnings)
	assert.Equal(t, []string{"cc-own12"}, client.gets)

	client = &fakeClient{listErr: listErr}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cc-gone1")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cloud credential not found: cc-gone1", ce.Message)
	assert.Equal(t, "rancher get cloud-credential", ce.Hint)
	assert.Empty(t, ce.Candidates)
	assert.Equal(t, []string{"cc-gone1"}, client.gets)

	client = &fakeClient{listErr: listErr}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cattle-global-data:cc-gone1")
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, "cloud credential not found: cattle-global-data:cc-gone1", ce.Message, "the value as given")
	assert.Equal(t, []string{"cc-gone1"}, client.gets)

	plain := secretObj("db-password", nil, map[string]any{"password": []byte("x")})
	client = &fakeClient{listErr: listErr, objects: map[string]*unstructured.Unstructured{"db-password": plain}}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "db-password")
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code, "a Secret that is not a credential is NOT_FOUND")
	assert.Equal(t, "cloud credential not found: db-password", ce.Message)

	getErr := clierr.New(clierr.CodeRBAC, "secrets \"cc-other\" is forbidden")
	client = &fakeClient{listErr: listErr, getErr: getErr}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cc-other")
	assert.Same(t, getErr, err, "a refused GET is returned as the GET's error")

	client = &fakeClient{listErr: listErr}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "AWS Prod")
	assert.Same(t, listErr, err, "a value that cannot be a Secret name returns the List's error")
	assert.Empty(t, client.gets)

	serverErr := clierr.New(clierr.CodeGeneric, "the server is unavailable")
	client = &fakeClient{listErr: serverErr, objects: map[string]*unstructured.Unstructured{"cc-own12": own}}
	_, _, err = ResolveCloudCredential(t.Context(), fakeFactory{c: client}, "cc-own12")
	assert.Same(t, serverErr, err, "a List failure other than RBAC is returned")
	assert.Empty(t, client.gets)
}
