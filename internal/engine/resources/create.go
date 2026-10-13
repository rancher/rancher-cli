package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

type createCmd struct {
	clients engine.ClusterClientFactory
	src     engine.FileSource
	// lookupGVK resolves a manifest's apiVersion+kind to a table row.
	// Defaults to kinds.LookupGVK; production wiring never overrides it -
	// test-only injection lets resolver-mechanics tests prove behavior
	// against a synthetic Capability-bearing row without a way to register
	// one in the closed-world kinds table.
	lookupGVK func(apiVersion, kind string) (kinds.Kind, bool)

	// parseOnce memoizes the manifest parse to exactly one read per
	// invocation: ResolveCapability and Plan both need the unmarshaled
	// object and its resolved kind row, and -f - reads stdin, which is
	// single-shot. parsePath is the filename the memo was primed with -
	// both callers read it from the same Input in the same invocation, so
	// a mismatch is a command-bug, never a re-read.
	parseOnce sync.Once
	parsePath string
	parseObj  *unstructured.Unstructured
	parseKind kinds.Kind
	parseErr  error
}

// NewCreate builds `create resource -f <file>` (mutating): it reads a single
// manifest document, resolves its apiVersion+kind to a table row (closed-world -
// an un-tabled kind is a VALIDATION error), reconciles the manifest's
// namespace with the routing flags per the kind's NamespacePolicy, and previews
// the create through the shared PlanCreate. It also implements
// runtime.CapabilityResolver: the resolved kind row's requirement is what
// gates create-via-manifest, since the command's own Descriptor is
// kind-independent.
func NewCreate(clients engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
	return &createCmd{clients: clients, src: src, lookupGVK: kinds.LookupGVK}
}

func (*createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create resource",
		Usage:       "Create a resource from a manifest file",
		CommandType: runtime.Mutating,
		Flags: []runtime.ArgSpec{
			{Name: flagFilename, Type: "string", Required: true, Aliases: []string{"f"}, Desc: "manifest `file`, or - for stdin"},
			createClusterFlag(),
			namespaceFlag(),
			runtime.ProjectFlag(false, "the manifest's namespace must be in this project (namespaced kinds; refused for a Namespace)"),
		},
		Examples:    []string{"rancher create resource -f deploy.yaml --cluster prod --dry-run", "rancher create resource -f - --apply"},
		Destructive: "dependsOnInput", // depends on the manifest's kind (a Sensitive kind is destructive)
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// createClusterFlag is clusterFlag with create's own contract: the manifest
// can supply the cluster (see overrideInput), so the flag is not
// schema-required - routing rejects only when neither source names one.
func createClusterFlag() runtime.ArgSpec {
	f := engine.ClusterFlag(false, true)
	f.Desc = "target cluster `id|name` (the manifest may supply it)"
	return f
}

// overrideInput wraps a runtime.Input, preferring a manifest-derived value for
// the reconciled routing flags (cluster, namespace). Args and Bool pass
// through, so Route sees the same positional/boolean input the user gave.
type overrideInput struct {
	runtime.Input
	overrides map[string]string
}

func (o overrideInput) String(name string) string {
	if v, ok := o.overrides[name]; ok {
		return v
	}
	return o.Input.String(name)
}

// ResolveCapability implements runtime.CapabilityResolver: it runs the same
// parse Plan runs (shared via parseManifest's memo) and returns the resolved
// kind row's capability requirement. A NoCreateViaManifest row returns the
// zero requirement: Plan's existing refusal below stands and fires exactly
// as today, without spending a network probe on a kind that cannot be
// created via manifest regardless of server edition.
func (c *createCmd) ResolveCapability(_ context.Context, in runtime.Input) (runtime.CapabilityRequirement, error) {
	_, k, err := c.parseManifest(in)
	if err != nil {
		return runtime.CapabilityRequirement{}, err
	}
	if k.NoCreateViaManifest != "" {
		return runtime.CapabilityRequirement{}, nil
	}
	if err := c.projectRefusal(in); err != nil {
		return runtime.CapabilityRequirement{}, err
	}
	return k.Capability, nil
}

// projectRefusal refuses a typed --project the manifest's kind or a
// create does not take, from the parsed manifest alone: it runs before the
// capability stage's requests and before reconcile resolves a cluster.
func (c *createCmd) projectRefusal(in runtime.Input) error {
	typed := in.String(FlagProject)
	if typed == "" {
		return nil
	}
	obj, k, err := c.parseManifest(in)
	if err != nil {
		return err
	}
	return ProjectRefusal(k, OpCreate, obj.GetName(), in.String(FlagCluster), typed)
}

func (c *createCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	obj, k, err := c.parseManifest(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if k.NoCreateViaManifest != "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cannot create %s via manifest: %s", k.Name, k.NoCreateViaManifest))
	}

	overrides, err := c.reconcile(ctx, k, in, obj.GetNamespace())
	if err != nil {
		return safety.Effect{}, nil, err
	}

	clientCluster, ns, effectCluster, _, err := Route(ctx, c.clients, k, overrideInput{Input: in, overrides: overrides}, OpCreate, obj.GetName())
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, clientCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// The request namespace and the object must never disagree: Route owns the
	// namespace decision, so the object is normalized to it (cleared for a
	// cluster-scoped kind).
	obj.SetNamespace(ns)
	eff, exec, err := engine.PlanCreate(ctx, client, k.GVR, ns, obj, effectCluster, k.Name, k.K8sKind, k.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return eff, engine.RedactMutationResult(exec, k.RedactPaths, k.Sensitive), nil
}

// parseManifest returns the memoized parse of the manifest named by the
// filename flag, reading and resolving it at most once per invocation
// regardless of how many callers (ResolveCapability, Plan) ask. A mismatch
// between the path this memo was primed with and the path a later caller
// asks about is a command bug, not a re-read - both callers read the same
// Input in the same invocation, so they cannot legitimately disagree.
func (c *createCmd) parseManifest(in runtime.Input) (*unstructured.Unstructured, kinds.Kind, error) {
	path := in.String(flagFilename)
	c.parseOnce.Do(func() {
		c.parsePath = path
		c.parseObj, c.parseKind, c.parseErr = c.readAndResolve(path)
	})
	if path != c.parsePath {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("command bug: create resource parsed manifest %q but is now asked about %q", c.parsePath, path))
	}
	return c.parseObj, c.parseKind, c.parseErr
}

// readAndResolve performs the manifest parse itself: read, enforce the
// exactly-one-document rule, unmarshal, and resolve the kind via the
// injected lookup. Every error here is one of the manifest-shape VALIDATION
// errors Plan has always produced - they now surface one stage earlier (from
// the capability stage, via ResolveCapability) when that runs first, but the
// rendered envelope is byte-identical.
func (c *createCmd) readAndResolve(path string) (*unstructured.Unstructured, kinds.Kind, error) {
	b, err := c.src.Read(path)
	if err != nil {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cannot read manifest %q: %v", path, err))
	}
	n, derr := countDocs(b)
	if derr != nil {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, "invalid manifest: "+derr.Error())
	}
	if n != 1 {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("manifest must contain exactly one document, found %d; split a multi-document manifest into separate invocations", n))
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, "invalid manifest: "+err.Error())
	}
	obj := &unstructured.Unstructured{Object: m}

	k, ok := c.lookupGVK(obj.GetAPIVersion(), obj.GetKind())
	if !ok {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("unsupported resource %s/%s; supported kinds: %s", obj.GetAPIVersion(), obj.GetKind(), supportedKinds()))
	}
	return obj, k, nil
}

// reconcile applies the kind's NamespacePolicy to the manifest namespace and
// the routing flags, returning the overrides Route should see. A conflict
// between the manifest and an explicit flag is a VALIDATION error - flags fill
// gaps, they never silently override the manifest.
func (c *createCmd) reconcile(ctx context.Context, k kinds.Kind, in runtime.Input, manifestNS string) (map[string]string, error) {
	overrides := map[string]string{}
	switch k.Policy {
	case kinds.NSCluster:
		// The manifest namespace IS a cluster id.
		clusterFlag := in.String(FlagCluster)
		switch {
		case clusterFlag == "" && manifestNS != "":
			overrides[FlagCluster] = manifestNS // fully-specified manifest, no flag needed
		case clusterFlag != "" && manifestNS != "":
			id, err := c.clients.ResolveCluster(ctx, clusterFlag)
			if err != nil {
				return nil, err
			}
			if id != manifestNS {
				return nil, clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--cluster resolves to %q but the manifest namespace is %q; drop metadata.namespace or pass the matching --cluster", id, manifestNS))
			}
		}
		// Both empty: Route rejects the named op with a --cluster hint.

	case kinds.NSWorkspace, kinds.NSPlain:
		flagNS := in.String(FlagNamespace)
		if manifestNS != "" && flagNS != "" && manifestNS != flagNS {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--namespace %q conflicts with the manifest namespace %q; drop one of them", flagNS, manifestNS))
		}
		if ns := flagNS; ns != "" {
			overrides[FlagNamespace] = ns
		} else if manifestNS != "" {
			overrides[FlagNamespace] = manifestNS
		}

	case kinds.NSNone:
		if manifestNS != "" {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%s is cluster-scoped; the manifest must not set metadata.namespace", k.Name))
		}
	}
	return overrides, nil
}

// countDocs counts the YAML documents in b with a structural decoder, so a
// "---" inside a block or flow scalar is never mistaken for a document
// separator. JSON and single-object YAML count as one; empty documents (a bare
// "---" or trailing separator) are ignored. A decode error is surfaced so the
// caller reports an invalid manifest rather than a misleading count.
func countDocs(b []byte) (int, error) {
	dec := yamlv3.NewDecoder(bytes.NewReader(b))
	count := 0
	for {
		var doc any
		switch err := dec.Decode(&doc); {
		case errors.Is(err, io.EOF):
			return count, nil
		case err != nil:
			return 0, err
		}
		if doc != nil {
			count++
		}
	}
}

// supportedKinds is the sorted CLI-name list for the closed-world VALIDATION
// hint when a manifest names an un-tabled kind.
func supportedKinds() string {
	all := kinds.All()
	names := make([]string, 0, len(all))
	for _, k := range all {
		names = append(names, k.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
