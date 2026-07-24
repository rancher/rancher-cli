// Package kinds is the single source of kind knowledge for generic verbs:
// canonical MCP-vocabulary keys (conformance-tested against rancher-ai-mcp's
// converter table), CLI-facing names/aliases, GVRs, scope, deletability,
// tier/stability bindings for GENERATED ops, and table projections.
package kinds

import (
	"fmt"
	"strings"

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
	// Bool marks the leaf boolean-typed: an absent field renders "false"
	// instead of blank, because an omitempty bool (e.g. NodeDriverSpec's
	// builtin) drops its false value from the wire entirely. Unmarked
	// (string-typed) columns keep rendering absent fields blank.
	Bool bool
}

// NamespacePolicy decides namespace semantics and flag generation for a
// kind. Scope decides WHICH cluster serves the request; Policy decides how
// the namespace is chosen. The two are orthogonal.
type NamespacePolicy string

const (
	// NSNone: cluster-scoped resource — no namespace flag.
	NSNone NamespacePolicy = "none"
	// NSCluster: the namespace IS a cluster id (project,
	// clusterroletemplatebinding) — --cluster flag, resolved via
	// ResolveCluster, required for named ops; client is management.
	NSCluster NamespacePolicy = "cluster"
	// NSWorkspace: management-plane namespace with a default (fleet,
	// provisioning, CAPI kinds — all default fleet-default) — --namespace
	// flag with DefaultNamespace fallback, NO --cluster; client is management.
	NSWorkspace NamespacePolicy = "workspace"
	// NSPlain: ordinary namespace — --namespace flag, required for named
	// ops; --cluster routes when Scope is Downstream. For the one Management
	// NSPlain kind (projectroletemplatebinding) --namespace is a project id.
	NSPlain NamespacePolicy = "plain"
)

type Kind struct {
	Key              string   // canonical MCP-vocabulary key ("managementcluster")
	Name             string   // CLI-facing singular ("cluster")
	Aliases          []string // plural/short forms
	GVR              schema.GroupVersionResource
	Policy           NamespacePolicy // namespace semantics + flag generation
	DefaultNamespace string          // NSWorkspace fallback ("" elsewhere)
	K8sKind          string          // manifest `kind:` value ("Deployment") — consumed by parity branch 2 reverse lookup
	Scope            Scope
	Deletable        bool // false = no generic delete generated (clusters, nodes)
	Sensitive        bool // patch on this kind is Destructive (parity branch 2)
	// WaitDefault is the --for expression bare --wait applies; "" = no default
	// (bare --wait rejects) — an explicit --for is honored regardless. Set only
	// for kinds where the expression is grounded in a real, well-known
	// status.state/status.conditions shape; when in doubt, leave empty.
	WaitDefault string
	// Completable marks this kind as eligible for live name completion (AC-15).
	// Deny-by-default: a new row gets no completion until this is explicitly set.
	// Secrets and cloud-credentials are never set (AC-25).
	Completable bool
	Binding     safety.Binding
	Columns     []Column // extra table columns beyond NAME
}

// MachineConfigGroup is the GVR group shared by every rke-machine-config
// kind row below (amazonec2config, azureconfig, digitaloceanconfig,
// harvesterconfig, vmwarevsphereconfig) — the single source createcluster.go
// filters kinds.All() on to derive the --machine-config kind set.
const MachineConfigGroup = "rke-machine-config.cattle.io"

var table = []Kind{
	{
		Key: "managementcluster", Name: "cluster", Aliases: []string{"clusters"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"},
		Policy: NSNone, K8sKind: "Cluster", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		Completable: true,
		Binding:     safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns:     []Column{{Header: "DISPLAY", Path: []string{"spec", "displayName"}}, {Header: "STATE", Path: []string{"status", "state"}}},
	},
	{
		Key: "project", Name: "project", Aliases: []string{"projects"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"},
		Policy: NSCluster, K8sKind: "Project", Scope: Management, Deletable: true,
		Completable: true,
		Binding:     safety.Binding{Tier: safety.TierPublic, Stability: safety.Stable},
		Columns:     []Column{{Header: "DISPLAY", Path: []string{"spec", "displayName"}}, {Header: "CLUSTER", Path: []string{"spec", "clusterName"}}},
	},
	{
		Key: "node", Name: "node", Aliases: []string{"nodes"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"},
		Policy: NSNone, K8sKind: "Node", Scope: Downstream, Deletable: false, WaitDefault: "condition=Ready",
		Completable: true,
		Binding:     safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "namespace", Name: "namespace", Aliases: []string{"namespaces", "ns"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"},
		Policy: NSNone, K8sKind: "Namespace", Scope: Downstream, Deletable: true,
		Completable: true,
		Binding:     safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "pod", Name: "pod", Aliases: []string{"pods", "po"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		Policy: NSPlain, K8sKind: "Pod", Scope: Downstream, Deletable: true, WaitDefault: "condition=Ready",
		Completable: true,
		Binding:     safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns:     []Column{{Header: "STATUS", Path: []string{"status", "phase"}}},
	},
	{
		Key: "deployment", Name: "deployment", Aliases: []string{"deployments", "deploy"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Policy: NSPlain, K8sKind: "Deployment", Scope: Downstream, Deletable: true, WaitDefault: "condition=Available",
		Completable: true,
		Binding:     safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "service", Name: "service", Aliases: []string{"services", "svc"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"},
		Policy: NSPlain, K8sKind: "Service", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "configmap", Name: "configmap", Aliases: []string{"configmaps", "cm"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Policy: NSPlain, K8sKind: "ConfigMap", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "secret", Name: "secret", Aliases: []string{"secrets"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
		Policy: NSPlain, K8sKind: "Secret", Scope: Downstream, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},

	{
		Key: "statefulset", Name: "statefulset", Aliases: []string{"statefulsets", "sts"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
		Policy: NSPlain, K8sKind: "StatefulSet", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "daemonset", Name: "daemonset", Aliases: []string{"daemonsets", "ds"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"},
		Policy: NSPlain, K8sKind: "DaemonSet", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "replicaset", Name: "replicaset", Aliases: []string{"replicasets", "rs"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"},
		Policy: NSPlain, K8sKind: "ReplicaSet", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "job", Name: "job", Aliases: []string{"jobs"},
		GVR:    schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"},
		Policy: NSPlain, K8sKind: "Job", Scope: Downstream, Deletable: true, WaitDefault: "condition=Complete",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "cronjob", Name: "cronjob", Aliases: []string{"cronjobs"},
		GVR:    schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"},
		Policy: NSPlain, K8sKind: "CronJob", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "ingress", Name: "ingress", Aliases: []string{"ingresses", "ing"},
		GVR:    schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		Policy: NSPlain, K8sKind: "Ingress", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "persistentvolume", Name: "persistentvolume", Aliases: []string{"persistentvolumes", "pv"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"},
		Policy: NSNone, K8sKind: "PersistentVolume", Scope: Downstream, Deletable: false,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "persistentvolumeclaim", Name: "persistentvolumeclaim", Aliases: []string{"persistentvolumeclaims", "pvc"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		Policy: NSPlain, K8sKind: "PersistentVolumeClaim", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "serviceaccount", Name: "serviceaccount", Aliases: []string{"serviceaccounts", "sa"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"},
		Policy: NSPlain, K8sKind: "ServiceAccount", Scope: Downstream, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "event", Name: "event", Aliases: []string{"events", "ev"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"},
		Policy: NSPlain, K8sKind: "Event", Scope: Downstream, Deletable: false,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},

	{
		Key: "gitrepo", Name: "gitrepo", Aliases: []string{"gitrepos"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "gitrepos"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "GitRepo", Scope: Management, Deletable: true, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "bundle", Name: "bundle", Aliases: []string{"bundles"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "bundles"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Bundle", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "bundledeployment", Name: "bundledeployment", Aliases: []string{"bundledeployments"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "bundledeployments"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "BundleDeployment", Scope: Management, Deletable: false,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "clustergroup", Name: "clustergroup", Aliases: []string{"clustergroups"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "clustergroups"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "ClusterGroup", Scope: Management, Deletable: false,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		// K8sKind "Cluster" is shared with provisioningcluster/capicluster —
		// branch 2's manifest reverse lookup disambiguates by apiVersion group.
		Key: "fleetcluster", Name: "fleet-cluster", Aliases: []string{"fleetclusters"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "clusters"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Cluster", Scope: Management, Deletable: false, Sensitive: true, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},

	{
		Key: "setting", Name: "setting", Aliases: []string{"settings"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "settings"},
		Policy: NSNone, K8sKind: "Setting", Scope: Management, Deletable: false, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		// Feature's interesting fields (spec.value, status.default,
		// status.lockedValue) are BOOLS. The table renderer can render bools
		// now (nodedriver's ACTIVE/BUILTIN), but the single DESCRIPTION column
		// stays: spec.value is a tri-state (absent = follow the default), and
		// a blank-vs-"false" cell would misread — -o json is the real read.
		Key: "feature", Name: "feature", Aliases: []string{"features"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "features"},
		Policy: NSNone, K8sKind: "Feature", Scope: Management, Deletable: false, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns: []Column{{Header: "DESCRIPTION", Path: []string{"status", "description"}}},
	},
	{
		Key: "user", Name: "user", Aliases: []string{"users"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "users"},
		Policy: NSNone, K8sKind: "User", Scope: Management, Deletable: false, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "roletemplate", Name: "roletemplate", Aliases: []string{"roletemplates"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "roletemplates"},
		Policy: NSNone, K8sKind: "RoleTemplate", Scope: Management, Deletable: false, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns: []Column{{Header: "CONTEXT", Path: []string{"context"}}, {Header: "DISPLAY", Path: []string{"displayName"}}},
	},
	{
		Key: "globalrole", Name: "globalrole", Aliases: []string{"globalroles"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "globalroles"},
		Policy: NSNone, K8sKind: "GlobalRole", Scope: Management, Deletable: false, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "globalrolebinding", Name: "globalrolebinding", Aliases: []string{"globalrolebindings"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "globalrolebindings"},
		Policy: NSNone, K8sKind: "GlobalRoleBinding", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "clusterroletemplatebinding", Name: "clusterroletemplatebinding", Aliases: []string{"crtb"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusterroletemplatebindings"},
		Policy: NSCluster, K8sKind: "ClusterRoleTemplateBinding", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		// The one Management + NSPlain kind: --namespace is a project id, not a
		// cluster id — reflected in the generated flag help.
		Key: "projectroletemplatebinding", Name: "projectroletemplatebinding", Aliases: []string{"prtb"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projectroletemplatebindings"},
		Policy: NSPlain, K8sKind: "ProjectRoleTemplateBinding", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},

	{
		Key: "provisioningcluster", Name: "provisioning-cluster", Aliases: []string{"provisioningclusters"},
		GVR:    schema.GroupVersionResource{Group: "provisioning.cattle.io", Version: "v1", Resource: "clusters"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Cluster", Scope: Management, Deletable: false, Sensitive: true, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		// Empty GVR version: cluster.x-k8s.io is resolved via discovery at call
		// time (the served version varies by Rancher release).
		Key: "capicluster", Name: "capi-cluster", Aliases: []string{"capiclusters"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "clusters"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Cluster", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "capimachine", Name: "machine", Aliases: []string{"machines"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machines"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Machine", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "capimachineset", Name: "machineset", Aliases: []string{"machinesets"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machinesets"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "MachineSet", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "capimachinedeployment", Name: "machinedeployment", Aliases: []string{"machinedeployments"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machinedeployments"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "MachineDeployment", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "k3kcluster", Name: "k3k-cluster", Aliases: []string{"k3kclusters"},
		GVR:    schema.GroupVersionResource{Group: "k3k.io", Version: "v1beta1", Resource: "clusters"},
		Policy: NSPlain, K8sKind: "Cluster", Scope: Downstream, Deletable: true, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		// Empty GVR version: ext.cattle.io is resolved via discovery at call
		// time; group absence means an older Rancher without the aggregated
		// imperative API (both kinds shipped in v2.12; the CLI's own
		// supported floor is now 2.14 — PS-8).
		Key: "token", Name: "token", Aliases: []string{"tokens"},
		GVR:    schema.GroupVersionResource{Group: "ext.cattle.io", Version: "", Resource: "tokens"},
		Policy: NSNone, K8sKind: "Token", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierPublic, Stability: safety.Beta},
		Columns: []Column{
			// STRING leaves only — table Rows renders int64/array paths as ""
			// (bools render since the nodedriver row's fallback; there is
			// still no EXPIRED bool column — EXPIRES carries the signal).
			{Header: "USER", Path: []string{"spec", "userID"}},
			{Header: "KIND", Path: []string{"spec", "kind"}},
			{Header: "EXPIRES", Path: []string{"status", "expiresAt"}},
			{Header: "DESCRIPTION", Path: []string{"spec", "description"}},
		},
	},
	{
		Key: "kubeconfig", Name: "kubeconfig", Aliases: []string{"kubeconfigs"},
		GVR:    schema.GroupVersionResource{Group: "ext.cattle.io", Version: "", Resource: "kubeconfigs"},
		Policy: NSNone, K8sKind: "Kubeconfig", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Tier: safety.TierPublic, Stability: safety.Beta},
		Columns: []Column{
			// STRING leaves only (see the token entry's note): TTL (int64) and
			// CLUSTERS ([]string) would render blank — deliberately omitted.
			{Header: "SUMMARY", Path: []string{"status", "summary"}},
			{Header: "DESCRIPTION", Path: []string{"spec", "description"}},
		},
	},

	{
		// K8sKind casing: derived from rancher's convert.Capitalize(driverName +
		// "Config") rule (dynamicschema.go:66-84,304-337; driver names from
		// machinedriver_data.go:24-39). "Amazonec2Config" is literal-confirmed in
		// rancher's own tests (status_test.go:70); the other four machine-config
		// kinds below follow the same capitalization rule but are not individually
		// literal-confirmed — verify against a real server before relying on the
		// exact casing in a live/Windows acceptance pass.
		// Sensitive: false deliberately — these CONFIGS carry instance parameters
		// (region, size, image) for the node driver; credentials live in the
		// driver's cc-* Secret, not on this resource.
		Key: "amazonec2config", Name: "amazonec2config", Aliases: []string{"amazonec2configs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "amazonec2configs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Amazonec2Config", Scope: Management, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "azureconfig", Name: "azureconfig", Aliases: []string{"azureconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "azureconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "AzureConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "digitaloceanconfig", Name: "digitaloceanconfig", Aliases: []string{"digitaloceanconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "digitaloceanconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "DigitaloceanConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "harvesterconfig", Name: "harvesterconfig", Aliases: []string{"harvesterconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "harvesterconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "HarvesterConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},
	{
		Key: "vmwarevsphereconfig", Name: "vmwarevsphereconfig", Aliases: []string{"vmwarevsphereconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "vmwarevsphereconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "VmwarevsphereConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
	},

	{
		// The server-side registry of node drivers (management.cattle.io/v3
		// NodeDriver) — the discovery surface behind `create cluster`'s
		// node-driver mode. Read surface only for now: Deletable: false, and
		// Sensitive: true so the generated patch (toggling spec.active flips
		// driver availability platform-wide, like a setting/feature) escalates
		// to --yes. Column field paths follow rancher's NodeDriverSpec
		// (machine_types.go): displayName is a string; active and builtin are
		// BOOLS (Bool: true), rendered via the table renderer's bool fallback.
		// builtin is `json:"builtin,omitempty"` upstream, so a non-builtin
		// driver omits the field — Bool makes the absent field render "false"
		// instead of blank (active, `json:"active"`, is always present, but
		// is marked too: it is boolean-typed all the same).
		Key: "nodedriver", Name: "nodedriver", Aliases: []string{"nodedrivers"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "nodedrivers"},
		Policy: NSNone, K8sKind: "NodeDriver", Scope: Management, Deletable: false, Sensitive: true,
		Completable: true,
		Binding:     safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns: []Column{
			{Header: "DISPLAYNAME", Path: []string{"spec", "displayName"}},
			{Header: "ACTIVE", Path: []string{"spec", "active"}, Bool: true},
			{Header: "BUILTIN", Path: []string{"spec", "builtin"}, Bool: true},
		},
	},

	{
		// Namespace = the cluster's management namespace; NSPlain gives the
		// --namespace flag. Deletable: false — CRTs are consumed, not deleted, in
		// the generic verb surface. This row carries identity only: its get is
		// overridden to a Steve read and its patch is refused (Task 5).
		Key: "clusterregistrationtoken", Name: "clusterregistrationtoken", Aliases: []string{"clusterregistrationtokens", "crt"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusterregistrationtokens"},
		Policy: NSPlain, K8sKind: "ClusterRegistrationToken", Scope: Management, Deletable: false,
		Binding: safety.Binding{Tier: safety.TierCRD, Stability: safety.Beta},
		Columns: []Column{{Header: "CLUSTER", Path: []string{"spec", "clusterName"}}},
	},
}

func All() []Kind { out := make([]Kind, len(table)); copy(out, table); return out }

// Bindings returns the tier bindings for generated generic ops: "get <Name>"
// and "patch <Name>" for every kind, and "delete <Name>" for deletable kinds.
// BuildRoot composes these with the bespoke resolver (single source per op).
func Bindings() map[string]safety.Binding {
	out := map[string]safety.Binding{}
	for _, k := range table {
		out["get "+k.Name] = k.Binding
		out["patch "+k.Name] = k.Binding
		if k.Deletable {
			out["delete "+k.Name] = k.Binding
		}
	}
	return out
}

// ProjectScopable reports whether --project applies to k (PC-3): a
// Downstream NSPlain kind validates/filters by project membership, plus the
// one carve-out — the namespace kind itself (NSNone/Downstream), which
// converts an unnamed --project into a label-selector list rather than a
// rejection (Route's default branch). Every other kind rejects a non-empty
// project outright. This is the single source of the rule; Route and the
// generated-command descriptor builders both consume it.
func ProjectScopable(k Kind) bool {
	if k.Policy == NSPlain && k.Scope == Downstream {
		return true
	}
	return k.Name == "namespace" && k.Policy == NSNone && k.Scope == Downstream
}

// LookupGVK resolves a manifest's apiVersion+kind to a table row. Group,
// VERSION, and K8sKind must all match — four rows share K8sKind "Cluster"
// across groups, and a version mismatch (apps/v1beta1 Deployment) must be
// rejected, never silently dispatched on the table's fixed GVR. Rows with an
// empty GVR.Version (CAPI) match any version of their group.
func LookupGVK(apiVersion, kind string) (Kind, bool) {
	group, version := "", apiVersion
	if i := strings.Index(apiVersion, "/"); i >= 0 {
		group, version = apiVersion[:i], apiVersion[i+1:]
	}
	for _, k := range table {
		if k.K8sKind != kind || k.GVR.Group != group {
			continue
		}
		if k.GVR.Version == "" || k.GVR.Version == version {
			return k, true
		}
	}
	return Kind{}, false
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
