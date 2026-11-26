package provisioning

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// cloudCredNamespace is the fixed namespace Rancher stores cloud
	// credentials in (norman proxy_store.go's cattle-global-data convention).
	cloudCredNamespace = "cattle-global-data"
	// cloudCredPrefix + a 5-char suffix is Rancher's cloud-credential secret
	// name pattern (norman proxy_store.go:440-446 + GenerateTypePrefix).
	cloudCredPrefix = "cc-"
	// displayNameAnnotation carries the user-facing name (rancher
	// credentialmapper.go:15-25); backfilled from the object name when absent.
	displayNameAnnotation = "field.cattle.io/name"
	// descriptionAnnotation carries the credential's description.
	descriptionAnnotation = "field.cattle.io/description"
	// credentialConfigMarker is the fixed substring every driver's data key
	// carries before its own config block (rancher credentialmapper.go:36-51;
	// literals validator.go:435, aks/handler.go:247, vsphere/handler.go:55,
	// cred/store.go:116) - the driver name is the key's prefix before this.
	credentialConfigMarker = "credentialConfig-"
)

// secretsGVR is the core/v1 Secrets resource cloud credentials are stored as.
var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// CloudCredRef is the resolved internal reference plus display metadata.
type CloudCredRef struct {
	// ID is the namespaced form the provisioning spec consumes:
	// "cattle-global-data:cc-xxxxx".
	ID          string `json:"id"`
	SecretName  string `json:"secretName"`  // "cc-xxxxx"
	DisplayName string `json:"displayName"` // annotation field.cattle.io/name (may equal SecretName for legacy entries)
	Driver      string `json:"driver"`      // derived from the <driver>credentialConfig- data-key prefix; "" if unidentifiable
}

// credentialLookup says how a command selects a cloud credential from the
// value the user typed. A function that resolves a credential takes it as an
// explicit parameter, so no call falls back to a default.
type credentialLookup int

const (
	// lookupForRead is the lookup of a command that only reads: the value is a
	// credential id or a display name.
	lookupForRead credentialLookup = iota + 1
	// lookupForChange is the lookup of a mutation that takes a cloud
	// credential: the value is a credential id only, the Secret name with or
	// without "cattle-global-data:".
	lookupForChange
)

// ResolveCloudCredential resolves nameOrID to exactly one credential for the
// command that calls it. With lookupForRead the value is an id (with or
// without the "cattle-global-data:" prefix) or a display name under the
// shared match rule; with lookupForChange it is an id only (see
// resolveCredentialObj). Reads Secret objects but retains ONLY key names +
// metadata - never a data value. The Kubernetes authorizer's refusal of the
// GET by id gets the hint that names get on secrets: the command reads the
// credential before it writes the object that uses it.
func ResolveCloudCredential(ctx context.Context, clients engine.ClusterClientFactory, nameOrID string, lookup credentialLookup) (CloudCredRef, string, error) {
	obj, warning, err := resolveCredentialObj(ctx, clients, nameOrID, "--cloud-credential", lookup)
	if err != nil {
		return CloudCredRef{}, "", engine.WithMissingGetHint(err, "command", "cloud-credential", secretsGVR, cloudCredNamespace, credentialSecretName(nameOrID), "")
	}
	return refFrom(obj), warning, nil
}

// credentialSecretName is the name of the Secret that nameOrID names. The
// namespaced id form (cattle-global-data:cc-...) names the same Secret as the
// name without the namespace. The hint of a refused GET must name what the GET
// asked for; the GET and the hint both take the name from here.
func credentialSecretName(nameOrID string) string {
	return strings.TrimPrefix(nameOrID, cloudCredNamespace+":")
}

// resolveCredentialObj is ResolveCloudCredential's shared innards, returning
// the raw Secret so the read command can also derive Age from
// creationTimestamp without a second resolve pass; flag is "" for a
// positional value. Every value pays one List: an id carries no type
// signal, and the match needs every credential's display name.
//
// With lookupForRead, an id takes precedence over another credential's display
// name and the warning names the others; two display-name matches are
// AMBIGUOUS_SELECTION; no match is NOT_FOUND with candidates, after one GET
// by id for a credential the List did not carry.
//
// With lookupForChange the value selects a credential only by id and the
// result has no warning. A value that is only a display name is NOT_FOUND,
// with the credentials of that display name as candidates. A value typed
// without "cattle-global-data:" that is the id of one credential and the
// display name of another is AMBIGUOUS_SELECTION naming every one; the
// prefixed form selects the Secret of that name. Candidates are written
// with the prefix, which never resolves as ambiguous.
//
// Any other lookup, the zero value included, is an error before any request.
//
// Rancher grants a credential's creator its own Secret by name only, so a
// List refused with RBAC falls back to the GET by id when the value is a
// Secret name: a credential is the result, with no warning (the caller has no
// permission to list other credentials to compare with); a 404 or a Secret that
// is not a credential is NOT_FOUND with no candidates; any other GET error is
// returned. Never inspects `data` values - see the package doc.
func resolveCredentialObj(ctx context.Context, clients engine.ClusterClientFactory, nameOrID string, flag clierr.CommandWord, lookup credentialLookup) (*unstructured.Unstructured, string, error) {
	switch lookup {
	case lookupForRead, lookupForChange:
	default:
		return nil, "", clierr.New(clierr.CodeGeneric, "internal error: this command did not say how to select a cloud credential")
	}
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return nil, "", err
	}
	name := credentialSecretName(nameOrID)
	t := engine.MatchText{Noun: "cloud credential", Plural: "cloud credentials", Flag: flag,
		NotFound: "cloud credential not found: " + nameOrID, NotFoundHint: clierr.NewCommand("get cloud-credential")}

	list, err := client.List(ctx, secretsGVR, cloudCredNamespace, "")
	if err != nil {
		if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeRBAC || len(validation.IsDNS1123Subdomain(name)) > 0 {
			return nil, "", err
		}
		obj, gerr := getCredential(ctx, client, name, clierr.New(clierr.CodeNotFound, t.NotFound))
		return obj, "", gerr
	}
	candidates := credentialObjects(list)
	if lookup == lookupForChange {
		obj, err := selectCredentialByID(ctx, client, nameOrID, name, candidates, t)
		return obj, "", err
	}

	ids := make([]string, 0, len(candidates))
	displayByID := map[string]string{}
	objByID := map[string]*unstructured.Unstructured{}
	cands := make([]clierr.Candidate, 0, len(candidates))
	for _, obj := range candidates {
		id := obj.GetName()
		display := displayNameOf(obj)
		ids = append(ids, id)
		displayByID[id] = display
		objByID[id] = obj
		cands = append(cands, clierr.Candidate{ID: id, Name: display})
	}

	id, warning, err := engine.MatchOne(name, ids, func(id string) string { return displayByID[id] }, cands, t)
	if err == nil {
		return objByID[id], warning, nil
	}
	if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeNotFound || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return nil, "", err
	}
	// A credential the List did not carry yet: one GET by id.
	obj, gerr := getCredential(ctx, client, name, err)
	return obj, "", gerr
}

// selectCredentialByID is resolveCredentialObj's lookupForChange: typed is
// the value as typed and name the Secret name it has after the optional
// prefix. The credential is the Secret named name, from candidates or, when
// the List did not carry it yet, from one GET. A value typed without the
// prefix that is also another credential's display name (compared exactly) is
// AMBIGUOUS_SELECTION: a credential's display name is its own Secret name
// when it has no annotation, which is no collision.
func selectCredentialByID(ctx context.Context, client engine.ClusterClient, typed, name string, candidates []*unstructured.Unstructured, t engine.MatchText) (*unstructured.Unstructured, error) {
	var byID *unstructured.Unstructured
	var sameDisplay []*unstructured.Unstructured
	for _, obj := range candidates {
		switch {
		case obj.GetName() == name:
			byID = obj
		case displayNameOf(obj) == name:
			sameDisplay = append(sameDisplay, obj)
		}
	}
	if byID == nil {
		notFound := clierr.New(clierr.CodeNotFound, t.NotFound).
			WithCandidatesMatchesFirst(prefixedCandidates(candidates), name).WithHintCommand(t.NotFoundHint)
		if len(sameDisplay) > 0 {
			isDisplayName := name + " is a display name"
			notFound = clierr.New(clierr.CodeNotFound, t.NotFound+"; the command takes the credential id, and "+isDisplayName).
				WithCandidatesMatchesFirst(prefixedCandidates(sameDisplay), name).WithHintCommand(t.NotFoundHint)
		}
		if len(validation.IsDNS1123Subdomain(name)) > 0 {
			return nil, notFound
		}
		// A credential the List did not carry yet: one GET by id.
		obj, err := getCredential(ctx, client, name, notFound)
		if err != nil {
			return nil, err
		}
		byID = obj
	}
	if typed != name || len(sameDisplay) == 0 {
		return byID, nil
	}
	matches := append([]*unstructured.Unstructured{byID}, sameDisplay...)
	return nil, clierr.New(clierr.CodeAmbiguousSelection,
		fmt.Sprintf("%d %s match %s: one has it as its id and the others have it as their display name; to use one, give its id as %s<id>",
			len(matches), t.Plural, clierr.ShellQuote(typed), cloudCredNamespace+":")).
		WithCandidates(prefixedCandidates(matches))
}

// prefixedCandidates lists objs as candidates, sorted by id, each id written
// with "cattle-global-data:" so a candidate can be passed as typed.
func prefixedCandidates(objs []*unstructured.Unstructured) []clierr.Candidate {
	out := make([]clierr.Candidate, 0, len(objs))
	for _, obj := range objs {
		out = append(out, clierr.Candidate{ID: cloudCredNamespace + ":" + obj.GetName(), Name: displayNameOf(obj)})
	}
	slices.SortFunc(out, func(a, b clierr.Candidate) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// getCredential reads the Secret name by id. A missing Secret, or one that
// is not a credential, is notFound, which never names the Secret behind the
// credential; any other GET error is returned as is.
func getCredential(ctx context.Context, client engine.ClusterClient, name string, notFound error) (*unstructured.Unstructured, error) {
	obj, err := client.Get(ctx, secretsGVR, cloudCredNamespace, name)
	switch {
	case clierr.IsNotFound(err):
		return nil, notFound
	case err != nil:
		return nil, err
	case len(credentialObjects(&unstructured.UnstructuredList{Items: []unstructured.Unstructured{*obj}})) == 0:
		return nil, notFound
	}
	return obj, nil
}

// storedCredential is the Secret readStoredCredential reads for patch and delete: the
// reference and the metadata the write needs. The annotations go only into the
// body of an update and, for the name and the description, the preview's diff;
// no `data` value is kept.
type storedCredential struct {
	ref             CloudCredRef
	resourceVersion string
	uid             string
	annotations     map[string]string
}

// readStoredCredential resolves nameOrID as ResolveCloudCredential does under
// lookup, and returns the Secret's resourceVersion, uid and annotations with
// the reference. It reads key names and metadata only. The result has no
// warning: patch and delete select a credential by id only. verb is the verb
// of the write, in the hint of a refused GET.
func readStoredCredential(ctx context.Context, clients engine.ClusterClientFactory, nameOrID, verb string, lookup credentialLookup) (storedCredential, error) {
	obj, _, err := resolveCredentialObj(ctx, clients, nameOrID, "", lookup)
	if err != nil {
		return storedCredential{}, engine.WithMissingGetHint(err, "command", "cloud-credential", secretsGVR, cloudCredNamespace, credentialSecretName(nameOrID), verb)
	}
	return storedCredential{
		ref:             refFrom(obj),
		resourceVersion: obj.GetResourceVersion(),
		uid:             string(obj.GetUID()),
		annotations:     obj.GetAnnotations(),
	}, nil
}

// credentialObjects filters list to credential-shaped secrets: named with the
// cc- prefix, or whose first data key in sorted order that contains
// credentialConfig- has a driver name in front of it (the
// cattle.io/creator=norman label is corroborating in real Rancher data, not
// required here).
func credentialObjects(list *unstructured.UnstructuredList) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for i := range list.Items {
		obj := &list.Items[i]
		if strings.HasPrefix(obj.GetName(), cloudCredPrefix) || driverFromDataKeys(obj) != "" {
			out = append(out, obj)
		}
	}
	return out
}

// displayNameOf reads the field.cattle.io/name annotation, backfilling from
// the object's own name when absent (rancher credentialmapper.go:15-25).
func displayNameOf(obj *unstructured.Unstructured) string {
	if display := obj.GetAnnotations()[displayNameAnnotation]; display != "" {
		return display
	}
	return obj.GetName()
}

// refFrom builds the public CloudCredRef from a Secret object, reading ONLY
// metadata and `data` KEY NAMES - never a `data` value.
func refFrom(obj *unstructured.Unstructured) CloudCredRef {
	name := obj.GetName()
	return CloudCredRef{
		ID:          cloudCredNamespace + ":" + name,
		SecretName:  name,
		DisplayName: displayNameOf(obj),
		Driver:      driverFromDataKeys(obj),
	}
}

// driverFromDataKeys scans `data` KEY NAMES ONLY - never a value - for the
// first key containing "credentialConfig-"; Driver is the prefix before that
// marker (rancher credentialmapper.go:36-51). Keys are sorted first so the
// choice is deterministic if a secret somehow carries more than one.
func driverFromDataKeys(obj *unstructured.Unstructured) string {
	raw, ok := obj.Object["data"]
	if !ok {
		return ""
	}
	data, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if before, _, ok := strings.Cut(k, credentialConfigMarker); ok {
			return before
		}
	}
	return ""
}

// now is Age's clock, overridden in tests for a deterministic duration -
// the same seam the kind-table renderer's AGE column uses.
var now = time.Now

// ageFrom renders metadata.creationTimestamp with duration.HumanDuration -
// the formatter kubectl's own AGE column uses (translateTimestampSince).
// A missing/unparseable timestamp renders blank, a deliberate house
// deviation from kubectl's own "<unknown>" placeholder: no fabricated
// values, no placeholders. (This function previously used
// ShortHumanDuration, which is the wrong formatter, and "<unknown>" for
// absent - neither is kept.)
func ageFrom(obj *unstructured.Unstructured) string {
	ts := obj.GetCreationTimestamp()
	if ts.IsZero() {
		return ""
	}
	return duration.HumanDuration(now().Sub(ts.Time))
}

// cloudCredRow is one `get cloud-credential` row: the resolved ref plus a
// display-only Age. creationTimestamp is metadata, not a `data` value, so
// adding it here does not weaken the hygiene guarantee.
type cloudCredRow struct {
	CloudCredRef
	Age string `json:"age"`
}

func rowFrom(obj *unstructured.Unstructured) cloudCredRow {
	return cloudCredRow{CloudCredRef: refFrom(obj), Age: ageFrom(obj)}
}

// cloudCredRows is `get cloud-credential`'s payload; it implements
// output.Tabular so `-o table` shows NAME/DISPLAY NAME/DRIVER/AGE - secret
// material never appears in a column.
type cloudCredRows []cloudCredRow

func (r cloudCredRows) Columns() []string { return []string{"NAME", "DISPLAY NAME", "DRIVER", "AGE"} }

func (r cloudCredRows) Rows() [][]string {
	out := make([][]string, 0, len(r))
	for _, row := range r {
		driver := row.Driver
		if driver == "" {
			driver = "<none>"
		}
		out = append(out, []string{row.SecretName, row.DisplayName, driver, row.Age})
	}
	return out
}

type getCloudCredCmd struct {
	clients engine.ClusterClientFactory
}

// NewGetCloudCredential builds `get cloud-credential` (read-only listing:
// SecretName, DisplayName, Driver, Age - secret material NEVER shown).
func NewGetCloudCredential(clients engine.ClusterClientFactory) runtime.Command {
	return &getCloudCredCmd{clients: clients}
}

func (c *getCloudCredCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "get cloud-credential",
		Usage:       "List cloud credentials or show one (secret values never shown)",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Auth:        true,
		Shape:       runtime.ShapeTabular,
	}
}

func (c *getCloudCredCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if len(in.Args()) > 0 && in.Args()[0] != "" {
		obj, warning, err := resolveCredentialObj(ctx, c.clients, in.Args()[0], "", lookupForRead)
		if err != nil {
			return runtime.Result{}, err
		}
		res := runtime.Result{Kind: "CloudCredential", Data: cloudCredRows{rowFrom(obj)}}
		if warning != "" {
			res.Warnings = []string{warning}
		}
		return res, nil
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	list, err := client.List(ctx, secretsGVR, cloudCredNamespace, "")
	if err != nil {
		return runtime.Result{}, err
	}
	objs := credentialObjects(list)
	rows := make(cloudCredRows, 0, len(objs))
	for _, obj := range objs {
		rows = append(rows, rowFrom(obj))
	}
	return runtime.Result{Kind: "CloudCredentialList", Data: rows}, nil
}

var _ runtime.ReadCommand = (*getCloudCredCmd)(nil)
