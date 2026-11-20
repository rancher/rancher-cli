package catalog

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

type createCall struct {
	gvr    schema.GroupVersionResource
	ns     string
	obj    *unstructured.Unstructured
	dryRun bool
}

// addCluster is the local cluster as the add-repository command sees it: a
// list of ClusterRepos and a create that records its calls.
type addCluster struct {
	engine.ClusterClient
	items     []*unstructured.Unstructured
	listErr   error
	createErr error
	lists     int
	creates   []createCall
}

func (c *addCluster) List(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.UnstructuredList, error) {
	c.lists++
	if c.listErr != nil {
		return nil, c.listErr
	}
	list := &unstructured.UnstructuredList{}
	for _, it := range c.items {
		list.Items = append(list.Items, *it)
	}
	return list, nil
}

func (c *addCluster) Create(_ context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.creates = append(c.creates, createCall{gvr: gvr, ns: ns, obj: obj.DeepCopy(), dryRun: dryRun})
	if c.createErr != nil {
		return nil, c.createErr
	}
	for _, it := range c.items {
		if it.GetName() == obj.GetName() {
			return nil, clierr.New(clierr.CodeConflict, "clusterrepos.catalog.cattle.io "+clierr.ShellQuote(obj.GetName())+" already exists")
		}
	}
	return obj.DeepCopy(), nil
}

// addClients hands out one addCluster and records the cluster ids asked for.
type addClients struct {
	engine.ClusterClientFactory
	cluster *addCluster
	asked   []string
}

func (f *addClients) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.asked = append(f.asked, id)
	return f.cluster, nil
}

// editionClients is addClients with the edition provider.
type editionClients struct {
	*addClients
	edition engine.Edition
	err     error
	reads   int
}

func (f *editionClients) ServerEdition(context.Context) (engine.Edition, error) {
	f.reads++
	return f.edition, f.err
}

// addInput is a test input that carries the positional arguments.
type addInput struct {
	input
	args []string
}

func (i addInput) Args() []string { return i.args }

func plannedAdd(t *testing.T, clients engine.ClusterClientFactory, name string) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	cmd, ok := NewExtensionAddRepository(clients).(runtime.MutatingCommand)
	require.True(t, ok)
	return cmd.Plan(t.Context(), addInput{args: []string{name}})
}

func codeOf(t *testing.T, err error) *clierr.Error {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v is not a clierr error", err)
	return ce
}

func repoWith(name string, fields map[string]any, statusURL string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.cattle.io/v1",
		"kind":       "ClusterRepo",
		"metadata":   map[string]any{"name": name},
		"spec":       fields,
	}}
	if statusURL != "" {
		obj.Object["status"] = map[string]any{"url": statusURL}
	}
	return obj
}

// addRepositoryName is the registered name of the command.
var addRepositoryName = NewExtensionAddRepository(nil).Describe().Name

const (
	partnerURL = "https://github.com/rancher/partner-extensions"
	primeURL   = "https://github.com/rancher/ui-plugin-charts"
)

// TestAddRepositoryRefusals asserts each refusal of the command, in the order
// of its checks, with its code, text and the requests it cost: an unknown name
// costs none, the Prime repository reads the edition and a community answer for
// the partner repository does not.
func TestAddRepositoryRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arg       string
		edition   engine.Edition
		editionOK bool
		err       error
		items     []*unstructured.Unstructured
		code      string
		message   string
		hint      string
		reads     int
		lists     int
	}{
		{name: "unknown name", arg: "rancher-charts", edition: engine.EditionPrime, editionOK: true, code: clierr.CodeValidation,
			message: "rancher-charts is not a Rancher extension repository; the Rancher extension repositories are partner-extensions and, on Rancher Prime, rancher-ui-plugins"},
		{name: "community with the Prime repository", arg: "rancher-ui-plugins", edition: engine.EditionCommunity, editionOK: true, code: clierr.CodeValidation,
			message: "rancher-ui-plugins is offered only on Rancher Prime and this server is not Rancher Prime; the repository every edition offers is partner-extensions", reads: 1},
		{name: "unknown edition with the Prime repository", arg: "rancher-ui-plugins", edition: engine.EditionUnknown, editionOK: true, code: clierr.CodeValidation,
			message: "the edition of this server could not be read; rancher-ui-plugins is offered only on Rancher Prime and is not added; the repository every edition offers is partner-extensions", reads: 1},
		{name: "edition read failed", arg: "rancher-ui-plugins", editionOK: true, err: errors.New("boom"), code: clierr.CodeValidation,
			message: "the edition of this server could not be read; rancher-ui-plugins is offered only on Rancher Prime and is not added; the repository every edition offers is partner-extensions", reads: 1},
		{name: "already added by status url", arg: "partner-extensions", edition: engine.EditionCommunity, editionOK: true, code: clierr.CodeConflict,
			items:   []*unstructured.Unstructured{repoWith("mine", map[string]any{"gitRepo": "https://example.com/x"}, partnerURL)},
			message: "the repository " + partnerURL + " is already added as ClusterRepo 'mine'", hint: "get clusterrepo 'mine' --cluster 'local'", lists: 1},
		{name: "already added by git repo", arg: "partner-extensions", edition: engine.EditionCommunity, editionOK: true, code: clierr.CodeConflict,
			items:   []*unstructured.Unstructured{repoWith("mine", map[string]any{"gitRepo": partnerURL}, "")},
			message: "the repository " + partnerURL + " is already added as ClusterRepo 'mine'", hint: "get clusterrepo 'mine' --cluster 'local'", lists: 1},
		{name: "already added by url", arg: "partner-extensions", edition: engine.EditionCommunity, editionOK: true, code: clierr.CodeConflict,
			items:   []*unstructured.Unstructured{repoWith("mine", map[string]any{"url": partnerURL}, "")},
			message: "the repository " + partnerURL + " is already added as ClusterRepo 'mine'", hint: "get clusterrepo 'mine' --cluster 'local'", lists: 1},
		{name: "already added, Prime repository", arg: "rancher-ui-plugins", edition: engine.EditionPrime, editionOK: true, code: clierr.CodeConflict,
			items:   []*unstructured.Unstructured{repoWith("mine", map[string]any{"gitRepo": primeURL}, "")},
			message: "the repository " + primeURL + " is already added as ClusterRepo 'mine'", hint: "get clusterrepo 'mine' --cluster 'local'", reads: 1, lists: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clients := &editionClients{addClients: &addClients{cluster: &addCluster{items: tt.items}}, edition: tt.edition, err: tt.err}
			_, _, err := plannedAdd(t, clients, tt.arg)
			require.Error(t, err)
			ce := codeOf(t, err)
			assert.Equal(t, tt.code, ce.Code)
			assert.Equal(t, tt.message, ce.Message)
			if tt.hint != "" {
				assert.Contains(t, ce.Hint, tt.hint)
			}
			assert.Equal(t, tt.reads, clients.reads, "edition reads")
			assert.Equal(t, tt.lists, clients.cluster.lists, "ClusterRepo lists")
			assert.Empty(t, clients.cluster.creates, "no create is sent for a refusal")
		})
	}
}

// TestAddRepositoryDoesNotReadTheEditionForThePartnerRepository asserts the
// partner repository, offered on every edition, never asks for the edition.
func TestAddRepositoryDoesNotReadTheEditionForThePartnerRepository(t *testing.T) {
	t.Parallel()
	clients := &editionClients{addClients: &addClients{cluster: &addCluster{}}, edition: engine.EditionCommunity}
	_, _, err := plannedAdd(t, clients, "partner-extensions")
	require.NoError(t, err)
	assert.Zero(t, clients.reads)
}

// TestAddRepositoryWithoutAnEditionProvider asserts a factory that cannot
// report the edition gives the unknown edition: the Prime repository is
// refused and the partner repository is added.
func TestAddRepositoryWithoutAnEditionProvider(t *testing.T) {
	t.Parallel()
	clients := &addClients{cluster: &addCluster{}}
	_, _, err := plannedAdd(t, clients, "rancher-ui-plugins")
	require.Error(t, err)
	assert.Equal(t, clierr.CodeValidation, codeOf(t, err).Code)
	_, _, err = plannedAdd(t, clients, "partner-extensions")
	require.NoError(t, err)
}

// TestAddRepositoryBody asserts the object of the dry-run create and of the
// real create holds exactly the fields the Rancher UI sends, on the local
// cluster, and that the first is a dry run and the second is not.
func TestAddRepositoryBody(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, url string }{{"partner-extensions", partnerURL}, {"rancher-ui-plugins", primeURL}} {
		clients := &editionClients{addClients: &addClients{cluster: &addCluster{}}, edition: engine.EditionPrime}
		_, exec, err := plannedAdd(t, clients, tt.name)
		require.NoError(t, err, tt.name)
		require.Len(t, clients.cluster.creates, 1)
		_, err = exec(t.Context())
		require.NoError(t, err)
		require.Len(t, clients.cluster.creates, 2)
		assert.Equal(t, []string{engine.LocalCluster}, clients.asked[:1])
		want := map[string]any{
			"apiVersion": "catalog.cattle.io/v1",
			"kind":       "ClusterRepo",
			"metadata":   map[string]any{"name": tt.name},
			"spec":       map[string]any{"gitRepo": tt.url, "gitBranch": "main"},
		}
		for i, call := range clients.cluster.creates {
			assert.Equal(t, want, call.obj.Object, "call %d", i)
			assert.Equal(t, "clusterrepos", call.gvr.Resource)
			assert.Equal(t, "catalog.cattle.io", call.gvr.Group)
			assert.Empty(t, call.ns)
		}
		assert.True(t, clients.cluster.creates[0].dryRun)
		assert.False(t, clients.cluster.creates[1].dryRun)
	}
}

// TestAddRepositoryEffect asserts the preview: from a server-side dry-run, not
// destructive, reversible, with conflict detection, for the ClusterRepo on the
// local cluster; and that the applied result names the repository and carries
// the download note.
func TestAddRepositoryEffect(t *testing.T) {
	t.Parallel()
	clients := &addClients{cluster: &addCluster{}}
	eff, exec, err := plannedAdd(t, clients, "partner-extensions")
	require.NoError(t, err)
	assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.False(t, eff.Destructive)
	assert.True(t, eff.Reversible)
	assert.True(t, eff.ConflictDetection)
	assert.Equal(t, "clusterrepo", eff.DisplayKind)
	assert.Equal(t, "partner-extensions", eff.DisplayName)
	assert.Equal(t, engine.LocalCluster, eff.Cluster)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "ClusterRepo", res.Kind)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "Rancher downloads the repository from github.com after this command")
	assert.Contains(t, res.Warnings[0], "get clusterrepo 'partner-extensions' --cluster 'local'")
	assert.Contains(t, res.Warnings[0], "air-gapped")
}

// TestAddRepositoryConflictFromTheDryRun asserts a ClusterRepo of the same name
// with another URL, which the server's dry-run answers with AlreadyExists, is
// CONFLICT, and a refused list is the server's answer.
func TestAddRepositoryConflictFromTheDryRun(t *testing.T) {
	t.Parallel()
	clients := &addClients{cluster: &addCluster{createErr: clierr.New(clierr.CodeConflict, "clusterrepos.catalog.cattle.io \"partner-extensions\" already exists")}}
	_, _, err := plannedAdd(t, clients, "partner-extensions")
	require.Error(t, err)
	assert.Equal(t, clierr.CodeConflict, codeOf(t, err).Code)

	clients = &addClients{cluster: &addCluster{listErr: clierr.New(clierr.CodeRBAC, "forbidden")}}
	_, _, err = plannedAdd(t, clients, "partner-extensions")
	require.Error(t, err)
	assert.Equal(t, clierr.CodeRBAC, codeOf(t, err).Code)
	assert.Empty(t, clients.cluster.creates)
}

// TestAddRepositoryDescribe asserts the descriptor: mutating, not destructive,
// one required positional with no static enum (the Prime repository is offered
// only on Prime) and no wait flags.
func TestAddRepositoryDescribe(t *testing.T) {
	t.Parallel()
	d := NewExtensionAddRepository(nil).Describe()
	assert.Equal(t, "extension add-repository", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
	assert.Equal(t, "false", d.Destructive)
	assert.Empty(t, d.Flags, "the command has no flag of its own and no wait flag")
	require.Len(t, d.Args, 1)
	assert.True(t, d.Args[0].Required)
	assert.Empty(t, d.Args[0].Enum)
	assert.Contains(t, d.Desc, "Add Partners Extensions Repository")
	assert.Contains(t, d.Desc, "Add Rancher Prime Extensions Repository")
}
