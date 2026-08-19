// Package inspect holds the read composites (`inspect pod`, `inspect
// deployment`, `images`, `usage`) — bespoke ReadCommands that join
// several cluster reads into one document. Metrics and log fetch failures
// degrade to Result.Warnings; only core object fetch failures are command
// errors.
package inspect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A composite runs several collectors per invocation, and a caller
// authorized for some but not all of them is the normal state for a
// non-admin. Required collectors (identity, scope, substance — declared per
// command and mode) keep full error identity and hard-error. Every other
// collector degrades: its section is recorded here, the payload keeps its
// existing null/omitted/empty convention, and the command still exits 0.
//
// The closed section vocabulary below names COLLECTORS, not JSON keys:
// metrics and ownerResolution collect into row-shaped payloads that have no
// such key. Nothing outside this list may be emitted.
const (
	sectionClusters        = "clusters"
	sectionLogs            = "logs"
	sectionMachines        = "machines"
	sectionMembers         = "members"
	sectionMetrics         = "metrics"
	sectionNamespaces      = "namespaces"
	sectionOwnerResolution = "ownerResolution"
	sectionOwners          = "owners"
	sectionPods            = "pods"
	sectionProvisioning    = "provisioning"
	sectionRegistration    = "registration"
	sectionUsage           = "usage"
)

// stateUnavailable means the collector produced nothing; statePartial means
// data is present but one or more items or enrichments failed.
const (
	stateUnavailable = "unavailable"
	statePartial     = "partial"
)

// targetRule is the target format a section's entries must carry. A section
// is either whole (no target, always unavailable) or per-item (a target,
// always partial) — the matrix declares which, and mixing the two would let
// one section report contradictory states.
type targetRule int

const (
	targetNone targetRule = iota
	targetToken
	targetOwner
)

// sectionTargetRules is the complete vocabulary with each section's target
// format. A command registers the subset its matrix row allows.
var sectionTargetRules = map[string]targetRule{
	sectionClusters:        targetToken,
	sectionLogs:            targetToken,
	sectionMachines:        targetNone,
	sectionMembers:         targetNone,
	sectionMetrics:         targetNone,
	sectionNamespaces:      targetNone,
	sectionOwnerResolution: targetOwner,
	sectionOwners:          targetOwner,
	sectionPods:            targetNone,
	sectionProvisioning:    targetNone,
	sectionRegistration:    targetNone,
	sectionUsage:           targetNone,
}

// sectionOutcome is one entry of a composite payload's additive `degraded`
// field. Section and target are separate fields, so no qualified-identifier
// escaping exists at this level.
type sectionOutcome struct {
	Section string `json:"section"`
	State   string `json:"state"`
	Code    string `json:"code"`
	Target  string `json:"target,omitempty"`
}

// sectionRecord keeps an outcome and its warning together so the pairing
// survives sorting: every entry has exactly one warning and vice versa.
type sectionRecord struct {
	outcome sectionOutcome
	warning string
}

// sections is the one degradation accumulator every composite uses. It
// applies the cancellation gate, extracts and validates the clierr code,
// builds the paired warning, and enforces the closed vocabulary, the
// per-section target format, and the no-duplicate-(section,target) rule.
// The mutex covers `images --all-clusters`, whose per-cluster scans run
// concurrently.
type sections struct {
	ctx     context.Context
	allowed map[string]targetRule

	mu      sync.Mutex
	records []sectionRecord
	seen    map[[2]string]bool
}

// newSections registers a command's allowed section vocabulary. A name
// outside sectionTargetRules is a programmer error.
func newSections(ctx context.Context, names ...string) *sections {
	allowed := make(map[string]targetRule, len(names))
	for _, n := range names {
		rule, ok := sectionTargetRules[n]
		if !ok {
			panic("inspect: unknown section " + n)
		}
		allowed[n] = rule
	}
	return &sections{ctx: ctx, allowed: allowed, seen: map[[2]string]bool{}}
}

// Unavailable records a whole section that produced nothing. A non-nil
// return is the caller's error to propagate: the collector was aborted by
// the CALLER's cancellation, which is never a degradation.
func (s *sections) Unavailable(section string, err error) error {
	return s.record(section, stateUnavailable, "", err,
		section+" unavailable: "+err.Error())
}

// Partial records one failed item or enrichment inside a section that still
// carries data. Same cancellation contract as Unavailable. A token target is
// one server-derived string (a container name, a cluster id) and is escaped
// here rather than trusted; an owner target arrives already escaped
// component-wise from ownerTarget.
func (s *sections) Partial(section, target string, err error) error {
	if sectionTargetRules[section] == targetToken {
		target = escapeTargetComponent(target)
	}
	return s.record(section, statePartial, target, err,
		section+" unavailable for "+target+": "+err.Error())
}

// Blocked records a DEPENDENT section's degradation: blockedBy names the
// input the section could not be computed without, while code and
// cancellation still come from the ORIGINAL error. Dependency context is a
// separate parameter so callers never pre-wrap the error.
func (s *sections) Blocked(section, blockedBy string, err error) error {
	return s.record(section, stateUnavailable, "", err,
		fmt.Sprintf("%s unavailable: %s unknown (%s)", section, blockedBy, err.Error()))
}

func (s *sections) record(section, state, target string, err error, warning string) error {
	if lookupCanceled(s.ctx, err) {
		return err
	}
	rule, ok := s.allowed[section]
	if !ok {
		panic("inspect: section " + section + " is not in this command's vocabulary")
	}
	validateTarget(section, state, target, rule)

	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{section, target}
	if s.seen[key] {
		panic("inspect: duplicate degraded entry for section " + section + " target " + target)
	}
	s.seen[key] = true
	s.records = append(s.records, sectionRecord{
		outcome: sectionOutcome{Section: section, State: state, Code: degradedCode(err), Target: target},
		warning: warning,
	})
	return nil
}

// validateTarget enforces the per-section target format. Every violation is
// a programmer error: the vocabulary, the states, and the target grammars
// are all fixed in code, and no server value can reach them.
func validateTarget(section, state, target string, rule targetRule) {
	switch rule {
	case targetNone:
		if state != stateUnavailable {
			panic("inspect: section " + section + " has no target and cannot be " + state)
		}
		if target != "" {
			panic("inspect: section " + section + " takes no target")
		}
	case targetToken:
		if state != statePartial {
			panic("inspect: section " + section + " is per-item and cannot be " + state)
		}
		if target == "" {
			panic("inspect: section " + section + " requires a target")
		}
	case targetOwner:
		if state != statePartial {
			panic("inspect: section " + section + " is per-item and cannot be " + state)
		}
		if _, ok := splitOwnerTarget(target); !ok {
			panic("inspect: section " + section + " target is not a canonical owner identity")
		}
	}
}

// knownCodes is AllCodes() as a set, built once: degradedCode runs on every
// degraded entry (per denied owner in a fleet-wide images scan), and
// AllCodes() allocates a fresh slice per call.
var knownCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range clierr.AllCodes() {
		m[c] = true
	}
	return m
}()

// degradedCode is the entry's code: the collector error's clierr code when
// it carries a known one, GENERIC otherwise.
func degradedCode(err error) string {
	var ce *clierr.Error
	if errors.As(err, &ce) && knownCodes[ce.Code] {
		return ce.Code
	}
	return clierr.CodeGeneric
}

// Finalize yields the payload's `degraded` entries and their warnings from
// one source, sorted by (section, target). Entries and warnings stay index
// aligned, which is the parity the tests assert.
func (s *sections) Finalize() ([]sectionOutcome, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return nil, nil
	}
	recs := slices.Clone(s.records)
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i].outcome, recs[j].outcome
		if a.Section != b.Section {
			return a.Section < b.Section
		}
		return a.Target < b.Target
	})
	entries := make([]sectionOutcome, 0, len(recs))
	warnings := make([]string, 0, len(recs))
	for _, r := range recs {
		entries = append(entries, r.outcome)
		warnings = append(warnings, r.warning)
	}
	return entries, warnings
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

// ownerTargetSep joins canonical owner-target components. No component
// grammar admits it, while apiVersion carries "/" (apps/v1), so a
// slash-joined target would be ambiguous.
const ownerTargetSep = "|"

// escapeTargetComponent makes one server-derived string safe to carry in a
// target. Kubernetes' own grammars admit neither the delimiter nor the
// escape character, but this CLI reads through a proxy and never validated
// the objects it walks, so a value that breaks the grammar is escaped rather
// than trusted or fatal. Escaping the escape character keeps the mapping
// one-to-one, so two components that differ still produce distinct targets.
func escapeTargetComponent(s string) string {
	if !strings.ContainsAny(s, "%"+ownerTargetSep) {
		return s
	}
	s = strings.ReplaceAll(s, "%", "%25")
	return strings.ReplaceAll(s, ownerTargetSep, "%7C")
}

// ownerTargetParts is a canonical owner target decomposed. ClusterID is
// empty for single-cluster targets.
type ownerTargetParts struct {
	ClusterID  string
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        string
}

// ownerTarget builds the canonical owner identity shared by inspect pod and
// both images modes: <apiVersion>|<kind>|<ns>|<name>|<uid>, prefixed with
// <clusterID>| in all-clusters mode. API version and UID are identity
// fields in owner resolution — the same ns/kind/name in another group, or a
// recreated generation, is a DIFFERENT owner.
// Every component is server data, so each is escaped before the join.
func ownerTarget(clusterID, ns string, ref ownerRef) string {
	parts := []string{ref.apiVersion, ref.kind, ns, ref.name, ref.uid}
	if clusterID != "" {
		parts = append([]string{clusterID}, parts...)
	}
	for i, p := range parts {
		parts[i] = escapeTargetComponent(p)
	}
	return strings.Join(parts, ownerTargetSep)
}

// splitOwnerTarget decomposes a canonical owner target, reporting false for
// anything that is not one. It is the validator's grammar check and the
// round-trip half of ownerTarget.
func splitOwnerTarget(target string) (ownerTargetParts, bool) {
	fields := strings.Split(target, ownerTargetSep)
	switch len(fields) {
	case 5:
		return ownerTargetParts{
			APIVersion: fields[0], Kind: fields[1],
			Namespace: fields[2], Name: fields[3], UID: fields[4],
		}, true
	case 6:
		return ownerTargetParts{
			ClusterID: fields[0], APIVersion: fields[1], Kind: fields[2],
			Namespace: fields[3], Name: fields[4], UID: fields[5],
		}, true
	default:
		return ownerTargetParts{}, false
	}
}

// rejectProject enforces PC-3 for the composites that bypass Route
// (inspect pod/deployment, images): a TYPED --project flag would otherwise
// reach them and be silently ignored, exactly the class of no-op DR-004
// removed --project for. cmd is the command name for the message
// ("inspect pod", "images", ...).
func rejectProject(in runtime.Input, cmd string) error {
	if resources.TypedProject(in) == "" {
		return nil
	}
	return clierr.New(clierr.CodeValidation, "--project does not apply to "+cmd).
		WithHint("drop --project for this command")
}

// podMetricsGVR is a metrics GVR constant — deliberately not a kind-table
// row: the metrics pseudo-kinds are not user-addressable nouns, only fetched
// internally here.
var podMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// nodeMetricsGVR is the metrics.k8s.io sibling of podMetricsGVR. Consumed by
// the node grouping collector in usage_node.go.
var nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

// ownerChainMaxDepth caps the owner walk (pod -> ReplicaSet -> Deployment is
// depth 2; the extra headroom covers StatefulSet/DaemonSet/Job shapes without
// risking an unbounded walk on a cyclic or malformed ownerRef chain).
const ownerChainMaxDepth = 3

// recursableOwnerKinds are the controller kinds worth walking one level
// further up (RS -> Deployment, being the only multi-hop case today).
var recursableOwnerKinds = map[string]bool{
	"ReplicaSet":  true,
	"StatefulSet": true,
	"DaemonSet":   true,
	"Job":         true,
}

// ownerChain walks obj's controller ownerRef chain, fetching each recursable
// owner in turn. Unknown owner kinds (no kind-table row, or not one of the
// recursable controller kinds) are reported as-is and end the walk — never an
// error, per the spec's risk note on custom controllers. A fetch failure
// keeps the ref info and records a partial outcome under section instead of
// aborting the walk; clusterID prefixes the owner target in all-clusters
// mode and is empty otherwise. The bool reports whether the walk degraded —
// callers that treat a partially-walked chain as unverified need it. The
// error is the caller's cancellation, to propagate.
func ownerChain(ctx context.Context, client engine.ClusterClient, sec *sections, section, clusterID, ns string, obj *unstructured.Unstructured) ([]map[string]any, bool, error) {
	var owners []map[string]any
	current := obj
	for range ownerChainMaxDepth {
		ref, ok := controllerRef(current)
		if !ok {
			break
		}
		owners = append(owners, map[string]any{
			"kind": ref.kind, "name": ref.name,
			"apiVersion": ref.apiVersion, "uid": ref.uid,
		})
		if !recursableOwnerKinds[ref.kind] {
			break
		}
		k, found := kinds.LookupGVK(ref.apiVersion, ref.kind)
		if !found {
			break
		}
		var walkErr error
		if ref.uid == "" {
			walkErr = errors.New("owner reference has no uid")
		} else {
			next, err := client.Get(ctx, k.GVR, ns, ref.name)
			switch {
			case err != nil:
				walkErr = err
			case string(next.GetUID()) != ref.uid:
				walkErr = errors.New("uid mismatch (stale owner reference)")
			default:
				current = next
			}
		}
		if walkErr != nil {
			if herr := sec.Partial(section, ownerTarget(clusterID, ns, ref), walkErr); herr != nil {
				return owners, true, herr
			}
			return owners, true, nil
		}
	}
	return owners, false, nil
}

type ownerRef struct{ apiVersion, kind, name, uid string }

// controllerRef returns the controller ownerRef (controller: true), falling
// back to the first ownerRef when none is marked controller.
func controllerRef(obj *unstructured.Unstructured) (ownerRef, bool) {
	refs, _, _ := unstructured.NestedSlice(obj.Object, "metadata", "ownerReferences")
	if len(refs) == 0 {
		return ownerRef{}, false
	}
	first, fOK := asOwnerRef(refs[0])
	for _, r := range refs {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if isController, _, _ := unstructured.NestedBool(m, "controller"); isController {
			return asOwnerRef(r)
		}
	}
	return first, fOK
}

func asOwnerRef(r any) (ownerRef, bool) {
	m, ok := r.(map[string]any)
	if !ok {
		return ownerRef{}, false
	}
	kind, _ := m["kind"].(string)
	name, _ := m["name"].(string)
	apiVersion, _ := m["apiVersion"].(string)
	uid, _ := m["uid"].(string)
	if kind == "" || name == "" {
		return ownerRef{}, false
	}
	return ownerRef{apiVersion: apiVersion, kind: kind, name: name, uid: uid}, true
}

// kindGroupName renders kubectl's -o name form: <kind>.<group>/<name>,
// with core-group kinds bare (pod/web-1). Kind is lowercased.
func kindGroupName(apiVersion, kind, name string) string {
	group := ""
	if i := strings.Index(apiVersion, "/"); i >= 0 {
		group = apiVersion[:i]
	}
	k := strings.ToLower(kind)
	if group != "" {
		k += "." + group
	}
	return k + "/" + name
}

// degradedContractDesc is the schema-desc sentence every composite carries
// verbatim — one source, so the documented degraded contract cannot drift
// per command when the payload shape evolves.
const degradedContractDesc = "Partial access degrades rather than fails: a collector this caller cannot read is named in data.degraded ({section, state, code, target}) with one paired warning each, and the command still exits 0. "
