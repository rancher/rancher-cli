package complete

import (
	"context"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// The conventions Rancher stores cloud credentials under, the ones create
// cluster resolves --cloud-credential by: the fixed namespace, the secret
// name prefix, the display name annotation, and the marker every driver's
// data key carries.
const (
	cloudCredentialNamespace = "cattle-global-data"
	cloudCredentialPrefix    = "cc-"
	cloudCredentialDisplay   = "field.cattle.io/name"
	credentialConfigMarker   = "credentialConfig-"
)

// CreateClusterSource is create cluster's own source. It claims two of
// the command's flag values: --machine-config once the typed value
// carries a `<kind>/` prefix naming a machine-config kind (the bare
// prefixes are a compiled-in projection the surface serves before the
// network gate), answering that kind's objects in its workspace
// namespace behind the prefix; and --cloud-credential, answering the
// cloud credential Secrets of cattle-global-data. Both need a management
// token and one listing; every other site of the command is declined.
func CreateClusterSource() completion.Source {
	return completion.Source{Claim: claimCreateCluster, Complete: completeCreateCluster}
}

func claimCreateCluster(req completion.Request) (completion.Requirements, bool) {
	switch req.Flag {
	case "cloud-credential":
		return completion.ManagementListing(), true
	case "machine-config":
		if _, ok := machineConfigKind(req.Partial); ok {
			return completion.ManagementListing(), true
		}
	}
	return completion.Requirements{}, false
}

func completeCreateCluster(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	switch req.Flag {
	case "cloud-credential":
		return completeCloudCredentials(ctx, conn)
	case "machine-config":
		return completeMachineConfigs(ctx, conn, req)
	}
	return completion.Result{}
}

// machineConfigKind resolves the `<kind>/` prefix of a typed
// --machine-config value to its kind table row: the token before the
// first "/", when there is one and it names a kind of the machine-config
// group. A value without a "/" belongs to the static prefix layer.
func machineConfigKind(partial string) (kinds.Kind, bool) {
	kindTok, _, hasSlash := strings.Cut(partial, "/")
	if !hasSlash {
		return kinds.Kind{}, false
	}
	k, ok := kinds.Lookup(kindTok)
	if !ok || k.GVR.Group != kinds.MachineConfigGroup {
		return kinds.Kind{}, false
	}
	return k, true
}

// completeMachineConfigs lists the typed kind's objects in its default
// workspace namespace, the one create cluster provisions into, and hands
// the surface the kind's `<kind>/` prefix to put in front of every name.
func completeMachineConfigs(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	k, ok := machineConfigKind(req.Partial)
	if !ok {
		return completion.Result{}
	}
	p, err := completion.ManagementNamespacedPath(k.GVR.Group, k.GVR.Version, k.DefaultNamespace, k.GVR.Resource)
	if err != nil {
		return completion.Result{}
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	return completion.Result{Names: list.Names(), Prefix: k.Name + "/"}
}

// completeCloudCredentials lists the Secrets of cattle-global-data and
// answers the cloud credential values create cluster accepts.
func completeCloudCredentials(ctx context.Context, conn *completion.Connection) completion.Result {
	p, err := completion.ManagementNamespacedPath("", "v1", cloudCredentialNamespace, "secrets")
	if err != nil {
		return completion.Result{}
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	return completion.Result{Names: cloudCredentialNames(list.Items)}
}

// cloudCredentialNames projects the Secrets of cattle-global-data into
// the values create cluster resolves --cloud-credential to: the id
// (metadata.name) of every credential-shaped Secret - named cc-*, or
// carrying a data key with the credentialConfig- marker - and each
// display name (the field.cattle.io/name annotation) that is on exactly
// one credential and is not itself an id, so a value offered never
// resolves ambiguously. List order, each value once. Only metadata and
// data KEY names are read; a data value never reaches a candidate.
func cloudCredentialNames(items []map[string]any) []string {
	type entry struct{ id, display string }
	var entries []entry
	ids := map[string]bool{}
	displays := map[string]int{}
	for _, item := range items {
		id := completion.ItemName(item)
		if id == "" || ids[id] || !credentialShaped(item, id) {
			continue
		}
		ids[id] = true
		display := ""
		if meta, _ := item["metadata"].(map[string]any); meta != nil {
			if anns, _ := meta["annotations"].(map[string]any); anns != nil {
				display, _ = anns[cloudCredentialDisplay].(string)
			}
		}
		entries = append(entries, entry{id: id, display: display})
		if display != "" {
			displays[display]++
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		out = append(out, e.id)
		if e.display != "" && displays[e.display] == 1 && !ids[e.display] && !seen[e.display] {
			seen[e.display] = true
			out = append(out, e.display)
		}
	}
	return out
}

// credentialShaped reports whether a Secret of cattle-global-data is a
// cloud credential: named with the credential prefix, or carrying a data
// KEY with the credentialConfig- marker. Keys only; values are never read.
func credentialShaped(item map[string]any, id string) bool {
	if strings.HasPrefix(id, cloudCredentialPrefix) {
		return true
	}
	data, _ := item["data"].(map[string]any)
	for key := range data {
		if strings.Contains(key, credentialConfigMarker) {
			return true
		}
	}
	return false
}

// PoolSource is the --pool source of the cluster-family verbs that take
// a pool (scale, set-autoscaling, clear-autoscaling): the machine pool
// names of the provisioning cluster typed as the first positional, read
// from that one object in the --namespace typed on the line or the
// kind's default workspace. It needs a management token and one request;
// without a typed cluster, and for every other site, it declines.
func PoolSource() completion.Source {
	return completion.Source{Claim: claimPool, Complete: completePools}
}

func claimPool(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "pool" || len(req.Positionals) == 0 {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

func completePools(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	k, ok := kinds.ByKey("provisioningcluster")
	if !ok {
		return completion.Result{}
	}
	ns := completion.TailFlag(req.Tail, "namespace")
	if ns == "" {
		ns = k.DefaultNamespace
	}
	p, err := completion.ManagementNamedResourcePath(k.GVR.Group, k.GVR.Version, ns, k.GVR.Resource, req.Positionals[0])
	if err != nil {
		return completion.Result{}
	}
	var obj struct {
		Spec struct {
			RKEConfig struct {
				MachinePools []struct {
					Name string `json:"name"`
				} `json:"machinePools"`
			} `json:"rkeConfig"`
		} `json:"spec"`
	}
	if err := conn.GetJSON(ctx, p, "", &obj); err != nil {
		return completion.Result{}
	}
	names := make([]string, 0, len(obj.Spec.RKEConfig.MachinePools))
	for _, pool := range obj.Spec.RKEConfig.MachinePools {
		names = append(names, pool.Name)
	}
	return completion.Result{Names: names}
}
