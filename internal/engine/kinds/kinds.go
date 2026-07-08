// Package kinds is the single source of kind knowledge for generic verbs:
// canonical MCP-vocabulary keys (conformance-tested against rancher-ai-mcp's
// converter table), CLI-facing names/aliases, GVRs, scope, deletability,
// tier/stability bindings for GENERATED ops, and table projections.
package kinds

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type Scope string

const (
	Management Scope = "management" // served by the management cluster ("local")
	Downstream Scope = "downstream" // served by a --cluster target
)

type Column struct {
	Header string
	Path   []string // unstructured.NestedString path; empty = metadata.name
}

type Kind struct {
	Key        string   // canonical MCP-vocabulary key ("managementcluster")
	Name       string   // CLI-facing singular ("cluster")
	Aliases    []string // plural/short forms
	GVR        schema.GroupVersionResource
	Namespaced bool
	Scope      Scope
	Deletable  bool // false = no generic delete generated (clusters, nodes)
	Binding    safety.Binding
	Columns    []Column // extra table columns beyond NAME
}

var table = []Kind{
	{
		Key: "managementcluster", Name: "cluster", Aliases: []string{"clusters"},
		GVR:   schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"},
		Scope: Management, Deletable: false,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns: []Column{{Header: "DISPLAY", Path: []string{"spec", "displayName"}}, {Header: "STATE", Path: []string{"status", "state"}}},
	},
	{
		Key: "project", Name: "project", Aliases: []string{"projects"},
		GVR:        schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"},
		Namespaced: true, Scope: Management, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierPublic, Stability: safety.Stable},
		Columns: []Column{{Header: "DISPLAY", Path: []string{"spec", "displayName"}}, {Header: "CLUSTER", Path: []string{"spec", "clusterName"}}},
	},
	{
		Key: "node", Name: "node", Aliases: []string{"nodes"},
		GVR:   schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"},
		Scope: Downstream, Deletable: false,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "namespace", Name: "namespace", Aliases: []string{"namespaces", "ns"},
		GVR:   schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"},
		Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "pod", Name: "pod", Aliases: []string{"pods", "po"},
		GVR:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		Namespaced: true, Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns: []Column{{Header: "STATUS", Path: []string{"status", "phase"}}},
	},
	{
		Key: "deployment", Name: "deployment", Aliases: []string{"deployments", "deploy"},
		GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespaced: true, Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "service", Name: "service", Aliases: []string{"services", "svc"},
		GVR:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"},
		Namespaced: true, Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "configmap", Name: "configmap", Aliases: []string{"configmaps", "cm"},
		GVR:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Namespaced: true, Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "secret", Name: "secret", Aliases: []string{"secrets"},
		GVR:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
		Namespaced: true, Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
}

func All() []Kind { out := make([]Kind, len(table)); copy(out, table); return out }

// Bindings returns the tier bindings for generated generic ops: "get <Name>"
// for every kind and "delete <Name>" for deletable kinds. BuildRoot composes
// these with the bespoke resolver (single source per op).
func Bindings() map[string]safety.Binding {
	out := map[string]safety.Binding{}
	for _, k := range table {
		out["get "+k.Name] = k.Binding
		if k.Deletable {
			out["delete "+k.Name] = k.Binding
		}
	}
	return out
}

// Lookup resolves a CLI-facing name, alias, or canonical key to a Kind.
func Lookup(q string) (Kind, bool) {
	for _, k := range table {
		if q == k.Name || q == k.Key {
			return k, true
		}
		for _, a := range k.Aliases {
			if q == a {
				return k, true
			}
		}
	}
	return Kind{}, false
}

// Validate rejects Key duplicates and Name/Alias/Key collisions across kinds
// (BuildRoot calls it).
func Validate() error { return validateKinds(table) }

func validateKinds(ts []Kind) error {
	// Key uniqueness is its own pass: the self-Key exemption in the collision
	// pass below would otherwise mask two kinds sharing a Key.
	seenKeys := map[string]bool{}
	for _, k := range ts {
		if seenKeys[k.Key] {
			return fmt.Errorf("duplicate kind Key %q", k.Key)
		}
		seenKeys[k.Key] = true
	}
	seen := map[string]string{}
	for _, k := range ts {
		for _, id := range append([]string{k.Name, k.Key}, k.Aliases...) {
			if prev, dup := seen[id]; dup && prev != k.Key {
				return fmt.Errorf("kind identifier %q claimed by both %q and %q", id, prev, k.Key)
			}
			seen[id] = k.Key
		}
	}
	return nil
}
