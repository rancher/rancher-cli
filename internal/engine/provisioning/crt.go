// crt.go implements the override for the clusterregistrationtoken row:
// `get clusterregistrationtoken` reads Steve directly (the ONLY place the
// real token is spliced into the command/manifest fields - the k8s proxy's
// own object carries the literal "{token}" placeholder in those fields;
// rancher's steve formatter clusterregistrationtoken.go:24-72 does the
// ReplaceAll at :69 using the object's own status.token as the real value),
// and `patch clusterregistrationtoken` is refused outright (the row is
// read-only surface).
package provisioning

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/labels"
)

// crtCollectionPath is the Steve /v1 collection route for
// clusterregistrationtokens. A single object's path appends "/<ns>/<name>".
const crtCollectionPath = "/v1/management.cattle.io.clusterregistrationtokens"

// placeholderToken is the literal string the k8s proxy's own object carries
// in the seven command/manifest fields until Steve's formatter splices in
// the real token. Surfacing it as data would hand back a non-functional
// command, so the named get rejects it over PlaceholderFields.
const placeholderToken = "{token}"

// placeholderError is the NAMED-get policy over PlaceholderFields' report:
// one bad object IS the one thing the caller asked for, so failing loud is
// correct (TestNewGetCRTPlaceholderGuardTable asserts the message).
// The list path degrades in place instead - see the Execute list loop.
func placeholderError(path, field string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("clusterregistrationtoken at %s: field %q still carries the unformatted %s placeholder - the server never spliced in the real token",
			path, field, placeholderToken))
}

// crtRow is one `get clusterregistrationtoken` row: every field CRTObject
// carries, so -o json/-o yaml show the full command/manifest set even
// though the table only renders NAME/CLUSTER/EXPIRES.
type crtRow struct {
	Namespace                  string            `json:"namespace"`
	Name                       string            `json:"name"`
	ClusterName                string            `json:"clusterName"`
	Labels                     map[string]string `json:"labels,omitempty"`
	Command                    string            `json:"command"`
	InsecureCommand            string            `json:"insecureCommand"`
	NodeCommand                string            `json:"nodeCommand"`
	InsecureNodeCommand        string            `json:"insecureNodeCommand"`
	WindowsNodeCommand         string            `json:"windowsNodeCommand"`
	InsecureWindowsNodeCommand string            `json:"insecureWindowsNodeCommand"`
	ManifestURL                string            `json:"manifestUrl"`
	Token                      string            `json:"token"`
	ExpiresAt                  string            `json:"expiresAt"`
}

// crtRowFrom is a direct struct conversion: crtRow carries exactly
// CRTObject's fields (plus JSON tags), so the compiler enforces the two
// views can never drift apart.
func crtRowFrom(o CRTObject) crtRow {
	return crtRow(o)
}

// crtRows is `get clusterregistrationtoken`'s payload; it implements
// output.Tabular so `-o table` shows NAME/CLUSTER/EXPIRES - the full
// command/manifest set is still in the envelope Data for -o json/-o yaml.
type crtRows []crtRow

func (r crtRows) Columns() []string { return []string{"NAME", "CLUSTER", "EXPIRES"} }

func (r crtRows) Rows() [][]string {
	out := make([][]string, 0, len(r))
	for _, row := range r {
		out = append(out, []string{row.Name, row.ClusterName, row.ExpiresAt})
	}
	return out
}

type getCRTCmd struct {
	k     kinds.Kind
	roots engine.RootClientFactory
}

// NewGetCRT builds the get override for the clusterregistrationtoken row: a
// Steve /v1 read instead of the generic dynamic-client Get/List, since only
// Steve splices the real token into the command/manifest fields. Its
// Descriptor mirrors resources.NewGet(k, ...) (same Name and args) and its
// flags are the generic get's without the wait flags - only execution is
// overridden.
func NewGetCRT(k kinds.Kind, roots engine.RootClientFactory) runtime.Command {
	return &getCRTCmd{k: k, roots: roots}
}

// Describe derives the row's Descriptor from the LIVE generic
// resources.NewGet(k, ...) rather than hand-duplicating its flag-generation
// logic: a future change to getFlags automatically propagates
// here too, instead of requiring this override to be updated in lockstep by
// discipline alone. resources.NewGet's Describe() is a pure function of k -
// it never touches its clients argument - so nil is safe to pass here.
func (c *getCRTCmd) Describe() runtime.Descriptor {
	d := resources.NewGet(c.k, nil).Describe()
	d.Flags = resources.WithoutWaitFlags(d.Flags)
	return d
}

func (c *getCRTCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	ns := in.String(resources.FlagNamespace)
	named := len(in.Args()) > 0 && in.Args()[0] != ""
	// The descriptor is the generic get's, so --selector arrives here too;
	// it is applied to the decoded list below, never as a Steve query.
	selector, err := resources.ParseListSelector(c.k.Name, named, in.String(resources.FlagSelector))
	if err != nil {
		return runtime.Result{}, err
	}
	root, err := c.roots.Root()
	if err != nil {
		return runtime.Result{}, err
	}

	if named {
		if ns == "" {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"--namespace is required to address a "+c.k.Name+" by name")
		}
		name := in.Args()[0]
		path := crtCollectionPath + "/" + ns + "/" + name
		body, err := root.GetJSON(ctx, path)
		if err != nil {
			return runtime.Result{}, err
		}
		obj, err := DecodeCRTObject(path, body)
		if err != nil {
			return runtime.Result{}, err
		}
		if fields := PlaceholderFields(obj); len(fields) > 0 {
			return runtime.Result{}, placeholderError(path, fields[0])
		}
		return runtime.Result{Kind: "ClusterRegistrationToken", Data: crtRows{crtRowFrom(obj)}}, nil
	}

	// NAMESPACE SEMANTICS: always GET the root collection and
	// filter the DECODED objects locally by metadata.namespace when
	// --namespace is set - deterministic, and avoids assuming Steve's
	// query-filter grammar.
	body, err := root.GetJSON(ctx, crtCollectionPath)
	if err != nil {
		return runtime.Result{}, err
	}
	views, err := DecodeCRTCollection(crtCollectionPath, body)
	if err != nil {
		return runtime.Result{}, err
	}
	rows := make(crtRows, 0, len(views))
	var warnings []string
	for i := range views {
		v := &views[i]
		if ns != "" && v.Namespace != ns {
			continue
		}
		if selector != nil && !selector.Matches(labels.Set(v.Labels)) {
			continue
		}
		// A bad row degrades in place - blank the offending
		// field(s) and warn - rather than aborting the entire list, which
		// would hide every healthy row behind one unspliced token.
		for _, field := range clearPlaceholderFields(v) {
			warnings = append(warnings, fmt.Sprintf(
				"registration token %s/%s: %s not formatted by the server; omitted",
				v.Namespace, v.Name, field))
		}
		rows = append(rows, crtRowFrom(*v))
	}
	return runtime.Result{Kind: "ClusterRegistrationTokenList", Data: rows, Warnings: warnings}, nil
}

var _ runtime.ReadCommand = (*getCRTCmd)(nil)

type refuseCRTPatchCmd struct {
	k kinds.Kind
}

// NewRefuseCRTPatch builds the patch override for the clusterregistrationtoken
// row: a VALIDATION refusal - the row is read-only surface, and patching
// registration tokens is not supported. Its Descriptor mirrors
// resources.NewPatch(k, ...) exactly (same Name, args, flags) so the row's
// schema output is unchanged; Plan refuses before ever touching
// the network.
func NewRefuseCRTPatch(k kinds.Kind) runtime.Command {
	return &refuseCRTPatchCmd{k: k}
}

// Describe derives the row's Descriptor from the LIVE generic
// resources.NewPatch(k, ...) rather than hand-duplicating its flag-
// generation logic, for the same reason as getCRTCmd.Describe
// above. resources.NewPatch's Describe() is a pure function of k - it never
// touches its clients/src arguments - so nil is safe to pass for both here.
func (c *refuseCRTPatchCmd) Describe() runtime.Descriptor {
	return resources.NewPatch(c.k, nil, nil).Describe()
}

func (c *refuseCRTPatchCmd) Plan(_ context.Context, _ runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
		c.k.Name+" is read-only: patching a clusterregistrationtoken is not supported; "+
			"registration tokens are consumed, not edited - create a new one instead")
}

var _ runtime.MutatingCommand = (*refuseCRTPatchCmd)(nil)
