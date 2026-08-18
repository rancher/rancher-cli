// Package engine holds pure domain operations. It depends on the runtime
// contracts and safety types only — never on the provider adapter, urfave,
// or the terminal.
package engine

import (
	"context"

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
}

// LogOptions parameterizes a Logs call. SinceSeconds and TailLines are both
// pointers because nil (unset) and *0 (an explicit zero) are different
// requests to corev1.PodLogOptions — SinceSeconds' nil means "no limit" vs
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
	// ResolveCluster maps a cluster display name or id to its cluster id
	// (rancher-ai-mcp GetClusterID semantics); "" and LocalCluster
	// short-circuit to LocalCluster. Memoized per invocation.
	ResolveCluster(ctx context.Context, nameOrID string) (string, error)
}

// RootClient is the GET-only port for reads against the Rancher server
// ROOT (Steve /v1 collections, the channel-server release routes) — the
// surfaces ClusterClient's per-cluster /k8s/clusters/<id> proxy cannot
// reach. GET-only BY DESIGN: no write verb exists on this port, keeping
// the tranche's zero-Norman / writes-through-CRDs posture structural
// (spec D-10). Every call sends the session's credentials, including to
// routes that happen to be unauthenticated server-side — one policy, no
// anonymous special-casing (spec D-10).
type RootClient interface {
	// GetJSON GETs path (absolute, starting "/") against the server root
	// and returns the response body. Non-2xx maps to a clierr code:
	// 401/403 → auth, 404 → not-found, else generic with a body excerpt.
	GetJSON(ctx context.Context, path string) ([]byte, error)
}

// RootClientFactory hands out the root client at call time, mirroring
// ClusterClientFactory's deferred-network shape.
type RootClientFactory interface {
	Root() (RootClient, error)
}

// SessionTokenNamer exposes the ID component of the current session's bearer
// credential — the part before ":" in the stored "id:secret" token. Only the
// ID (the token object name) is returned; the secret never crosses this
// boundary. Used by the management-token delete guard to detect when the
// target name matches the token backing the active session.
type SessionTokenNamer interface {
	// ActiveTokenID returns the name portion of the current session's bearer
	// token ("id" from "id:secret"). Returns "" when no credential is resolved
	// (not logged in). Never returns the secret half.
	ActiveTokenID() (string, error)
}
