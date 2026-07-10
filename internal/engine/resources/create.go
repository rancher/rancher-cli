package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

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
}

// NewCreate builds `create resource -f <file>` (mutating): it reads a single
// manifest document, resolves its apiVersion+kind to a table row (closed-world
// — an un-tabled kind is a VALIDATION error), reconciles the manifest's
// namespace with the routing flags per the kind's NamespacePolicy, and previews
// the create through the shared PlanCreate.
func NewCreate(clients engine.ClusterClientFactory, src engine.FileSource) runtime.Command {
	return &createCmd{clients: clients, src: src}
}

func (createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create resource",
		Usage:       "Create a resource from a manifest file",
		CommandType: runtime.Mutating,
		Flags: []runtime.ArgSpec{
			{Name: flagFilename, Type: "string", Required: true, Aliases: []string{"f"}},
			{Name: FlagCluster, Type: "string"},
			{Name: FlagNamespace, Type: "string"},
		},
		Examples:    []string{"rancher create resource -f deploy.yaml --cluster prod --dry-run", "rancher create resource -f - --apply"},
		Destructive: "dependsOnInput", // depends on the manifest's kind (a Sensitive kind is destructive)
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
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

func (c createCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	path := in.String(flagFilename)
	b, err := c.src.Read(path)
	if err != nil {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("cannot read manifest %q: %v", path, err)).
			WithHint("pass -f <file> or -f - for stdin")
	}
	n, derr := countDocs(b)
	if derr != nil {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "invalid manifest: "+derr.Error())
	}
	if n != 1 {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("manifest must contain exactly one document, found %d", n)).
			WithHint("split a multi-document manifest into separate invocations")
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "invalid manifest: "+err.Error())
	}
	obj := &unstructured.Unstructured{Object: m}

	k, ok := kinds.LookupGVK(obj.GetAPIVersion(), obj.GetKind())
	if !ok {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("unsupported resource %s/%s", obj.GetAPIVersion(), obj.GetKind())).
			WithHint("supported kinds: " + supportedKinds())
	}

	overrides, err := c.reconcile(ctx, k, in, obj.GetNamespace())
	if err != nil {
		return safety.Effect{}, nil, err
	}

	clientCluster, ns, effectCluster, _, err := Route(ctx, c.clients, k, overrideInput{Input: in, overrides: overrides}, true)
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
	eff, exec, err := engine.PlanCreate(ctx, client, k.GVR, ns, obj, effectCluster, k.K8sKind, k.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return eff, exec, nil
}

// reconcile applies the kind's NamespacePolicy to the manifest namespace and
// the routing flags, returning the overrides Route should see. A conflict
// between the manifest and an explicit flag is a VALIDATION error — flags fill
// gaps, they never silently override the manifest.
func (c createCmd) reconcile(ctx context.Context, k kinds.Kind, in runtime.Input, manifestNS string) (map[string]string, error) {
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
					fmt.Sprintf("--cluster resolves to %q but the manifest namespace is %q", id, manifestNS)).
					WithHint("drop metadata.namespace or pass the matching --cluster")
			}
		}
		// Both empty: Route rejects the named op with a --cluster hint.

	case kinds.NSWorkspace, kinds.NSPlain:
		flagNS := in.String(FlagNamespace)
		if manifestNS != "" && flagNS != "" && manifestNS != flagNS {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--namespace %q conflicts with the manifest namespace %q", flagNS, manifestNS)).
				WithHint("drop one of them")
		}
		if ns := flagNS; ns != "" {
			overrides[FlagNamespace] = ns
		} else if manifestNS != "" {
			overrides[FlagNamespace] = manifestNS
		}

	case kinds.NSNone:
		if manifestNS != "" {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%s is cluster-scoped; the manifest must not set metadata.namespace", k.Name)).
				WithHint("remove metadata.namespace")
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
