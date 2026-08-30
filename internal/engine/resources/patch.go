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
// provisioningcluster override's etcd-quorum preview, Task 8). It receives
// the original and patched objects Plan already computed and a pointer to
// the Effect about to be returned; it never gates — Action/Destructive/
// Stability remain the only gating fields — it only informs.
type EffectEnricher func(orig, patched *unstructured.Unstructured, eff *safety.Effect)

// NewPatch builds `patch <kind> <name>` (mutating) from a Kind. It applies an
// RFC 6902 JSON patch locally against the fetched object, diffs the original
// and patched objects field-by-field, and updates with the fetched
// resourceVersion as a precondition. Destructiveness is per-kind (Sensitive),
// not a constant — patching a setting or identity kind requires --yes.
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
		// article choice; see the note in delete.go. The patch format (RFC
		// 6902) is documented once, on the --patch flag that carries the
		// payload — repeating it in every leaf's usage made `patch -h` read
		// like a stutter, and "patch" appeared twice per line.
		Usage:        "Patch the " + c.k.Name,
		CommandType:  runtime.Mutating,
		Args:         []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags:        patchFlags(c.k),
		Destructive:  destructive,
		Network:      true,
		Auth:         true,
		ProjectScope: kinds.ProjectScopable(c.k),
		Capability:   c.k.Capability,
		Shape:        runtime.ShapeText,
	}
}

// patchFlags is delete's Route flag set (same Required-per-policy rules — patch
// is always a named op) plus the two input sources: --patch (inline ops) and
// --filename/-f (a file or "-" for stdin). Exactly one of the two is required,
// enforced in Plan.
func patchFlags(k kinds.Kind) []runtime.ArgSpec {
	flags := deleteFlags(k)
	return append(flags,
		runtime.ArgSpec{Name: flagPatch, Type: "string", Desc: "inline RFC 6902 JSON `patch` ops"},
		runtime.ArgSpec{Name: flagFilename, Type: "string", Aliases: []string{"f"}, Desc: "patch `file`, or - for stdin"},
	)
}

func (c patchCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required").WithHint("provide <name>")
	}
	name := args[0]
	if c.k.Key == "managementcluster" {
		resolved, rerr := c.clients.ResolveCluster(ctx, name)
		if rerr != nil {
			return safety.Effect{}, nil, rerr
		}
		name = resolved
	}

	patch, err := c.decodePatch(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	clientCluster, ns, effectCluster, _, err := Route(ctx, c.clients, c.k, in, true)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, clientCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	orig, err := client.Get(ctx, c.k.GVR, ns, name)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	rv := orig.GetResourceVersion()

	origJSON, err := orig.MarshalJSON()
	if err != nil {
		return safety.Effect{}, nil, err
	}
	patchedJSON, err := patch.Apply(origJSON)
	if err != nil {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "cannot apply patch: "+err.Error()).
			WithHint("check the patch ops against the resource")
	}
	patched := &unstructured.Unstructured{}
	if err := patched.UnmarshalJSON(patchedJSON); err != nil {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "patched object is not valid: "+err.Error())
	}

	// Diff the fetched and patched objects field-by-field — correct for every op
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
	eff, exec, err := engine.PlanUpdate(ctx, client, c.k.GVR, ns, patched, effectCluster, c.k.Name, resultKind(c.k), diff, c.k.Sensitive)
	if err != nil {
		return eff, exec, err
	}
	for _, e := range c.enrich {
		e(orig, patched, &eff)
	}
	return eff, engine.RedactMutationResult(exec, c.k.RedactPaths, c.k.Sensitive), nil
}

// decodePatch enforces the exactly-one-of-{--patch,-f} rule and decodes the
// RFC 6902 patch. A read error, both sources, neither source, or malformed ops
// are VALIDATION errors.
func (c patchCmd) decodePatch(in runtime.Input) (jsonpatch.Patch, error) {
	inline := in.String(flagPatch)
	file := in.String(flagFilename)
	var raw []byte
	switch {
	case inline != "" && file != "":
		return nil, clierr.New(clierr.CodeValidation, "provide exactly one of --patch or -f").WithHint("drop one of them")
	case inline != "":
		raw = []byte(inline)
	case file != "":
		b, err := c.src.Read(file)
		if err != nil {
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("cannot read patch %q: %v", file, err)).
				WithHint("pass -f <file> or -f - for stdin")
		}
		raw = b
	default:
		return nil, clierr.New(clierr.CodeValidation, "a patch is required").WithHint("pass --patch '[...]' or -f <file>")
	}
	patch, err := jsonpatch.DecodePatch(raw)
	if err != nil {
		return nil, clierr.New(clierr.CodeValidation, "invalid patch: "+err.Error()).
			WithHint("expected an RFC 6902 JSON patch array")
	}
	return patch, nil
}

// redactSensitiveDiff blanks From/To for diff entries under /data or
// /stringData in place — the JSON path stays visible (so a caller can see
// which keys changed) but the raw value never reaches the Effect. Gated on
// c.k.Sensitive (the same flag that gates the --yes destructive gate on this
// same command), not the literal "secret" kind, so redaction stays correct
// by construction for any future Sensitive kind with a confidential payload
// under these paths — today only `secret` has one.
func redactSensitiveDiff(diff []safety.DiffEntry) {
	for i, e := range diff {
		if isSecretDataPath(e.Path) {
			diff[i].From = "<redacted>"
			diff[i].To = "<redacted>"
		}
	}
}

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
// "/metadata/name" and "/metadata/name/anything".
func matchesRedactPath(entryPath string, keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	pointer := "/" + strings.Join(keys, "/")
	return entryPath == pointer || strings.HasPrefix(entryPath, pointer+"/")
}
