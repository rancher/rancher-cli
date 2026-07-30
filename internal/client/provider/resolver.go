package provider

import (
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// NewResolver returns the bespoke op map: bindings for non-generic operations
// only (composites, auth-adjacent). Bindings for generated generic ops
// (get/delete <kind>) are owned by the kind table (kinds.Bindings) and composed
// in at BuildRoot — an op defined by both sources fails composition. Keys are
// the canonical-singular Descriptor.Name; promotions of a bespoke op are edits
// here only.
func NewResolver() map[string]safety.Binding {
	return map[string]safety.Binding{
		"create project": {Tier: safety.TierPublic, Stability: safety.Stable},
		// The three namespace mutations ride the same core-v1 dynamic path
		// as the generated get/delete namespace (PlanCreate/PlanUpdate on
		// the kind row's GVR) — bound to match the row (kinds.go), not the
		// project verbs they sit beside.
		"create namespace":         {Tier: safety.TierCRD, Stability: safety.Beta},
		"project add-namespace":    {Tier: safety.TierCRD, Stability: safety.Beta},
		"project remove-namespace": {Tier: safety.TierCRD, Stability: safety.Beta},
		"grant":                    {Tier: safety.TierPublic, Stability: safety.Beta},
		"revoke":                   {Tier: safety.TierPublic, Stability: safety.Beta},
		"create secret":            {Tier: safety.TierPublic, Stability: safety.Beta},
		"create resource":          {Tier: safety.TierCRD, Stability: safety.Beta},
		"create token":             {Tier: safety.TierPublic, Stability: safety.Beta},
		"create kubeconfig":        {Tier: safety.TierPublic, Stability: safety.Beta},
		"create cluster":           {Tier: safety.TierCRD, Stability: safety.Beta},
		"create machine-config":    {Tier: safety.TierCRD, Stability: safety.Beta},

		"inspect cluster":         {Tier: safety.TierCRD, Stability: safety.Beta},
		"inspect pod":             {Tier: safety.TierCRD, Stability: safety.Beta},
		"inspect deployment":      {Tier: safety.TierCRD, Stability: safety.Beta},
		"inspect project":         {Tier: safety.TierPublic, Stability: safety.Beta},
		"get cloud-credential":    {Tier: safety.TierCRD, Stability: safety.Beta},
		"get kubernetes-versions": {Tier: safety.TierCRD, Stability: safety.Beta},
		"images":                  {Tier: safety.TierCRD, Stability: safety.Beta},
		"usage":                   {Tier: safety.TierCRD, Stability: safety.Beta},

		"logs":           {Tier: safety.TierCRD, Stability: safety.Beta},
		"support-bundle": {Tier: safety.TierCRD, Stability: safety.Beta},
	}
}
