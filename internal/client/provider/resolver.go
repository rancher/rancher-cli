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
		// Every op with a 1:1 backing kind row is ABSENT here deliberately:
		// create project/token/kubeconfig/namespace/secret/cluster and the
		// two project namespace mutations derive their bindings from their
		// rows at composition time in BuildRoot (derivedOpKinds, the same
		// no-hand-copied-literal rule as the delete overrides), so an edit
		// to a row's Binding cannot silently diverge from the ops it backs.
		// Only ops with no single backing row live in this map.
		//
		// grant/revoke span several listed RBAC kinds (RoleTemplate, the
		// binding kinds, GlobalRole) so no single row can be derived from;
		// public/stable per the Rancher Kubernetes Public API semi-official
		// list, 2026-07-30.
		"grant":  {Contract: safety.ContractPublic, Stability: safety.Stable},
		"revoke": {Contract: safety.ContractPublic, Stability: safety.Stable},
		// create resource takes any kind; create machine-config spans the
		// five machine-config kinds — no single row for either.
		"create resource":       {Contract: safety.ContractInternal, Stability: safety.Beta},
		"create machine-config": {Contract: safety.ContractInternal, Stability: safety.Beta},

		// Composites are CLI value-adds, not API resources: internal/beta regardless
		// of the underlying kinds they read. inspect project is a deliberate
		// downgrade from its former public binding.
		"inspect cluster":         {Contract: safety.ContractInternal, Stability: safety.Beta},
		"inspect pod":             {Contract: safety.ContractInternal, Stability: safety.Beta},
		"inspect deployment":      {Contract: safety.ContractInternal, Stability: safety.Beta},
		"inspect project":         {Contract: safety.ContractInternal, Stability: safety.Beta},
		"get cloud-credential":    {Contract: safety.ContractInternal, Stability: safety.Beta},
		"get kubernetes-versions": {Contract: safety.ContractInternal, Stability: safety.Beta},
		"images":                  {Contract: safety.ContractInternal, Stability: safety.Beta},
		"usage":                   {Contract: safety.ContractInternal, Stability: safety.Beta},

		"logs":           {Contract: safety.ContractInternal, Stability: safety.Beta},
		"support-bundle": {Contract: safety.ContractInternal, Stability: safety.Beta},
	}
}
