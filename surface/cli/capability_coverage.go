package cli

import (
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// capabilityCoverageExtraOps is the reviewed, in-test kind mapping for op
// families no other wiring source encodes: create machine-config backs
// every rke-machine-config row through one --kind-enum command; grant and
// revoke back both binding rows (they operate at cluster or project scope);
// the inspect composites map to their declared PRIMARY target kind only -
// incidental follow-up reads (inspect's owner-reference chain) stay outside
// the gate deliberately. Adding a family here is a reviewed
// edit, same as kindOverrideTable or derivedOpKinds.
var capabilityCoverageExtraOps = map[string][]string{
	"create machine-config": {"amazonec2config", "azureconfig", "digitaloceanconfig", "harvesterconfig", "vmwarevsphereconfig"},
	"grant":                 {"clusterroletemplatebinding", "projectroletemplatebinding"},
	"revoke":                {"clusterroletemplatebinding", "projectroletemplatebinding"},
	"inspect cluster":       {"managementcluster"},
	"inspect pod":           {"pod"},
	"inspect deployment":    {"deployment"},
	"inspect project":       {"project"},
	"inspect user":          {"user"},
	// create user touches the user kind row directly (the composite's User
	// half) alongside its own ext.cattle.io-discovery Capability
	// requirement - mapped here so a future Capability gate on the user
	// kind itself would also flag this op for review, same as inspect user.
	"create user": {"user"},
}

// capabilityCoverageAllowlist is the closed, reviewed set of registered ops
// with no kind linkage at all: auth/config/local ops, and kind-adjacent ops
// deliberately classified as outside the gate. A new op present in neither a
// kind mapping nor this allowlist fails TestCapabilityCoverageRealWiring
// with a "classify this op" message - silence is impossible, the property
// the retired tripwire (TestNoKindDeclaresCapabilityUntilCreateGatingLands)
// had.
var capabilityCoverageAllowlist = map[string]string{
	"access can":                     "posts a SubjectAccessReview through the proxy - a review is created, never listed or deleted, so it carries no kind row",
	"access list":                    "posts a SelfSubjectRulesReview through the proxy - a review is created, never listed or deleted, so it carries no kind row",
	"access who-can":                 "posts one SubjectAccessReview per binding subject through the proxy - reviews are created, never listed or deleted, so it carries no kind row",
	"access explain":                 "posts one review and reads bindings, roles, and management objects the caller may already read - nothing is created, so it carries no kind row",
	"access clusters":                "reads the steve cluster collection and posts one SelfSubjectReview per cluster - reads and reviews only, so it carries no kind row",
	"access projects":                "reads the steve cluster, project and namespace collections and posts one SelfSubjectAccessReview per project namespace - reads and reviews only, so it carries no kind row",
	"auth login":                     "local auth, no kind row",
	"auth logout":                    "local auth, no kind row",
	"auth status":                    "local auth, no kind row",
	"auth get-token":                 "local auth, no kind row",
	"auth list-tokens":               "local auth, no kind row",
	"auth delete-token":              "local auth, no kind row",
	"token":                          "legacy top-level token shim (bare), no kind row",
	"config add-server":              "local config, no kind row",
	"config update-server":           "local config, no kind row",
	"config delete-server":           "local config, no kind row",
	"config edit":                    "local config, no kind row",
	"config list-servers":            "local config, no kind row",
	"config path":                    "local config, no kind row",
	"config set-completion":          "local config, no kind row",
	"config set-token-reuse":         "local config, no kind row",
	"config status":                  "local config, no kind row",
	"config use-server":              "local config, no kind row",
	"skill install":                  "agent-skill install, no kind row",
	"skill status":                   "agent-skill status, no kind row",
	"skill uninstall":                "agent-skill uninstall, no kind row",
	"schema":                         "fixed introspection command, no kind row",
	"completion":                     "fixed shell-completion command, no kind row",
	"get cloud-credential":           "bespoke discovery read, no kind row",
	"get kubernetes-versions":        "bespoke discovery read, no kind row",
	"images":                         "inspect-adjacent discovery read, no kind row",
	"kubeconfig":                     "top-level kubeconfig read (session identity, not a kind row)",
	"logs":                           "bespoke log read, no kind row",
	"usage":                          "bespoke discovery read, no kind row",
	"support-bundle":                 "diagnostic bundle collector, no kind row",
	"create password-change-request": "ext.cattle.io PasswordChangeRequest is create-only (no get/list/delete verb), so it carries no kind row - its own ext.cattle.io-discovery Capability requirement is declared directly on the command",
}

// registeredCommand is the minimal shape the coverage rule needs from a
// registered command: whether its Descriptor carries a static requirement
// and whether it implements runtime.CapabilityResolver. Passed in as data
// (never constructed by the rule itself) so the rule stays a pure function,
// testable with synthetic rows the closed-world kinds table cannot host.
type registeredCommand struct {
	capability runtime.CapabilityRequirement
	resolver   bool
}

// capabilityCoverageInput bundles every source the coverage rule consults.
// rows/overrides/derivedOps/registered are the real wiring for
// TestCapabilityCoverageRealWiring; unit tests substitute synthetic values
// for every field independently.
type capabilityCoverageInput struct {
	rows       []kinds.Kind
	overrides  map[string]kindOverride
	derivedOps map[string]string   // op -> kind key, single-backing-row bespoke ops
	extraOps   map[string][]string // op -> kind keys, families no wiring source encodes
	// createOp, when non-empty, is the create-via-manifest op name: it is
	// checked against EVERY row in rows except one carrying
	// NoCreateViaManifest, which is exempt on this edge (the same
	// structural-refusal class as an override's refusals entry).
	createOp   string
	allowlist  map[string]string
	registered map[string]registeredCommand
}

// capabilityCoverageResult is what checkCapabilityCoverage reports. All
// three fields are conformance failures - every field must be empty for the
// rule to pass.
type capabilityCoverageResult struct {
	// uncoveredGated lists an op that targets a Capability-bearing kind
	// through a command that neither carries the requirement in its static
	// Descriptor nor implements CapabilityResolver.
	uncoveredGated []string
	// unclassified lists a registered op present in neither a kind mapping
	// nor the allowlist.
	unclassified []string
	// danglingRefusals lists an override's refusals entry with no
	// corresponding constructor - a refusal marker that cannot fire.
	danglingRefusals []string
	// unknownKinds lists a mapping entry (override, derived op, extra op)
	// whose kind key resolves to no row - a stale or typo'd key. Without
	// this, checkOne would mark the op covered and silently skip the row
	// lookup, so a later Capability-bearing kind reachable only through
	// that op would ship ungated while the rule stayed green - the exact
	// silence the rule exists to make impossible.
	unknownKinds []string
}

func (r capabilityCoverageResult) clean() bool {
	return len(r.uncoveredGated) == 0 && len(r.unclassified) == 0 &&
		len(r.danglingRefusals) == 0 && len(r.unknownKinds) == 0
}

// overrideVerbConstructor returns the constructor kindOverride wires for
// verb ("get", "patch", "delete"), or nil.
func overrideVerbConstructor(ov kindOverride, verb string) func(kinds.Kind, runDeps) runtime.Command {
	switch verb {
	case "get":
		return ov.get
	case "patch":
		return ov.patch
	case "delete":
		return ov.del
	default:
		return nil
	}
}

// checkCapabilityCoverage is the coverage rule's pure function: given every
// mapping source and the registered commands' shapes, it reports every
// Capability-bearing kind reachable through an uncovered command, every
// registered op neither mapped to a kind nor allowlisted, and every
// dangling refusal marker. It never constructs a command - registered
// carries the shape (capability, resolver) the caller already extracted,
// which is what lets the rule run identically against synthetic rows and
// the real registry.
func checkCapabilityCoverage(in capabilityCoverageInput) capabilityCoverageResult {
	rowByKey := make(map[string]kinds.Kind, len(in.rows))
	for _, k := range in.rows {
		rowByKey[k.Key] = k
	}

	// refusedOps: op names a kindOverride marks a structural refusal for -
	// exempt from the Capability-carrying requirement (the refusal declines
	// before any network access, the same rationale as NoCreateViaManifest's
	// zero-return) but still classified.
	refusedOps := map[string]bool{}
	var res capabilityCoverageResult
	for key, ov := range in.overrides {
		row, ok := rowByKey[key]
		if !ok {
			res.unknownKinds = append(res.unknownKinds,
				fmt.Sprintf("override %q: no kind row with that key", key))
			continue
		}
		for _, verb := range []string{"get", "patch", "delete"} {
			_, refused := ov.refusals[verb]
			if refused && overrideVerbConstructor(ov, verb) == nil {
				res.danglingRefusals = append(res.danglingRefusals,
					fmt.Sprintf("%s: refusals[%q] is set but no constructor is wired", key, verb))
			}
			if refused {
				refusedOps[verb+" "+row.Name] = true
			}
		}
	}

	covered := map[string]bool{}
	checkOne := func(op, key string) {
		covered[op] = true
		if refusedOps[op] {
			return
		}
		row, ok := rowByKey[key]
		if !ok {
			res.unknownKinds = append(res.unknownKinds,
				fmt.Sprintf("%s: mapped to kind key %q, but no kind row has that key", op, key))
			return
		}
		if !row.Capability.IsSet() {
			return
		}
		cmd, ok := in.registered[op]
		if ok && (cmd.capability.IsSet() || cmd.resolver) {
			return
		}
		res.uncoveredGated = append(res.uncoveredGated,
			fmt.Sprintf("%s: targets Capability-bearing kind %q but is neither Capability-carrying nor a resolver", op, key))
	}

	for _, k := range in.rows {
		checkOne("get "+k.Name, k.Key)
		checkOne("patch "+k.Name, k.Key)
		if k.Deletable || delOverrideOnNonDeletable(k, in.overrides[k.Key]) {
			checkOne("delete "+k.Name, k.Key)
		}
	}
	for op, key := range in.derivedOps {
		checkOne(op, key)
	}
	for op, keys := range in.extraOps {
		for _, key := range keys {
			checkOne(op, key)
		}
	}
	if in.createOp != "" {
		for _, k := range in.rows {
			if k.NoCreateViaManifest != "" {
				covered[in.createOp] = true
				continue
			}
			checkOne(in.createOp, k.Key)
		}
	}

	for op := range in.registered {
		if covered[op] {
			continue
		}
		if _, ok := in.allowlist[op]; ok {
			continue
		}
		res.unclassified = append(res.unclassified,
			fmt.Sprintf("%s: classify this op - add it to a kind mapping or the reviewed allowlist", op))
	}

	sort.Strings(res.uncoveredGated)
	sort.Strings(res.unclassified)
	sort.Strings(res.danglingRefusals)
	sort.Strings(res.unknownKinds)
	return res
}
