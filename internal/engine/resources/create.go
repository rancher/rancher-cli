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
			engine.NamespaceFlag(false),
			runtime.ProjectFlag(false, "the manifest's namespace must be in this project (namespaced kinds; refused for a Namespace)"),
		},
		Examples:    []string{"rancher create resource -f deploy.yaml --cluster prod --dry-run", "rancher create resource -f - --apply"},
		Destructive: "dependsOnInput", // depends on the manifest's kind (a Sensitive kind is destructive)
		Auth:        true,
		Shape:       runtime.ShapeText,
		SecretInput: true,
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
			fmt.Sprintf("cannot create %s %s from a manifest; %s", createArticle(k), k.Name, k.NoCreateViaManifest))
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
	eff, exec, err := engine.PlanCreate(ctx, client, k.GVR, ns, obj, k.Redaction(), effectCluster, k.Name, k.K8sKind, k.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return eff, exec, nil
}

// anArticleKeys lists the keys of the rows whose name takes "an" in the
// refusal message of a manifest create; every other row takes "a". A row
// whose name starts with a vowel sound adds its key here.
var anArticleKeys = map[string]bool{"app": true, "operation": true}

// createArticle returns the article the refusal message puts before k's name.
func createArticle(k kinds.Kind) string {
	if anArticleKeys[k.Key] {
		return "an"
	}
	return "a"
}

// parseManifest returns the memoized parse of the manifest named by the
// filename flag, reading and resolving it at most once per invocation
// regardless of how many callers (ResolveCapability, Plan) ask. A mismatch
// between the path this memo was primed with and the path a later caller
// asks about is an internal error, not a re-read - both callers read the same
// Input in the same invocation, so they cannot legitimately disagree.
func (c *createCmd) parseManifest(in runtime.Input) (*unstructured.Unstructured, kinds.Kind, error) {
	path := in.String(flagFilename)
	c.parseOnce.Do(func() {
		c.parsePath = path
		c.parseObj, c.parseKind, c.parseErr = c.readAndResolve(path)
	})
	if path != c.parsePath {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("internal error: create resource read manifest %q and was then given %q", c.parsePath, path))
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
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, invalidManifestMessage(derr))
	}
	if n != 1 {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("manifest must contain exactly one document, found %d; split a multi-document manifest and run the command once for each document", n))
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, invalidManifestMessage(err))
	}
	obj := &unstructured.Unstructured{Object: m}

	k, ok := c.lookupGVK(obj.GetAPIVersion(), obj.GetKind())
	if !ok {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("unsupported manifest kind %s/%s; supported kinds: %s", obj.GetAPIVersion(), obj.GetKind(), supportedKinds()))
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
					fmt.Sprintf("--cluster resolves to %q but the manifest namespace is %q; remove metadata.namespace or pass the matching --cluster", id, manifestNS))
			}
		}
		// Both empty: Route rejects the named op with a --cluster hint.

	case kinds.NSWorkspace, kinds.NSPlain:
		flagNS := in.String(FlagNamespace)
		if manifestNS != "" && flagNS != "" && manifestNS != flagNS {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--namespace %q conflicts with the manifest namespace %q; remove one of them", flagNS, manifestNS))
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

// unreadableManifest is the message for a manifest the YAML decoders refuse
// with a text that may quote its content.
const unreadableManifest = "invalid manifest: the YAML could not be decoded"

// yamlSyntaxPrefix starts a YAML decoder's syntax error, which names a line
// and quotes none of the document.
const yamlSyntaxPrefix = "yaml: line "

// yamlProblems are fixed problem texts that the scanner (its tag scanner
// included), parser and reader of gopkg.in/yaml.v3 report, copied from its
// scannerc.go, parserc.go and readerc.go. Each quotes none of the document.
// A syntax error on the first line carries one of them with no line number,
// as "yaml: " and the text. The list makes no claim to hold every text of
// those files: a text that is not on it gets the fixed message.
var yamlProblems = map[string]bool{
	"block sequence entries are not allowed in this context":       true,
	"control characters are not allowed":                           true,
	"could not find expected ':'":                                  true,
	"could not find expected directive name":                       true,
	"did not find URI escaped octet":                               true,
	"did not find expected '!'":                                    true,
	"did not find expected ',' or ']'":                             true,
	"did not find expected ',' or '}'":                             true,
	"did not find expected '-' indicator":                          true,
	"did not find expected <document start>":                       true,
	"did not find expected <stream-start>":                         true,
	"did not find expected alphabetic or numeric character":        true,
	"did not find expected comment or line break":                  true,
	"did not find expected digit or '.' character":                 true,
	"did not find expected hexdecimal number":                      true,
	"did not find expected key":                                    true,
	"did not find expected node content":                           true,
	"did not find expected tag URI":                                true,
	"did not find expected version number":                         true,
	"did not find expected whitespace":                             true,
	"did not find expected whitespace or line break":               true,
	"did not find the expected '>'":                                true,
	"expected low surrogate area":                                  true,
	"found a tab character that violates indentation":              true,
	"found a tab character where an indentation space is expected": true,
	"found an incorrect leading UTF-8 octet":                       true,
	"found an incorrect trailing UTF-8 octet":                      true,
	"found an indentation indicator equal to 0":                    true,
	"found character that cannot start any token":                  true,
	"found duplicate %TAG directive":                               true,
	"found duplicate %YAML directive":                              true,
	"found extremely long version number":                          true,
	"found incompatible YAML document":                             true,
	"found invalid Unicode character escape code":                  true,
	"found undefined tag handle":                                   true,
	"found unexpected document indicator":                          true,
	"found unexpected end of stream":                               true,
	"found unexpected non-alphabetical character":                  true,
	"found unknown directive name":                                 true,
	"found unknown escape character":                               true,
	"incomplete UTF-16 character":                                  true,
	"incomplete UTF-16 surrogate pair":                             true,
	"incomplete UTF-8 octet sequence":                              true,
	"invalid Unicode character":                                    true,
	"invalid leading UTF-8 octet":                                  true,
	"invalid length of a UTF-8 sequence":                           true,
	"invalid trailing UTF-8 octet":                                 true,
	"mapping keys are not allowed in this context":                 true,
	"mapping values are not allowed in this context":               true,
	"unexpected low surrogate area":                                true,
}

// invalidManifestMessage is the message for err, a decoder's refusal of a
// manifest. The decoders quote the document in some refusals: a scalar that
// does not fit its explicit tag, an unknown alias, a map key that is a
// collection, a repeated key. A manifest may hold a secret, so the
// decoder's text is kept only for a syntax error: one that starts with
// "yaml: line " (it names a line and nothing of the content), or that is
// "yaml: " and exactly one of the fixed problem texts in yamlProblems.
// Every other refusal, whatever it says, gets a fixed text.
func invalidManifestMessage(err error) string {
	text := strings.TrimPrefix(err.Error(), "error converting YAML to JSON: ")
	if strings.HasPrefix(text, yamlSyntaxPrefix) {
		return "invalid manifest: " + text
	}
	if problem, ok := strings.CutPrefix(text, "yaml: "); ok && yamlProblems[problem] {
		return "invalid manifest: " + text
	}
	return unreadableManifest
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
