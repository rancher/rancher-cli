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

// capiClusterNameLabel is the CAPI label key used to filter Machine objects
// by their owning provisioning cluster (metadata.name of the provisioning
// cluster CR). Defined as a package const so tests can assert the exact
// selector without duplicating the string.
const capiClusterNameLabel = "cluster.x-k8s.io/cluster-name"

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
		Usage:       "Inspect a cluster: status, agent connectivity, machine pools, members, machines, and registration",
		Desc:        "One document answering what state a cluster is in, what it is made of, and how nodes join it: the management cluster identity (id, name, state, kubernetesVersion — observed version first, the provisioning spec's desired version for pending clusters, empty when neither exists), its agent connectivity (agent.connected and agent.ready from the management Cluster's Connected and Ready conditions with the failing condition's message, preferring Connected's; the section is present only when those conditions exist and each field only when its own condition does; a disconnected agent adds a warning — the provisioning-derived status cannot see an agent outage), the provisioning.cattle.io Cluster's ready flag (derived from the Ready condition when present; falls back to status.ready) and conditions, its machine pools (name, machineConfigRef, quantity, roles), its members (ClusterRoleTemplateBindings with display names resolved from Users and UserAttributes; a failed binding read omits the members field from JSON while the text card renders a members-unavailable marker, with a warning naming the cause; enrichment failure only drops display names, never rows; entries whose principal id is prefixed system:// carry system: true (Rancher-internal accounts, not operator grants)), its machines (CAPI Machine objects in fleet-default filtered by the cluster.x-k8s.io/cluster-name label; each entry reports name, phase, and the first failing condition message — preferring conditions whose reason contains \"Error\"; a failed LIST omits the machines field from JSON while the text card renders a machines-unavailable marker, with a warning naming the cause; absent when the provisioning cluster was not found), and the default registration token's node-join pair (nodeCommand/insecureNodeCommand), import pair (command/insecureCommand), and manifestUrl (on an imported cluster — provisioning spec.rkeConfig absent — notApplicable lists the node-join fields that are present but do not apply; fields are annotated, never dropped). Sections degrade independently: provisioningNote marks a confirmed missing provisioning Cluster; an unavailable or ambiguous lookup and a failed registration read omit their sections with a warning naming the cause.",
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
	res.Agent = agentFrom(obj)
	var warnings []string
	if ag := res.Agent; ag != nil && ag.Connected != nil && !*ag.Connected {
		w := "cluster agent is not connected"
		if ag.Message != "" {
			w += ": " + engine.Printable(ag.Message)
		}
		warnings = append(warnings, w)
	}

	prov, lookup, pwarn, absentNote, perr := correlateProvisioningCluster(ctx, mgmt, clusterID)
	if perr != nil {
		return runtime.Result{}, perr
	}
	warnings = append(warnings, pwarn...)
	var imported bool
	switch lookup {
	case provFound:
		res.Status = statusFrom(prov)
		// Imported clusters omit spec.rkeConfig entirely — the same signal
		// the create path pins. Custom clusters carry rkeConfig (poolless),
		// and BOTH registration pairs apply to them.
		rkeCfg, hasRKE, _ := unstructured.NestedFieldNoCopy(prov.Object, "spec", "rkeConfig")
		imported = !hasRKE || rkeCfg == nil
		pools := machinePoolsFrom(prov)
		res.MachinePools = &pools
		machs, machWarn, machErr := clusterMachines(ctx, mgmt, prov.GetName())
		if machErr != nil {
			return runtime.Result{}, machErr
		}
		warnings = append(warnings, machWarn...)
		res.Machines = machs
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
	if imported && reg != nil {
		reg.markNodeCommandsInapplicable()
	}
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
//
// The Ready field is derived from the Ready condition when present:
// status.ready can flip true ~90s before the Ready condition leaves Unknown
// (live exercise observed this), so the condition is authoritative and
// status.ready is the fallback only when the condition is absent.
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
		if typ == "Ready" {
			st.Ready = status == "True"
		}
	}
	return st
}

// agentFrom reads the management Cluster's Connected and Ready conditions.
// Returns nil when neither exists — a management object without them (the
// e2e façade fixtures, a mid-import cluster) simply omits the section.
func agentFrom(obj *unstructured.Unstructured) *agentStatus {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var ag *agentStatus
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(cm, "type")
		if typ != "Connected" && typ != "Ready" {
			continue
		}
		status, _, _ := unstructured.NestedString(cm, "status")
		message, _, _ := unstructured.NestedString(cm, "message")
		if ag == nil {
			ag = &agentStatus{}
		}
		v := status == "True"
		switch typ {
		case "Connected":
			ag.Connected = &v
			if !v && message != "" {
				ag.Message = message // Connected's message wins
			}
		case "Ready":
			ag.Ready = &v
			if !v && message != "" && ag.Message == "" {
				ag.Message = message
			}
		}
	}
	return ag
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
	// NotApplicable is set only by Execute after this returns; the emptiness
	// decision considers only the surfaced string fields.
	if reg.NodeCommand == "" && reg.InsecureNodeCommand == "" && reg.Command == "" && reg.InsecureCommand == "" && reg.ManifestURL == "" {
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
		//lint:ignore S1016 explicit field copy: clusterMember and memberRow evolve independently
		rows = append(rows, clusterMember{Principal: m.Principal, DisplayName: m.DisplayName, Role: m.Role, System: m.System}) //nolint:staticcheck
	}
	return &rows, warnings, nil
}

// clusterMachines lists the provisioning cluster's CAPI Machines in
// fleet-default filtered by the cluster.x-k8s.io/cluster-name label. nil =
// LIST failed (field omitted from JSON, text renders unavailable); non-nil
// empty = verified no machines. Runs only when the provisioning cluster was
// found (the caller is responsible for the provFound guard).
func clusterMachines(ctx context.Context, mgmt engine.ClusterClient, provName string) (*[]clusterMachine, []string, error) {
	machineKind, _ := kinds.Lookup("capimachine")
	sel := capiClusterNameLabel + "=" + provName
	list, err := mgmt.List(ctx, machineKind.GVR, machineKind.DefaultNamespace, sel)
	if err != nil {
		if lookupCanceled(ctx, err) {
			return nil, nil, err
		}
		return nil, []string{"machines unavailable: " + err.Error()}, nil
	}
	rows := make([]clusterMachine, 0, len(list.Items))
	for i := range list.Items {
		name := list.Items[i].GetName()
		phase, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "phase")
		errMsg := machineErrorMessage(list.Items[i].Object)
		rows = append(rows, clusterMachine{Name: name, Phase: phase, Error: errMsg})
	}
	return &rows, nil, nil
}

// machineErrorMessage extracts the most relevant failing condition message
// from a CAPI Machine object. Conditions whose reason contains "Error" (e.g.
// CreateError from the DigitalOcean 422 live exercise) are preferred; any
// other non-True condition with a non-empty message is the fallback.
func machineErrorMessage(obj map[string]any) string {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	var errCond, anyCond string
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		status, _, _ := unstructured.NestedString(cm, "status")
		reason, _, _ := unstructured.NestedString(cm, "reason")
		message, _, _ := unstructured.NestedString(cm, "message")
		if status == "True" || message == "" {
			continue
		}
		if anyCond == "" {
			anyCond = message
		}
		if strings.Contains(reason, "Error") && errCond == "" {
			errCond = message
		}
	}
	if errCond != "" {
		return errCond
	}
	return anyCond
}

// clusterResult is `inspect cluster`'s payload — the JSON shape IS the
// agent contract (IC-5). Pointer semantics carry the tri-state: nil =
// section absent (never null/{}), a nil *[]machinePool = absent while a
// pointer to an empty slice marshals "machinePools":[] (IC-9 vs IC-7/7a —
// Go's omitempty drops non-nil empty slices, so a plain slice cannot
// distinguish them). ProvisioningNote is RESERVED for confirmed absence,
// at the root.
type clusterResult struct {
	Cluster          clusterIdentity   `json:"cluster"`
	Agent            *agentStatus      `json:"agent,omitempty"`
	ProvisioningNote string            `json:"provisioningNote,omitempty"`
	Status           *clusterStatus    `json:"status,omitempty"`
	MachinePools     *[]machinePool    `json:"machinePools,omitempty"`
	Members          *[]clusterMember  `json:"members,omitempty"`
	Machines         *[]clusterMachine `json:"machines,omitempty"`
	Registration     *registration     `json:"registration,omitempty"`
}

// agentStatus is the MANAGEMENT Cluster's agent-connectivity view — the
// Connected and Ready conditions the provisioning-derived status never
// carries (a live exercise saw status.ready stay true through a real agent
// outage). Tri-state per the clusterResult pointer convention: the section
// is nil when neither condition exists; each field is nil when its own
// condition is absent — nothing is fabricated. Message carries the first
// failing condition's message, preferring Connected's.
type agentStatus struct {
	Connected *bool  `json:"connected,omitempty"`
	Ready     *bool  `json:"ready,omitempty"`
	Message   string `json:"message,omitempty"`
}

type clusterMachine struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	// Error is the first failing condition message (preferring conditions
	// whose reason contains "Error"); omitted when no failing condition
	// carries a message.
	Error string `json:"error,omitempty"`
}

type clusterMember struct {
	Principal   string `json:"principal"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role"`
	// System marks Rancher-internal principals (system:// prefix);
	// omitted when false so existing entries are byte-identical.
	System bool `json:"system,omitempty"`
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
	// NotApplicable names registration fields that are present but do not
	// apply to this cluster's creation mode: an imported cluster
	// (provisioning spec.rkeConfig absent) has no node-join workflow, so
	// nodeCommand/insecureNodeCommand are annotated rather than dropped —
	// the JSON contract is additive and consumers may pin field presence.
	// Never names a field the payload does not carry.
	NotApplicable []string `json:"notApplicable,omitempty"`
}

// markNodeCommandsInapplicable annotates the node-join pair as not
// applicable (imported cluster). A placeholder-dropped or server-empty
// command stays unannotated — the list never names an absent field.
func (r *registration) markNodeCommandsInapplicable() {
	if r.NodeCommand != "" {
		r.NotApplicable = append(r.NotApplicable, "nodeCommand")
	}
	if r.InsecureNodeCommand != "" {
		r.NotApplicable = append(r.NotApplicable, "insecureNodeCommand")
	}
}

// regLabel suffixes a registration field label that is present but does
// not apply to this cluster's creation mode.
func regLabel(label string, notApplicable bool) string {
	if notApplicable {
		return label + " [not applicable: imported cluster]"
	}
	return label
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
	renderAgentLine(&b, r.Agent)
	renderConditionsSection(&b, r)
	renderMachinePoolsSection(&b, r)
	renderMembersSection(&b, r)
	renderMachinesSection(&b, r)
	// Registration values render FULL-WIDTH and are never truncated: they
	// are meant to be copy-pasted (IC-1a).
	b.WriteString("\nREGISTRATION\n")
	if r.Registration == nil {
		b.WriteString("  registration: not available\n")
	} else {
		na := map[string]bool{}
		for _, f := range r.Registration.NotApplicable {
			na[f] = true
		}
		writeRegistrationField(&b, regLabel("node join", na["nodeCommand"]), r.Registration.NodeCommand)
		writeRegistrationField(&b, regLabel("node join (insecure)", na["insecureNodeCommand"]), r.Registration.InsecureNodeCommand)
		writeRegistrationField(&b, "import", r.Registration.Command)
		writeRegistrationField(&b, "import (insecure)", r.Registration.InsecureCommand)
		writeRegistrationField(&b, "manifestUrl", r.Registration.ManifestURL)
	}
	return b.String()
}

// renderAgentLine appends the agent-connectivity line to the CLUSTER
// section. Silent when the Connected condition is absent — the text card
// never claims a connectivity state the server did not report.
func renderAgentLine(b *strings.Builder, ag *agentStatus) {
	if ag == nil || ag.Connected == nil {
		return
	}
	if *ag.Connected {
		b.WriteString("  agent: connected\n")
		return
	}
	line := "  agent: disconnected"
	if ag.Message != "" {
		line += " — " + truncateRunes(engine.Printable(ag.Message), conditionMessageLimit)
	}
	b.WriteString(line + "\n")
}

func renderConditionsSection(b *strings.Builder, r clusterResult) {
	b.WriteString("\nCONDITIONS\n")
	switch {
	case r.Status == nil && r.ProvisioningNote != "":
		b.WriteString("  " + r.ProvisioningNote + "\n")
	case r.Status == nil:
		b.WriteString("  status: unavailable\n")
	default:
		fmt.Fprintf(b, "  ready: %t\n", r.Status.Ready)
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
}

func renderMachinePoolsSection(b *strings.Builder, r clusterResult) {
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
			fmt.Fprintf(b, "  %s: %s/%s quantity=%d roles=%s\n",
				p.Name, p.MachineConfigKind, p.MachineConfigName, p.Quantity, strings.Join(p.Roles, ","))
		}
	}
}

func renderMembersSection(b *strings.Builder, r clusterResult) {
	b.WriteString("\nMEMBERS\n")
	switch {
	case r.Members == nil:
		b.WriteString("  members: unavailable\n")
	case len(*r.Members) == 0:
		b.WriteString("  none\n")
	default:
		for _, m := range *r.Members {
			suffix := ""
			if m.System {
				suffix = " [system]"
			}
			if m.DisplayName != "" {
				fmt.Fprintf(b, "  %s (%s)  %s%s\n", engine.Printable(m.DisplayName), engine.Printable(m.Principal), engine.Printable(m.Role), suffix)
			} else {
				fmt.Fprintf(b, "  %s  %s%s\n", engine.Printable(m.Principal), engine.Printable(m.Role), suffix)
			}
		}
	}
}

func renderMachinesSection(b *strings.Builder, r clusterResult) {
	b.WriteString("\nMACHINES\n")
	switch {
	case r.Machines == nil && r.ProvisioningNote != "":
		b.WriteString("  " + r.ProvisioningNote + "\n")
	case r.Machines == nil:
		b.WriteString("  machines: unavailable\n")
	case len(*r.Machines) == 0 && r.MachinePools != nil && len(*r.MachinePools) == 0:
		// Same signal as the MACHINE POOLS empty state: a poolless
		// provisioning CR means imported or custom — no CAPI Machines is
		// the expected shape, not a gap.
		b.WriteString("  none (imported or custom cluster)\n")
	case len(*r.Machines) == 0:
		b.WriteString("  none\n")
	default:
		for _, m := range *r.Machines {
			// Name and phase are server-sourced strings too — same
			// sanitizer as the error message and the members section.
			fmt.Fprintf(b, "  %s  %s\n", engine.Printable(m.Name), engine.Printable(m.Phase))
			if m.Error != "" {
				fmt.Fprintf(b, "    %s\n", engine.Printable(m.Error))
			}
		}
	}
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
