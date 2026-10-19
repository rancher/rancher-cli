package resources

import (
	"context"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type patchCmd struct {
	k       kinds.Kind
	clients engine.ClusterClientFactory
	src     engine.FileSource
	enrich  []EffectEnricher
}

// EffectEnricher lets a per-kind override append advisory content to a
// patch's Effect after the server dry-run built it (e.g. the
// provisioningcluster override's etcd-quorum preview). It receives
// the original and patched objects Plan already computed and a pointer to
// the Effect about to be returned; it never gates - Action/Destructive/
// Stability remain the only gating fields - it only informs.
type EffectEnricher func(orig, patched *unstructured.Unstructured, eff *safety.Effect)

// NewPatch builds `patch <kind> <name>` (mutating) from a Kind. It applies an
// RFC 6902 JSON patch (--type json, the default) or an RFC 7386 JSON merge
// patch (--type merge) locally against the fetched object, diffs the original
// and patched objects field-by-field, and updates with the fetched
// resourceVersion as a precondition. Destructiveness is per-kind (Sensitive),
// not a constant - patching a setting or identity kind requires --yes.
// enrich is optional (variadic keeps every existing call site compiling
// unchanged); each enricher runs, in order, after PlanUpdate builds the
// Effect and before Plan returns it.
func NewPatch(k kinds.Kind, clients engine.ClusterClientFactory, src engine.FileSource, enrich ...EffectEnricher) runtime.Command {
	return &patchCmd{k: k, clients: clients, src: src, enrich: enrich}
}

func (c patchCmd) Describe() runtime.Descriptor {
	destructive := "false"
	if c.k.Sensitive {
		destructive = "true"
	}
	return runtime.Descriptor{
		Name:    "patch " + c.k.Name,
		Aliases: c.k.Aliases,
		// "Patch the <kind>" (a single named resource), matching delete's
		// article choice; see the note in delete.go. The patch formats are
		// documented once, on the --patch and --type flags - repeating them
		// in every leaf's usage made `patch -h` read like a stutter, and
		// "patch" appeared twice per line.
		Usage:       "Patch the " + c.k.Name,
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{argSpec(c.k, true)},
		Flags:       patchFlags(c.k),
		Destructive: destructive,
		Auth:        true,
		Capability:  c.k.Capability,
		Shape:       runtime.ShapeText,
	}
}

// patchFlags is delete's Route flag set (same Required-per-policy rules - patch
// is always a named op) plus the two input sources, spelled as kubectl spells
// them: --patch/-p (inline) and --patch-file (a file or "-" for stdin), and
// --type, which names the patch format. Exactly one source is required,
// enforced in Plan.
func patchFlags(k kinds.Kind) []runtime.ArgSpec {
	flags := deleteFlags(k)
	return append(flags,
		runtime.ArgSpec{Name: flagPatch, Type: "string", Aliases: []string{"p"}, Desc: "inline `patch`: RFC 6902 ops, or a JSON object with --type merge"},
		runtime.ArgSpec{Name: flagPatchFile, Type: "string", Desc: "patch `file`, or - for stdin"},
		runtime.ArgSpec{Name: flagPatchType, Type: "string", Enum: []string{patchTypeJSON, patchTypeMerge},
			Desc: "patch type: json (RFC 6902 ops, the default) or merge (RFC 7386 JSON merge patch)"},
	)
}

func (c patchCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <"+argName(c.k)+"> is required")
	}
	name := args[0]
	// The patch is checked before the cluster name lookup, so a patch that
	// cannot be used is refused before any request.
	patch, err := c.decodePatch(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if c.k.Key == "managementcluster" {
		resolved, rerr := engine.ResolveClusterArgument(ctx, c.clients, name)
		if rerr != nil {
			return safety.Effect{}, nil, rerr
		}
		name = resolved
	}

	clientCluster, ns, effectCluster, _, err := Route(ctx, c.clients, c.k, in, OpNamed, in.Args()[0])
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, clientCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	orig, err := client.Get(ctx, c.k.GVR, ns, name)
	if err != nil {
		return safety.Effect{}, nil, lookupHandle(ctx, c.clients, client, c.k, ns, name, err)
	}
	rv := orig.GetResourceVersion()

	origJSON, err := orig.MarshalJSON()
	if err != nil {
		return safety.Effect{}, nil, err
	}
	patchedJSON, err := patch.apply(origJSON)
	if err != nil {
		// Only an op patch fails here: a merge of a checked object into a
		// fetched object always succeeds.
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"cannot apply patch: "+err.Error()+"; check the patch ops against the resource")
	}
	patched, err := decodePatched(patchedJSON)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// Whatever the patch did to metadata, the server receives the managed
	// fields exactly as fetched: a PUT that dropped them would reset field
	// ownership on the server, and one that changed them would forge it.
	patched.SetManagedFields(orig.GetManagedFields())

	// Diff the fetched and patched objects field-by-field - correct for every op
	// type, unlike mapping ops to entries (a move would misreport as a single
	// change). Computed before the RV is stamped, so the precondition never
	// appears as a field change.
	diff := engine.DiffObjects(orig.Object, patched.Object)
	if c.k.Sensitive {
		redactSensitiveDiff(diff)
	}
	if len(c.k.RedactPaths) > 0 {
		redactKindPaths(diff, c.k.RedactPaths)
	}

	patched.SetResourceVersion(rv)
	// PlanUpdate dry-runs the update for an admission preview and returns the
	// executor; the Effect carries the field diff, the RV precondition, and the
	// per-kind destructiveness.
	eff, stored, exec, err := engine.PlanUpdateObserved(ctx, client, c.k.GVR, ns, patched, effectCluster, c.k.Name, resultKind(c.k), diff, c.k.Sensitive)
	if err != nil {
		return eff, exec, err
	}
	// A changed field another manager applies (Flux, Argo) may be put back
	// on that manager's next reconcile; say so before the operator confirms.
	// The managed fields are on the fetched object already (the diff skips
	// them), so this costs no request.
	managedFields, _, _ := unstructured.NestedSlice(orig.Object, "metadata", "managedFields")
	eff.Warnings = append(eff.Warnings, ownershipWarnings(managedFields, diff)...)
	if patch.isMerge() {
		// The dry-run answer carries the server's defaults on every list
		// entry, as the fetched object does; the locally merged object does
		// not, so comparing with it would count kept entries as dropped.
		after := patched.Object
		if stored != nil {
			after = stored.Object
		}
		eff.Warnings = append(eff.Warnings, droppedListWarnings(patch.merge, orig.Object, after)...)
	}
	for _, e := range c.enrich {
		e(orig, patched, &eff)
	}
	return eff, engine.RedactMutationResult(exec, c.k.RedactPaths, c.k.Sensitive), nil
}

// decodePatch enforces the exactly-one-of-{--patch,--patch-file} rule and
// checks and decodes the patch as --type says (json when unset). A read
// error, both sources, neither source, or a patch that cannot be used are
// VALIDATION errors.
func (c patchCmd) decodePatch(in runtime.Input) (patchDoc, error) {
	inline := in.String(flagPatch)
	file := in.String(flagPatchFile)
	var raw []byte
	switch {
	case inline != "" && file != "":
		return patchDoc{}, clierr.New(clierr.CodeValidation, "provide exactly one of --patch or --patch-file, not both")
	case inline != "":
		raw = []byte(inline)
	case file != "":
		b, err := c.src.Read(file)
		if err != nil {
			return patchDoc{}, clierr.New(clierr.CodeValidation, fmt.Sprintf("cannot read patch %q: %v", file, err))
		}
		raw = b
	default:
		return patchDoc{}, clierr.New(clierr.CodeValidation, "a patch is required: pass --patch or --patch-file")
	}
	return parsePatch(raw, in.String(flagPatchType))
}

// refuseServerOwnedOps rejects any op that would change a field the server
// owns (engine.ServerOwnedPaths): an op whose path, or whose from on a move
// or copy, is that field or anything under it, and an op whose value
// carries the field into an ancestor. The CLI sends the fetched managed
// fields and resource version back as they were (Plan restores both after
// the local apply), so this check exists to say clearly why an op is
// pointless rather than to protect the server; a move or copy that lands a
// forged value under /metadata is caught by the restore alone.
func refuseServerOwnedOps(patch jsonpatch.Patch) error {
	for i, op := range patch {
		path, _ := op.Path()
		from, fromErr := op.From() // only move and copy carry one; the library answers "unknown" with an error otherwise
		for _, keys := range engine.ServerOwnedPaths {
			if matchesRedactPath(path, keys) || (fromErr == nil && matchesRedactPath(from, keys)) || valueCarries(op, path, keys) {
				return clierr.New(clierr.CodeValidation, fmt.Sprintf(
					"patch op %d targets %s; the server sets this field and a patch cannot change it", i, engine.PointerFor(keys)))
			}
		}
	}
	return nil
}

// valueCarries reports whether an op on an ancestor of the server-owned
// path (the whole object, or /metadata) has a value that contains the
// field, which would smuggle it past a path check. Ops without a value
// (remove, move, copy, test) never match.
func valueCarries(op jsonpatch.Operation, path string, keys []string) bool {
	pointer := engine.PointerFor(keys)
	if path != "" && !strings.HasPrefix(pointer, path+"/") {
		return false
	}
	value, err := op.ValueInterface()
	if err != nil {
		return false
	}
	node, ok := value.(map[string]any)
	if !ok {
		return false
	}
	depth := strings.Count(path, "/")
	_, found, _ := unstructured.NestedFieldNoCopy(node, keys[depth:]...)
	return found
}

// redactSensitiveDiff blanks From/To for diff entries under /data or
// /stringData in place - the JSON path stays visible (so a caller can see
// which keys changed) but the raw value never reaches the Effect. Gated on
// c.k.Sensitive (the same flag that gates the --yes destructive gate on this
// same command), not the literal "secret" kind, so redaction stays correct
// by construction for any future Sensitive kind with a confidential payload
// under these paths - today only `secret` has one.
func redactSensitiveDiff(diff []safety.DiffEntry) {
	for i, e := range diff {
		if isSecretDataPath(e.Path) {
			diff[i].From = "<redacted>"
			diff[i].To = "<redacted>"
		}
	}
}

// isSecretDataPath compares against the RFC 6901 pointer as written, with
// no unescaping: a Secret data key matches [-._a-zA-Z0-9]+ (the apiserver
// rejects anything else), so it never contains "/" or "~", and the escaped
// and unescaped spellings of every segment below /data or /stringData are
// identical.
func isSecretDataPath(path string) bool {
	return path == "/data" || strings.HasPrefix(path, "/data/") ||
		path == "/stringData" || strings.HasPrefix(path, "/stringData/")
}

// redactKindPaths blanks From/To for diff entries whose JSON Pointer path
// matches any of the per-kind RedactPaths ([]string key sequence → JSON
// Pointer "/key0/key1/..."). This covers credential fields declared on
// individual kind rows (e.g. root-level "token" on management-token) that
// the generic /data//stringData guard does not reach.
func redactKindPaths(diff []safety.DiffEntry, paths [][]string) {
	for i, e := range diff {
		for _, path := range paths {
			if matchesRedactPath(e.Path, path) {
				diff[i].From = "<redacted>"
				diff[i].To = "<redacted>"
				break
			}
		}
	}
}

// matchesRedactPath reports whether the JSON Pointer entryPath equals or is
// nested under the path specified by keys. For keys ["token"], it matches
// "/token" and "/token/anything"; for keys ["metadata","name"], it matches
// "/metadata/name" and "/metadata/name/anything". Each key is escaped before
// joining, so a key containing "/" or "~" matches the single pointer segment
// it produced, not a path through it.
func matchesRedactPath(entryPath string, keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	pointer := engine.PointerFor(keys)
	return entryPath == pointer || strings.HasPrefix(entryPath, pointer+"/")
}
