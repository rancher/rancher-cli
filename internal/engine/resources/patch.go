package resources

import (
	"context"
	"fmt"
	"reflect"
	"sort"

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
}

// NewPatch builds `patch <kind> <name>` (mutating) from a Kind. It applies an
// RFC 6902 JSON patch locally against the fetched object, diffs the original
// and patched objects field-by-field, and updates with the fetched
// resourceVersion as a precondition. Destructiveness is per-kind (Sensitive),
// not a constant — patching a setting or identity kind requires --yes.
func NewPatch(k kinds.Kind, clients engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
	return &patchCmd{k: k, clients: clients, src: src}
}

func (c patchCmd) Describe() runtime.Descriptor {
	destructive := "false"
	if c.k.Sensitive {
		destructive = "true"
	}
	return runtime.Descriptor{
		Name:        "patch " + c.k.Name,
		Aliases:     c.k.Aliases,
		Usage:       "Patch a " + c.k.Name + " with an RFC 6902 JSON patch",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags:       patchFlags(c.k),
		Destructive: destructive,
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

// patchFlags is delete's route flag set (same Required-per-policy rules — patch
// is always a named op) plus the two input sources: --patch (inline ops) and
// --filename/-f (a file or "-" for stdin). Exactly one of the two is required,
// enforced in Plan.
func patchFlags(k kinds.Kind) []runtime.ArgSpec {
	flags := deleteFlags(k)
	return append(flags,
		runtime.ArgSpec{Name: "patch", Type: "string"},
		runtime.ArgSpec{Name: "filename", Type: "string", Aliases: []string{"f"}},
	)
}

func (c patchCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required").WithHint("provide <name>")
	}
	name := args[0]

	patch, err := c.decodePatch(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	clientCluster, ns, effectCluster, err := route(ctx, c.clients, c.k, in, true)
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
	diff := diffObjects("", orig.Object, patched.Object)

	patched.SetResourceVersion(rv)
	// Admission preview: a server-side dry-run update surfaces RBAC/webhook
	// denials before the user confirms.
	if _, err := client.Update(ctx, c.k.GVR, ns, patched, true); err != nil {
		return safety.Effect{}, nil, err
	}

	title := kindTitle(c.k.Name)
	eff := safety.Effect{
		Action:            safety.ActionUpdate,
		Target:            c.k.GVR.Resource + "/" + name,
		Cluster:           effectCluster,
		Diff:              diff,
		ResourceVersion:   rv,
		ConflictDetection: true,
		Reversible:        true,
		Destructive:       c.k.Sensitive,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		updated, err := client.Update(ctx, c.k.GVR, ns, patched, false)
		if err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: title, Data: updated.Object}, nil
	}
	return eff, exec, nil
}

// decodePatch enforces the exactly-one-of-{--patch,-f} rule and decodes the
// RFC 6902 patch. A read error, both sources, neither source, or malformed ops
// are VALIDATION errors.
func (c patchCmd) decodePatch(in runtime.Input) (jsonpatch.Patch, error) {
	inline := in.String("patch")
	file := in.String("filename")
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

// diffObjects walks two decoded-JSON trees and emits one DiffEntry per changed
// leaf, keyed by JSON pointer. A key present on one side only yields a nil
// From or To. metadata.resourceVersion and metadata.managedFields are skipped
// (server-managed churn, not a user-visible change).
func diffObjects(prefix string, a, b any) []safety.DiffEntry {
	switch prefix {
	case "/metadata/resourceVersion", "/metadata/managedFields":
		return nil
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return []safety.DiffEntry{{Path: prefix, From: a, To: b}}
		}
		var out []safety.DiffEntry
		for _, k := range unionKeys(av, bv) {
			out = append(out, diffObjects(prefix+"/"+k, av[k], bv[k])...)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return []safety.DiffEntry{{Path: prefix, From: a, To: b}}
		}
		var out []safety.DiffEntry
		for i := 0; i < max(len(av), len(bv)); i++ {
			var ai, bi any
			if i < len(av) {
				ai = av[i]
			}
			if i < len(bv) {
				bi = bv[i]
			}
			out = append(out, diffObjects(fmt.Sprintf("%s/%d", prefix, i), ai, bi)...)
		}
		return out
	default:
		if !reflect.DeepEqual(a, b) {
			return []safety.DiffEntry{{Path: prefix, From: a, To: b}}
		}
		return nil
	}
}

// unionKeys returns the sorted union of two maps' keys, for deterministic diffs.
func unionKeys(a, b map[string]any) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
