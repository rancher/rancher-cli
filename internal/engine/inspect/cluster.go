package inspect

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	accesscomplete "github.com/rancher/rancher-cli/internal/engine/access/complete"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// conditionMessageLimit is the text-view truncation bound for condition
// messages (truncated sanely); the JSON envelope always carries
// the full message. Join commands are exempt - they are copy-pasted.
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
// node and import commands, joined into one document. Every section
// but the first degrades: confirmed provisioning absence sets
// provisioningNote, an unavailable or ambiguous lookup and a failed
// registration read each omit their section(s) with a warning.
func NewInspectCluster(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &clusterCmd{clients: clients, roots: roots}
}

func (c *clusterCmd) Describe() runtime.Descriptor {
	k, _ := kinds.Lookup("cluster")
	return runtime.Descriptor{
		Name:    "inspect cluster",
		Aliases: k.Aliases,
		Usage:   "Inspect a cluster: status, agent connectivity, machine pools, members, machines, and registration",
		Desc: "One document answering what state a cluster is in, what it is made of, and how nodes join it: " +
			"the management.cattle.io Cluster's id, name, state, and kubernetesVersion (the observed version first, " +
			"then the provisioning spec's desired version for pending clusters, empty when neither exists), " +
			"its agent connectivity (agent.connected and agent.ready from the management.cattle.io Cluster's " +
			"Connected and Ready conditions with the failing condition's message, preferring Connected's; " +
			"the section is present only when those conditions report an explicit True or False " +
			"(a condition with status Unknown is ignored and never shows the agent as not connected) " +
			"and each field only when its own condition does; an agent that is not connected or a False Ready " +
			"condition adds a warning - the status of the provisioning.cattle.io Cluster cannot show an agent outage), " +
			"the provisioning.cattle.io Cluster's ready flag (derived from the Ready condition when present; " +
			"falls back to status.ready) and conditions, its machine pools (name, machineConfigRef, quantity, roles), " +
			"its members (ClusterRoleTemplateBindings with display names resolved from Users and UserAttributes, " +
			"each row reporting principal, displayName, role, binding (the name of the ClusterRoleTemplateBinding), " +
			"and system; a failed binding read leaves the members field out of the JSON while the text output shows " +
			"members: unavailable, with a warning naming the cause; a failure to look up display names leaves out only " +
			"the display names, never rows; entries whose principal id starts with system: " +
			"(Rancher's system principals, e.g. system://provisioning/...) carry system: true " +
			"(accounts Rancher uses internally, not bindings an administrator added)), " +
			"its machines (CAPI Machine objects in fleet-default filtered by the cluster.x-k8s.io/cluster-name label; " +
			"each entry reports name, phase, and the first failing condition message - " +
			"preferring conditions whose reason contains \"Error\"; a failed list leaves the machines field out of the JSON " +
			"while the text output shows machines: unavailable, with a warning naming the cause; " +
			"left out when the provisioning cluster was not found), " +
			"and the commands in the default cluster registration token: nodeCommand and insecureNodeCommand (to add nodes), " +
			"command and insecureCommand (to import a cluster), and manifestUrl. " +
			"On an imported cluster (provisioning spec.rkeConfig absent), notApplicable lists the node command fields " +
			"that are present but do not apply; such fields are marked, never left out. " +
			"By default the registration section holds only a message and a hint, and the commands are left out, " +
			"because they contain the cluster registration token (a credential that lets anyone add nodes to the cluster); " +
			"pass --registration to fetch and show them. " +
			"Each section can be missing without affecting the others: provisioningNote marks a confirmed missing " +
			"provisioning Cluster; an unavailable or ambiguous lookup and a failed registration read leave out their " +
			"sections with a warning naming the cause. " +
			degradedContractDesc +
			"Finding the cluster by id or name and reading its management.cattle.io Cluster are required: " +
			"the command fails when any of them fails. " +
			"Sections that can degrade: provisioning (unavailable - the lookup of the provisioning.cattle.io Cluster " +
			"that matches this cluster, one entry covering the status, machinePools, and machines, " +
			"which are then left out), machines (unavailable - a machine list that failed after that Cluster was found), " +
			"members (unavailable - the binding list; a failure to look up display names stays a warning), " +
			"registration (unavailable - reading the cluster registration token).",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true, Desc: "cluster `id|name`"}},
		Flags: []runtime.ArgSpec{{
			Name: "registration",
			Type: "bool",
			Desc: "show the registration commands (they contain the cluster registration token and are left out by default); also available with: `rancher get clusterregistrationtoken default-token --namespace <cluster-id>`",
		}},
		Auth:  true,
		Shape: runtime.ShapeText,
	}
}

// LiveSource completes the positional with the clusters the caller can
// read, the set the command resolves it against.
func (*clusterCmd) LiveSource() completion.Source { return accesscomplete.SteveClusterSource() }

func (c *clusterCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	clusterID, err := engine.ResolveClusterArgument(ctx, c.clients, in.Args()[0])
	if err != nil {
		return runtime.Result{}, err
	}
	mgmt, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	obj, err := steve.GetCluster(ctx, steve.RootFactoryGetter(c.roots), clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	displayName, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
	state := steve.ObjectState(obj)
	observedVersion, _, _ := unstructured.NestedString(obj.Object, "status", "version", "gitVersion")

	res := clusterResult{Cluster: clusterIdentity{
		ID:                clusterID,
		Name:              displayName,
		State:             state,
		KubernetesVersion: observedVersion,
	}}
	res.Agent = agentFrom(obj)
	sec := newSections(ctx, sectionProvisioning, sectionMachines, sectionMembers, sectionRegistration)
	// Advisory warnings: agent verdicts, member display-name enrichment, and
	// registration placeholder drops describe a section that is present and
	// usable, so they never become degraded entries.
	var warnings []string
	switch res.Agent.verdict() {
	case agentDisconnected:
		w := "cluster agent is not connected"
		if res.Agent.Message != "" {
			w += ": " + output.Printable(res.Agent.Message)
		}
		warnings = append(warnings, w)
	case agentConnectedNotReady, agentNotReady:
		w := "cluster is not ready"
		if res.Agent.Message != "" {
			w += ": " + output.Printable(res.Agent.Message)
		}
		warnings = append(warnings, w)
	}

	// The two independent legs fan out concurrently (the errgroup pattern
	// images --all-clusters already uses; the accumulator is mutex-guarded
	// for exactly this): the provisioning Cluster lookup with its dependent
	// machine lookup, and the member list. Each leg writes only its own
	// variables; a non-nil leg error is the caller's cancellation, which
	// aborts the group. The registration fetch stays AFTER the join: it
	// needs the provisioning leg's imported verdict, and - asserted by the
	// cancellation tests - a join-token fetch must never be issued once the
	// caller has gone away. Warnings are appended in a fixed order after
	// the join so output stays deterministic.
	var (
		imported bool
		lookup   provLookup
		members  *[]rbac.MemberRow
		mwarn    []string
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var prov *unstructured.Unstructured
		var absentNote string
		var perr error
		prov, lookup, absentNote, perr = correlateProvisioningCluster(gctx, mgmt, sec, clusterID)
		if perr != nil {
			return perr
		}
		switch lookup {
		case provFound:
			res.Status = statusFrom(prov)
			// provisioning.IsImported: spec.rkeConfig nil-vs-{} is the create path's discriminator.
			imported = provisioning.IsImported(prov)
			pools := machinePoolsFrom(prov)
			res.MachinePools = &pools
			machs, machErr := clusterMachines(gctx, mgmt, sec, prov.GetName())
			if machErr != nil {
				return machErr
			}
			res.Machines = machs
			if res.Cluster.KubernetesVersion == "" {
				// Pending clusters carry only the DESIRED version on the
				// provisioning spec; "" stays "" - never a fabricated value.
				desired, _, _ := unstructured.NestedString(prov.Object, "spec", "kubernetesVersion")
				res.Cluster.KubernetesVersion = desired
			}
		case provAbsent:
			res.ProvisioningNote = absentNote
		case provUnavailable:
			// Sections stay omitted; the warning already names the cause.
		}
		return nil
	})
	g.Go(func() error {
		var merr error
		members, mwarn, merr = clusterMembers(gctx, mgmt, sec, clusterID)
		return merr
	})
	if err := g.Wait(); err != nil {
		return runtime.Result{}, err
	}

	if in.Bool("registration") {
		reg, rwarn, rerr := c.fetchRegistration(ctx, sec, clusterID)
		if rerr != nil {
			return runtime.Result{}, rerr
		}
		warnings = append(warnings, rwarn...)
		switch {
		case imported && reg != nil:
			reg.markNodeCommandsInapplicable()
		case lookup == provUnavailable && reg != nil:
			// imported is UNKNOWN, not false: the provisioning Cluster lookup
			// was unavailable: the node command fields cannot be annotated
			// either way. Advisory - the section itself is present and
			// usable, only its applicability verdict is missing.
			warnings = append(warnings,
				"cannot determine whether the node commands apply: the provisioning.cattle.io Cluster for this cluster is unavailable")
		}
		res.Registration = reg
	} else {
		// Structural gate: the registration section embeds the cluster
		// registration token. No fetch is issued when --registration is absent.
		res.Registration = &registration{
			Withheld: true,
			Message:  withheldMessage(),
			Hint:     withheldHint(clusterID),
		}
	}

	warnings = append(warnings, mwarn...)
	res.Members = members

	degraded, dwarn := sec.Finalize()
	res.Degraded = degraded
	warnings = append(warnings, dwarn...)

	text := renderClusterText(res)
	return runtime.Result{
		Kind:       "ClusterInspection",
		Data:       res,
		Text:       text,
		StyledText: func(s style.Style) string { return styleClusterText(text, s) },
		Warnings:   warnings,
	}, nil
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
// user-supplied provisioning name and spec.displayName can be renamed -
// neither correlates reliably. Zero matches is confirmed absence;
// a list failure or multiple matches is "could not determine",
// each recorded as one `provisioning` degraded entry: the
// lookup is why status, machinePools, and the dependent machine
// lookup are all absent, so its fan-out gets one entry and one warning,
// not one per missing key. The third return is the confirmed-absence
// note (non-empty only when provAbsent), scoped to the namespace that was
// searched - derived from provKind.DefaultNamespace, never a second
// hardcoded string. An expired context is the one list failure that does
// NOT degrade: it returns as the error.
func correlateProvisioningCluster(ctx context.Context, mgmt engine.ClusterClient, sec *sections, clusterID string) (*unstructured.Unstructured, provLookup, string, error) {
	provKind, _ := kinds.Lookup("provisioning-cluster")
	list, err := mgmt.List(ctx, provKind.GVR, provKind.DefaultNamespace, "")
	if err != nil {
		return nil, provUnavailable, "", sec.Unavailable(sectionProvisioning, err)
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
		return nil, provAbsent, "no provisioning.cattle.io Cluster for this cluster in " + provKind.DefaultNamespace, nil
	case 1:
		return matches[0], provFound, "", nil
	default:
		ambiguous := clierr.New(clierr.CodeAmbiguousSelection, fmt.Sprintf(
			"%d provisioning.cattle.io Clusters report status.clusterName %q; cannot tell which one belongs to this cluster",
			len(matches), clusterID))
		return nil, provUnavailable, "", sec.Unavailable(sectionProvisioning, ambiguous)
	}
}

// statusFrom reads exactly what provisioning.cattle.io/v1.ClusterStatus
// exposes: ready and conditions (type/status/reason/message). Conditions
// are ALWAYS a non-nil slice so the marshaled envelope carries
// "conditions":[]; no phase/transitioning/error fields - they
// belong to other API representations and never appear on this CR.
//
// The Ready field is derived from the Ready condition when present:
// status.ready can flip true ~90s before the Ready condition leaves Unknown, so
// the condition is authoritative and status.ready is the fallback only when the
// condition is absent.
func statusFrom(prov *unstructured.Unstructured) *clusterStatus {
	ready, _, _ := unstructured.NestedBool(prov.Object, "status", "ready")
	st := &clusterStatus{Ready: ready, Conditions: []clusterCondition{}}
	engine.EachCondition(prov.Object, func(typ, status, reason, message string) bool {
		st.Conditions = append(st.Conditions, clusterCondition{Type: typ, Status: status, Reason: reason, Message: message})
		if typ == "Ready" {
			st.Ready = status == "True"
		}
		return true
	})
	return st
}

// agentFrom reads the management Cluster's Connected and Ready conditions.
// Returns nil when neither exists - a management object without them (the
// e2e facade fixtures, a mid-import cluster) simply omits the section.
// Only an explicit "True"/"False" status sets a field: "Unknown" (the normal
// pre-reconciliation state before the first agent heartbeat) is not a verdict
// and never collapses into disconnected.
func agentFrom(obj *unstructured.Unstructured) *agentStatus {
	var ag *agentStatus
	engine.EachCondition(obj.Object, func(typ, status, reason, message string) bool {
		if typ != "Connected" && typ != "Ready" {
			return true
		}
		if status != "True" && status != "False" {
			return true
		}
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
		return true
	})
	return ag
}

// machinePoolsFrom reads spec.rkeConfig.machinePools: name,
// machineConfigRef kind/name, quantity, and the etcd/controlplane/worker
// roles as a sorted string slice. Always returns a non-nil slice - the
// CALLER's pointer distinguishes "correlated but poolless" (pointer to
// empty) from "section absent" (nil pointer).
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
		pool := machinePool{
			Name:              name,
			MachineConfigKind: kind,
			MachineConfigName: cfgName,
			Quantity:          quantity,
			Roles:             roles,
		}
		if minSize, found, _ := unstructured.NestedInt64(pm, "autoscalingMinSize"); found {
			pool.AutoscalingMinSize = &minSize
		}
		if maxSize, found, _ := unstructured.NestedInt64(pm, "autoscalingMaxSize"); found {
			pool.AutoscalingMaxSize = &maxSize
		}
		pools = append(pools, pool)
	}
	return pools
}

// fetchRegistration reads the default registration token via the
// fetch and applies inspect's OWN placeholder policy over the
// policy-neutral reporter: a placeholder-bearing field is dropped (its
// omitempty tag makes it disappear from the wire) and warned about - an
// advisory warning, since the section is still present and usable. A
// failed read - Root() or the fetch - omits the whole section as a
// `registration` degraded entry, except an expired context, which
// returns as the error.
// When NO surfaced field survives (all five empty after placeholder
// drops, or a server response with every field empty) the section is nil -
// absent from the wire, never "registration":{}. The named-get and
// list-get policies live solely in their respective callers in
// internal/engine/provisioning.
func (c *clusterCmd) fetchRegistration(ctx context.Context, sec *sections, clusterID string) (*registration, []string, error) {
	root, err := c.roots.Root()
	if err != nil {
		return nil, nil, sec.Unavailable(sectionRegistration, err)
	}
	obj, err := provisioning.FetchDefaultCRT(ctx, root, clusterID)
	if err != nil {
		return nil, nil, sec.Unavailable(sectionRegistration, err)
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
	// Withheld, Hint, and NotApplicable are never assigned inside this
	// function - their only write sites are in Execute, after this
	// returns. Zero all three before the DeepEqual so that if a write
	// site is ever added here the emptiness verdict stays anchored to
	// the surfaced string fields rather than silently changing. DeepEqual
	// over == because the []string NotApplicable makes the struct
	// non-comparable; the probe copy keeps future fields in scope.
	probe := *reg
	probe.NotApplicable = nil
	probe.Withheld = false
	probe.Hint = ""
	if reflect.DeepEqual(probe, registration{}) {
		return nil, warnings, nil
	}
	return reg, warnings, nil
}

// clusterMembers lists the cluster's crtbs (namespace = cluster id) and
// enriches them via rbac.ResolveMembers. nil = binding LIST failed (field
// omitted, text renders unavailable, one `members` degraded entry);
// non-nil empty = verified no members. Display-name enrichment failures
// stay advisory: the rows are there, only their names are missing.
func clusterMembers(ctx context.Context, mgmt engine.ClusterClient, sec *sections, clusterID string) (*[]rbac.MemberRow, []string, error) {
	crtbKind, _ := kinds.Lookup("clusterroletemplatebinding")
	list, err := mgmt.List(ctx, crtbKind.GVR, clusterID, "")
	if err != nil {
		return nil, nil, sec.Unavailable(sectionMembers, err)
	}
	rows, warnings, err := rbac.ResolveMembers(ctx, mgmt, list.Items)
	if err != nil {
		return nil, nil, err
	}
	return &rows, warnings, nil
}

// clusterMachines lists the provisioning cluster's CAPI Machines in
// fleet-default filtered by the cluster.x-k8s.io/cluster-name label. nil =
// LIST failed (field omitted from JSON, text renders unavailable); non-nil
// empty = verified no machines. Runs only when the provisioning cluster was
// found (the caller is responsible for the provFound guard).
func clusterMachines(ctx context.Context, mgmt engine.ClusterClient, sec *sections, provName string) (*[]clusterMachine, error) {
	machineKind, _ := kinds.Lookup("capimachine")
	sel := capiClusterNameLabel + "=" + provName
	list, err := mgmt.List(ctx, machineKind.GVR, machineKind.DefaultNamespace, sel)
	if err != nil {
		return nil, sec.Unavailable(sectionMachines, err)
	}
	rows := make([]clusterMachine, 0, len(list.Items))
	for i := range list.Items {
		name := list.Items[i].GetName()
		phase, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "phase")
		errMsg := machineErrorMessage(list.Items[i].Object)
		rows = append(rows, clusterMachine{Name: name, Phase: phase, Error: errMsg})
	}
	return &rows, nil
}

// machineErrorMessage extracts the most relevant failing condition message
// from a CAPI Machine object. Conditions whose reason contains "Error" (e.g.
// CreateError from a DigitalOcean 422 response) are preferred; any
// other non-True condition with a non-empty message is the fallback.
func machineErrorMessage(obj map[string]any) string {
	var errCond, anyCond string
	engine.EachCondition(obj, func(typ, status, reason, message string) bool {
		if status == "True" || message == "" {
			return true
		}
		if anyCond == "" {
			anyCond = message
		}
		if strings.Contains(reason, "Error") && errCond == "" {
			errCond = message
		}
		return true
	})
	if errCond != "" {
		return errCond
	}
	return anyCond
}

// clusterResult is `inspect cluster`'s payload - the JSON shape IS the agent
// contract. Pointer semantics carry the tri-state: nil = section absent (never
// null/{}), a nil *[]machinePool = absent while a pointer to an empty slice
// marshals "machinePools":[] (Go's omitempty drops non-nil empty slices, so a
// plain slice cannot distinguish them). ProvisioningNote is RESERVED for
// confirmed absence, at the root.
type clusterResult struct {
	Cluster          clusterIdentity   `json:"cluster"`
	Agent            *agentStatus      `json:"agent,omitempty"`
	ProvisioningNote string            `json:"provisioningNote,omitempty"`
	Status           *clusterStatus    `json:"status,omitempty"`
	MachinePools     *[]machinePool    `json:"machinePools,omitempty"`
	Members          *[]rbac.MemberRow `json:"members,omitempty"`
	Machines         *[]clusterMachine `json:"machines,omitempty"`
	Registration     *registration     `json:"registration,omitempty"`
	// Degraded names the collectors this run could not read in full. Absent
	// when every collector succeeded.
	Degraded []sectionOutcome `json:"degraded,omitempty"`
}

// agentStatus is the MANAGEMENT Cluster's agent-connectivity view - the
// Connected and Ready conditions the provisioning-derived status never
// carries (status.ready can stay true through a real agent
// outage). Tri-state per the clusterResult pointer convention: the section
// is nil when neither condition exists; each field is nil when its own
// condition is absent - nothing is fabricated. Message carries the first
// failing condition's message, preferring Connected's.
type agentStatus struct {
	Connected *bool  `json:"connected,omitempty"`
	Ready     *bool  `json:"ready,omitempty"`
	Message   string `json:"message,omitempty"`
}

type agentVerdict int

const (
	agentNoVerdict agentVerdict = iota
	agentDisconnected
	agentConnectedNotReady
	agentNotReady
	agentConnected
)

// verdict collapses the reported Connected/Ready pointers into the single
// priority rule shared by the warning gate and the text renderer:
// disconnected beats not-ready beats connected; no explicit verdicts, no
// classification.
func (ag *agentStatus) verdict() agentVerdict {
	switch {
	case ag == nil:
		return agentNoVerdict
	case ag.Connected != nil && !*ag.Connected:
		return agentDisconnected
	case ag.Connected != nil && ag.Ready != nil && !*ag.Ready:
		return agentConnectedNotReady
	case ag.Connected != nil:
		return agentConnected
	case ag.Ready != nil && !*ag.Ready:
		return agentNotReady
	default:
		return agentNoVerdict
	}
}

type clusterMachine struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	// Error is the first failing condition message (preferring conditions
	// whose reason contains "Error"); omitted when no failing condition
	// carries a message.
	Error string `json:"error,omitempty"`
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
	// "conditions":[] even when the CR reports none.
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
	// AutoscalingMinSize/MaxSize are nil on a manual pool (both absent
	// server-side) and both set together on an autoscaled one - the CRD's
	// write-side CEL requires both or neither. On an autoscaled pool,
	// Quantity is the autoscaler's current count, not operator intent: the
	// autoscaler re-asserts its own count within its loop, and the
	// provisioning webhook syncs that choice back into
	// machinePools[].quantity, so the field stays live even though nothing
	// here calls `cluster scale`.
	AutoscalingMinSize *int64 `json:"autoscalingMinSize,omitempty"`
	AutoscalingMaxSize *int64 `json:"autoscalingMaxSize,omitempty"`
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

// withheldMessage explains why the registration.hint field carries no
// fields when --registration is absent: rerunning with --registration is
// the alternative to withheldHint's token lookup.
func withheldMessage() string {
	return "registration was left out because it contains the cluster registration token; rerun with --registration to include it"
}

// withheldHint builds the machine-readable rerun suggestion carried by the
// JSON registration.hint field when --registration is absent: a single
// paste-valid command, never prose. The default registration token lives at
// default-token in the cluster's own namespace (namespace = management
// cluster id), so the concrete get command can be rendered with the values
// Execute already holds. Tests assert an exact match so any wording change
// fails loudly. The hint is a remedy command, so the json and yaml output name
// the invocation's config directory in it.
func withheldHint(clusterID string) string {
	return clierr.Rancher + " get clusterregistrationtoken default-token --namespace " + clierr.ShellQuote(clusterID)
}

// registration carries BOTH workflow pairs explicitly - the node pair
// (custom-cluster join) and the import pair (imported-cluster manifest) -
// plus manifestUrl; one is never substituted for the other. Every field
// is individually omitempty so a placeholder-bearing or empty field
// DISAPPEARS from the wire rather than marshaling "".
//
// When --registration is absent the struct carries only Withheld=true,
// Message, and Hint, serializing as
// {"withheld":true,"message":"...","hint":"..."} - the same keys in the
// envelope, a shape that is distinct from both the live-data case (string
// fields present) and the unavailable case (key absent). Text rendering checks
// Withheld before rendering fields.
type registration struct {
	NodeCommand         string `json:"nodeCommand,omitempty"`
	InsecureNodeCommand string `json:"insecureNodeCommand,omitempty"`
	Command             string `json:"command,omitempty"`
	InsecureCommand     string `json:"insecureCommand,omitempty"`
	ManifestURL         string `json:"manifestUrl,omitempty"`
	// NotApplicable names registration fields that are present but do not
	// apply to this cluster's creation mode: an imported cluster
	// (provisioning spec.rkeConfig absent) has no node command workflow:
	// nodeCommand/insecureNodeCommand are annotated rather than dropped -
	// the JSON contract is additive and consumers may rely on field presence.
	// Never names a field the payload does not carry.
	NotApplicable []string `json:"notApplicable,omitempty"`
	// Withheld is true when --registration was absent. Message explains why
	// the section carries no fields; Hint carries the single paste-valid
	// rerun command. All three are omitempty so they are absent from the
	// live-data payload.
	Withheld bool   `json:"withheld,omitempty"`
	Message  string `json:"message,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// The registration field names shared by the notApplicable writer
// (markNodeCommandsInapplicable) and the text renderer - one literal,
// no silent typo drift between the two sides.
const (
	regFieldNodeCommand         = "nodeCommand"
	regFieldInsecureNodeCommand = "insecureNodeCommand"
)

// markNodeCommandsInapplicable annotates the node command pair as not
// applicable (imported cluster). A placeholder-dropped or server-empty
// command stays unannotated - the list never names an absent field.
func (r *registration) markNodeCommandsInapplicable() {
	if r.NodeCommand != "" {
		r.NotApplicable = append(r.NotApplicable, regFieldNodeCommand)
	}
	if r.InsecureNodeCommand != "" {
		r.NotApplicable = append(r.NotApplicable, regFieldInsecureNodeCommand)
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

// renderClusterText is the default rendering: a deliberate
// multi-section text view (a summary row cannot carry conditions, pools,
// and join commands). Omitted sections render a CONCISE availability
// marker in place; the detailed cause lives only in Result.Warnings,
// which the runtime prints to stderr - never duplicated here.
func renderClusterText(r clusterResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CLUSTER\n  id: %s\n  name: %s\n  kubernetesVersion: %s\n  state: %s\n",
		r.Cluster.ID, output.Printable(r.Cluster.Name), output.Printable(r.Cluster.KubernetesVersion), output.Printable(r.Cluster.State))
	renderAgentLine(&b, r.Agent)
	renderConditionsSection(&b, r)
	renderMachinePoolsSection(&b, r)
	renderMembersSection(&b, r)
	renderMachinesSection(&b, r)
	// Registration values render FULL-WIDTH and are never truncated: they
	// are meant to be copy-pasted.
	b.WriteString("\nREGISTRATION\n")
	switch {
	case r.Registration != nil && r.Registration.Withheld:
		b.WriteString("  registration: left out (contains the cluster registration token) - rerun with --registration\n")
	case r.Registration == nil:
		b.WriteString("  registration: unavailable\n")
	default:
		writeRegistrationField(&b, regLabel("node command", slices.Contains(r.Registration.NotApplicable, regFieldNodeCommand)), r.Registration.NodeCommand)
		writeRegistrationField(&b, regLabel("node command (insecure)", slices.Contains(r.Registration.NotApplicable, regFieldInsecureNodeCommand)), r.Registration.InsecureNodeCommand)
		writeRegistrationField(&b, "import command", r.Registration.Command)
		writeRegistrationField(&b, "import command (insecure)", r.Registration.InsecureCommand)
		writeRegistrationField(&b, "manifestUrl", r.Registration.ManifestURL)
	}
	return b.String()
}

// clusterSectionHeaders is the fixed, always-present section header set
// renderClusterText emits - every one of them, every call, regardless of
// which sub-objects came back unavailable.
var clusterSectionHeaders = map[string]bool{
	"CLUSTER": true, "CONDITIONS": true, "MACHINE POOLS": true,
	"MEMBERS": true, "MACHINES": true, "REGISTRATION": true,
}

// styleClusterText applies inspect cluster's bold spans to text, the plain
// rendering renderClusterText already produced: every section header, bold,
// whole line. Nothing inside a section is styled.
func styleClusterText(text string, s style.Style) string {
	return runtime.BoldExactLines(text, clusterSectionHeaders, s)
}

// renderAgentLine appends the agent-connectivity line to the CLUSTER
// section. Each clause renders only what the server explicitly reported:
// no Connected/Ready verdicts, no line. Ready=True alone renders nothing -
// "ready" without a connectivity verdict would overstate the report.
func renderAgentLine(b *strings.Builder, ag *agentStatus) {
	if ag == nil {
		return
	}
	msg := ""
	if ag.Message != "" {
		msg = " - " + truncateRunes(output.Printable(ag.Message), conditionMessageLimit)
	}
	switch ag.verdict() {
	case agentDisconnected:
		b.WriteString("  agent: not connected" + msg + "\n")
	case agentConnectedNotReady:
		b.WriteString("  agent: connected, cluster not ready" + msg + "\n")
	case agentConnected:
		b.WriteString("  agent: connected\n")
	case agentNotReady:
		b.WriteString("  agent: connection not reported, cluster not ready" + msg + "\n")
	}
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
			line := fmt.Sprintf("  %s=%s", output.Printable(c.Type), output.Printable(c.Status))
			if c.Reason != "" {
				line += " reason=" + output.Printable(c.Reason)
			}
			if c.Message != "" {
				line += " " + truncateRunes(output.Printable(c.Message), conditionMessageLimit)
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
			line := fmt.Sprintf("  %s: %s/%s quantity=%d roles=%s",
				output.Printable(p.Name), output.Printable(p.MachineConfigKind), output.Printable(p.MachineConfigName), p.Quantity, strings.Join(p.Roles, ","))
			if p.AutoscalingMinSize != nil && p.AutoscalingMaxSize != nil {
				line += fmt.Sprintf(" autoscaling=%d-%d (quantity is the autoscaler's current count)",
					*p.AutoscalingMinSize, *p.AutoscalingMaxSize)
			}
			b.WriteString(line + "\n")
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
				fmt.Fprintf(b, "  %s (%s)  %s%s\n", output.Printable(m.DisplayName), output.Printable(m.Principal), output.Printable(m.Role), suffix)
			} else {
				fmt.Fprintf(b, "  %s  %s%s\n", output.Printable(m.Principal), output.Printable(m.Role), suffix)
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
		// provisioning CR means imported or custom - no CAPI Machines is
		// the expected shape, not a gap.
		b.WriteString("  none (imported or custom cluster)\n")
	case len(*r.Machines) == 0:
		b.WriteString("  none\n")
	default:
		for _, m := range *r.Machines {
			// Name and phase are server-sourced strings too - same
			// sanitizer as the error message and the members section.
			fmt.Fprintf(b, "  %s  %s\n", output.Printable(m.Name), output.Printable(m.Phase))
			if m.Error != "" {
				fmt.Fprintf(b, "    %s\n", output.Printable(m.Error))
			}
		}
	}
}

func writeRegistrationField(b *strings.Builder, label, value string) {
	if value == "" {
		return // dropped or server-empty: absent in place; detail is in Warnings
	}
	fmt.Fprintf(b, "  %s:\n    %s\n", label, output.Printable(value))
}

// orderedConditions is the one place inspect cluster derives a trouble
// ordering, and only in the text view: conditions with status "False" whose
// reason or message is non-empty list first; everything else keeps its
// server order. The JSON envelope always carries the raw order.
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
