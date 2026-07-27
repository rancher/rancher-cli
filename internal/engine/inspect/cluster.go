package inspect

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// conditionMessageLimit is the text-view truncation bound for condition
// messages (IC-1a "truncated sanely"); the JSON envelope always carries
// the full message. Join commands are exempt — they are copy-pasted.
const conditionMessageLimit = 120

type clusterCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewInspectCluster builds `inspect cluster <name-or-id>`: the management
// Cluster identity (the one hard requirement), the provisioning
// Cluster CR correlated by status.clusterName in fleet-default (ready +
// conditions + machine pools), and the default registration token's
// node-join and import commands, joined into one document. Every section
// but the first degrades: confirmed provisioning absence sets
// provisioningNote, an unavailable or ambiguous lookup and a failed
// registration read each omit their section(s) with a warning.
func NewInspectCluster(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &clusterCmd{clients: clients, roots: roots}
}

func (c *clusterCmd) Describe() runtime.Descriptor {
	k, _ := kinds.Lookup("cluster")
	return runtime.Descriptor{
		Name:        "inspect cluster",
		Aliases:     k.Aliases,
		Usage:       "Inspect a cluster: status, machine pools, members, and registration",
		Desc:        "One document answering what state a cluster is in, what it is made of, and how nodes join it: the management cluster identity (id, name, state, kubernetesVersion — observed version first, the provisioning spec's desired version for pending clusters, empty when neither exists), the provisioning.cattle.io Cluster's ready flag and conditions, its machine pools (name, machineConfigRef, quantity, roles), its members (ClusterRoleTemplateBindings with display names resolved from Users and UserAttributes; a failed binding read omits the members field from JSON while the text card renders a members-unavailable marker, with a warning naming the cause; enrichment failure only drops display names, never rows), and the default registration token's node-join pair (nodeCommand/insecureNodeCommand), import pair (command/insecureCommand), and manifestUrl. Sections degrade independently: provisioningNote marks a confirmed missing provisioning Cluster; an unavailable or ambiguous lookup and a failed registration read omit their sections with a warning naming the cause.",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true, Desc: "cluster `id|name`"}},
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c *clusterCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if err := rejectProject(c.clients, in, "inspect cluster"); err != nil {
		return runtime.Result{}, err
	}
	clusterID, err := c.clients.ResolveCluster(ctx, in.Args()[0])
	if err != nil {
		return runtime.Result{}, err
	}
	mgmt, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	mgmtKind, _ := kinds.Lookup("cluster")
	obj, err := mgmt.Get(ctx, mgmtKind.GVR, "", clusterID)
	if err != nil {
		return runtime.Result{}, err // IC-6: the one hard requirement
	}
	displayName, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
	state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
	observedVersion, _, _ := unstructured.NestedString(obj.Object, "status", "version", "gitVersion")

	res := clusterResult{Cluster: clusterIdentity{
		ID:                clusterID,
		Name:              displayName,
		State:             state,
		KubernetesVersion: observedVersion,
	}}
	var warnings []string

	prov, lookup, pwarn, absentNote, perr := correlateProvisioningCluster(ctx, mgmt, clusterID)
	if perr != nil {
		return runtime.Result{}, perr
	}
	warnings = append(warnings, pwarn...)
	switch lookup {
	case provFound:
		res.Status = statusFrom(prov)
		pools := machinePoolsFrom(prov)
		res.MachinePools = &pools
		if res.Cluster.KubernetesVersion == "" {
			// IC-4b: pending clusters carry only the DESIRED version on the
			// provisioning spec; "" stays "" — never a fabricated value.
			desired, _, _ := unstructured.NestedString(prov.Object, "spec", "kubernetesVersion")
			res.Cluster.KubernetesVersion = desired
		}
	case provAbsent:
		res.ProvisioningNote = absentNote
	case provUnavailable:
		// Sections stay omitted; the warning already names the cause (IC-7a).
	}

	reg, rwarn, rerr := c.fetchRegistration(ctx, clusterID)
	if rerr != nil {
		return runtime.Result{}, rerr
	}
	warnings = append(warnings, rwarn...)
	res.Registration = reg

	members, mwarn, merr := clusterMembers(ctx, mgmt, clusterID)
	if merr != nil {
		return runtime.Result{}, merr
	}
	warnings = append(warnings, mwarn...)
	res.Members = members

	return runtime.Result{
		Kind:     "ClusterInspection",
		Data:     res,
		Text:     renderClusterText(res),
		Warnings: warnings,
	}, nil
}

// lookupCanceled reports whether a failed optional lookup was aborted
// because the CALLER's context was canceled or its deadline expired. The
// optional lookups degrade only on ordinary availability failures — a
// caller abort must propagate as the command's error, never continue
// issuing requests behind a warning. Two complementary checks: ctx.Err()
// consults the caller context's OWN state, which survives any error
// wrapping — the production adapters rebuild their errors from strings
// (provider's mapErr, the root client's GetJSON), so the sentinel never
// survives the chain there; the errors.Is walk covers callers and fakes
// that DO preserve the cause. An adapter-owned per-request deadline firing
// while the caller's context is still active — the provider's opCtx child
// timeout (internal/client/provider requestTimeout) or the root
// http.Client's timeout (surface/cli rootTimeout) — is deliberately NOT
// cancellation: neither check trips (the caller ctx is fine and the
// rebuilt error carries no sentinel), so it degrades to a warning like any
// other availability failure.
func lookupCanceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
}

type provLookup int

const (
	provFound provLookup = iota
	provAbsent
	provUnavailable
)

// correlateProvisioningCluster LISTs provisioning Clusters in
// fleet-default and selects the unique object whose status.clusterName
// equals the resolved management cluster id. metadata.name is the
// user-supplied provisioning name and spec.displayName can be renamed —
// neither correlates reliably (IC-4). Zero matches is confirmed absence
// (IC-7); a list failure or multiple matches is "could not determine"
// (IC-7a), each with a warning naming the cause. The fourth return is the
// IC-7 confirmed-absence note (non-empty only when provAbsent), scoped to
// the namespace that was searched — derived from provKind.DefaultNamespace,
// never a second hardcoded string. An expired context is the one list
// failure that does NOT degrade: it returns as the error.
func correlateProvisioningCluster(ctx context.Context, mgmt engine.ClusterClient, clusterID string) (*unstructured.Unstructured, provLookup, []string, string, error) {
	provKind, _ := kinds.Lookup("provisioning-cluster")
	list, err := mgmt.List(ctx, provKind.GVR, provKind.DefaultNamespace, "")
	if err != nil {
		if lookupCanceled(ctx, err) {
			return nil, provUnavailable, nil, "", err
		}
		return nil, provUnavailable, []string{"provisioning cluster lookup unavailable: " + err.Error()}, "", nil
	}
	var matches []*unstructured.Unstructured
	for i := range list.Items {
		name, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "clusterName")
		if name == clusterID {
			matches = append(matches, &list.Items[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, provAbsent, nil, "no provisioning.cattle.io Cluster for this cluster in " + provKind.DefaultNamespace, nil
	case 1:
		return matches[0], provFound, nil, "", nil
	default:
		return nil, provUnavailable, []string{fmt.Sprintf(
			"provisioning cluster lookup ambiguous: %d provisioning.cattle.io Clusters report status.clusterName %q",
			len(matches), clusterID)}, "", nil
	}
}

// statusFrom reads exactly what provisioning.cattle.io/v1.ClusterStatus
// exposes: ready and conditions (type/status/reason/message). Conditions
// are ALWAYS a non-nil slice so the marshaled envelope carries
// "conditions":[] (IC-5); no phase/transitioning/error fields — they
// belong to other API representations and never appear on this CR.
func statusFrom(prov *unstructured.Unstructured) *clusterStatus {
	ready, _, _ := unstructured.NestedBool(prov.Object, "status", "ready")
	st := &clusterStatus{Ready: ready, Conditions: []clusterCondition{}}
	conds, _, _ := unstructured.NestedSlice(prov.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(cm, "type")
		status, _, _ := unstructured.NestedString(cm, "status")
		reason, _, _ := unstructured.NestedString(cm, "reason")
		message, _, _ := unstructured.NestedString(cm, "message")
		st.Conditions = append(st.Conditions, clusterCondition{Type: typ, Status: status, Reason: reason, Message: message})
	}
	return st
}

// machinePoolsFrom reads spec.rkeConfig.machinePools: name,
// machineConfigRef kind/name, quantity, and the etcd/controlplane/worker
// roles as a sorted string slice. Always returns a non-nil slice — the
// CALLER's pointer distinguishes "correlated but poolless" (pointer to
// empty, IC-9) from "section absent" (nil pointer, IC-7/7a).
func machinePoolsFrom(prov *unstructured.Unstructured) []machinePool {
	pools := []machinePool{}
	raw, _, _ := unstructured.NestedSlice(prov.Object, "spec", "rkeConfig", "machinePools")
	for _, p := range raw {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(pm, "name")
		kind, _, _ := unstructured.NestedString(pm, "machineConfigRef", "kind")
		cfgName, _, _ := unstructured.NestedString(pm, "machineConfigRef", "name")
		quantity, _, _ := unstructured.NestedInt64(pm, "quantity")
		roles := []string{}
		// Appended in sorted order by construction: controlplane < etcd < worker.
		if b, _, _ := unstructured.NestedBool(pm, "controlPlaneRole"); b {
			roles = append(roles, "controlplane")
		}
		if b, _, _ := unstructured.NestedBool(pm, "etcdRole"); b {
			roles = append(roles, "etcd")
		}
		if b, _, _ := unstructured.NestedBool(pm, "workerRole"); b {
			roles = append(roles, "worker")
		}
		pools = append(pools, machinePool{
			Name:              name,
			MachineConfigKind: kind,
			MachineConfigName: cfgName,
			Quantity:          quantity,
			Roles:             roles,
		})
	}
	return pools
}

// fetchRegistration reads the default registration token via the IC-4a
// fetch and applies inspect's OWN placeholder policy over the
// policy-neutral reporter: a placeholder-bearing field is dropped (its
// omitempty tag makes it disappear from the wire) and warned about; a
// failed read — Root() or the fetch — omits the whole section with a
// warning (IC-8), except an expired context, which returns as the error.
// When NO surfaced field survives (all five empty after placeholder
// drops, or a server response with every field empty) the section is nil
// — absent from the wire, never "registration":{}. The named-get and
// list-get policies live solely in their respective callers in
// internal/engine/provisioning.
func (c *clusterCmd) fetchRegistration(ctx context.Context, clusterID string) (*registration, []string, error) {
	root, err := c.roots.Root()
	if err != nil {
		return nil, []string{"registration token unavailable: " + err.Error()}, nil
	}
	obj, err := provisioning.FetchDefaultCRT(ctx, root, clusterID)
	if err != nil {
		if lookupCanceled(ctx, err) {
			return nil, nil, err
		}
		return nil, []string{"registration token unavailable: " + err.Error()}, nil
	}
	dropped := map[string]bool{}
	var warnings []string
	for _, field := range provisioning.PlaceholderFields(obj) {
		dropped[field] = true
		if registrationSurfacedFields[field] {
			warnings = append(warnings, fmt.Sprintf(
				"registration token %s/default-token: %s not formatted by the server; omitted", clusterID, field))
		}
	}
	reg := &registration{}
	if !dropped["nodeCommand"] {
		reg.NodeCommand = obj.NodeCommand
	}
	if !dropped["insecureNodeCommand"] {
		reg.InsecureNodeCommand = obj.InsecureNodeCommand
	}
	if !dropped["command"] {
		reg.Command = obj.Command
	}
	if !dropped["insecureCommand"] {
		reg.InsecureCommand = obj.InsecureCommand
	}
	if !dropped["manifestUrl"] {
		reg.ManifestURL = obj.ManifestURL
	}
	if *reg == (registration{}) {
		return nil, warnings, nil
	}
	return reg, warnings, nil
}

// clusterMembers lists the cluster's crtbs (namespace = cluster id) and
// enriches them via resolveMembers. nil = binding LIST failed (field
// omitted, text renders unavailable); non-nil empty = verified no members.
func clusterMembers(ctx context.Context, mgmt engine.ClusterClient, clusterID string) (*[]clusterMember, []string, error) {
	crtbKind, _ := kinds.Lookup("clusterroletemplatebinding")
	list, err := mgmt.List(ctx, crtbKind.GVR, clusterID, "")
	if err != nil {
		if lookupCanceled(ctx, err) {
			return nil, nil, err
		}
		return nil, []string{"clusterroletemplatebindings unavailable: " + err.Error()}, nil
	}
	resolved, warnings, err := resolveMembers(ctx, mgmt, list.Items)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]clusterMember, 0, len(resolved))
	for _, m := range resolved {
		rows = append(rows, clusterMember{Principal: m.Principal, DisplayName: m.DisplayName, Role: m.Role}) //nolint:staticcheck // explicit field copy: clusterMember and memberRow evolve independently
	}
	return &rows, warnings, nil
}

// clusterResult is `inspect cluster`'s payload — the JSON shape IS the
// agent contract (IC-5). Pointer semantics carry the tri-state: nil =
// section absent (never null/{}), a nil *[]machinePool = absent while a
// pointer to an empty slice marshals "machinePools":[] (IC-9 vs IC-7/7a —
// Go's omitempty drops non-nil empty slices, so a plain slice cannot
// distinguish them). ProvisioningNote is RESERVED for confirmed absence,
// at the root.
type clusterResult struct {
	Cluster          clusterIdentity  `json:"cluster"`
	ProvisioningNote string           `json:"provisioningNote,omitempty"`
	Status           *clusterStatus   `json:"status,omitempty"`
	MachinePools     *[]machinePool   `json:"machinePools,omitempty"`
	Members          *[]clusterMember `json:"members,omitempty"`
	Registration     *registration    `json:"registration,omitempty"`
}

type clusterMember struct {
	Principal   string `json:"principal"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role"`
}

type clusterIdentity struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	State             string `json:"state"`
	KubernetesVersion string `json:"kubernetesVersion"`
}

type clusterStatus struct {
	Ready bool `json:"ready"`
	// Conditions is always non-nil: the marshaled envelope must contain
	// "conditions":[] even when the CR reports none (IC-5).
	Conditions []clusterCondition `json:"conditions"`
}

type clusterCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type machinePool struct {
	Name              string   `json:"name"`
	MachineConfigKind string   `json:"machineConfigKind"`
	MachineConfigName string   `json:"machineConfigName"`
	Quantity          int64    `json:"quantity"`
	Roles             []string `json:"roles"`
}

// registrationSurfacedFields is the set of CRTObject field names that
// fetchRegistration maps onto the registration struct. The two Windows-only
// fields (windowsNodeCommand, insecureWindowsNodeCommand) are Steve-guarded
// but never appear in inspect cluster's envelope; warnings for them would
// name fields that do not exist in the output.
var registrationSurfacedFields = map[string]bool{
	"nodeCommand":         true,
	"insecureNodeCommand": true,
	"command":             true,
	"insecureCommand":     true,
	"manifestUrl":         true,
}

// registration carries BOTH workflow pairs explicitly — the node pair
// (custom-cluster join) and the import pair (imported-cluster manifest) —
// plus manifestUrl; one is never substituted for the other. Every field
// is individually omitempty so a placeholder-bearing or empty field
// DISAPPEARS from the wire rather than marshaling "" (IC-5).
type registration struct {
	NodeCommand         string `json:"nodeCommand,omitempty"`
	InsecureNodeCommand string `json:"insecureNodeCommand,omitempty"`
	Command             string `json:"command,omitempty"`
	InsecureCommand     string `json:"insecureCommand,omitempty"`
	ManifestURL         string `json:"manifestUrl,omitempty"`
}

// renderClusterText is the IC-1a default rendering: a deliberate
// multi-section text view (a summary row cannot carry conditions, pools,
// and join commands). Omitted sections render a CONCISE availability
// marker in place; the detailed cause lives only in Result.Warnings,
// which the runtime prints to stderr — never duplicated here.
func renderClusterText(r clusterResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CLUSTER\n  id: %s\n  name: %s\n  kubernetesVersion: %s\n  state: %s\n",
		r.Cluster.ID, r.Cluster.Name, r.Cluster.KubernetesVersion, r.Cluster.State)

	b.WriteString("\nCONDITIONS\n")
	switch {
	case r.Status == nil && r.ProvisioningNote != "":
		b.WriteString("  " + r.ProvisioningNote + "\n")
	case r.Status == nil:
		b.WriteString("  status: unavailable\n")
	default:
		fmt.Fprintf(&b, "  ready: %t\n", r.Status.Ready)
		for _, c := range orderedConditions(r.Status.Conditions) {
			line := fmt.Sprintf("  %s=%s", c.Type, c.Status)
			if c.Reason != "" {
				line += " reason=" + c.Reason
			}
			if c.Message != "" {
				line += " " + truncateRunes(c.Message, conditionMessageLimit)
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\nMACHINE POOLS\n")
	switch {
	case r.MachinePools == nil && r.ProvisioningNote != "":
		b.WriteString("  " + r.ProvisioningNote + "\n")
	case r.MachinePools == nil:
		b.WriteString("  machine pools: unavailable\n")
	case len(*r.MachinePools) == 0:
		b.WriteString("  none (imported or custom cluster)\n")
	default:
		for _, p := range *r.MachinePools {
			fmt.Fprintf(&b, "  %s: %s/%s quantity=%d roles=%s\n",
				p.Name, p.MachineConfigKind, p.MachineConfigName, p.Quantity, strings.Join(p.Roles, ","))
		}
	}

	b.WriteString("\nMEMBERS\n")
	switch {
	case r.Members == nil:
		b.WriteString("  members: unavailable\n")
	case len(*r.Members) == 0:
		b.WriteString("  none\n")
	default:
		for _, m := range *r.Members {
			if m.DisplayName != "" {
				fmt.Fprintf(&b, "  %s (%s)  %s\n", printable(m.DisplayName), printable(m.Principal), printable(m.Role))
			} else {
				fmt.Fprintf(&b, "  %s  %s\n", printable(m.Principal), printable(m.Role))
			}
		}
	}

	// Registration values render FULL-WIDTH and are never truncated: they
	// are meant to be copy-pasted (IC-1a).
	b.WriteString("\nREGISTRATION\n")
	if r.Registration == nil {
		b.WriteString("  registration: not available\n")
	} else {
		writeRegistrationField(&b, "node join", r.Registration.NodeCommand)
		writeRegistrationField(&b, "node join (insecure)", r.Registration.InsecureNodeCommand)
		writeRegistrationField(&b, "import", r.Registration.Command)
		writeRegistrationField(&b, "import (insecure)", r.Registration.InsecureCommand)
		writeRegistrationField(&b, "manifestUrl", r.Registration.ManifestURL)
	}
	return b.String()
}

func writeRegistrationField(b *strings.Builder, label, value string) {
	if value == "" {
		return // dropped or server-empty: absent in place; detail is in Warnings
	}
	fmt.Fprintf(b, "  %s:\n    %s\n", label, value)
}

// orderedConditions applies the ONLY derived trouble summarization the
// spec allows, and only in the text view: conditions with status "False"
// whose reason or message is non-empty list first; everything else keeps
// its server order. The JSON envelope always carries the raw order.
func orderedConditions(conds []clusterCondition) []clusterCondition {
	trouble := make([]clusterCondition, 0, len(conds))
	rest := make([]clusterCondition, 0, len(conds))
	for _, c := range conds {
		if c.Status == "False" && (c.Reason != "" || c.Message != "") {
			trouble = append(trouble, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(trouble, rest...)
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
