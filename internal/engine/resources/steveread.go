package resources

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// steveGetter returns the getter that reads Steve, and true. It returns false
// unless the Kubernetes API responded with 403 and the command's clients can
// read Rancher's root.
func (c getCmd) steveGetter(k8sErr error) (steve.Getter, bool) {
	if st, ok := clierr.HTTPStatus(k8sErr); !ok || st != http.StatusForbidden {
		return nil, false
	}
	roots, ok := c.clients.(engine.RootClientFactory)
	if !ok {
		return nil, false
	}
	return steve.RootFactoryGetter(roots), true
}

// steveRefused reports whether err is a 403 or 404 response from Steve.
func steveRefused(err error) bool {
	st, ok := clierr.HTTPStatus(err)
	return ok && (st == http.StatusForbidden || st == http.StatusNotFound)
}

// steveList reads through Steve, as the caller, the list that a Kubernetes
// List refused with k8sErr. It reads only for a row that sets
// SteveReadOnForbidden and only when k8sErr is a 403 from the Kubernetes API.
// ns is the namespace of the list: the id of the cluster for projects and
// management nodes ("" for the projects or management nodes of every cluster),
// and the namespace of the objects for provisioning clusters and etcd
// snapshots. sel, nil for none, is applied here because Steve ignores a label
// selector. In every other case, and when Steve responds with 403 or 404, the
// result is k8sErr.
func (c getCmd) steveList(ctx context.Context, ns string, sel labels.Selector, k8sErr error) ([]unstructured.Unstructured, error) {
	if !c.k.SteveReadOnForbidden {
		return nil, k8sErr
	}
	get, ok := c.steveGetter(k8sErr)
	if !ok {
		return nil, k8sErr
	}
	var items []unstructured.Unstructured
	var err error
	switch c.k.Key {
	case "managementcluster":
		items, err = steve.ClusterObjects(ctx, get)
	case "project":
		items, err = steve.ProjectObjects(ctx, get, ns)
	case "provisioningcluster", "etcdsnapshot", "managementnode":
		items, err = steve.TypeObjects(ctx, get, c.k.SteveType(), ns)
	default:
		return nil, clierr.New(clierr.CodeGeneric, "internal error: the get command has no way to read a "+c.k.Name+" through the Steve API; report this as a defect in rancher-cli")
	}
	if err != nil {
		if steveRefused(err) {
			return nil, k8sErr
		}
		return nil, err
	}
	if sel != nil {
		items = slices.DeleteFunc(items, func(item unstructured.Unstructured) bool {
			return !sel.Matches(labels.Set(item.GetLabels()))
		})
	}
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int {
		if n := strings.Compare(a.GetNamespace(), b.GetNamespace()); n != 0 {
			return n
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
	for i := range items {
		items[i] = unstructured.Unstructured{Object: steve.KubernetesObject(items[i].Object)}
	}
	return items, nil
}

// steveGet reads the object a Kubernetes Get refused with k8sErr through
// Steve, as the caller, under the same conditions as steveList. ns is the
// namespace of the object: the id of its cluster for a project or a
// management node, "" for a cluster, and the namespace of the object for a
// provisioning cluster or an etcd snapshot. When Steve responds with 403 or
// 404 the result is k8sErr, except while waiting: a Steve 404 there is
// NOT_FOUND, the object is gone, and the wait ends.
func (c getCmd) steveGet(ctx context.Context, ns, name string, waiting bool, k8sErr error) (*unstructured.Unstructured, error) {
	if !c.k.SteveReadOnForbidden {
		return nil, k8sErr
	}
	get, ok := c.steveGetter(k8sErr)
	if !ok {
		return nil, k8sErr
	}
	var obj *unstructured.Unstructured
	var err error
	switch c.k.Key {
	case "managementcluster":
		obj, err = steve.GetClusterObject(ctx, get, name)
	case "project":
		obj, err = steve.GetProject(ctx, get, ns, name)
	case "provisioningcluster", "etcdsnapshot", "managementnode":
		obj, err = steve.GetTypeObject(ctx, get, c.k.SteveType(), ns, name)
	default:
		return nil, clierr.New(clierr.CodeGeneric, "internal error: the get command has no way to read a "+c.k.Name+" through the Steve API; report this as a defect in rancher-cli")
	}
	if err != nil {
		if st, ok := clierr.HTTPStatus(err); ok && st == http.StatusNotFound && waiting {
			return nil, c.steveNotFound(ns, name, err)
		}
		if steveRefused(err) {
			return nil, k8sErr
		}
		return nil, err
	}
	return &unstructured.Unstructured{Object: steve.KubernetesObject(obj.Object)}, nil
}

// steveNotFound is the NOT_FOUND error for an object when Steve responds with
// 404 to the request for it.
func (c getCmd) steveNotFound(ns, name string, cause error) error {
	hint := clierr.NewCommand("access clusters")
	switch c.k.Key {
	case "project":
		hint = clierr.NewCommand("access projects").FlagValue("--cluster", ns)
	case "provisioningcluster":
		hint = clierr.NewCommand("get provisioning-clusters").FlagValue("--namespace", ns)
	case "etcdsnapshot":
		hint = clierr.NewCommand("get etcd-snapshots").FlagValue("--namespace", ns)
	case "managementnode":
		hint = clierr.NewCommand("get management-nodes").FlagValue("--cluster", ns)
	}
	return clierr.New(clierr.CodeNotFound, c.k.Name+" not found: "+clierr.ShellQuote(name)).
		WithHintCommand(hint).WithCause(cause)
}

// wrapSteveWait returns client unchanged for a row that does not set
// SteveReadOnForbidden. For a row that does, it returns a client whose Get
// calls steveGet once the Kubernetes Get has responded with 403. List and
// watch go to client. The returned client implements engine.ObjectWatcher only
// when client does.
func wrapSteveWait(c getCmd, client engine.ClusterClient, ns string) engine.ClusterClient {
	if !c.k.SteveReadOnForbidden {
		return client
	}
	base := newSteveWaitClient(c, client, ns)
	if w, ok := client.(engine.ObjectWatcher); ok {
		return &steveWatchWaitClient{steveWaitClient: base, watcher: w}
	}
	return base
}

// steveWaitClient is the client of a wait on a row that reads through Steve.
// After the Kubernetes Get responds with 403 once, it sends no more Kubernetes
// Gets: each later Get calls steveGet with that first 403 as the Kubernetes
// error.
type steveWaitClient struct {
	engine.ClusterClient
	read    func(ctx context.Context, name string, k8sErr error) (*unstructured.Unstructured, error)
	refused error
}

// newSteveWaitClient is the wait client of the object in ns, the namespace
// engine.WaitObject passes to every Get.
func newSteveWaitClient(c getCmd, client engine.ClusterClient, ns string) *steveWaitClient {
	return &steveWaitClient{ClusterClient: client, read: func(ctx context.Context, name string, k8sErr error) (*unstructured.Unstructured, error) {
		return c.steveGet(ctx, ns, name, true, k8sErr)
	}}
}

func (w *steveWaitClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if w.refused == nil {
		obj, err := w.ClusterClient.Get(ctx, gvr, ns, name)
		if st, ok := clierr.HTTPStatus(err); err == nil || !ok || st != http.StatusForbidden {
			return obj, err
		}
		w.refused = err
	}
	return w.read(ctx, name, w.refused)
}

// steveWatchWaitClient is steveWaitClient over a client that lists and
// watches one object.
type steveWatchWaitClient struct {
	*steveWaitClient
	watcher engine.ObjectWatcher
}

func (w *steveWatchWaitClient) ListObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, string, error) {
	return w.watcher.ListObject(ctx, gvr, ns, name)
}

func (w *steveWatchWaitClient) WatchObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, timeout time.Duration) (<-chan engine.ObjectEvent, error) {
	return w.watcher.WatchObject(ctx, gvr, ns, name, resourceVersion, timeout)
}

// ManagementClusterThroughSteve reads the management Cluster id through Steve
// and returns it in the shape the Kubernetes API returns it, ready to be
// updated: a caller whose role lets it read the cluster only through Steve
// (Cluster Owner) gets the same object a Kubernetes read gives other callers.
// The errors are those of steve.GetClusterObject.
func ManagementClusterThroughSteve(ctx context.Context, roots engine.RootClientFactory, id string) (*unstructured.Unstructured, error) {
	obj, err := steve.GetClusterObject(ctx, steve.RootFactoryGetter(roots), id)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: steve.KubernetesObject(obj.Object)}, nil
}

// EtcdSnapshotsAfterForbidden lists the etcd snapshots in namespace ns through
// Steve, as the caller, after the Kubernetes List of them failed with k8sErr,
// and returns them in the shape the Kubernetes API returns them: a caller whose
// role lets it read an etcd snapshot only by name (Cluster Owner) lists the ones
// it may read. It reads only when k8sErr is a 403 from the Kubernetes API. In
// every other case, and when Steve responds with 403 or 404, the result is
// k8sErr; any other error is that of steve.TypeObjects. Like steveList, it
// reads only while the etcdsnapshot row sets SteveReadOnForbidden.
func EtcdSnapshotsAfterForbidden(ctx context.Context, roots engine.RootClientFactory, ns string, k8sErr error) ([]unstructured.Unstructured, error) {
	k, ok := kinds.ByKey("etcdsnapshot")
	if !ok {
		return nil, clierr.New(clierr.CodeGeneric, "internal error: the etcd-snapshot kind is missing; report this as a defect in rancher-cli")
	}
	return steveEtcdSnapshots(ctx, k, roots, ns, k8sErr)
}

// steveEtcdSnapshots is EtcdSnapshotsAfterForbidden for the row k.
func steveEtcdSnapshots(ctx context.Context, k kinds.Kind, roots engine.RootClientFactory, ns string, k8sErr error) ([]unstructured.Unstructured, error) {
	if !k.SteveReadOnForbidden {
		return nil, k8sErr
	}
	if st, ok := clierr.HTTPStatus(k8sErr); !ok || st != http.StatusForbidden {
		return nil, k8sErr
	}
	items, err := steve.TypeObjects(ctx, steve.RootFactoryGetter(roots), k.SteveType(), ns)
	if err != nil {
		if steveRefused(err) {
			return nil, k8sErr
		}
		return nil, err
	}
	out := make([]unstructured.Unstructured, 0, len(items))
	for _, item := range items {
		out = append(out, unstructured.Unstructured{Object: steve.KubernetesObject(item.Object)})
	}
	return out, nil
}

// ManagementClustersAfterForbidden lists the management clusters through
// Steve, as the caller, after the Kubernetes List of them failed with k8sErr,
// and returns them in the shape the Kubernetes API returns them: a caller who
// cannot list the clusters through the Kubernetes API lists the ones its role
// lets it read. It reads only when k8sErr is a 403 from the Kubernetes API and
// the managementcluster row sets SteveReadOnForbidden. In every other case,
// and when Steve responds with 403 or 404, the result is k8sErr; any other
// error is that of steve.ClusterObjects.
func ManagementClustersAfterForbidden(ctx context.Context, roots engine.RootClientFactory, k8sErr error) ([]unstructured.Unstructured, error) {
	k, ok := kinds.ByKey("managementcluster")
	if !ok {
		return nil, clierr.New(clierr.CodeGeneric, "internal error: the cluster kind is missing; report this as a defect in rancher-cli")
	}
	return steveListAfterForbidden(k, roots, k8sErr, func(get steve.Getter) ([]unstructured.Unstructured, error) {
		return steve.ClusterObjects(ctx, get)
	})
}

// ProvisioningClustersAfterForbidden lists the provisioning clusters of every
// namespace through Steve, as the caller, after the Kubernetes List of them
// failed with k8sErr, under the conditions and with the results of
// ManagementClustersAfterForbidden; any other error is that of
// steve.TypeObjects.
func ProvisioningClustersAfterForbidden(ctx context.Context, roots engine.RootClientFactory, k8sErr error) ([]unstructured.Unstructured, error) {
	k, ok := kinds.ByKey("provisioningcluster")
	if !ok {
		return nil, clierr.New(clierr.CodeGeneric, "internal error: the provisioning-cluster kind is missing; report this as a defect in rancher-cli")
	}
	return steveListAfterForbidden(k, roots, k8sErr, func(get steve.Getter) ([]unstructured.Unstructured, error) {
		return steve.TypeObjects(ctx, get, k.SteveType(), "")
	})
}

// steveListAfterForbidden is the common part of the readers above for the row
// k: it calls read with the Steve getter only after a Kubernetes 403 and while
// the row sets SteveReadOnForbidden, and returns the objects in the Kubernetes
// shape.
func steveListAfterForbidden(k kinds.Kind, roots engine.RootClientFactory, k8sErr error, read func(steve.Getter) ([]unstructured.Unstructured, error)) ([]unstructured.Unstructured, error) {
	if !k.SteveReadOnForbidden {
		return nil, k8sErr
	}
	if st, ok := clierr.HTTPStatus(k8sErr); !ok || st != http.StatusForbidden {
		return nil, k8sErr
	}
	items, err := read(steve.RootFactoryGetter(roots))
	if err != nil {
		if steveRefused(err) {
			return nil, k8sErr
		}
		return nil, err
	}
	out := make([]unstructured.Unstructured, 0, len(items))
	for _, item := range items {
		out = append(out, unstructured.Unstructured{Object: steve.KubernetesObject(item.Object)})
	}
	return out, nil
}
