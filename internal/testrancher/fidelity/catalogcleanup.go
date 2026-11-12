package fidelity

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"
)

// catalogReleaseNamespace is the namespace the catalog write probes install
// their release in. The after-snapshot check fails on every new name in this
// namespace, so it must be one where Rancher runs no system chart
// Operations, never cattle-system. The namespace exists on every cluster and
// the cleanup never deletes it.
const catalogReleaseNamespace = "default"

// catalogSystemNamespace is the namespace Rancher runs each Operation's pod
// in, with the pod's ServiceAccount, Secrets and kubeconfig ConfigMaps.
const catalogSystemNamespace = "cattle-system"

// tableOnlyAccept asks for a Table of object metadata and nothing else: a
// Secret read this way carries no data.
const tableOnlyAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

// forRoleLabel names, on a ClusterRoleBinding Rancher creates for an
// Operation's pod, the ClusterRole the binding was made for. That binding
// has no owner reference.
const forRoleLabel = "pod-impersonation.cattle.io/for-role"

// podClusterRoleAnnotation names, on an Operation's pod, the ClusterRole
// that owns the pod.
const podClusterRoleAnnotation = "pod-impersonation.cattle.io/cluster-role"

// requestIDCapture is the capture the cleanup's setup stores the install
// request id under; a recorded name, so a golden shows it as {requestid}.
const requestIDCapture = "requestid"

// The values the cleanup stores for its own steps. They are names, uids,
// times and markers, never a response body, a label value or an
// annotation value; the cluster values the install sends are the one
// exception (catalogValuePrefix).
const (
	catalogPrefix        = unrecordedPrefix + "catalog."
	catalogBeforePrefix  = catalogPrefix + "before."
	catalogSnapshotTaken = catalogPrefix + "snapshot"
	catalogIndexDeadline = catalogPrefix + "indexdeadline"
	catalogIndexListed   = catalogPrefix + "indexlisted"
	catalogCleanupOp     = catalogPrefix + "cleanupop"
	catalogRecorded      = catalogPrefix + "recorded"
	// catalogValuePrefix opens the name of each cluster value the install
	// sends, stored as its JSON encoding and written unquoted into the
	// body. Request input only: never logged or put in an error.
	catalogValuePrefix = catalogPrefix + "value."
)

// catalogValueKeys are the keys under global.cattle that the install
// sends, as app install adds them for a cluster without Windows workers.
var catalogValueKeys = []string{"clusterId", "clusterName", "systemProjectId", "url", "rkePathPrefix", "rkeWindowsPathPrefix"}

// operationPrefixes are the name prefixes of the objects Rancher creates
// for each Operation, its own and the system charts' alike. A new name with
// one of them fails the after-snapshot check unless its owners lead to an
// Operation outside the release namespace.
var operationPrefixes = []string{
	// The Operation, its pod, its Secret in cattle-system, and its Role
	// and RoleBinding in the release namespace.
	"helm-operation-",
	// The pod's ClusterRole, its ServiceAccount and token Secret, and the
	// ClusterRoleBinding the ClusterRole owns.
	"pod-impersonation-helm-op-",
	// The ClusterRoleBinding with no owner, found by its for-role label.
	"helm-op-pod-impersonation-helm-op-",
	// The two kubeconfig ConfigMaps.
	"impersonation-helm-op-",
	// The RoleTemplates the harvester-rbac chart creates.
	"virt-",
}

// catalogAPIs maps each object set the cleanup reads to its API prefix.
var catalogAPIs = map[string]string{
	"pods":                "/api/v1",
	"secrets":             "/api/v1",
	"configmaps":          "/api/v1",
	"serviceaccounts":     "/api/v1",
	"clusterroles":        "/apis/rbac.authorization.k8s.io/v1",
	"clusterrolebindings": "/apis/rbac.authorization.k8s.io/v1",
	"roles":               "/apis/rbac.authorization.k8s.io/v1",
	"rolebindings":        "/apis/rbac.authorization.k8s.io/v1",
	"operations":          "/apis/catalog.cattle.io/v1",
	"apps":                "/apis/catalog.cattle.io/v1",
	"roletemplates":       "/apis/management.cattle.io/v3",
}

// catalogSet is one list the snapshots read: a set of objects of one
// resource in one namespace, or cluster-wide when namespace is empty.
type catalogSet struct {
	name      string
	namespace string
}

// id is the set's name in the stored snapshot keys.
func (s catalogSet) id() string {
	if s.namespace == "" {
		return s.name
	}
	return s.namespace + "." + s.name
}

// collectionPath is the path of a set below /k8s/clusters/<cluster>.
func collectionPath(set, namespace string) string {
	p := catalogAPIs[set]
	if namespace != "" {
		p += "/namespaces/" + namespace
	}
	return p + "/" + set
}

// objRef is one object the cleanup names: its set, namespace (empty for a
// cluster-scoped set), name and uid.
type objRef struct {
	set       string
	namespace string
	name      string
	uid       string
}

// String names the object as the log lines and errors do.
func (o objRef) String() string {
	if o.namespace == "" {
		return o.set + " " + o.name
	}
	return o.set + " " + o.namespace + "/" + o.name
}

func (o objRef) line() string {
	return strings.Join([]string{o.set, o.namespace, o.name, o.uid}, "\t")
}

func encodeRefs(refs []objRef) string {
	lines := make([]string, len(refs))
	for i, r := range refs {
		lines[i] = r.line()
	}
	return strings.Join(lines, "\n")
}

func decodeRefs(s string) []objRef {
	var refs []objRef
	for line := range strings.SplitSeq(s, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		refs = append(refs, objRef{set: f[0], namespace: f[1], name: f[2], uid: f[3]})
	}
	return refs
}

// ownerRef is one owner reference of a listed object.
type ownerRef struct {
	kind string
	name string
	uid  string
}

// tableRow is the metadata of one listed object. Labels and annotations
// stay in memory: they are compared, never printed or stored.
type tableRow struct {
	ref         objRef
	owners      []ownerRef
	labels      map[string]string
	annotations map[string]string
}

// tableMetadata decodes the metadata of a Table row or of a JSON object.
type tableMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	OwnerReferences []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"ownerReferences"`
}

func (m tableMetadata) row(set string) tableRow {
	r := tableRow{
		ref:         objRef{set: set, namespace: m.Namespace, name: m.Name, uid: m.UID},
		labels:      m.Labels,
		annotations: m.Annotations,
	}
	for _, o := range m.OwnerReferences {
		r.owners = append(r.owners, ownerRef{kind: o.Kind, name: o.Name, uid: o.UID})
	}
	return r
}

// errTableNotFound is adminTable's answer to a 404.
var errTableNotFound = errors.New("not found")

// adminTable reads path as a Table of object metadata with the admin
// bearer, sending the Table-only Accept header alone. A 404 returns
// errTableNotFound; any other status outside 2xx, an answer whose kind is
// not Table, or one that is not JSON, is an error that names the status
// only. rows: null is an empty list. The rows are tagged with set.
func adminTable(ctx context.Context, tgt Target, path, set string) ([]tableRow, error) {
	tok, err := tgt.Bearer(ctx, IdentityAdmin)
	if err != nil {
		return nil, fmt.Errorf("resolving admin bearer: %w", err)
	}
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.BaseURL()+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", tableOnlyAccept)
		req.Header.Set("Authorization", "Bearer "+tok)
		return req, nil
	}
	resp, err := doResilient(tgt.Client(), build)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.New("reading the answer failed")
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errTableNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var table struct {
		Kind string `json:"kind"`
		Rows []struct {
			Object struct {
				Metadata tableMetadata `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}
	if json.Unmarshal(raw, &table) != nil {
		return nil, fmt.Errorf("status %d with an answer that is not a JSON Table", resp.StatusCode)
	}
	if table.Kind != "Table" {
		return nil, fmt.Errorf("status %d with an answer that is not a Table", resp.StatusCode)
	}
	rows := make([]tableRow, 0, len(table.Rows))
	for _, r := range table.Rows {
		rows = append(rows, r.Object.Metadata.row(set))
	}
	return rows, nil
}

// catalogCleanup builds the setup and teardown steps of one catalog write
// probe: a check for leftovers of an earlier run, a wait for the chart in
// the repository index, the request id, the cluster values, a snapshot of
// the names the run can touch, and, after the observations, an uninstall
// of what the run installed, the removal of every object the run's
// Operations created, each by name with its uid as the delete
// precondition, and a comparison of the names before and after. It deletes
// nothing it did not record: a name is never deleted only because it is
// new.
type catalogCleanup struct {
	// cluster is the cluster id, local or a {downstream} placeholder.
	cluster   string
	repo      string
	chart     string
	version   string
	release   string
	namespace string
	// installOp and uninstallOp are the captures the probe's install and
	// uninstall observations store their Operation names in; uninstallOp
	// is empty for a probe that sends no uninstall.
	installOp   string
	uninstallOp string

	// pollInterval paces every read the steps repeat.
	pollInterval time.Duration
	// indexWait bounds the wait for the chart in the index, spread over
	// steps of at most indexStep each.
	indexWait time.Duration
	indexStep time.Duration
	// waitBudget is the time the probe's Until observations share, from
	// the last setup step.
	waitBudget time.Duration
	// requestTimeout bounds every request but the index reads.
	requestTimeout time.Duration
	// The bound of each step, inside its own step context.
	precheckBound time.Duration
	valuesBound   time.Duration
	snapshotBound time.Duration
	uninstallTime time.Duration
	recordBound   time.Duration
	removeBound   time.Duration
	goneBound     time.Duration
	compareBound  time.Duration
}

// newCatalogCleanup is the cleanup of a harvester-rbac install from
// rancher-charts on cluster, with the bounds the oracle runs with. The
// downstream gets longer bounds for the steps that read many lists through
// the tunnel.
func newCatalogCleanup(cluster, installOp, uninstallOp string) catalogCleanup {
	c := catalogCleanup{
		cluster:        cluster,
		repo:           "rancher-charts",
		chart:          "harvester-rbac",
		version:        "110.0.0+up0.1.1",
		release:        "harvester-rbac",
		namespace:      catalogReleaseNamespace,
		installOp:      installOp,
		uninstallOp:    uninstallOp,
		pollInterval:   time.Second,
		indexWait:      120 * time.Second,
		indexStep:      25 * time.Second,
		waitBudget:     50 * time.Second,
		requestTimeout: 5 * time.Second,
		precheckBound:  5 * time.Second,
		valuesBound:    5 * time.Second,
		snapshotBound:  10 * time.Second,
		uninstallTime:  15 * time.Second,
		recordBound:    10 * time.Second,
		removeBound:    10 * time.Second,
		goneBound:      10 * time.Second,
		compareBound:   15 * time.Second,
	}
	if cluster != "local" {
		c.snapshotBound = 20 * time.Second
		c.recordBound = 20 * time.Second
		c.removeBound = 20 * time.Second
		c.goneBound = 20 * time.Second
		c.compareBound = 20 * time.Second
	}
	return c
}

// The names of the cleanup's steps.
const (
	stepPrecheck  = "check for objects an earlier run left behind"
	stepIndex     = "wait for the chart in the repository index"
	stepRequestID = "generate the request id"
	stepValues    = "read the cluster values"
	stepSnapshot  = "record the names before the run"
	stepDeadline  = "start the wait budget"
	stepUninstall = "uninstall what this run installed"
	stepRecord    = "record each operation's objects"
	stepRemove    = "remove the recorded objects"
	stepGone      = "wait until the recorded objects are gone"
	stepCompare   = "compare the names before and after the run"
)

// steps returns the probe's setup steps, which go first in its Setup, and
// its teardown steps, which are its whole Teardown.
func (c catalogCleanup) steps() (setup, teardown []Step) {
	setup = []Step{guarded(stepPrecheck, c.precheck)}
	count := int(math.Ceil(float64(c.indexWait) / float64(c.indexStep)))
	for i := range count {
		setup = append(setup, guarded(fmt.Sprintf("%s (%d of %d)", stepIndex, i+1, count), c.indexWaitStep(i == count-1)))
	}
	setup = append(setup,
		guarded(stepRequestID, generateRequestID),
		guarded(stepValues, c.clusterValues),
		guarded(stepSnapshot, c.snapshot),
		guarded(stepDeadline, c.startWaitBudget),
	)
	teardown = []Step{
		guarded(stepUninstall, c.uninstall),
		guarded(stepRecord, c.record),
		guarded(stepRemove, c.remove),
		guarded(stepGone, c.waitGone),
		guarded(stepCompare, c.compare),
	}
	return setup, teardown
}

// guarded is a Step that returns a panic of do as an error naming the
// step, with no stack: the teardown steps after it still run.
func guarded(name string, do func(ctx context.Context, tgt Target, captured map[string]string) error) Step {
	return Step{Name: name, Do: func(ctx context.Context, tgt Target, captured map[string]string) (err error) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// A runtime error's text is Go's own; any other panic value
			// can hold server text and is not shown.
			if rerr, ok := r.(runtime.Error); ok {
				err = fmt.Errorf("the step %q panicked: %s", name, rerr.Error())
				return
			}
			err = fmt.Errorf("the step %q panicked", name)
		}()
		return do(ctx, tgt, captured)
	}}
}

// kube is the Kubernetes API prefix of the probe's cluster.
func (c catalogCleanup) kube(captured map[string]string) string {
	return "/k8s/clusters/" + substitute(c.cluster, captured)
}

// steve is the steve prefix of the probe's cluster: none for local, as the
// CLI sends it.
func (c catalogCleanup) steve(captured map[string]string) string {
	if c.cluster == "local" {
		return ""
	}
	return "/k8s/clusters/" + substitute(c.cluster, captured)
}

// sets are the object sets the snapshots read on the probe's cluster.
func (c catalogCleanup) sets() []catalogSet {
	sets := []catalogSet{
		{"pods", catalogSystemNamespace},
		{"secrets", catalogSystemNamespace},
		{"configmaps", catalogSystemNamespace},
		{"serviceaccounts", catalogSystemNamespace},
		{"clusterroles", ""},
		{"clusterrolebindings", ""},
		{"operations", c.namespace},
		{"apps", c.namespace},
		{"roles", c.namespace},
		{"rolebindings", c.namespace},
		{"secrets", c.namespace},
	}
	if c.cluster == "local" {
		sets = append(sets, catalogSet{"roletemplates", ""})
	}
	return sets
}

// table reads one set within the request timeout.
func (c catalogCleanup) table(ctx context.Context, tgt Target, captured map[string]string, s catalogSet) ([]tableRow, error) {
	rctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	rows, err := adminTable(rctx, tgt, c.kube(captured)+collectionPath(s.name, s.namespace), s.name)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", catalogSetText(s), err)
	}
	return rows, nil
}

// catalogSetText names a set in an error.
func catalogSetText(s catalogSet) string {
	if s.namespace == "" {
		return s.name
	}
	return s.name + " in " + s.namespace
}

// getJSON sends one admin request within the request timeout. Its body is
// read by the caller and never put in an error.
func (c catalogCleanup) getJSON(ctx context.Context, tgt Target, method, path, body string) (map[string]any, int, error) {
	rctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	return adminJSON(rctx, tgt, method, path, body)
}

func (c catalogCleanup) appPath(captured map[string]string) string {
	return c.kube(captured) + collectionPath("apps", c.namespace) + "/" + c.release
}

func (c catalogCleanup) operationPath(captured map[string]string, name string) string {
	return c.kube(captured) + collectionPath("operations", c.namespace) + "/" + name
}

func (c catalogCleanup) appText() string { return "App " + c.namespace + "/" + c.release }

// precheck refuses, writing nothing, when the cluster holds the App, any
// Operation in the release namespace or, on local, a RoleTemplate of the
// chart: objects an earlier run left behind, whose Operation names that
// run no longer has. The refusal names each one and the steps that remove
// it.
func (c catalogCleanup) precheck(ctx context.Context, tgt Target, captured map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, c.precheckBound)
	defer cancel()
	var leftovers []string
	rctx, rcancel := context.WithTimeout(ctx, c.requestTimeout)
	_, err := adminTable(rctx, tgt, c.appPath(captured), "apps")
	rcancel()
	switch {
	case err == nil:
		leftovers = append(leftovers, c.appText()+": check that no Operation in namespace "+c.namespace+
			" still has its Reconciling condition True; send the App's uninstall action once and wait until the App answers 404."+
			" The Operation that uninstall creates is then removed like any leftover Operation.")
	case !errors.Is(err, errTableNotFound):
		return fmt.Errorf("reading %s: %w", c.appText(), err)
	}
	ops, err := c.table(ctx, tgt, captured, catalogSet{"operations", c.namespace})
	if err != nil {
		return err
	}
	for _, op := range ops {
		leftovers = append(leftovers, "Operation "+op.ref.namespace+"/"+op.ref.name+": check that its Reconciling condition is not True;"+
			" read its pod (status.podName, status.podNamespace) and the ClusterRole the pod's owner reference names."+
			" Then remove by name, each with its uid as the delete precondition: the Operation, the pod, the ClusterRoleBindings whose "+
			forRoleLabel+" label names that ClusterRole, then the ClusterRole (the objects it owns go with it).")
	}
	if c.cluster == "local" {
		templates, err := c.table(ctx, tgt, captured, catalogSet{"roletemplates", ""})
		if err != nil {
			return err
		}
		for _, rt := range templates {
			if strings.HasPrefix(rt.ref.name, "virt-") {
				leftovers = append(leftovers, "RoleTemplate "+rt.ref.name+": check that no App "+c.release+
					" exists on the cluster (its uninstall removes them). Then remove it by name with its uid as the delete precondition.")
			}
		}
	}
	if len(leftovers) == 0 {
		return nil
	}
	return fmt.Errorf("cluster %s holds objects an earlier run left behind, and this run wrote nothing; remove each one as its line says, then run again:\n- %s",
		substitute(c.cluster, captured), strings.Join(leftovers, "\n- "))
}

// indexWaitStep is one step of the wait for the chart version in the
// repository index. The first step stores the deadline of the whole wait;
// a step that finds the chart listed stores a marker, and a later step that
// finds the marker sends nothing. A step whose time ends before the
// deadline returns nil, so the next step goes on; the last step, or one
// that finds the deadline passed, fails.
func (c catalogCleanup) indexWaitStep(last bool) func(ctx context.Context, tgt Target, captured map[string]string) error {
	return func(ctx context.Context, tgt Target, captured map[string]string) error {
		if captured[catalogIndexListed] != "" {
			return nil
		}
		if captured[catalogIndexDeadline] == "" {
			captured[catalogIndexDeadline] = time.Now().Add(c.indexWait).Format(time.RFC3339Nano)
		}
		deadline, err := time.Parse(time.RFC3339Nano, captured[catalogIndexDeadline])
		if err != nil {
			return fmt.Errorf("%s is not an RFC 3339 time", catalogIndexDeadline)
		}
		end := time.Now().Add(c.indexStep)
		if deadline.Before(end) {
			end = deadline
		}
		sctx, cancel := context.WithDeadline(ctx, end)
		defer cancel()
		path := c.steve(captured) + "/v1/catalog.cattle.io.clusterrepos/" + c.repo + "?link=index"
		lastStatus := "no answer"
		for {
			index, status, err := adminJSON(sctx, tgt, http.MethodGet, path, "")
			if err == nil {
				lastStatus = fmt.Sprintf("status %d", status)
				if status == http.StatusOK && indexLists(index, c.chart, c.version) {
					captured[catalogIndexListed] = "true"
					return nil
				}
			}
			select {
			case <-sctx.Done():
				if last || !time.Now().Before(deadline) {
					return fmt.Errorf("the index of ClusterRepo %s does not list %s %s after %s (last %s)", c.repo, c.chart, c.version, c.indexWait, lastStatus)
				}
				return nil
			case <-time.After(c.pollInterval):
			}
		}
	}
}

// indexLists reports whether a decoded Helm repository index lists the
// chart at the version.
func indexLists(index map[string]any, chart, version string) bool {
	entries, _ := index["entries"].(map[string]any)
	versions, _ := entries[chart].([]any)
	for _, v := range versions {
		if m, ok := v.(map[string]any); ok && m["version"] == version {
			return true
		}
	}
	return false
}

// generateRequestID stores a new request id as app install generates one:
// 16 random bytes in hex.
func generateRequestID(_ context.Context, _ Target, captured map[string]string) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generating the request id: %w", err)
	}
	captured[requestIDCapture] = hex.EncodeToString(b)
	return nil
}

// clusterValues reads the values app install adds for the probe's cluster
// from the same server reads and stores each as its JSON encoding.
func (c catalogCleanup) clusterValues(ctx context.Context, tgt Target, captured map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, c.valuesBound)
	defer cancel()
	id := substitute(c.cluster, captured)
	read := func(path string) (map[string]any, error) {
		body, status, err := c.getJSON(ctx, tgt, http.MethodGet, path, "")
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		if status != http.StatusOK || body == nil {
			return nil, fmt.Errorf("reading %s: status %d", path, status)
		}
		return body, nil
	}
	cluster, err := read("/v1/management.cattle.io.clusters/" + id)
	if err != nil {
		return err
	}
	projects, err := read("/v1/management.cattle.io.projects/" + id)
	if err != nil {
		return err
	}
	setting, err := read("/v1/management.cattle.io.settings/server-url")
	if err != nil {
		return err
	}
	name := nestedString(cluster, "spec.displayName")
	if name == "" {
		name = nestedString(cluster, "metadata.name")
	}
	var system []string
	items, _ := projects["data"].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		meta, _ := m["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if n, _ := meta["name"].(string); n != "" && labels["authz.management.cattle.io/system-project"] == "true" {
			system = append(system, n)
		}
	}
	systemProject := ""
	if len(system) > 0 {
		systemProject = slices.Min(system)
	}
	url, _ := setting["value"].(string)
	if url == "" {
		url, _ = setting["default"].(string)
	}
	values := map[string]string{
		"clusterId":            id,
		"clusterName":          name,
		"systemProjectId":      systemProject,
		"url":                  url,
		"rkePathPrefix":        nestedString(cluster, "spec.rancherKubernetesEngineConfig.prefixPath"),
		"rkeWindowsPathPrefix": nestedString(cluster, "spec.rancherKubernetesEngineConfig.winPrefixPath"),
	}
	for _, key := range catalogValueKeys {
		encoded, err := json.Marshal(values[key])
		if err != nil {
			return fmt.Errorf("encoding the cluster value %s: %w", key, err)
		}
		if strings.Contains(string(encoded), "{") {
			return fmt.Errorf("the cluster value %s holds a { character, which a request placeholder cannot carry", key)
		}
		captured[catalogValuePrefix+key] = string(encoded)
	}
	return nil
}

// snapshot stores the names and uids of every set, then the marker that the
// snapshot was taken.
func (c catalogCleanup) snapshot(ctx context.Context, tgt Target, captured map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, c.snapshotBound)
	defer cancel()
	for _, s := range c.sets() {
		rows, err := c.table(ctx, tgt, captured, s)
		if err != nil {
			return err
		}
		refs := make([]objRef, len(rows))
		for i, r := range rows {
			refs[i] = r.ref
		}
		captured[catalogBeforePrefix+s.id()] = encodeRefs(refs)
	}
	captured[catalogSnapshotTaken] = "true"
	return nil
}

// startWaitBudget stores the time the probe's Until observations must end
// by. It is the last setup step, right before the install, so the budget
// covers the reads of the waits only.
func (c catalogCleanup) startWaitBudget(_ context.Context, _ Target, captured map[string]string) error {
	captured[waitDeadlineCapture] = time.Now().Add(c.waitBudget).Format(time.RFC3339Nano)
	return nil
}

// conditionStatus is the status of the condition of type typ in a decoded
// object's status.conditions, and whether the condition is present.
func conditionStatus(obj map[string]any, typ string) (string, bool) {
	status, _ := obj["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, c := range conditions {
		m, _ := c.(map[string]any)
		if m["type"] == typ {
			s, _ := m["status"].(string)
			return s, true
		}
	}
	return "", false
}

// generationObserved reports whether a decoded object's
// status.observedGeneration is at least its metadata.generation.
func generationObserved(obj map[string]any) bool {
	meta, _ := obj["metadata"].(map[string]any)
	status, _ := obj["status"].(map[string]any)
	generation, _ := meta["generation"].(float64)
	observed, ok := status["observedGeneration"].(float64)
	return ok && observed >= generation
}

// operationState describes an Operation's conditions for a wait message.
func operationState(obj map[string]any) string {
	parts := []string{}
	if !generationObserved(obj) {
		parts = append(parts, "observedGeneration behind generation")
	}
	for _, typ := range []string{"Reconciling", "Stalled"} {
		s, ok := conditionStatus(obj, typ)
		if !ok {
			s = "absent"
		}
		parts = append(parts, typ+" "+s)
	}
	return "Operation with " + strings.Join(parts, ", ")
}

// operationEnded reports whether Helm's run of an Operation has ended, as
// app install --wait decides it: the generation observed and Stalled True
// or Reconciling False.
func operationEnded(obj map[string]any) (ended, stalled bool) {
	if !generationObserved(obj) {
		return false, false
	}
	if s, ok := conditionStatus(obj, "Stalled"); ok && s == "True" {
		return true, true
	}
	if s, ok := conditionStatus(obj, "Reconciling"); ok && s == "False" {
		return true, false
	}
	return false, false
}

// waitFor polls check at the poll interval until it reports done or ctx
// ends. A check error ends it at once.
func (c catalogCleanup) waitFor(ctx context.Context, check func() (bool, error)) error {
	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.pollInterval):
		}
	}
}

// uninstall sends one uninstall for the App this run installed when the
// probe's own uninstall did not run: only for an App that carries this
// run's request id, after the install Operation has ended. It sends
// nothing without a request id or a recorded install Operation, and fails,
// sending nothing, for any App or Operation it cannot prove is this run's.
func (c catalogCleanup) uninstall(ctx context.Context, tgt Target, captured map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, c.uninstallTime)
	defer cancel()
	requestID := captured[requestIDCapture]
	installOp := captured[c.installOp]
	if requestID == "" || installOp == "" {
		return nil
	}
	if c.uninstallOp != "" && captured[c.uninstallOp] != "" {
		_, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.appPath(captured), "")
		switch {
		case err != nil:
			return fmt.Errorf("reading %s after the recorded uninstall: %w", c.appText(), err)
		case status == http.StatusNotFound:
			return nil
		}
		return fmt.Errorf("%s still answers status %d after the recorded uninstall; it is left in place", c.appText(), status)
	}
	before := map[string]bool{}
	for _, r := range decodeRefs(captured[catalogBeforePrefix+catalogSet{"operations", c.namespace}.id()]) {
		before[r.name] = true
	}
	ops, err := c.table(ctx, tgt, captured, catalogSet{"operations", c.namespace})
	if err != nil {
		return err
	}
	for _, op := range ops {
		if before[op.ref.name] || op.ref.name == installOp {
			continue
		}
		obj, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.operationPath(captured, op.ref.name), "")
		if err != nil {
			return fmt.Errorf("reading the Operation %s/%s: %w", c.namespace, op.ref.name, err)
		}
		if status != http.StatusOK {
			continue
		}
		if nestedString(obj, "status.action") == "uninstall" {
			return fmt.Errorf("the Operation %s/%s is an uninstall this run did not record; nothing was sent", c.namespace, op.ref.name)
		}
	}
	err = c.waitFor(ctx, func() (bool, error) {
		obj, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.operationPath(captured, installOp), "")
		if err != nil || status != http.StatusOK {
			return false, nil
		}
		ended, _ := operationEnded(obj)
		return ended, nil
	})
	if err != nil {
		return fmt.Errorf("the install Operation %s/%s did not end within %s; nothing was sent", c.namespace, installOp, c.uninstallTime)
	}
	app, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.appPath(captured), "")
	switch {
	case err != nil:
		return fmt.Errorf("reading %s: %w", c.appText(), err)
	case status == http.StatusNotFound:
		return nil
	case status != http.StatusOK:
		return fmt.Errorf("%s answered status %d; nothing was sent", c.appText(), status)
	}
	annotations, _ := nestedValue(app, "spec.chart.metadata.annotations").(map[string]any)
	if id, _ := annotations[catalogRequestIDAnnotation].(string); id == "" || id != requestID {
		return fmt.Errorf("%s does not carry this run's request id; it is left in place", c.appText())
	}
	if strings.HasPrefix(nestedString(app, "spec.info.status"), "pending-") {
		return fmt.Errorf("%s is still pending; it is left in place", c.appText())
	}
	answer, status, err := c.getJSON(ctx, tgt, http.MethodPost, c.steve(captured)+"/v1/catalog.cattle.io.apps/"+c.namespace+"/"+c.release+"?action=uninstall", "{}")
	if err != nil {
		return fmt.Errorf("sending the uninstall of %s: %w", c.appText(), err)
	}
	cleanupOp, _ := answer["operationName"].(string)
	if status != http.StatusCreated || cleanupOp == "" {
		return fmt.Errorf("the uninstall of %s answered status %d with no Operation name", c.appText(), status)
	}
	captured[catalogCleanupOp] = cleanupOp
	err = c.waitFor(ctx, func() (bool, error) {
		obj, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.operationPath(captured, cleanupOp), "")
		if err != nil || status != http.StatusOK {
			return false, nil
		}
		ended, stalled := operationEnded(obj)
		if stalled {
			return false, fmt.Errorf("the uninstall Operation %s/%s is Stalled", c.namespace, cleanupOp)
		}
		if !ended {
			return false, nil
		}
		_, appStatus, err := c.getJSON(ctx, tgt, http.MethodGet, c.appPath(captured), "")
		return err == nil && appStatus == http.StatusNotFound, nil
	})
	if err != nil {
		return fmt.Errorf("the uninstall of %s did not finish within %s: %w", c.appText(), c.uninstallTime, err)
	}
	return nil
}

// nestedValue reads a dot-separated path of keys from a decoded object.
func nestedValue(obj map[string]any, path string) any {
	var v any = obj
	for k := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// recordedOperations are the Operation names this run recorded.
func (c catalogCleanup) recordedOperations(captured map[string]string) []string {
	var names []string
	for _, key := range []string{c.installOp, c.uninstallOp, catalogCleanupOp} {
		if key != "" && captured[key] != "" && !slices.Contains(names, captured[key]) {
			names = append(names, captured[key])
		}
	}
	return names
}

// operationChain reads one Operation, its pod and the ClusterRole that owns
// both. It returns the objects it can prove belong to the Operation: all
// three when the pod's annotation names the Operation's ClusterRole owner
// and the owner uids of the Operation and the pod equal the ClusterRole's
// uid, else the Operation and the pod alone, with an error.
func (c catalogCleanup) operationChain(ctx context.Context, tgt Target, captured map[string]string, name string) ([]objRef, error) {
	opText := "Operation " + c.namespace + "/" + name
	op, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.operationPath(captured, name), "")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", opText, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s answered status %d; nothing of it is recorded", opText, status)
	}
	opMeta := metadataOf(op, "operations")
	refs := []objRef{opMeta.ref}
	podName := nestedString(op, "status.podName")
	podNamespace := nestedString(op, "status.podNamespace")
	if podName == "" || podNamespace == "" {
		return refs, fmt.Errorf("%s names no pod; only the Operation is recorded", opText)
	}
	pod, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.kube(captured)+collectionPath("pods", podNamespace)+"/"+podName, "")
	if err != nil {
		return refs, fmt.Errorf("reading the pod of %s: %w", opText, err)
	}
	if status != http.StatusOK {
		return refs, fmt.Errorf("the pod of %s answered status %d; only the Operation is recorded", opText, status)
	}
	podMeta := metadataOf(pod, "pods")
	refs = append(refs, podMeta.ref)
	opOwner, ok := clusterRoleOwner(opMeta)
	podOwner, podOK := clusterRoleOwner(podMeta)
	if !ok || !podOK || podMeta.annotations[podClusterRoleAnnotation] != opOwner.name {
		return refs, fmt.Errorf("the pod of %s does not name the Operation's ClusterRole owner; only the Operation and the pod are recorded", opText)
	}
	role, status, err := c.getJSON(ctx, tgt, http.MethodGet, c.kube(captured)+collectionPath("clusterroles", "")+"/"+opOwner.name, "")
	if err != nil {
		return refs, fmt.Errorf("reading the ClusterRole of %s: %w", opText, err)
	}
	if status != http.StatusOK {
		return refs, fmt.Errorf("the ClusterRole of %s answered status %d; only the Operation and the pod are recorded", opText, status)
	}
	roleMeta := metadataOf(role, "clusterroles")
	if opOwner.uid == "" || opOwner.uid != podOwner.uid || opOwner.uid != roleMeta.ref.uid {
		return refs, fmt.Errorf("the owner uids of %s, its pod and its ClusterRole differ; only the Operation and the pod are recorded", opText)
	}
	return append(refs, roleMeta.ref), nil
}

// metadataOf decodes the metadata of a JSON object.
func metadataOf(obj map[string]any, set string) tableRow {
	var meta tableMetadata
	if raw, err := json.Marshal(obj["metadata"]); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	return meta.row(set)
}

// clusterRoleOwner is the ClusterRole among a row's owner references.
func clusterRoleOwner(r tableRow) (ownerRef, bool) {
	for _, o := range r.owners {
		if o.kind == "ClusterRole" {
			return o, true
		}
	}
	return ownerRef{}, false
}

// deleteRank orders the deletes: Operations, pods, the ClusterRoleBindings
// found by their for-role label, ClusterRoles, then the rest.
func deleteRank(r objRef, forRole map[string]bool) int {
	switch {
	case r.set == "operations":
		return 0
	case r.set == "pods":
		return 1
	case r.set == "clusterrolebindings" && forRole[r.uid]:
		return 2
	case r.set == "clusterroles":
		return 3
	}
	return 4
}

// record stores every object this run's Operations created: each
// Operation's chain, then every listed object whose owner references reach
// a recorded uid, and every ClusterRoleBinding whose for-role label names a
// recorded ClusterRole. An Operation it cannot read or prove is named in
// the error; the other Operations are still recorded.
func (c catalogCleanup) record(ctx context.Context, tgt Target, captured map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, c.recordBound)
	defer cancel()
	var errs []error
	var recorded []objRef
	uids := map[string]bool{}
	roles := map[string]bool{}
	add := func(r objRef) bool {
		if r.uid == "" || uids[r.uid] {
			return false
		}
		uids[r.uid] = true
		recorded = append(recorded, r)
		if r.set == "clusterroles" {
			roles[r.name] = true
		}
		return true
	}
	for _, name := range c.recordedOperations(captured) {
		refs, err := c.operationChain(ctx, tgt, captured, name)
		for _, r := range refs {
			add(r)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	forRole := map[string]bool{}
	if len(recorded) > 0 {
		var rows []tableRow
		for _, s := range c.sets() {
			listed, err := c.table(ctx, tgt, captured, s)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			rows = append(rows, listed...)
		}
		for added := true; added; {
			added = false
			for _, row := range rows {
				if uids[row.ref.uid] {
					continue
				}
				labelled := row.ref.set == "clusterrolebindings" && roles[row.labels[forRoleLabel]]
				owned := slices.ContainsFunc(row.owners, func(o ownerRef) bool { return uids[o.uid] })
				if (labelled || owned) && add(row.ref) {
					if labelled {
						forRole[row.ref.uid] = true
					}
					added = true
				}
			}
		}
	}
	slices.SortStableFunc(recorded, func(a, b objRef) int {
		return cmp.Compare(deleteRank(a, forRole), deleteRank(b, forRole))
	})
	for _, r := range recorded {
		stepLog(ctx, "created: "+r.String())
	}
	if len(recorded) > 0 {
		captured[catalogRecorded] = encodeRefs(recorded)
	}
	return errors.Join(errs...)
}

// deleteRecorded deletes one recorded object by its name, with its
// recorded uid as the precondition, and returns the status. It is the
// cleanup's one delete.
func (c catalogCleanup) deleteRecorded(ctx context.Context, tgt Target, captured map[string]string, r objRef) (int, error) {
	body := fmt.Sprintf(`{"preconditions":{"uid":%q}}`, r.uid)
	_, status, err := c.getJSON(ctx, tgt, http.MethodDelete, c.kube(captured)+collectionPath(r.set, r.namespace)+"/"+r.name, body)
	return status, err
}

// remove deletes every recorded object in the recorded order. A 2xx or a
// 404 is done; every other answer is collected, and the next object is
// still deleted.
func (c catalogCleanup) remove(ctx context.Context, tgt Target, captured map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, c.removeBound)
	defer cancel()
	var errs []error
	for _, r := range decodeRefs(captured[catalogRecorded]) {
		status, err := c.deleteRecorded(ctx, tgt, captured, r)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("deleting %s: %w", r, err))
		case status >= 200 && status <= 299:
			stepLog(ctx, "deleted: "+r.String())
		case status == http.StatusNotFound:
		default:
			errs = append(errs, fmt.Errorf("deleting %s: status %d", r, status))
		}
	}
	return errors.Join(errs...)
}

// waitGone reads each recorded object as a Table at the poll interval until
// each answers 404 or carries another uid.
func (c catalogCleanup) waitGone(ctx context.Context, tgt Target, captured map[string]string) error {
	left := decodeRefs(captured[catalogRecorded])
	if len(left) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.goneBound)
	defer cancel()
	err := c.waitFor(ctx, func() (bool, error) {
		left = slices.DeleteFunc(left, func(r objRef) bool {
			rctx, rcancel := context.WithTimeout(ctx, c.requestTimeout)
			defer rcancel()
			rows, err := adminTable(rctx, tgt, c.kube(captured)+collectionPath(r.set, r.namespace)+"/"+r.name, r.set)
			if errors.Is(err, errTableNotFound) {
				return true
			}
			return err == nil && !slices.ContainsFunc(rows, func(row tableRow) bool { return row.ref.uid == r.uid })
		})
		return len(left) == 0, nil
	})
	if err == nil {
		return nil
	}
	names := make([]string, len(left))
	for i, r := range left {
		names[i] = r.String()
	}
	return fmt.Errorf("still present after %s: %s", c.goneBound, strings.Join(names, ", "))
}

// compare lists every set again and checks each name that was not there
// before the run. A name in the release namespace, or one with an
// operation prefix whose owners lead to no Operation outside the release
// namespace, fails the probe and stays in place; any other new name is
// logged as not ours.
func (c catalogCleanup) compare(ctx context.Context, tgt Target, captured map[string]string) error {
	if captured[catalogSnapshotTaken] == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.compareBound)
	defer cancel()
	before := map[string]bool{}
	for _, s := range c.sets() {
		for _, r := range decodeRefs(captured[catalogBeforePrefix+s.id()]) {
			before[r.set+"\t"+r.namespace+"\t"+r.name] = true
		}
	}
	var after []tableRow
	for _, s := range c.sets() {
		rows, err := c.table(ctx, tgt, captured, s)
		if err != nil {
			return err
		}
		after = append(after, rows...)
	}
	allOps, err := adminTableWithin(ctx, c.requestTimeout, tgt, c.kube(captured)+"/apis/catalog.cattle.io/v1/operations", "operations")
	if err != nil {
		return fmt.Errorf("listing operations in every namespace: %w", err)
	}
	byUID := map[string]tableRow{}
	for _, r := range after {
		byUID[r.ref.uid] = r
	}
	roleByName := map[string]tableRow{}
	for _, r := range after {
		if r.ref.set == "clusterroles" {
			roleByName[r.ref.name] = r
		}
	}
	var candidates []objRef
	for _, r := range after {
		if before[r.ref.set+"\t"+r.ref.namespace+"\t"+r.ref.name] {
			continue
		}
		switch {
		case r.ref.namespace == c.namespace:
			candidates = append(candidates, r.ref)
		case slices.ContainsFunc(operationPrefixes, func(p string) bool { return strings.HasPrefix(r.ref.name, p) }):
			ns, ok := operationNamespaceOf(r, byUID, roleByName, allOps)
			if !ok || ns == c.namespace {
				candidates = append(candidates, r.ref)
				continue
			}
			stepLog(ctx, "not ours: "+r.ref.String())
		default:
			stepLog(ctx, "not ours: "+r.ref.String())
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Rancher removes a RoleTemplate's finalizer only after it has deleted
	// the RoleTemplate's ClusterRoles in every cluster, so an object the
	// uninstall removes can still be listed for a while after it.
	_ = c.waitFor(ctx, func() (bool, error) {
		candidates = slices.DeleteFunc(candidates, func(r objRef) bool {
			rows, err := c.table(ctx, tgt, captured, catalogSet{r.set, r.namespace})
			return err == nil && !slices.ContainsFunc(rows, func(row tableRow) bool { return row.ref.name == r.name })
		})
		return len(candidates) == 0, nil
	})
	if len(candidates) == 0 {
		return nil
	}
	names := make([]string, len(candidates))
	for i, r := range candidates {
		names[i] = r.String()
	}
	return fmt.Errorf("new objects this run did not record remain and are left in place: %s", strings.Join(names, ", "))
}

// adminTableWithin is adminTable within timeout.
func adminTableWithin(ctx context.Context, timeout time.Duration, tgt Target, path, set string) ([]tableRow, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return adminTable(rctx, tgt, path, set)
}

// operationNamespaceOf follows a row's owner references, and a
// ClusterRoleBinding's for-role label, to a ClusterRole, and returns the
// namespace of the Operation that ClusterRole owns. ok is false when no
// ClusterRole is reached or it owns no Operation.
func operationNamespaceOf(r tableRow, byUID, roleByName map[string]tableRow, ops []tableRow) (string, bool) {
	seen := map[string]bool{}
	var role *tableRow
	for cur := &r; cur != nil && role == nil; {
		if seen[cur.ref.uid] {
			break
		}
		seen[cur.ref.uid] = true
		if cur.ref.set == "clusterroles" {
			role = cur
			break
		}
		if cur.ref.set == "clusterrolebindings" {
			if named, ok := roleByName[cur.labels[forRoleLabel]]; ok {
				role = &named
				break
			}
		}
		var next *tableRow
		for _, o := range cur.owners {
			if owner, ok := byUID[o.uid]; ok {
				next = &owner
				break
			}
		}
		cur = next
	}
	if role == nil {
		return "", false
	}
	for _, op := range ops {
		if slices.ContainsFunc(op.owners, func(o ownerRef) bool { return o.uid == role.ref.uid }) {
			return op.ref.namespace, true
		}
	}
	return "", false
}
