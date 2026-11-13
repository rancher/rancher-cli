package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

const (
	scrubOutsideAnnotation = "example.invalid/outside"
	scrubOutsideSpec       = "outsideSpec"
	scrubCluster           = "c-x"
	scrubProjectID         = "p-1"
	scrubObjectName        = "obj1"
)

// scrubRequiredPaths are the paths a kind's redaction must hide, named here
// apart from the kind row so a row that loses a path fails the table: the
// value an ext token or kubeconfig create returns once, the credential
// fields of the v3 token and the user, and the Helm values and template
// values of the helmop, bundle and bundledeployment kinds (in the helmop and
// bundle paths, a "*" wildcard segment stands for every target).
var scrubRequiredPaths = map[string][][]string{
	"token":           {{"status", "bearerToken"}, {"status", "value"}},
	"kubeconfig":      {{"status", "value"}},
	"managementtoken": {{"token"}},
	"user":            {{"password"}},
	"helmop": {
		{"spec", "helm", "values"}, {"spec", "helm", "templateValues"},
		{"spec", "targets", "*", "helm", "values"}, {"spec", "targets", "*", "helm", "templateValues"},
	},
	"bundle": {
		{"spec", "helm", "values"}, {"spec", "helm", "templateValues"},
		{"spec", "targets", "*", "helm", "values"}, {"spec", "targets", "*", "helm", "templateValues"},
	},
	"bundledeployment": {
		{"spec", "options", "helm", "values"}, {"spec", "options", "helm", "templateValues"},
		{"spec", "stagedOptions", "helm", "values"}, {"spec", "stagedOptions", "helm", "templateValues"},
	},
}

// plantSet is what the fake plants in its answers for one row: every value
// the row's redaction hides (each name also in a second letter case), and
// the values outside that set that must stay in the result.
type plantSet struct {
	// fragment is merged into every answer for the row's kind.
	fragment map[string]any
	// values are the planted values, one per hidden location.
	values []string
	// outside are the values the redaction leaves alone.
	outside []string
	// pathValues maps a dotted path of the first-case planting to its value,
	// for a path with no wildcard segment.
	pathValues map[string]string
	// items is the length of the list planted at each wildcard segment.
	items int
	// itemValues[i] maps the dotted path of the first-case planting ("*" for
	// the wildcard segment) to the value planted in list item i.
	itemValues []map[string]string
	// payload is the Secret payload of a Sensitive row, in the form each
	// value is planted; the values are also in values.
	payload []payloadValue
	next    int
}

// payloadValue is one planted Secret payload value: the value and the
// string the answer carries for it (the value, or its base64 form).
type payloadValue struct {
	value, served string
}

// value returns the next planted value: single-line, no spaces, with a
// double quote in every third and a "<" in every third.
func (p *plantSet) value() string {
	i := p.next
	p.next++
	letter := string(rune('A' + i%26))
	switch i % 3 {
	case 1:
		return fmt.Sprintf(`s3cret"%s%d`, letter, i)
	case 2:
		return fmt.Sprintf("s3cret<%s%d", letter, i)
	}
	return fmt.Sprintf("s3cret%s%d", letter, i)
}

func setNested(m map[string]any, path []string, v any) {
	setNestedItem(m, path, 0, v)
}

// setNestedItem sets v at path in m, building the maps along it. At a "*"
// segment it builds a list of maps long enough to have an item at index
// `item` and continues in that item. It panics if path ends in "*".
func setNestedItem(m map[string]any, path []string, item int, v any) {
	if path[len(path)-1] == "*" {
		panic(fmt.Sprintf("setNestedItem: path %v ends in a wildcard segment", path))
	}
	for i := 0; i < len(path)-1; i++ {
		k := path[i]
		if path[i+1] == "*" {
			list, _ := m[k].([]any)
			for len(list) <= item {
				list = append(list, map[string]any{})
			}
			m[k] = list
			child, ok := list[item].(map[string]any)
			if !ok {
				child = map[string]any{}
				list[item] = child
			}
			m = child
			i++
			continue
		}
		child, ok := m[k].(map[string]any)
		if !ok {
			child = map[string]any{}
			m[k] = child
		}
		m = child
	}
	m[path[len(path)-1]] = v
}

func upperPath(path []string) []string {
	out := make([]string, len(path))
	for i, s := range path {
		out[i] = strings.ToUpper(s)
	}
	return out
}

// plantValue plants one value at path and another at the upper-case path.
// A path with a wildcard segment plants a list whose length is p.items; each
// item gets its own values.
func (p *plantSet) plantValue(path []string) {
	if slices.Contains(path, "*") {
		dotted := strings.Join(path, ".")
		for i := range p.items {
			v := p.value()
			setNestedItem(p.fragment, path, i, v)
			p.itemValues[i][dotted] = v
			v2 := p.value()
			setNestedItem(p.fragment, upperPath(path), i, v2)
			p.values = append(p.values, v, v2)
		}
		return
	}
	v := p.value()
	setNested(p.fragment, path, v)
	p.pathValues[strings.Join(path, ".")] = v
	p.values = append(p.values, v)
	v2 := p.value()
	setNested(p.fragment, upperPath(path), v2)
	p.values = append(p.values, v2)
}

// plantOutsideItems plants a value the redaction leaves alone in each list
// item that the first wildcard segment of a path matches: outsideList, the
// list's number, Item and the item's index, at the name scrubOutsideSpec,
// beside the hidden values of the item.
func (p *plantSet) plantOutsideItems(paths [][]string) {
	var done []string
	for _, path := range paths {
		i := slices.Index(path, "*")
		if i < 0 {
			continue
		}
		prefix := append(slices.Clone(path[:i+1]), scrubOutsideSpec)
		dotted := strings.Join(prefix, ".")
		if slices.Contains(done, dotted) {
			continue
		}
		done = append(done, dotted)
		for item := range p.items {
			v := fmt.Sprintf("outsideList%dItem%d", len(done)-1, item)
			setNestedItem(p.fragment, prefix, item, v)
			p.outside = append(p.outside, v)
		}
	}
}

// hiddenAnnotationNames are the annotations every row that hides anything
// hides, written out so that dropping one from kinds.HiddenAnnotations fails
// the tests that plant them.
var hiddenAnnotationNames = []string{kinds.LastAppliedAnnotation, "objectset.rio.cattle.io/applied"}

// newPlantSet plants what the redaction of kind k hides, with a list of
// length `items` at each wildcard segment. A nil k plants only the outside
// values.
func newPlantSet(k *kinds.Kind, items int) *plantSet {
	p := &plantSet{fragment: map[string]any{}, pathValues: map[string]string{}, items: items}
	for range items {
		p.itemValues = append(p.itemValues, map[string]string{})
	}
	hides := false
	if k != nil {
		r := k.Redaction()
		hides = r.Hides()
		if k.Sensitive {
			v1, v2, v3, v4 := p.value(), p.value(), p.value(), p.value()
			b1, b3 := base64.StdEncoding.EncodeToString([]byte(v1)), base64.StdEncoding.EncodeToString([]byte(v3))
			setNested(p.fragment, []string{"data", "k1"}, b1)
			setNested(p.fragment, []string{"stringData", "k2"}, v2)
			setNested(p.fragment, []string{"Data", "k3"}, b3)
			setNested(p.fragment, []string{"STRINGDATA", "k4"}, v4)
			p.values = append(p.values, v1, v2, v3, v4)
			p.payload = append(p.payload, payloadValue{v1, b1}, payloadValue{v2, v2}, payloadValue{v3, b3}, payloadValue{v4, v4})
		}
		paths := slices.Clone(k.RedactPaths)
		for _, path := range scrubRequiredPaths[k.Key] {
			if !slices.ContainsFunc(paths, func(q []string) bool { return slices.Equal(q, path) }) {
				paths = append(paths, path)
			}
		}
		for _, path := range paths {
			p.plantValue(path)
		}
		p.plantOutsideItems(paths)
		if k.GVR.Group == kinds.MachineConfigGroup {
			if f, ok := kinds.MachineConfigFieldsFor(k.Name); ok {
				for _, name := range f.Secret {
					p.plantValue([]string{name})
				}
			}
		}
		if hides {
			for _, name := range hiddenAnnotationNames {
				p.plantValue([]string{"metadata", "annotations", name})
			}
		}
	}
	setNested(p.fragment, []string{"metadata", "annotations", scrubOutsideAnnotation}, "outsideA")
	setNested(p.fragment, []string{"spec", scrubOutsideSpec}, "outsideB")
	p.outside = append(p.outside, "outsideA", "outsideB")
	if k != nil && !hides {
		// A kind that hides nothing shows the annotations by design.
		for i, name := range hiddenAnnotationNames {
			v := fmt.Sprintf("applied%d", i)
			setNested(p.fragment, []string{"metadata", "annotations", name}, v)
			p.outside = append(p.outside, v)
		}
	}
	return p
}

// plantItemCounts returns the list lengths the rows of kind key run with: 1
// and 2 when a path the kind must hide has a wildcard segment (code that
// handles only the first item fails with 2), and 1 otherwise.
func plantItemCounts(key string) []int {
	k, ok := kinds.ByKey(key)
	if !ok {
		return []int{1}
	}
	paths := append(slices.Clone(k.RedactPaths), scrubRequiredPaths[k.Key]...)
	if slices.ContainsFunc(paths, func(p []string) bool { return slices.Contains(p, "*") }) {
		return []int{1, 2}
	}
	return []int{1}
}

// scrubSubtestName is the subtest name of a row in a format, with `items=N`
// added when the row runs with more than one list length.
func scrubSubtestName(name, format string, items int, counts []int) string {
	if len(counts) > 1 {
		return fmt.Sprintf("%s %s items=%d", name, format, items)
	}
	return name + " " + format
}

// needles are the forms of a planted value the fake's answers are scanned
// for: the value and its base64 form.
func needles(values []string) []string {
	var out []string
	for _, v := range values {
		out = append(out, v, base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return out
}

// mergeMissing adds every entry of src to dst that dst does not have.
func mergeMissing(dst, src map[string]any) {
	for k, v := range src {
		cur, ok := dst[k]
		if !ok {
			dst[k] = deepCopyValue(v)
			continue
		}
		curMap, ok1 := cur.(map[string]any)
		srcMap, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			mergeMissing(curMap, srcMap)
		}
	}
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, c := range t {
			out[k] = deepCopyValue(c)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, c := range t {
			out[i] = deepCopyValue(c)
		}
		return out
	}
	return v
}

// scrubFake is the ClusterClient and ClusterClientFactory of the result
// table. It answers every read and write with the object a server would
// hold, with the values the row's redaction hides planted in each answer
// for the row's kind. One fake serves one row and format; it opens no
// socket.
type scrubFake struct {
	kind     *kinds.Kind
	plant    *plantSet
	needles  []string
	mu       sync.Mutex
	served   int
	updates  int
	oidc     *oidcFixture
	decorate func(obj *unstructured.Unstructured)
	// firstGet changes the response to the first Get of the row's kind only.
	firstGet func(obj *unstructured.Unstructured)
	// sent are copies of the objects the real (not dry-run) updates received.
	sent []map[string]any
	list func(gvr schema.GroupVersionResource) []unstructured.Unstructured
	// lists are the List calls received, in order.
	lists []scrubListCall
}

// scrubListCall is one List the fake received: the resource and the label
// selector it was asked for.
type scrubListCall struct {
	gvr      schema.GroupVersionResource
	selector string
}

// oidcFixture is the stateful OIDCClient and backing Secret of the
// oidc-client rows: the client is ready at the first read, and a real
// update moves it to its after state.
type oidcFixture struct {
	mode string
	// before and after are the Secret values by key before and after the
	// real update.
	before, after map[string]string
	// keys are the OIDCClient status.clientSecrets keys before and after.
	keysBefore, keysAfter []string
}

func (f *scrubFake) isKind(gvr schema.GroupVersionResource) bool {
	return f.kind != nil && gvr.Group == f.kind.GVR.Group && gvr.Resource == f.kind.GVR.Resource
}

func (f *scrubFake) base(gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	version := gvr.Version
	if version == "" {
		version = "v1"
	}
	apiVersion := version
	if gvr.Group != "" {
		apiVersion = gvr.Group + "/" + version
	}
	kindName := gvr.Resource
	if f.kind != nil && f.isKind(gvr) {
		kindName = f.kind.K8sKind
	}
	meta := map[string]any{"name": name, "resourceVersion": "5"}
	if ns != "" {
		meta["namespace"] = ns
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kindName, "metadata": meta}}
}

// scan counts an answer that carries a value the row must not show.
func (f *scrubFake) scan(obj *unstructured.Unstructured) {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	walkStrings(obj.Object, func(s string) {
		for _, n := range f.needles {
			if strings.Contains(s, n) {
				found = true
			}
		}
	})
	if found {
		f.served++
	}
}

// answer plants the row's values in an answer for the row's kind and counts
// it when it carries a value the row must not show.
func (f *scrubFake) answer(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) *unstructured.Unstructured {
	if f.isKind(gvr) {
		mergeMissing(obj.Object, f.plant.fragment)
		if f.decorate != nil {
			f.decorate(obj)
		}
	}
	f.scan(obj)
	return obj
}

// walkStrings calls visit with every map key and string value of v.
func walkStrings(v any, visit func(string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			visit(k)
			walkStrings(c, visit)
		}
	case []any:
		for _, c := range t {
			walkStrings(c, visit)
		}
	case string:
		visit(t)
	}
}

func (f *scrubFake) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if f.oidc != nil {
		switch gvr.Resource {
		case "oidcclients":
			return f.answer(gvr, f.oidcClient(name)), nil
		case "secrets":
			return f.oidcSecret(ns, name), nil
		}
	}
	switch {
	case gvr.Resource == "projects" && gvr.Group == "management.cattle.io":
		obj := f.base(gvr, ns, name)
		_ = unstructured.SetNestedField(obj.Object, ns+"-"+name, "status", "backingNamespace")
		return f.answer(gvr, obj), nil
	case gvr.Resource == "secrets" && !f.isKind(gvr):
		return f.base(gvr, ns, name), nil
	}
	obj := f.base(gvr, ns, name)
	if gvr.Resource == "roletemplates" {
		obj.Object["context"] = "project"
	}
	out := f.answer(gvr, obj)
	if f.isKind(gvr) {
		f.mu.Lock()
		hook := f.firstGet
		f.firstGet = nil
		f.mu.Unlock()
		if hook != nil {
			hook(out)
		}
	}
	return out, nil
}

func (f *scrubFake) oidcClient(name string) *unstructured.Unstructured {
	f.mu.Lock()
	keys := f.oidc.keysBefore
	if f.updates > 0 {
		keys = f.oidc.keysAfter
	}
	f.mu.Unlock()
	obj := f.base(schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "oidcclients"}, "", name)
	secrets := map[string]any{}
	for _, k := range keys {
		secrets[k] = map[string]any{"createdAt": "2026-01-01T00:00:00Z"}
	}
	_ = unstructured.SetNestedField(obj.Object, "client-id-1", "status", "clientID")
	_ = unstructured.SetNestedField(obj.Object, secrets, "status", "clientSecrets")
	return obj
}

func (f *scrubFake) oidcSecret(ns, name string) *unstructured.Unstructured {
	f.mu.Lock()
	values := f.oidc.before
	if f.updates > 0 {
		values = f.oidc.after
	}
	f.mu.Unlock()
	obj := f.base(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, ns, name)
	data := map[string]any{}
	for k, v := range values {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	obj.Object["data"] = data
	f.scan(obj)
	return obj
}

func (f *scrubFake) List(_ context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	f.mu.Lock()
	f.lists = append(f.lists, scrubListCall{gvr: gvr, selector: selector})
	f.mu.Unlock()
	out := &unstructured.UnstructuredList{}
	if f.list != nil {
		for _, item := range f.list(gvr) {
			out.Items = append(out.Items, *f.answer(gvr, item.DeepCopy()))
		}
		return out, nil
	}
	if gvr.Resource == "namespaces" {
		item := f.base(gvr, "", "team-a")
		out.Items = append(out.Items, *item)
	}
	return out, nil
}

func (f *scrubFake) Create(_ context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	out := obj.DeepCopy()
	if out.GetName() == "" {
		if gn := out.GetGenerateName(); gn != "" {
			out.SetName(gn + "x1")
		} else {
			out.SetName("preview")
		}
	}
	if ns != "" && out.GetNamespace() == "" {
		out.SetNamespace(ns)
	}
	switch gvr.Resource {
	case "selfusers":
		_ = unstructured.SetNestedField(out.Object, "u-self1", "status", "userID")
	case "passwordchangerequests":
		_ = unstructured.SetNestedSlice(out.Object, []any{map[string]any{"type": "PasswordUpdated", "status": "True"}}, "status", "conditions")
	}
	return f.answer(gvr, out), nil
}

func (f *scrubFake) Update(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if !dryRun {
		f.mu.Lock()
		f.updates++
		f.sent = append(f.sent, obj.DeepCopy().Object)
		f.mu.Unlock()
	}
	return f.answer(gvr, obj.DeepCopy()), nil
}

func (f *scrubFake) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (f *scrubFake) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", errors.New("scrubFake: unexpected logs")
}

func (f *scrubFake) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("scrubFake: unexpected review")
}

func (f *scrubFake) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("scrubFake: unexpected discovery")
}

func (f *scrubFake) Cluster(context.Context, string) (engine.ClusterClient, error) { return f, nil }

func (f *scrubFake) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	return f, nil
}

func (f *scrubFake) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

// scrubBase serves the server base URL of create oidc-client; the discovery
// document is never reachable, so the command degrades to the static issuer.
type scrubBase struct{}

func (scrubBase) ServerBaseURL() (string, error) { return "https://rancher.invalid", nil }

// scrubNamer names the session token of delete management-token.
type scrubNamer struct{ name string }

func (n scrubNamer) ActiveTokenID() (string, error) { return n.name, nil }

// scrubProjects resolves a typed --project to the row's project.
type scrubProjects struct{}

func (scrubProjects) ResolveProject(_ context.Context, _ runtime.Input, _ string) (runtime.Project, string, error) {
	return runtime.Project{
		Cluster: scrubCluster, ID: scrubProjectID, DisplayName: "web",
		Namespaces: func(context.Context) (map[string]bool, error) {
			return map[string]bool{"ns1": true, "team-a": true, "team-b": true}, nil
		},
	}, "", nil
}

// scrubAllowAll is a capability checker that allows every command.
type scrubAllowAll struct{}

func (scrubAllowAll) Check(context.Context, runtime.CapabilityRequirement) runtime.CapabilityDecision {
	return runtime.CapabilityDecision{Outcome: runtime.CapabilityProceed}
}

// rowClass is how a row's result relates to the committed object.
type rowClass string

const (
	// classEcho: the result is the committed object, scrubbed.
	classEcho rowClass = "object-echoing"
	// classNames: the result is built from names and holds no value of the
	// object.
	classNames rowClass = "built from names"
	// classOnce: the result shows one secret on purpose, once.
	classOnce rowClass = "once-only"
)

// scrubRow is one mutating command (or one input of a command with several)
// of the result table.
type scrubRow struct {
	// command is the manifest name of the command.
	command string
	// variant names one input of a command with several rows.
	variant string
	// kind is the key of the kind row whose redaction the answers plant for;
	// "" for a command whose objects have no kind row.
	kind string
	// class is the row's class. A row refused at Plan is classEcho: the
	// refusal is its refused field.
	class rowClass
	// reason says why a classNames row cannot show a hidden value, or which
	// value a classOnce row shows on purpose.
	reason string
	// format overrides json and yaml.
	formats []string
	input   func(ps *plantSet) fieldCoverageInput
	// manifest is the file create resource reads.
	manifest func(ps *plantSet) string
	setup    func(f *scrubFake, ps *plantSet)
	// namer is the session token name of the row.
	namer string
	// intended is the one value a class c row shows on purpose, found in
	// the plant set by its dotted path, or the literal when set by setup.
	intendedPath string
	intended     func(f *scrubFake, ps *plantSet) string
	// extra are values the row sends or echoes that the plant set does not
	// hold; they must be absent from the output.
	extra func(ps *plantSet) []string
	// refused is the text of the refusal a row ends in; "" for a success.
	refused string
	// shown returns the planted Secret payload values the row shows on
	// purpose in a format; the row must show them and the other planted
	// values stay hidden. nil for a row that shows none.
	shown func(ps *plantSet, format string) []payloadValue
	// dryRun runs the row as a dry run, which prints the Effect and its diff
	// instead of the result of the apply.
	dryRun bool
	// check makes extra assertions for the row, beyond the scan for hidden
	// values; nil for none.
	check func(t *testing.T, format string, run scrubRun)
	// deps builds the command's dependencies in place of the in-process fake,
	// for a command that reads through the link, index and action clients the
	// fake does not serve. It starts its own server and plants the row's
	// values through the server's plant hook; the runner fills in src from the
	// row's input when deps leaves it nil. nil for a row served by the fake.
	deps func(t *testing.T, f *scrubFake, ps *plantSet) runDeps
	// wrap, when set, wraps the command the runner builds, for a test that
	// reads what the command hands the pipeline.
	wrap func(runtime.Command) runtime.Command
}

func (r scrubRow) name() string {
	if r.variant == "" {
		return r.command
	}
	return r.command + " (" + r.variant + ")"
}

func scrubKindRow(t *testing.T, key string) *kinds.Kind {
	t.Helper()
	if key == "" {
		return nil
	}
	k, ok := kinds.ByKey(key)
	require.True(t, ok, key)
	return &k
}

func in(args []string, values map[string]string) func(*plantSet) fieldCoverageInput {
	return func(*plantSet) fieldCoverageInput {
		return fieldCoverageInput{args: args, values: values}
	}
}

// provisioningClusterWithPools gives a provisioning cluster one machine
// pool, with autoscaling bounds when autoscaling is set.
func provisioningClusterWithPools(autoscaling bool) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		if _, found, _ := unstructured.NestedSlice(obj.Object, "spec", "rkeConfig", "machinePools"); found {
			return
		}
		pool := map[string]any{
			"name": "pool1", "quantity": int64(1), "etcdRole": true, "controlPlaneRole": true, "workerRole": true,
			"machineConfigRef": map[string]any{"kind": "DigitaloceanConfig", "name": "cfg"},
		}
		if autoscaling {
			pool["autoscalingMinSize"] = int64(1)
			pool["autoscalingMaxSize"] = int64(3)
		}
		_ = unstructured.SetNestedSlice(obj.Object, []any{pool}, "spec", "rkeConfig", "machinePools")
	}
}

// generatedScrubRows are the patch and delete rows, one per command
// buildEntries generates from the kind table and its overrides.
func generatedScrubRows() []scrubRow {
	ov := kindOverrides()
	var rows []scrubRow
	for _, k := range kinds.All() {
		o := ov[k.Key]
		routing := map[string]string{"cluster": scrubCluster, "namespace": "ns1"}
		patch := scrubRow{
			command: "patch " + k.Name, kind: k.Key, class: classEcho,
			input: func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{
					args:   []string{scrubObjectName},
					values: map[string]string{"cluster": scrubCluster, "namespace": "ns1", "type": "merge", "patch": `{"metadata":{"labels":{"scrub":"1"}}}`},
				}
			},
		}
		if _, refused := o.refusals["patch"]; refused {
			patch.refused = map[string]string{
				"managementtoken":          "patch is not supported",
				"clusterregistrationtoken": "is read-only",
			}[k.Key]
		}
		if k.NoPatchReason != "" {
			patch.refused = "patch is not supported"
		}
		if k.Key == "provisioningcluster" {
			patch.setup = func(f *scrubFake, _ *plantSet) { f.decorate = provisioningClusterWithPools(false) }
		}
		rows = append(rows, patch)
		if !k.Deletable && !delOverrideOnNonDeletable(k, o) {
			continue
		}
		del := scrubRow{
			command: "delete " + k.Name, kind: k.Key, class: classNames,
			reason: "the result is {deleted: name}: it never holds the object, which the command reads only for its resourceVersion",
			input: func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{args: []string{scrubObjectName}, values: routing}
			},
		}
		switch k.Key {
		case "managementtoken":
			del.namer = scrubObjectName
			del.setup = func(f *scrubFake, _ *plantSet) {
				f.decorate = func(obj *unstructured.Unstructured) { _ = unstructured.SetNestedField(obj.Object, true, "isDerived") }
			}
		case "project":
			del.input = func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{args: []string{scrubObjectName}, values: map[string]string{"cluster": scrubCluster}}
			}
		}
		rows = append(rows, del)
	}
	return rows
}

func manifestRow(variant, kind, body string, refused string) scrubRow {
	row := scrubRow{
		command: "create resource", variant: variant, kind: kind, class: classEcho, refused: refused,
		input: in(nil, map[string]string{"filename": "m.yaml", "cluster": scrubCluster}),
		manifest: func(*plantSet) string {
			return body
		},
	}
	return row
}

// plantedTargets returns a copy of the targets list the plant set holds.
func plantedTargets(ps *plantSet) []any {
	targets, _ := ps.fragment["spec"].(map[string]any)["targets"].([]any)
	return deepCopyValue(targets).([]any)
}

// helmOpTargetsPatch is the input of a merge patch that sets spec.targets of
// a HelmOp to the planted targets.
func helmOpTargetsPatch(ps *plantSet) fieldCoverageInput {
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"targets": plantedTargets(ps)}})
	if err != nil {
		panic(err)
	}
	return fieldCoverageInput{
		args:   []string{scrubObjectName},
		values: map[string]string{"cluster": scrubCluster, "namespace": "ns1", "type": "merge", "patch": string(body)},
	}
}

// helmOpTargetsManifest is a HelmOp manifest whose targets are the planted
// targets.
func helmOpTargetsManifest(ps *plantSet) string {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "fleet.cattle.io/v1alpha1", "kind": "HelmOp",
		"metadata": map[string]any{"name": "op", "namespace": "fleet-default"},
		"spec":     map[string]any{"targets": plantedTargets(ps)},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// withoutTargetsAtFirstGet removes spec.targets from the response to the
// first Get of the HelmOp; the patch then adds the list. Later responses
// hold the planted targets.
func withoutTargetsAtFirstGet(f *scrubFake, _ *plantSet) {
	f.firstGet = func(obj *unstructured.Unstructured) { unstructured.RemoveNestedField(obj.Object, "spec", "targets") }
}

// requireTargetsDiffEntry asserts the dry run's diff has one entry at
// /spec/targets, and that its `to` is a list with one item for each planted
// target.
func requireTargetsDiffEntry(t *testing.T, format string, run scrubRun) {
	t.Helper()
	env := decodeEnvelope(t, format, run.stdout)
	data, _ := env["data"].(map[string]any)
	diff, _ := data["diff"].([]any)
	var entries []map[string]any
	for _, e := range diff {
		if m, _ := e.(map[string]any); m["path"] == "/spec/targets" {
			entries = append(entries, m)
		}
	}
	require.Len(t, entries, 1, "expected one /spec/targets entry in the diff, found %d: %v", len(entries), diff)
	entry := entries[0]
	to, ok := entry["to"].([]any)
	require.True(t, ok, "the `to` of the /spec/targets entry is not a list: %v", entry["to"])
	assert.Len(t, to, run.fake.plant.items)
	assert.Positive(t, run.fake.served)
}

// requireTargetsSentIntact asserts the real update sent every planted value
// of the targets as it was planted: the redaction of the preview must not
// change the object the command sends.
func requireTargetsSentIntact(t *testing.T, _ string, run scrubRun) {
	t.Helper()
	run.fake.mu.Lock()
	defer run.fake.mu.Unlock()
	require.Len(t, run.fake.sent, 1, "expected one real update")
	targets, found, err := unstructured.NestedSlice(run.fake.sent[0], "spec", "targets")
	require.NoError(t, err)
	require.True(t, found, "the update sent no spec.targets")
	ps := run.fake.plant
	require.Len(t, targets, ps.items)
	for i, target := range targets {
		for _, field := range []string{"values", "templateValues"} {
			got, _, _ := unstructured.NestedString(target.(map[string]any), "helm", field)
			assert.Equal(t, ps.itemValues[i]["spec.targets.*.helm."+field], got, "target %d helm.%s changed in the update", i, field)
		}
	}
}

// dryRunReason is the reason of a dry-run row.
const dryRunReason = "the result is the Effect of a dry run: its diff, never the object the server returns"

// appScrubDeps returns the deps of an app command row: a Rancher test server
// that prepare fills, and the real client factory over it, which serves the
// cluster, link and action reads and the action request. The server plants
// the row's values in every answer for the row's kind, and the fake counts
// the answers that carry a value the row must not show.
func appScrubDeps(prepare func(s *appServer)) func(t *testing.T, f *scrubFake, ps *plantSet) runDeps {
	return func(t *testing.T, f *scrubFake, ps *plantSet) runDeps {
		t.Helper()
		s := newAppServer(t, "community")
		prepare(s)
		s.plant = func(resource string, obj map[string]any) {
			if f.kind == nil || resource != f.kind.GVR.Resource {
				return
			}
			mergeMissing(obj, ps.fragment)
			f.scan(&unstructured.Unstructured{Object: obj})
		}
		dir := grConfigDir(t, s.srv)
		factory := newClientFactory(dir, config.NewFileStore(dir))
		return runDeps{clients: factory, roots: factory, actions: factory, serverBase: factory, sessionNamer: factory}
	}
}

// appResultReason is the reason of the app rows: the result is built from
// names, never from a server object.
const appResultReason = "the result is built from names: the release, the operation Rancher names in its answer and the request id; no server object"

// handWrittenScrubRows are the rows of the mutating commands that are not
// generated from the kind table.
func handWrittenScrubRows() []scrubRow {
	oidcSecret := "oidcIntended1"
	createHelmOp := in(nil, map[string]string{"filename": "m.yaml", "cluster": scrubCluster})
	rows := []scrubRow{
		{command: "patch helmop", variant: "targets dry run", kind: "helmop", class: classNames, reason: dryRunReason, dryRun: true,
			input: helmOpTargetsPatch, setup: withoutTargetsAtFirstGet, check: requireTargetsDiffEntry},
		{command: "patch helmop", variant: "targets apply", kind: "helmop", class: classEcho,
			input: helmOpTargetsPatch, setup: withoutTargetsAtFirstGet, check: requireTargetsSentIntact},
		{command: "create resource", variant: "helmop targets apply", kind: "helmop", class: classEcho,
			input: createHelmOp, manifest: helmOpTargetsManifest},
		{command: "create resource", variant: "helmop targets dry run", kind: "helmop", class: classNames, reason: dryRunReason, dryRun: true,
			input: createHelmOp, manifest: helmOpTargetsManifest},
		{command: "create project", kind: "project", class: classEcho, input: in([]string{"web"}, map[string]string{"cluster": scrubCluster})},
		{command: "create namespace", kind: "namespace", class: classEcho, input: in([]string{"team-b"}, map[string]string{"cluster": scrubCluster})},
		{command: "create secret", variant: "namespace", kind: "secret", class: classEcho,
			input: in([]string{"app"}, map[string]string{"cluster": scrubCluster, "namespace": "team-a", "from-literal": "k=submittedSecret1"}),
			extra: func(*plantSet) []string { return []string{"submittedSecret1"} }},
		{command: "create secret", variant: "project", kind: "secret", class: classEcho,
			input: in([]string{"app"}, map[string]string{"cluster": scrubCluster, "project": "web", "from-literal": "k=submittedSecret2"}),
			extra: func(*plantSet) []string { return []string{"submittedSecret2"} }},
		manifestRow("secret", "secret", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: team-a\ntype: Opaque\nstringData:\n  k: submittedA\n", ""),
		manifestRow("user", "user", "apiVersion: management.cattle.io/v3\nkind: User\nmetadata:\n  name: u-abc\nusername: alice\nenabled: true\n", ""),
		manifestRow("machine config", "digitaloceanconfig", "apiVersion: rke-machine-config.cattle.io/v1\nkind: DigitaloceanConfig\nmetadata:\n  name: web\n  namespace: fleet-default\nregion: nyc3\n", ""),
		manifestRow("management token", "managementtoken", "apiVersion: management.cattle.io/v3\nkind: Token\nmetadata:\n  name: t\n", "cannot create a management-token from a manifest"),
		manifestRow("token", "token", "apiVersion: ext.cattle.io/v1\nkind: Token\nmetadata:\n  name: t\nspec:\n  userID: u-1\n", "cannot create a token from a manifest"),
		manifestRow("kubeconfig", "kubeconfig", "apiVersion: ext.cattle.io/v1\nkind: Kubeconfig\nmetadata:\n  name: k\n", "cannot create a kubeconfig from a manifest"),
		{command: "create token", kind: "token", class: classOnce, reason: "shows the new token's value once, in data.bearerToken", intendedPath: "status.bearerToken",
			input: in(nil, map[string]string{"description": "ci"})},
		{command: "create kubeconfig", kind: "kubeconfig", class: classOnce, reason: "prints the new kubeconfig once, as raw output in table format", intendedPath: "status.value", formats: []string{"table"},
			input: in(nil, map[string]string{"clusters": scrubCluster})},
		{command: "create password-change-request", class: classNames,
			reason: "the result is {userID}: the created object echoes spec.newPassword and is never copied into it",
			input: func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{args: []string{"u-abc123"}, bools: map[string]bool{"password-stdin": true}, stdin: "pcrPassword1"}
			},
			extra: func(*plantSet) []string { return []string{"pcrPassword1"} }},
		{command: "create user", kind: "user", class: classNames,
			reason: "the result is built from the committed identity fields: the created User and password Secret echo the hashed payload and the password",
			input: func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{args: []string{"alice"}, bools: map[string]bool{"password-stdin": true}, stdin: "userPassword1"}
			},
			extra: func(*plantSet) []string { return []string{"userPassword1"} }},
		{command: "create oidc-client", kind: "oidcclient", class: classOnce, reason: "shows the client secret once, in data.clientSecret",
			input: in([]string{"app-a"}, nil),
			setup: func(f *scrubFake, _ *plantSet) {
				f.oidc = &oidcFixture{mode: "create",
					before: map[string]string{"secret-1": oidcSecret}, after: map[string]string{"secret-1": oidcSecret},
					keysBefore: []string{"secret-1"}, keysAfter: []string{"secret-1"}}
			},
			intended: func(*scrubFake, *plantSet) string { return oidcSecret }},
		{command: "oidc-client add-secret", kind: "oidcclient", class: classOnce, reason: "shows the added client secret once, in data.clientSecret",
			input: in([]string{"app-a"}, nil),
			setup: func(f *scrubFake, _ *plantSet) {
				f.oidc = &oidcFixture{mode: "add",
					before:     map[string]string{"secret-1": "oidcExisting1"},
					after:      map[string]string{"secret-1": "oidcExisting1", "secret-2": oidcSecret},
					keysBefore: []string{"secret-1"}, keysAfter: []string{"secret-1", "secret-2"}}
			},
			extra:    func(*plantSet) []string { return []string{"oidcExisting1"} },
			intended: func(*scrubFake, *plantSet) string { return oidcSecret }},
		{command: "oidc-client rotate-secret", kind: "oidcclient", class: classOnce, reason: "shows the regenerated client secret once, in data.clientSecret",
			input: in([]string{"app-a"}, nil),
			setup: func(f *scrubFake, _ *plantSet) {
				f.oidc = &oidcFixture{mode: "rotate",
					before: map[string]string{"secret-1": "oidcOld1"}, after: map[string]string{"secret-1": oidcSecret},
					keysBefore: []string{"secret-1"}, keysAfter: []string{"secret-1"}}
			},
			extra:    func(*plantSet) []string { return []string{"oidcOld1"} },
			intended: func(*scrubFake, *plantSet) string { return oidcSecret }},
		{command: "oidc-client remove-secret", kind: "oidcclient", class: classNames,
			reason: "the result is {clientID, secretName}: it holds names only",
			input:  in([]string{"app-a", "secret-2"}, nil),
			setup: func(f *scrubFake, _ *plantSet) {
				f.oidc = &oidcFixture{mode: "remove",
					before:     map[string]string{"secret-1": "oidcKept1", "secret-2": "oidcRemoved1"},
					after:      map[string]string{"secret-1": "oidcKept1"},
					keysBefore: []string{"secret-1", "secret-2"}, keysAfter: []string{"secret-1"}}
			}},
		{command: "create cluster", kind: "provisioningcluster", class: classEcho,
			input: func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{
					args: []string{"node-1"},
					values: map[string]string{
						"machine-config": "digitaloceanconfig/cfg", "cloud-credential": "cc-abc",
						"k8s-version": "v1.30.5+rke2r1", "pool-name": "pool1", "quantity": "1",
					},
					bools: map[string]bool{"etcd": true, "controlplane": true, "worker": true},
				}
			}},
		{command: "create machine-config", kind: "digitaloceanconfig", class: classEcho,
			input: func(ps *plantSet) fieldCoverageInput {
				return fieldCoverageInput{
					args:  []string{"digitaloceanconfig", "web-pool"},
					lists: map[string][]string{"set": {"sshKeyContents=submittedKey1", "sshPort=22"}},
				}
			},
			extra: func(*plantSet) []string { return []string{"submittedKey1"} }},
		{command: "project add-namespace", kind: "namespace", class: classEcho,
			input: in([]string{"team-a"}, map[string]string{"cluster": scrubCluster, "project": "web"})},
		{command: "project remove-namespace", kind: "namespace", class: classEcho,
			input: in([]string{"team-a"}, map[string]string{"cluster": scrubCluster}),
			setup: func(f *scrubFake, _ *plantSet) {
				f.decorate = func(obj *unstructured.Unstructured) { projects.SetMembership(obj, scrubCluster, scrubProjectID) }
			}},
		{command: "cluster scale", kind: "provisioningcluster", class: classEcho,
			input: in([]string{"node-1"}, map[string]string{"pool": "pool1", "nodes": "2"}),
			setup: func(f *scrubFake, _ *plantSet) { f.decorate = provisioningClusterWithPools(false) }},
		{command: "cluster set-autoscaling", kind: "provisioningcluster", class: classEcho,
			input: in([]string{"node-1"}, map[string]string{"pool": "pool1", "min": "1", "max": "4"}),
			setup: func(f *scrubFake, _ *plantSet) { f.decorate = provisioningClusterWithPools(false) }},
		{command: "cluster clear-autoscaling", kind: "provisioningcluster", class: classEcho,
			input: in([]string{"node-1"}, map[string]string{"pool": "pool1"}),
			setup: func(f *scrubFake, _ *plantSet) { f.decorate = provisioningClusterWithPools(true) }},
		{command: "cluster pause-autoscaling", kind: "provisioningcluster", class: classEcho,
			input: in([]string{"node-1"}, nil),
			setup: func(f *scrubFake, _ *plantSet) { f.decorate = provisioningClusterWithPools(false) }},
		{command: "cluster resume-autoscaling", kind: "provisioningcluster", class: classEcho,
			input: in([]string{"node-1"}, nil),
			setup: func(f *scrubFake, _ *plantSet) {
				f.decorate = func(obj *unstructured.Unstructured) {
					provisioningClusterWithPools(false)(obj)
					_ = unstructured.SetNestedField(obj.Object, "true", "metadata", "annotations", "provisioning.cattle.io/cluster-autoscaler-paused")
				}
			}},
		{command: "app install", kind: "operation", class: classNames, reason: appResultReason,
			input: in([]string{appRelease}, map[string]string{"cluster": "prod", "namespace": appNS, "repo": appRepo, "chart": appChart}),
			deps: appScrubDeps(func(s *appServer) {
				s.put(appOperationPath(appNS, "helm-operation-other"), map[string]any{
					"apiVersion": "catalog.cattle.io/v1", "kind": "Operation",
					"metadata": map[string]any{"name": "helm-operation-other", "namespace": appNS, "resourceVersion": "3"},
					"status":   map[string]any{"releaseName": "other-release"},
				})
			})},
		{command: "app uninstall", kind: "app", class: classNames, reason: appResultReason,
			input: in([]string{appRelease}, map[string]string{"cluster": "prod", "namespace": appNS}),
			deps:  appScrubDeps(func(s *appServer) { s.putApp("deployed") })},
		{command: "app upgrade", kind: "app", class: classNames, reason: appResultReason,
			input: in([]string{appRelease}, map[string]string{"cluster": "prod", "namespace": appNS}),
			deps: appScrubDeps(func(s *appServer) {
				s.putRelease("deployed", 3, map[string]any{"replicaCount": 3})
			})},
		{command: "grant", kind: "globalrolebinding", class: classEcho,
			input: func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{args: []string{"local://u-abc"}, values: map[string]string{"role": "admin"}, bools: map[string]bool{"global": true}}
			}},
		{command: "revoke", kind: "globalrolebinding", class: classNames,
			reason: "the result is {deleted: [names]}: the listed bindings are read for their names and resourceVersions",
			input: func(*plantSet) fieldCoverageInput {
				return fieldCoverageInput{args: []string{"local://u-abc"}, values: map[string]string{"role": "admin"}, bools: map[string]bool{"global": true}}
			},
			setup: func(f *scrubFake, _ *plantSet) {
				f.list = func(gvr schema.GroupVersionResource) []unstructured.Unstructured {
					if gvr.Resource != "globalrolebindings" {
						return nil
					}
					b := f.base(gvr, "", "grb-1")
					b.Object["userName"] = "u-abc"
					b.Object["globalRoleName"] = "admin"
					return []unstructured.Unstructured{*b}
				}
			}},
	}
	return rows
}

// scrubRun is what one row wrote in one format.
type scrubRun struct {
	stdout, stderr string
	status         int
	ce             *clierr.Error
	fake           *scrubFake
	absent         []string
	intended       string
	shown          []payloadValue
}

// runScrubRow runs one row through the pipeline with --apply and every gate
// confirmed (as a dry run when the row says so), against its own fake that
// plants lists whose length is `items`, and returns what it wrote.
func runScrubRow(t *testing.T, reg *runtime.Registry, entries map[string]entry, row scrubRow, format string, items int) scrubRun {
	t.Helper()
	k := scrubKindRow(t, row.kind)
	ps := newPlantSet(k, items)
	f := &scrubFake{kind: k, plant: ps}
	if row.setup != nil {
		row.setup(f, ps)
	}
	var intended string
	switch {
	case row.intendedPath != "":
		intended = ps.pathValues[row.intendedPath]
		require.NotEmpty(t, intended, "%s plants no value at %s", row.name(), row.intendedPath)
	case row.intended != nil:
		intended = row.intended(f, ps)
	}
	var shown []payloadValue
	if row.shown != nil {
		shown = row.shown(ps, format)
	}
	var absent []string
	for _, v := range ps.values {
		if v != intended && !slices.ContainsFunc(shown, func(p payloadValue) bool { return p.value == v }) {
			absent = append(absent, v)
		}
	}
	if row.extra != nil {
		absent = append(absent, row.extra(ps)...)
	}
	f.needles = needles(absent)

	input := row.input(ps)
	body := input.stdin
	if row.manifest != nil {
		body = row.manifest(ps)
	}
	e, ok := entries[row.command]
	require.True(t, ok, "no command %q", row.command)
	deps := runDeps{
		clients: f, roots: fieldCoverageRoots{}, serverBase: scrubBase{},
		src: fieldCoverageSource{body}, sessionNamer: scrubNamer{row.namer},
	}
	if row.deps != nil {
		deps = row.deps(t, f, ps)
		if deps.src == nil {
			deps.src = fieldCoverageSource{body}
		}
	}
	cmd := e.make(deps)
	if row.wrap != nil {
		cmd = row.wrap(cmd)
	}
	var out, errOut bytes.Buffer
	status, ce := runtime.Run(t.Context(), cmd, input, runtime.IO{
		Out: &out, Err: &errOut, Format: format,
		Apply: true, Yes: true, AllowDeprecated: true, DryRun: row.dryRun,
		Capability: scrubAllowAll{}, Projects: scrubProjects{},
	}, reg.Binding(row.command))
	return scrubRun{stdout: out.String(), stderr: errOut.String(), status: status, ce: ce, fake: f, absent: absent, intended: intended, shown: shown}
}

// forms are the spellings of a planted value the output must not hold: the
// value, its base64, and its JSON-escaped forms with and without the HTML
// escapes the encoder adds.
func forms(v string) []string {
	out := []string{v, base64.StdEncoding.EncodeToString([]byte(v)), strings.Trim(mustMarshal(v, true), `"`), strings.Trim(mustMarshal(v, false), `"`)}
	return slices.Compact(out)
}

func mustMarshal(v string, escapeHTML bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(escapeHTML)
	_ = enc.Encode(v)
	return strings.TrimSpace(buf.String())
}

// decodeEnvelope parses the envelope a json or yaml result wrote.
func decodeEnvelope(t *testing.T, format, stdout string) map[string]any {
	t.Helper()
	var env map[string]any
	switch format {
	case "json":
		require.NoError(t, json.Unmarshal([]byte(stdout), &env), stdout)
	case "yaml":
		require.NoError(t, yaml.Unmarshal([]byte(stdout), &env), stdout)
	}
	return env
}

// requireScrubbed asserts one row's output against the rule: no hidden
// value in stdout or stderr, no hidden value in any key or leaf of the
// envelope's data, every outside value present, the one value a once-only
// command shows present, and a fake that served at least one hidden value.
//
// The raw check is a substring scan of the bytes written: it is the one
// place a rendered result is string-matched, because a leak into a place
// the decoded envelope does not reach (a warning, an error, a header line)
// must fail too. The decoded check covers the structure.
func requireScrubbed(t *testing.T, row scrubRow, format string, run scrubRun) {
	t.Helper()
	if row.refused != "" {
		require.NotNil(t, run.ce, "%s: want a refusal", row.name())
		require.ErrorContains(t, run.ce, row.refused)
		return
	}
	require.Nil(t, run.ce, "%s: %v", row.name(), run.ce)
	require.Zero(t, run.status)

	raw := run.stdout + "\n" + run.stderr
	for _, v := range run.absent {
		for _, form := range forms(v) {
			assert.NotContains(t, raw, form, "%s: hidden value in the output", row.name())
		}
	}
	if run.intended != "" {
		assert.True(t, slices.ContainsFunc(forms(run.intended), func(form string) bool { return strings.Contains(run.stdout, form) }),
			"%s: the value shown once is missing from stdout", row.name())
	}

	if format == "json" || format == "yaml" {
		env := decodeEnvelope(t, format, run.stdout)
		var leaves []string
		walkStrings(env["data"], func(s string) { leaves = append(leaves, s) })
		for _, v := range run.absent {
			for _, leaf := range leaves {
				assert.NotContains(t, leaf, v, "%s: hidden value in the result's data", row.name())
			}
		}
		if run.intended != "" {
			assert.Contains(t, leaves, run.intended, "%s: the value shown once is missing from data", row.name())
		}
		for _, p := range run.shown {
			assert.Contains(t, leaves, p.served, "%s: a value the row shows on purpose is missing from data", row.name())
		}
		if row.class == classEcho {
			for _, v := range run.fake.plant.outside {
				assert.Contains(t, leaves, v, "%s: a value outside the redaction is missing from data", row.name())
			}
		}
	}
	if format != "json" && format != "yaml" {
		for _, p := range run.shown {
			assert.Contains(t, run.stdout, p.served, "%s: a value the row shows on purpose is missing from the output", row.name())
		}
	}
	if len(run.absent) > 0 {
		assert.Positive(t, run.fake.served, "%s: the fake served no hidden value, so the row proves nothing", row.name())
	}
}

func scrubRows() []scrubRow {
	return append(generatedScrubRows(), handWrittenScrubRows()...)
}

// TestResultScrubRowsNameTheirClass asserts every row has a class from the
// table's set, and a reason when its class is built from names or once-only.
func TestResultScrubRowsNameTheirClass(t *testing.T) {
	for _, r := range scrubRows() {
		switch r.class {
		case classEcho:
		case classNames, classOnce:
			assert.NotEmpty(t, r.reason, "%s: a %s row states why", r.name(), r.class)
		default:
			t.Errorf("%s: class %q is not one of the table's classes", r.name(), r.class)
		}
	}
}

// TestResultScrubRowsCoverEveryMutatingCommand asserts the result table has
// a row for exactly the commands the manifest lists as mutating, so a new
// mutating command fails until its row exists.
func TestResultScrubRowsCoverEveryMutatingCommand(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var want []string
	for _, c := range m.Commands {
		if c.CommandType == runtime.Mutating {
			want = append(want, c.Name)
		}
	}
	var got []string
	for _, r := range scrubRows() {
		got = append(got, r.command)
	}
	slices.Sort(want)
	got = slices.Compact(slices.Sorted(slices.Values(got)))
	assert.Equal(t, want, got)
}

// TestApplyResultHidesWhatThePreviewHides runs every mutating command with
// --apply in json and yaml (table for create kubeconfig) against a fake
// that plants, in every answer for the row's kind, each value the kind's
// redaction hides (each name also in a second letter case) beside values the
// redaction leaves alone. The result and everything else written must hold
// none of the hidden values and every outside value. A kind that hides a
// path with a wildcard segment runs with lists of one item and of two.
func TestApplyResultHidesWhatThePreviewHides(t *testing.T) {
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	entries := map[string]entry{}
	for _, e := range buildEntries() {
		entries[e.make(runDeps{}).Describe().Name] = e
	}
	for _, row := range scrubRows() {
		formats := row.formats
		if formats == nil {
			formats = []string{"json", "yaml"}
		}
		counts := plantItemCounts(row.kind)
		for _, items := range counts {
			for _, format := range formats {
				t.Run(scrubSubtestName(row.name(), format, items, counts), func(t *testing.T) {
					run := runScrubRow(t, wr.reg, entries, row, format, items)
					requireScrubbed(t, row, format, run)
					if row.check != nil {
						row.check(t, format, run)
					}
				})
			}
		}
	}
}
