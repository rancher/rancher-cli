package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// readBeforeWrite is what the hint of a command that reads an object before
// any write says: word is the command's name in the hint ("this <word> reads
// ..."), kind is the kind it reads ("" for the kind of the resource it read),
// and verb is the verb of the write, "" when the object it writes can differ
// from the object it reads.
type readBeforeWrite struct {
	word, kind, verb string
}

// patchReadsBeforeWrite is the hint of every patch command the registry
// builds from the kind table: it reads the object and updates it.
var patchReadsBeforeWrite = readBeforeWrite{word: "patch", verb: "update"}

// generatedPatchCommands are the patch commands the registry builds from the
// kind table, one per kind row.
func generatedPatchCommands() map[string]bool {
	out := map[string]bool{}
	for _, k := range kinds.All() {
		out["patch "+k.Name] = true
	}
	return out
}

// dedicatedReadsBeforeWrite lists the mutating commands, other than the patch
// and delete commands the registry builds from the kind table, that read an
// object with a GET before they send any write. A key is a command or, for a
// command of which only some rows read first, a row name; a row name is looked
// up before its command. A command added later that reads first fails
// TestEveryCommandThatReadsBeforeItWritesNamesTheMissingVerb until it is
// listed here.
var dedicatedReadsBeforeWrite = map[string]readBeforeWrite{
	"cluster scale":              {word: "command", verb: "update"},
	"cluster set-autoscaling":    {word: "command", verb: "update"},
	"cluster clear-autoscaling":  {word: "command", verb: "update"},
	"cluster pause-autoscaling":  {word: "command", verb: "update"},
	"cluster resume-autoscaling": {word: "command", verb: "update"},
	"cluster save-etcd-snapshot": {word: "command", verb: "update"},
	// An imported cluster's version and upgrade strategy are written to its
	// management Cluster, not to the provisioning Cluster the command reads.
	"cluster upgrade":              {word: "command"},
	"cluster set-upgrade-strategy": {word: "command"},
	"oidc-client add-secret":       {word: "command", kind: "oidc-client", verb: "update"},
	"oidc-client rotate-secret":    {word: "command", kind: "oidc-client", verb: "update"},
	"oidc-client remove-secret":    {word: "command", kind: "oidc-client", verb: "update"},
	"project add-namespace":        {word: "command", verb: "update"},
	"project remove-namespace":     {word: "command", verb: "update"},
	// The project that --project names is read before the Secret is created.
	"create secret (project)": {word: "command"},
	// The cloud credential the cluster uses is read before the cluster is
	// created.
	"create cluster": {word: "command", kind: "cloud-credential"},
	// The project that --project names is read for its backing namespace
	// before the binding is read.
	"patch projectroletemplatebinding (--project)": {word: "command"},
	// The Secret of the credential is read by its id when the list of Secrets
	// does not have it. The row that lists the credential sends no GET.
	"patch cloud-credential (id not listed)": {word: "command", kind: "cloud-credential", verb: "update"},
}

// readBeforeWriteLiteralHints are the hints of one row of each kind of
// command, written out.
var readBeforeWriteLiteralHints = map[string]string{
	"patch configmap":                              "this patch reads the configmap first; your role needs permission to get configmaps as well as update",
	"cluster scale":                                "this command reads the provisioning-cluster first; your role needs permission to get clusters.provisioning.cattle.io as well as update",
	"cluster upgrade (provisioned)":                "this command reads the provisioning-cluster first; your role needs permission to get clusters.provisioning.cattle.io",
	"oidc-client add-secret":                       "this command reads the oidc-client first; your role needs permission to get oidcclients.management.cattle.io as well as update",
	"project add-namespace":                        "this command reads the namespace first; your role needs permission to get namespaces as well as update",
	"create secret (project)":                      "this command reads the project first; your role needs permission to get projects.management.cattle.io",
	"create cluster":                               "this command reads the cloud-credential first; your role needs permission to get secrets",
	"patch projectroletemplatebinding (--project)": "this command reads the project first; your role needs permission to get projects.management.cattle.io",
	"patch cloud-credential (id not listed)":       "this command reads the cloud-credential first; your role needs permission to get secrets as well as update",
}

// readsNotRunHere are the mutating commands whose rows run against their own
// test server through the client factory, not against scrubFake, each with
// its reason.
var readsNotRunHere = map[string]string{
	"app install":         "the command previews from server reads and sends a Rancher catalog action, not a Kubernetes write",
	"app upgrade":         "the command previews from server reads and sends a Rancher catalog action, not a Kubernetes write",
	"app uninstall":       "the command previews from server reads and sends a Rancher catalog action, not a Kubernetes write",
	"extension install":   "the command previews from server reads and sends a Rancher catalog action, not a Kubernetes write",
	"extension upgrade":   "the command previews from server reads and sends a Rancher catalog action, not a Kubernetes write",
	"extension uninstall": "the command previews from server reads and sends a Rancher catalog action, not a Kubernetes write",
	"node drain":          "when the first list of the management nodes in Rancher's local cluster is refused, the command asks Rancher through its node action instead, and its own RBAC errors name the read it could not make",
	"node stop-drain":     "when the first list of the management nodes in Rancher's local cluster is refused, the command asks Rancher through its node action instead, and its own RBAC errors name the read it could not make",
}

// readsThroughSteve are the rows that read, through the Steve API, the object
// they write, each with its reason. Steve's 403 is not the Kubernetes
// authorizer's Status, and the error the command returns names the read and
// has the hint rancher access clusters.
var readsThroughSteve = map[string]string{
	"cluster upgrade (imported)":              "the management Cluster of an imported cluster is read through the Steve API, where a Cluster Owner can read it",
	"cluster set-upgrade-strategy (imported)": "the management Cluster of an imported cluster is read through the Steve API, where a Cluster Owner can read it",
}

// readBeforeWriteExtraRows are rows the result table does not have: a patch
// with --project, which reads the project before the object, and a patch of a
// cloud credential that the list of Secrets does not have, which reads the
// Secret by its id.
func readBeforeWriteExtraRows() []scrubRow {
	return []scrubRow{{
		command: "patch projectroletemplatebinding", variant: "--project", kind: "projectroletemplatebinding", class: classEcho,
		input: in([]string{scrubObjectName}, map[string]string{
			"project": scrubProjectID, "cluster": scrubCluster, "type": "merge", "patch": `{"metadata":{"labels":{"scrub":"1"}}}`,
		}),
	}, {
		command: "patch cloud-credential", variant: "id not listed", kind: "secret", class: classNames, reason: cloudCredentialPatchResultReason,
		input: cloudCredentialScrubPatchInput, manifest: cloudCredentialScrubFile, deps: cloudCredentialScrubDeps,
	}}
}

// scrubRefusal is the refusal the cluster client returns for a GET of name in
// ns when the Kubernetes authorizer refuses get to the caller: the
// authorizer's 403 with the hint the client adds for that kind and verb, when
// it adds one.
func scrubRefusal(gvr schema.GroupVersionResource, ns, name string) error {
	err := dwAuthorizerRefusal(gvr, ns, name)
	if k, ok := kinds.ByGVR(gvr); ok {
		if c, ok := kinds.ForbiddenCommand(k, "get", ns, name); ok {
			if ce, ok := errors.AsType[*clierr.Error](err); ok {
				_ = ce.WithHintCommand(c)
			}
		}
	}
	return err
}

// readBeforeWriteHint is the hint of a row that reads an object of the kind
// row k first, when the authorizer refuses the get.
func readBeforeWriteHint(want readBeforeWrite, k kinds.Kind) string {
	resource := k.GVR.Resource
	if k.GVR.Group != "" {
		resource += "." + k.GVR.Group
	}
	kind := want.kind
	if kind == "" {
		kind = k.Name
	}
	hint := "this " + want.word + " reads the " + kind + " first; your role needs permission to get " + resource
	if want.verb != "" {
		hint += " as well as " + want.verb
	}
	return hint
}

// servedByScrubFake reports whether the command's cluster clients are f.
func servedByScrubFake(deps runDeps, f *scrubFake) bool {
	switch c := deps.clients.(type) {
	case *scrubFake:
		return c == f
	case linkedScrubClients:
		return c.scrubFake == f
	}
	return false
}

// readBeforeWriteRun is one row run with --dry-run against its fake.
type readBeforeWriteRun struct {
	f  *scrubFake
	ce *clierr.Error
}

// runReadBeforeWriteRow runs row with --dry-run; refuse sets the fake to
// refuse every GET, and roots, when not nil, replaces the row's root client.
// A row that runs against its own server, not scrubFake, fails unless its
// command is in readsNotRunHere; ok is false for a row listed there.
func runReadBeforeWriteRow(t *testing.T, reg *runtime.Registry, entries map[string]entry, row scrubRow, refuse bool, roots engine.RootClientFactory) (run readBeforeWriteRun, ok bool) {
	t.Helper()
	k := scrubKindRow(t, row.kind)
	ps := newPlantSet(k, plantItemCounts(row.kind)[0])
	f := &scrubFake{kind: k, plant: ps}
	if row.setup != nil {
		row.setup(f, ps)
	}
	input := row.input(ps)
	body := input.stdin
	if row.manifest != nil {
		body = row.manifest(ps)
	}
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
	if _, listed := readsNotRunHere[row.command]; listed {
		assert.False(t, servedByScrubFake(deps, f), "%s is listed in readsNotRunHere and runs against scrubFake; remove it", row.command)
		return readBeforeWriteRun{}, false
	}
	require.True(t, servedByScrubFake(deps, f), "%s runs against its own server; list it in readsNotRunHere with the reason", row.command)
	if roots != nil {
		deps.roots = roots
	}
	f.refuseGets = refuse
	e, found := entries[row.command]
	require.True(t, found, "no command %q", row.command)
	format := "json"
	if len(row.formats) > 0 {
		format = row.formats[0]
	}
	var out, errOut bytes.Buffer
	_, ce := runtime.Run(t.Context(), e.make(deps), input, runtime.IO{
		Out: &out, Err: &errOut, Format: format, DryRun: true,
		Capability: scrubAllowAll{}, Projects: scrubProjects{},
	}, reg.Binding(row.command))
	return readBeforeWriteRun{f: f, ce: ce}, true
}

// TestEveryCommandThatReadsBeforeItWritesNamesTheMissingVerb runs, with
// --dry-run, every result-scrub row except the delete commands and the rows
// refused at Plan, and the rows of readBeforeWriteExtraRows. The fake it runs
// against returns the Kubernetes authorizer's refusal of get for every GET. A
// row that ends in an error after a refused GET, and sends no write, reads
// before it writes. It is a patch command the registry builds from the kind
// table or is listed in dedicatedReadsBeforeWrite, and it returns the refusal
// (RBAC, exit 6) with the hint that names get on the resource of the first
// refused GET and the verb of its write, or the hint the cluster client gave
// the refusal. One row of each kind of command is checked against the hint
// written out. A listed command or row that a refused GET does not stop
// fails, and so does a row that runs against another server and is not
// listed in readsNotRunHere. Reads that happen only when the command runs
// with --apply are not seen.
func TestEveryCommandThatReadsBeforeItWritesNamesTheMissingVerb(t *testing.T) {
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	entries := map[string]entry{}
	for _, e := range buildEntries() {
		entries[e.make(runDeps{}).Describe().Name] = e
	}
	generated := generatedPatchCommands()
	stopped := map[string]bool{}
	seen := map[string]bool{}
	for _, row := range append(scrubRows(), readBeforeWriteExtraRows()...) {
		if row.refused != "" || strings.HasPrefix(row.command, "delete ") {
			continue
		}
		seen[row.command] = true
		seen[row.name()] = true
		t.Run(row.name(), func(t *testing.T) {
			run, ok := runReadBeforeWriteRow(t, wr.reg, entries, row, true, nil)
			if !ok {
				return
			}
			want, listed := dedicatedReadsBeforeWrite[row.name()]
			if !listed {
				want, listed = dedicatedReadsBeforeWrite[row.command]
			}
			if !listed && generated[row.command] {
				want, listed = patchReadsBeforeWrite, true
			}
			run.f.mu.Lock()
			refused, writes := run.f.refused, run.f.writes
			run.f.mu.Unlock()
			ce := run.ce
			if ce == nil || len(refused) == 0 || len(writes) > 0 {
				assert.False(t, listed, "%s is listed as a command that reads before it writes, and the refusal of its GET does not stop it", row.name())
				return
			}
			stopped[row.command] = true
			stopped[row.name()] = true
			first := refused[0]
			require.True(t, listed, "%s stops at the refused GET of %s %s/%s and sends no write: list it in dedicatedReadsBeforeWrite with what it reads and the verb of its write", row.name(), first.gvr.Resource, first.ns, first.name)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			assert.Equal(t, 6, ce.ExitCode())
			require.True(t, clierr.AuthorizerRefused(ce, "get", first.gvr.Group, first.gvr.Resource, first.ns, first.name),
				"%s returns the authorizer's refusal of its first GET: %v", row.name(), ce)
			rk, ok := kinds.ByGVR(first.gvr)
			require.True(t, ok, "no kind row for %s", first.gvr)
			wantHint := readBeforeWriteHint(want, rk)
			if c, ok := kinds.ForbiddenCommand(rk, "get", first.ns, first.name); ok {
				// The cluster client's hint for that kind and verb is kept.
				wantHint = clierr.New(clierr.CodeRBAC, "refused").WithHintCommand(c).Hint
			}
			if literal, ok := readBeforeWriteLiteralHints[row.name()]; ok {
				wantHint = literal
			}
			assert.Equal(t, wantHint, ce.Hint)
		})
	}
	for name := range dedicatedReadsBeforeWrite {
		assert.True(t, stopped[name], "%s is listed in dedicatedReadsBeforeWrite and no row of it stopped at a refused GET", name)
	}
	for name := range readBeforeWriteLiteralHints {
		assert.True(t, stopped[name], "%s has a hint written out and did not stop at a refused GET", name)
	}
	for c := range readsNotRunHere {
		assert.True(t, seen[c], "%s is listed in readsNotRunHere and has no row; remove it", c)
	}
}

// steveRefusingRoots is a root client that returns the 403 Steve writes for a
// GET of a management Cluster through the Steve API, and returns what inner
// returns for every other GET.
type steveRefusingRoots struct{ inner engine.RootClientFactory }

func (r steveRefusingRoots) Root() (engine.RootClient, error) {
	inner, err := r.inner.Root()
	if err != nil {
		return nil, err
	}
	return steveRefusingRoot{inner}, nil
}

type steveRefusingRoot struct{ inner engine.RootClient }

func (r steveRefusingRoot) GetJSON(ctx context.Context, path string) ([]byte, error) {
	if name, ok := strings.CutPrefix(path, "/v1/management.cattle.io.clusters/"); ok {
		body := `{"type":"error","status":"403","code":"Forbidden","message":"clusters.management.cattle.io \"` + name +
			`\" is forbidden: User \"u-update\" cannot get resource \"clusters\" in API group \"management.cattle.io\" at the cluster scope"}`
		return nil, clierr.ResponseError(http.MethodGet, http.StatusForbidden, []byte(body))
	}
	return r.inner.GetJSON(ctx, path)
}

// TestACommandThatReadsThroughSteveBeforeItWritesNamesTheRead asserts, for
// every row of readsThroughSteve, that a 403 of the Steve read of the object
// the command writes is RBAC, exit 6, with a message that names the read and
// the hint rancher access clusters, and that no write is sent, not even a dry
// run.
func TestACommandThatReadsThroughSteveBeforeItWritesNamesTheRead(t *testing.T) {
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	entries := map[string]entry{}
	for _, e := range buildEntries() {
		entries[e.make(runDeps{}).Describe().Name] = e
	}
	seen := map[string]bool{}
	for _, row := range scrubRows() {
		if _, ok := readsThroughSteve[row.name()]; !ok {
			continue
		}
		seen[row.name()] = true
		t.Run(row.name(), func(t *testing.T) {
			require.NotNil(t, row.deps, "%s builds its root client", row.name())
			run, ok := runReadBeforeWriteRow(t, wr.reg, entries, row, false, steveRefusingRoots{inner: upgradeScrubRoots{}})
			require.True(t, ok)
			require.NotNil(t, run.ce, "%s: the refused Steve read stops the command", row.name())
			assert.Equal(t, clierr.CodeRBAC, run.ce.Code)
			assert.Equal(t, 6, run.ce.ExitCode())
			require.ErrorContains(t, run.ce, "you cannot read cluster 'c-m-scrub' (GET /v1/management.cattle.io.clusters/c-m-scrub answered 403)")
			assert.Equal(t, clierr.New(clierr.CodeRBAC, "refused").WithHintCommand(clierr.NewCommand("access clusters")).Hint, run.ce.Hint)
			run.f.mu.Lock()
			defer run.f.mu.Unlock()
			assert.Empty(t, run.f.writes, "no write is sent, not even a dry run")
		})
	}
	for name := range readsThroughSteve {
		assert.True(t, seen[name], "%s is listed in readsThroughSteve and has no row; remove it", name)
	}
}
