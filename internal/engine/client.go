// Package engine holds pure domain operations. It depends on the runtime
// contracts and safety types only - never on the provider adapter, urfave,
// or the terminal.
package engine

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// LocalCluster is the management cluster's proxy id.
const LocalCluster = "local"

// FileSource is the port mutation commands use to read manifest/patch input.
// Path "-" reads the invocation's stdin. The surface implements it; engine
// code never touches os.Stdin (surface-neutral engine).
type FileSource interface {
	Read(path string) ([]byte, error)
}

// ClusterClient is the port for one cluster's Rancher-proxied k8s API
// (https://<rancher>/k8s/clusters/<id>). The dynamic implementation lives in
// internal/client/provider. Cluster-scoped resources pass ns == "". Writes
// take a dryRun flag (server-side dry-run). Delete takes an optional
// resourceVersion precondition ("" = none).
type ClusterClient interface {
	Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error)
	// List lists objects in ns; selector "" lists all (no label filter).
	List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error)
	Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, dryRun bool) error
	// Logs fetches a container's log per opts.
	Logs(ctx context.Context, ns, pod string, opts LogOptions) (string, error)
	// PostReview posts one authorization or authentication review and
	// returns the server's answer with its HTTP status and raw body kept.
	// error is non-nil only when no HTTP response was obtained (transport,
	// timeout, cancellation) or a 2xx body failed to decode or lacks the
	// fields the kind requires; every HTTP status, 2xx or not, returns a
	// nil error and a populated ReviewResponse. The kind set is closed.
	PostReview(ctx context.Context, req ReviewRequest) (ReviewResponse, error)
	// ServerResources lists the server's preferred-version resources.
	// Three outcomes: a complete list with a nil error; a PARTIAL list
	// together with a non-nil *DiscoveryIncomplete naming the group
	// versions the server could not describe (the list is still what the
	// server did describe, possibly empty); or nil and another error when
	// discovery failed outright. A caller that treats every error as
	// fatal loses only the partial list; a caller that needs to tell a
	// complete inventory from a survivor unwraps *DiscoveryIncomplete.
	ServerResources(ctx context.Context) ([]APIResource, error)
}

// TableAnswer is the server's answer to a table request: the decoded Table,
// or, when the server answered with the ordinary list or object instead,
// those objects. A reader chooses by Table: it is set for a Table, and
// otherwise Items holds the answer.
type TableAnswer struct {
	// Table is the server's Table. Each row's Object holds a
	// *metav1.PartialObjectMetadata when the server sent the row's object
	// (DecodeRowObjects).
	Table *metav1.Table
	// Items is the answer when Table is nil: the listed objects, possibly
	// none, or the one named object.
	Items []unstructured.Unstructured
}

// TableReader is implemented by a ClusterClient that can ask the cluster's
// API server for a kind's table, as kubectl does: the columns and cells the
// server's printers produce, with each row's object metadata. A command
// type-asserts it, as it does ClusterArgumentResolver; a command that
// requires it while its client lacks it has a bug. The arguments and the
// errors are those of List and Get.
type TableReader interface {
	ListTable(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (TableAnswer, error)
	GetTable(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (TableAnswer, error)
}

// DecodeRowObjects decodes each row's raw object of t into
// *metav1.PartialObjectMetadata. A row without an object is left as it is.
func DecodeRowObjects(t *metav1.Table) error {
	for i := range t.Rows {
		row := &t.Rows[i]
		if row.Object.Object != nil || len(row.Object.Raw) == 0 {
			continue
		}
		var meta metav1.PartialObjectMetadata
		if err := json.Unmarshal(row.Object.Raw, &meta); err != nil {
			return fmt.Errorf("decoding the object of row %d of the table: %w", i, err)
		}
		row.Object.Object = &meta
	}
	return nil
}

// LogOptions parameterizes a Logs call. SinceSeconds and TailLines are both
// pointers because nil (unset) and *0 (an explicit zero) are different
// requests to corev1.PodLogOptions - SinceSeconds' nil means "no limit" vs
// *0 meaning "since now" (silently empty output); TailLines' nil means "no
// limit" vs *0 meaning "request exactly zero lines". A plain int64 here
// would let a caller that forgets to set TailLines silently ask for zero
// lines instead of "no limit". Callers that only have a duration/count
// string must decide which one they mean before constructing this struct.
type LogOptions struct {
	Container    string
	TailLines    *int64
	SinceSeconds *int64
	Previous     bool
	Timestamps   bool
}

// ClusterClientFactory hands out per-cluster clients at call time (network
// deferred; memoized per invocation).
//
// Server identity for Effect stamping is NOT part of this port: only the
// surface adapter needs it, and it reads it off the concrete factory
// (io.ServerIdentity) rather than through the engine-facing interface (ISP).
type ClusterClientFactory interface {
	Cluster(ctx context.Context, clusterID string) (ClusterClient, error)
	// ImpersonatedCluster is Cluster with an Impersonate-User header on
	// every request. The identity is part of the memo key: two subjects
	// never share a client.
	ImpersonatedCluster(ctx context.Context, clusterID string, as Impersonation) (ClusterClient, error)
	// ResolveCluster maps a cluster display name or id to its cluster id
	// (rancher-ai-mcp GetClusterID semantics); "" and LocalCluster
	// short-circuit to LocalCluster. Memoized per invocation.
	ResolveCluster(ctx context.Context, nameOrID string) (string, error)
}

// ClusterArgumentResolver is implemented by a ClusterClientFactory whose
// ResolveCluster warning names the --cluster flag: ResolveClusterArgument
// is the same lookup for a value given as a positional argument or as one
// entry of a list flag such as --clusters, worded without a flag.
type ClusterArgumentResolver interface {
	ResolveClusterArgument(ctx context.Context, nameOrID string) (string, error)
}

// ResolveClusterArgument resolves a cluster a command names by positional
// argument or list entry: clients' ResolveClusterArgument when it has one,
// else its ResolveCluster.
func ResolveClusterArgument(ctx context.Context, clients ClusterClientFactory, nameOrID string) (string, error) {
	if r, ok := clients.(ClusterArgumentResolver); ok {
		return r.ResolveClusterArgument(ctx, nameOrID)
	}
	return clients.ResolveCluster(ctx, nameOrID)
}

// ProjectRef is one project the caller can read: its id (metadata.name) and
// its display name.
type ProjectRef struct{ ID, DisplayName string }

// ProjectNameLister is implemented by a ClusterClientFactory that can list the
// projects the caller can read on one cluster. The generated verbs use it, on
// a miss, to tell a caller who typed a project's display name which id it
// has; a factory without it leaves the miss as the server answered it.
type ProjectNameLister interface {
	ListProjects(ctx context.Context, clusterID string) ([]ProjectRef, error)
}

// RootClient is the GET-only port for reads against the Rancher server
// ROOT (Steve /v1 collections, the channel-server release routes) - the
// surfaces ClusterClient's per-cluster /k8s/clusters/<id> proxy cannot
// reach. GET-only BY DESIGN: no write verb exists on this port, keeping
// the zero-Norman / writes-through-CRDs posture structural. Every call sends
// the session's credentials, including to routes that happen to be
// unauthenticated server-side - one policy, no anonymous special-casing.
type RootClient interface {
	// GetJSON GETs path (absolute, starting "/") against the server root
	// and returns the response body. A non-2xx answer is classified by
	// clierr's status table, and clierr.HTTPStatus reads its status.
	GetJSON(ctx context.Context, path string) ([]byte, error)
}

// RootClientFactory hands out the root client at call time, mirroring
// ClusterClientFactory's deferred-network shape.
type RootClientFactory interface {
	Root() (RootClient, error)
}

// ServerBaseURLProvider exposes the canonical, resolved server base URL
// (scheme preserved, no trailing slash) - the SAME value RootClient's
// requests are built against, including a RANCHER_CLI_URL override or an
// http development server. Narrow by design: only a command that must
// construct a URL itself (discovery, and its degraded fallback) needs this;
// everything else goes through RootClient or ClusterClientFactory, which
// already have the base baked into their requests.
type ServerBaseURLProvider interface {
	ServerBaseURL() (string, error)
}

// SessionTokenNamer exposes the ID component of the current session's bearer
// credential - the part before ":" in the stored "id:secret" token. Only the
// ID (the token object name) is returned; the secret never crosses this
// boundary. Used by the management-token delete guard to detect when the
// target name matches the token backing the active session.
type SessionTokenNamer interface {
	// ActiveTokenID returns the name portion of the current session's bearer
	// token ("id" from "id:secret"). Returns "" when no credential is resolved
	// (not logged in). Never returns the secret half.
	ActiveTokenID() (string, error)
}
