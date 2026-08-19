// Package provisioning holds the read-only cloud-credential resolver plus
// its `get cloud-credential` listing surface — the D-1 slice of Tranche D.
// SECRET HYGIENE (spec D-R2-1): every function here reads Kubernetes Secret
// objects in cattle-global-data but retains ONLY key names and metadata
// (name, the field.cattle.io/name annotation, creationTimestamp) — never a
// `data` VALUE. A `data` value must never flow into CloudCredRef, a
// runtime.Result, or an error string.
package provisioning

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
)

const (
	// cloudCredNamespace is the fixed namespace Rancher stores cloud
	// credentials in (norman proxy_store.go's cattle-global-data convention).
	cloudCredNamespace = "cattle-global-data"
	// cloudCredPrefix + a 5-char suffix is Rancher's cloud-credential secret
	// name pattern (norman proxy_store.go:440-446 + GenerateTypePrefix).
	cloudCredPrefix = "cc-"
	// displayNameAnnotation carries the user-facing name (rancher
	// credentialmapper.go:15-25); backfilled from the object name when absent.
	displayNameAnnotation = "field.cattle.io/name"
	// credentialConfigMarker is the fixed substring every driver's data key
	// carries before its own config block (rancher credentialmapper.go:36-51;
	// literals validator.go:435, aks/handler.go:247, vsphere/handler.go:55,
	// cred/store.go:116) — the driver name is the key's prefix before this.
	credentialConfigMarker = "credentialConfig-"
)

// secretsGVR is the core/v1 Secrets resource cloud credentials are stored as.
var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// CloudCredRef is the resolved internal reference plus display metadata.
type CloudCredRef struct {
	// ID is the namespaced form the provisioning spec consumes:
	// "cattle-global-data:cc-xxxxx".
	ID          string `json:"id"`
	SecretName  string `json:"secretName"`  // "cc-xxxxx"
	DisplayName string `json:"displayName"` // annotation field.cattle.io/name (may equal SecretName for legacy entries)
	Driver      string `json:"driver"`      // derived from the <driver>credentialConfig- data-key prefix; "" if unidentifiable
}

// ResolveCloudCredential resolves nameOrID to exactly one credential.
// Exact IDs ("cattle-global-data:cc-x" or bare "cc-x") are verified and
// returned; anything else matches against display names. 0 matches →
// NOT_FOUND with candidates; >1 → AMBIGUOUS_SELECTION requiring the ID.
// Reads Secret objects but retains ONLY key names + metadata — never a
// data value (spec D-R2-1).
func ResolveCloudCredential(ctx context.Context, clients engine.ClusterClientFactory, nameOrID string) (CloudCredRef, error) {
	obj, err := resolveCredentialObj(ctx, clients, nameOrID)
	if err != nil {
		return CloudCredRef{}, err
	}
	return refFrom(obj), nil
}

// resolveCredentialObj is ResolveCloudCredential's shared innards, returning
// the raw Secret so the read command can also derive Age from
// creationTimestamp without a second resolve pass. Never inspects `data`
// values — see the package doc.
func resolveCredentialObj(ctx context.Context, clients engine.ClusterClientFactory, nameOrID string) (*unstructured.Unstructured, error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return nil, err
	}

	// Exact-ID fast path: strip the namespaced prefix if present, then
	// verify a bare cc-* name directly rather than paying for a List.
	name := strings.TrimPrefix(nameOrID, cloudCredNamespace+":")
	if strings.HasPrefix(name, cloudCredPrefix) {
		obj, err := client.Get(ctx, secretsGVR, cloudCredNamespace, name)
		if err == nil {
			return obj, nil
		}
		if !clierr.IsNotFound(err) {
			// Non-NOT_FOUND errors (auth, network, etc.) propagate immediately.
			return nil, err
		}
		// NOT_FOUND from the fast-path Get falls through to the list-based
		// resolution below: the client curates 404s by literal kind name,
		// which would surface "secret not found" — leaking the Secret
		// implementation detail this resolver exists to hide. The list path's
		// MatchOneOrError yields the domain-specific shape ("cloud credential
		// not found") with proper candidates.
	}

	list, err := client.List(ctx, secretsGVR, cloudCredNamespace, "")
	if err != nil {
		return nil, err
	}
	candidates := credentialObjects(list)

	ids := make([]string, 0, len(candidates))
	displayByID := map[string]string{}
	objByID := map[string]*unstructured.Unstructured{}
	cands := make([]clierr.Candidate, 0, len(candidates))
	for _, obj := range candidates {
		id := obj.GetName()
		display := displayNameOf(obj)
		ids = append(ids, id)
		displayByID[id] = display
		objByID[id] = obj
		cands = append(cands, clierr.Candidate{ID: id, Name: display})
	}

	id, err := projects.MatchOneOrError(name, ids, func(id string) string { return displayByID[id] }, cands,
		"cloud credential not found: "+nameOrID,
		"run: rancher get cloud-credential",
		func(idFound bool, displayMatchCount int) string {
			if idFound {
				return fmt.Sprintf("%q matches both a cloud credential id and a different credential's display name", nameOrID)
			}
			return fmt.Sprintf("%d cloud credentials have display name %q", displayMatchCount, nameOrID)
		},
		"pass the credential id (cattle-global-data:cc-xxxxx)")
	if err != nil {
		return nil, err
	}
	return objByID[id], nil
}

// credentialObjects filters list to credential-shaped secrets: named cc-*,
// or carrying a *credentialConfig-* data key (the cattle.io/creator=norman
// label is corroborating in real Rancher data, not required here).
func credentialObjects(list *unstructured.UnstructuredList) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for i := range list.Items {
		obj := &list.Items[i]
		if strings.HasPrefix(obj.GetName(), cloudCredPrefix) || driverFromDataKeys(obj) != "" {
			out = append(out, obj)
		}
	}
	return out
}

// displayNameOf reads the field.cattle.io/name annotation, backfilling from
// the object's own name when absent (rancher credentialmapper.go:15-25).
func displayNameOf(obj *unstructured.Unstructured) string {
	if display := obj.GetAnnotations()[displayNameAnnotation]; display != "" {
		return display
	}
	return obj.GetName()
}

// refFrom builds the public CloudCredRef from a Secret object, reading ONLY
// metadata and `data` KEY NAMES — never a `data` value (spec D-R2-1).
func refFrom(obj *unstructured.Unstructured) CloudCredRef {
	name := obj.GetName()
	return CloudCredRef{
		ID:          cloudCredNamespace + ":" + name,
		SecretName:  name,
		DisplayName: displayNameOf(obj),
		Driver:      driverFromDataKeys(obj),
	}
}

// driverFromDataKeys scans `data` KEY NAMES ONLY — never a value — for the
// first key containing "credentialConfig-"; Driver is the prefix before that
// marker (rancher credentialmapper.go:36-51). Keys are sorted first so the
// choice is deterministic if a secret somehow carries more than one.
func driverFromDataKeys(obj *unstructured.Unstructured) string {
	raw, ok := obj.Object["data"]
	if !ok {
		return ""
	}
	data, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if idx := strings.Index(k, credentialConfigMarker); idx >= 0 {
			return k[:idx]
		}
	}
	return ""
}

// ageFrom renders metadata.creationTimestamp the same way kubectl's own AGE
// column does; a missing/unparseable timestamp degrades to "<unknown>"
// rather than a zero-time nonsense value.
func ageFrom(obj *unstructured.Unstructured) string {
	ts := obj.GetCreationTimestamp()
	if ts.IsZero() {
		return "<unknown>"
	}
	return duration.ShortHumanDuration(time.Since(ts.Time))
}

// cloudCredRow is one `get cloud-credential` row: the resolved ref plus a
// display-only Age. creationTimestamp is metadata, not a `data` value, so
// adding it here does not weaken the hygiene guarantee.
type cloudCredRow struct {
	CloudCredRef
	Age string `json:"age"`
}

func rowFrom(obj *unstructured.Unstructured) cloudCredRow {
	return cloudCredRow{CloudCredRef: refFrom(obj), Age: ageFrom(obj)}
}

// cloudCredRows is `get cloud-credential`'s payload; it implements
// output.Tabular so `-o table` shows NAME/DISPLAY/DRIVER/AGE — secret
// material never appears in a column.
type cloudCredRows []cloudCredRow

func (r cloudCredRows) Columns() []string { return []string{"NAME", "DISPLAY", "DRIVER", "AGE"} }

func (r cloudCredRows) Rows() [][]string {
	out := make([][]string, 0, len(r))
	for _, row := range r {
		driver := row.Driver
		if driver == "" {
			driver = "<none>"
		}
		out = append(out, []string{row.SecretName, row.DisplayName, driver, row.Age})
	}
	return out
}

type getCloudCredCmd struct {
	clients engine.ClusterClientFactory
}

// NewGetCloudCredential builds `get cloud-credential` (read-only listing:
// SecretName, DisplayName, Driver, Age — secret material NEVER shown).
func NewGetCloudCredential(clients engine.ClusterClientFactory) runtime.Command {
	return &getCloudCredCmd{clients: clients}
}

func (c *getCloudCredCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "get cloud-credential",
		Usage:       "List cloud credentials or show one (secret values never shown)",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Network:     true,
		Auth:        true,
	}
}

func (c *getCloudCredCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if len(in.Args()) > 0 && in.Args()[0] != "" {
		obj, err := resolveCredentialObj(ctx, c.clients, in.Args()[0])
		if err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: "CloudCredential", Data: cloudCredRows{rowFrom(obj)}}, nil
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	list, err := client.List(ctx, secretsGVR, cloudCredNamespace, "")
	if err != nil {
		return runtime.Result{}, err
	}
	objs := credentialObjects(list)
	rows := make(cloudCredRows, 0, len(objs))
	for _, obj := range objs {
		rows = append(rows, rowFrom(obj))
	}
	return runtime.Result{Kind: "CloudCredentialList", Data: rows}, nil
}

var _ runtime.ReadCommand = (*getCloudCredCmd)(nil)
