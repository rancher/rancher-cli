package fidelity

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// catalogCanary is in every label value, annotation value and body text the
// catalog fake serves, except the fields the cluster values are read from.
// No error, log line or stored value of the cleanup may hold it.
const catalogCanary = "canary-5e1f9b"

// catalogFakeUUID is the install id the catalog fake serves.
const catalogFakeUUID = "0f6c3e1a-7d2b-4c59-9e1f-2a4b6c8d0e1f"

// fakeObject is one object the catalog fake serves.
type fakeObject struct {
	set, namespace, name, uid string
	labels, annotations       map[string]string
	owners                    []ownerRef
	// extra holds the top-level keys besides metadata, such as status.
	extra    map[string]any
	deleting bool
	// ours marks an object a probe Operation created: the only kind of
	// object the cleanup may delete.
	ours bool
	// generation is metadata.generation.
	generation int
}

func (o *fakeObject) key() string { return o.set + "\t" + o.namespace + "\t" + o.name }

func (o *fakeObject) ref() objRef {
	return objRef{set: o.set, namespace: o.namespace, name: o.name, uid: o.uid}
}

func (o *fakeObject) metadata() map[string]any {
	labels := map[string]any{"canary": catalogCanary}
	for k, v := range o.labels {
		labels[k] = v
	}
	annotations := map[string]any{"canary": catalogCanary}
	for k, v := range o.annotations {
		annotations[k] = v
	}
	owners := []any{}
	for _, ow := range o.owners {
		owners = append(owners, map[string]any{"apiVersion": "v1", "kind": ow.kind, "name": ow.name, "uid": ow.uid})
	}
	m := map[string]any{
		"name": o.name, "uid": o.uid, "labels": labels, "annotations": annotations,
		"ownerReferences": owners, "generation": o.generation, "resourceVersion": "1",
		"creationTimestamp": "2026-11-11T00:00:00Z",
	}
	if o.namespace != "" {
		m["namespace"] = o.namespace
	}
	if o.deleting {
		m["deletionTimestamp"] = "2026-11-11T00:00:01Z"
	}
	return m
}

// fakeRequest is one request the catalog fake answered.
type fakeRequest struct {
	method, path, query, accept, body string
	status                            int
}

func (r fakeRequest) String() string { return r.method + " " + r.path }

// catalogFake is a Rancher that serves one cluster's catalog actions, the
// objects each Operation creates, Table and JSON reads, deletes with the uid
// precondition and a garbage collector that removes owned objects. It
// refuses a Secret read without the Table-only Accept header and records a
// violation for it, and for any DELETE that is not of one object a probe
// Operation created, by name, with a uid precondition and no selector.
type catalogFake struct {
	t       *testing.T
	srv     *httptest.Server
	cluster string

	mu         sync.Mutex
	objects    map[string]*fakeObject
	seq        int
	ops        int
	requests   []fakeRequest
	violations []string
	reads      map[string]int

	// installOutcome is what an install does: "deployed" (an App at
	// deployed), "stall" (a Stalled Operation and no App), "pending" (an
	// App that stays pending-install), "otherid" (an App at deployed with
	// another request id) or "noid" (an App at deployed with no request
	// id).
	installOutcome string
	// readsBeforeEnd is the number of reads an Operation answers
	// Reconciling True before it ends.
	readsBeforeEnd int
	// templateReadsAfterUninstall is the number of RoleTemplate list reads
	// that still list the chart's RoleTemplates, with a deletionTimestamp,
	// after the uninstall; -1 keeps them for good.
	templateReadsAfterUninstall int
	templatesGoneAfter          int
	// deleteStatus answers a DELETE of the object named "<set>/<name>"
	// with the status instead of deleting it.
	deleteStatus map[string]int
	// undeletable names objects a DELETE answers 200 for and keeps.
	undeletable map[string]bool
	// indexListed is false for an index without the chart.
	indexListed bool
	// answer, when set, answers a request before the fake does; it
	// returns false to let the fake answer.
	answer func(w http.ResponseWriter, r *http.Request) bool
	// afterInstall runs, under the lock, after an install created its
	// objects.
	afterInstall func(f *catalogFake, op string)
}

// newCatalogFake starts a fake for cluster, "local" or testDownstream.
func newCatalogFake(t *testing.T, cluster string) *catalogFake {
	t.Helper()
	f := &catalogFake{
		t: t, cluster: cluster, objects: map[string]*fakeObject{}, reads: map[string]int{},
		installOutcome: "deployed", indexListed: true,
		deleteStatus: map[string]int{}, undeletable: map[string]bool{},
	}
	if cluster != "local" {
		f.installOutcome = "stall"
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	// What every cluster has before a run: Rancher's own objects in the
	// snapshot sets.
	f.add(&fakeObject{set: "pods", namespace: "cattle-system", name: "cattle-cluster-agent-1"})
	f.add(&fakeObject{set: "secrets", namespace: "cattle-system", name: "tls-ca"})
	f.add(&fakeObject{set: "clusterroles", name: "cluster-admin"})
	if cluster == "local" {
		f.add(&fakeObject{set: "roletemplates", name: "cluster-owner"})
	}
	return f
}

// target is the own-pair target of the fake.
func (f *catalogFake) target() *OracleTarget {
	return &OracleTarget{baseURL: f.srv.URL, client: f.srv.Client(), admin: "admin-tok", downstream: testDownstream, pairInstallUUID: catalogFakeUUID}
}

// add stores o with a new uid and its own copy of its owner references, so
// a test can change one object's owners alone; the caller may hold the lock
// or not.
func (f *catalogFake) add(o *fakeObject) *fakeObject {
	o.owners = slices.Clone(o.owners)
	f.seq++
	o.uid = fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
	if o.generation == 0 {
		o.generation = 1
	}
	f.objects[o.key()] = o
	return o
}

func (f *catalogFake) find(set, namespace, name string) *fakeObject {
	return f.objects[set+"\t"+namespace+"\t"+name]
}

// lookup returns the object or nil, under the lock.
func (f *catalogFake) lookup(set, namespace, name string) *fakeObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(set, namespace, name)
}

// with runs change under the lock.
func (f *catalogFake) with(change func(f *catalogFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// sent returns the requests answered so far.
func (f *catalogFake) sent() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *catalogFake) problems() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.violations)
}

// count is the number of requests of method whose path contains part.
func (f *catalogFake) count(method, part string) int {
	n := 0
	for _, r := range f.sent() {
		if r.method == method && strings.Contains(r.path, part) {
			n++
		}
	}
	return n
}

// ours lists the objects a probe Operation created that still exist.
func (f *catalogFake) ours() []objRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	var refs []objRef
	for _, o := range f.objects {
		if o.ours {
			refs = append(refs, o.ref())
		}
	}
	return refs
}

// steve is the steve prefix of the fake's cluster.
func (f *catalogFake) steve() string {
	if f.cluster == "local" {
		return ""
	}
	return "/k8s/clusters/" + f.cluster
}

// operation creates an Operation of action with every object Rancher
// creates for one, and returns its name.
func (f *catalogFake) operation(action string) string {
	f.ops++
	n := f.ops
	opName := fmt.Sprintf("helm-operation-o%d", n)
	roleName := fmt.Sprintf("pod-impersonation-helm-op-r%d", n)
	role := f.add(&fakeObject{set: "clusterroles", name: roleName, ours: true})
	roleOwner := []ownerRef{{kind: "ClusterRole", name: roleName, uid: role.uid}}
	op := f.add(&fakeObject{set: "operations", namespace: catalogReleaseNamespace, name: opName, owners: roleOwner, ours: true, generation: 1,
		extra: map[string]any{"status": map[string]any{
			"action": action, "podName": opName, "podNamespace": "cattle-system", "namespace": catalogReleaseNamespace,
			"releaseName": "harvester-rbac", "token": catalogCanary, "command": []any{"helm", catalogCanary},
		}}})
	pod := f.add(&fakeObject{set: "pods", namespace: "cattle-system", name: opName, owners: roleOwner, ours: true,
		annotations: map[string]string{podClusterRoleAnnotation: roleName}})
	f.add(&fakeObject{set: "serviceaccounts", namespace: "cattle-system", name: roleName, ours: true,
		owners: []ownerRef{{kind: "Pod", name: opName, uid: pod.uid}}})
	f.add(&fakeObject{set: "secrets", namespace: "cattle-system", name: roleName + "-token", owners: roleOwner, ours: true})
	f.add(&fakeObject{set: "secrets", namespace: "cattle-system", name: fmt.Sprintf("helm-operation-s%d", n), owners: roleOwner, ours: true})
	f.add(&fakeObject{set: "configmaps", namespace: "cattle-system", name: fmt.Sprintf("impersonation-helm-op-admin-kubeconfig-k%d", n), owners: roleOwner, ours: true})
	f.add(&fakeObject{set: "configmaps", namespace: "cattle-system", name: fmt.Sprintf("impersonation-helm-op-user-kubeconfig-k%d", n), owners: roleOwner, ours: true})
	f.add(&fakeObject{set: "clusterrolebindings", name: roleName, owners: roleOwner, ours: true})
	f.add(&fakeObject{set: "clusterrolebindings", name: "helm-op-" + roleName + "-b", ours: true,
		labels: map[string]string{forRoleLabel: roleName}})
	opOwner := []ownerRef{{kind: "Operation", name: opName, uid: op.uid}}
	f.add(&fakeObject{set: "roles", namespace: catalogReleaseNamespace, name: opName + "-user-f4846-role", owners: opOwner, ours: true})
	f.add(&fakeObject{set: "rolebindings", namespace: catalogReleaseNamespace, name: opName + "-user-f4846-rolebinding", owners: opOwner, ours: true})
	return opName
}

// install answers the install action.
func (f *catalogFake) install(body string) (string, int) {
	var req struct {
		Charts []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"charts"`
	}
	if json.Unmarshal([]byte(body), &req) != nil || len(req.Charts) != 1 {
		return "", http.StatusBadRequest
	}
	op := f.operation("install")
	requestID := req.Charts[0].Annotations[catalogRequestIDAnnotation]
	switch f.installOutcome {
	case "deployed", "pending", "otherid", "noid":
		info := "deployed"
		if f.installOutcome == "pending" {
			info = "pending-install"
		}
		if f.installOutcome == "otherid" {
			requestID = "another-request-id"
		}
		annotations := map[string]any{catalogRequestIDAnnotation: requestID, "canary": catalogCanary}
		if f.installOutcome == "noid" {
			delete(annotations, catalogRequestIDAnnotation)
		}
		release := f.add(&fakeObject{set: "secrets", namespace: catalogReleaseNamespace, name: "sh.helm.release.v1.harvester-rbac.v1"})
		f.add(&fakeObject{set: "apps", namespace: catalogReleaseNamespace, name: "harvester-rbac", generation: 2,
			owners: []ownerRef{{kind: "Secret", name: release.name, uid: release.uid}},
			extra: map[string]any{
				"spec": map[string]any{
					"chart": map[string]any{"metadata": map[string]any{"name": "harvester-rbac", "version": "110.0.0+up0.1.1",
						"annotations": annotations}, "values": map[string]any{"a": catalogCanary}},
					"info":   map[string]any{"status": info, "description": "Install complete " + catalogCanary},
					"name":   "harvester-rbac",
					"values": map[string]any{"x": catalogCanary},
				},
				"status": map[string]any{"observedGeneration": 2, "summary": map[string]any{"state": info}},
			}})
		if f.cluster == "local" && f.installOutcome == "deployed" {
			for _, name := range []string{"virt-cluster-manage", "virt-view-cluster", "virt-project-manage", "virt-project-view"} {
				f.add(&fakeObject{set: "roletemplates", name: name})
			}
		}
	}
	if f.afterInstall != nil {
		f.afterInstall(f, op)
	}
	return op, http.StatusCreated
}

// uninstall answers the uninstall action: a new Operation, then the App,
// its release Secret and the chart's RoleTemplates go.
func (f *catalogFake) uninstall() (string, int) {
	app := f.find("apps", catalogReleaseNamespace, "harvester-rbac")
	if app == nil {
		return "", http.StatusNotFound
	}
	op := f.operation("uninstall")
	delete(f.objects, app.key())
	delete(f.objects, "secrets\t"+catalogReleaseNamespace+"\tsh.helm.release.v1.harvester-rbac.v1")
	f.templatesGoneAfter = f.templateReadsAfterUninstall
	for _, o := range f.objects {
		if o.set == "roletemplates" && strings.HasPrefix(o.name, "virt-") {
			if f.templatesGoneAfter == 0 {
				delete(f.objects, o.key())
			} else {
				o.deleting = true
			}
		}
	}
	return op, http.StatusCreated
}

// operationBody is an Operation's JSON body as it is read now: running for
// the first readsBeforeEnd reads, then ended.
func (f *catalogFake) operationBody(o *fakeObject) map[string]any {
	f.reads[o.key()]++
	status, _ := o.extra["status"].(map[string]any)
	stalled := f.installOutcome == "stall" && status["action"] == "install"
	conditions := []any{
		map[string]any{"type": "Reconciling", "status": "True", "lastUpdateTime": "2026-11-11T00:00:02Z"},
	}
	observed := 0
	if f.reads[o.key()] > f.readsBeforeEnd {
		observed = o.generation
		stall := "False"
		if stalled {
			stall = "True"
		}
		conditions = []any{
			map[string]any{"type": "Stalled", "status": stall, "message": " exit code: 123", "reason": "Stalled", "lastUpdateTime": "2026-11-11T00:00:03Z"},
			map[string]any{"type": "Reconciling", "status": "False", "lastUpdateTime": "2026-11-11T00:00:03Z"},
		}
	}
	out := maps.Clone(status)
	if out == nil {
		out = map[string]any{}
	}
	out["conditions"] = conditions
	out["observedGeneration"] = observed
	out["podCreated"] = true
	return map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "Operation", "metadata": o.metadata(), "status": out}
}

// collect removes o and, as the garbage collector does, every object an
// owner reference ties to it.
func (f *catalogFake) collect(o *fakeObject) {
	delete(f.objects, o.key())
	for _, other := range f.objects {
		if slices.ContainsFunc(other.owners, func(ow ownerRef) bool { return ow.uid == o.uid }) {
			f.collect(other)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func notFoundStatus(name string) map[string]any {
	return map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404,
		"message": name + " not found " + catalogCanary}
}

func (f *catalogFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	rec := fakeRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, accept: r.Header.Get("Accept"), body: body}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		rec.status = sw.status
		f.mu.Lock()
		f.requests = append(f.requests, rec)
		f.mu.Unlock()
	}()
	if f.answer != nil && f.answer(sw, r) {
		return
	}
	if r.URL.Path == "/rancherversion" {
		writeJSON(sw, http.StatusOK, map[string]any{"Version": "v2.15.2", "GitCommit": "abc"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer admin-tok" {
		writeJSON(sw, http.StatusUnauthorized, map[string]any{"message": catalogCanary})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	steve := f.steve() + "/v1/"
	switch {
	case path == "/v1/management.cattle.io.settings/install-uuid":
		writeJSON(sw, http.StatusOK, map[string]any{"value": catalogFakeUUID, "canaryText": catalogCanary})
	case path == "/v1/management.cattle.io.settings/server-url":
		writeJSON(sw, http.StatusOK, map[string]any{"value": "https://172.30.1.2", "metadata": map[string]any{"labels": map[string]any{"canary": catalogCanary}}})
	case path == "/v1/management.cattle.io.clusters/"+f.cluster:
		writeJSON(sw, http.StatusOK, map[string]any{
			"metadata": map[string]any{"name": f.cluster, "labels": map[string]any{"canary": catalogCanary}, "annotations": map[string]any{"canary": catalogCanary}},
			"spec":     map[string]any{"displayName": "display-" + f.cluster, "description": catalogCanary, "rancherKubernetesEngineConfig": map[string]any{"prefixPath": "/opt/rke", "winPrefixPath": "c:/rke"}},
		})
	case path == "/v1/management.cattle.io.projects/"+f.cluster:
		writeJSON(sw, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": f.cluster + "/p-zz", "metadata": map[string]any{"name": "p-zz", "labels": map[string]any{"authz.management.cattle.io/system-project": "true", "canary": catalogCanary}}},
			map[string]any{"id": f.cluster + "/p-aa", "metadata": map[string]any{"name": "p-aa", "labels": map[string]any{"authz.management.cattle.io/system-project": "true"}}},
			map[string]any{"id": f.cluster + "/p-00", "metadata": map[string]any{"name": "p-00", "labels": map[string]any{"canary": catalogCanary}}},
		}})
	case path == steve+"catalog.cattle.io.clusterrepos/rancher-charts" && r.Method == http.MethodGet && r.URL.Query().Get("link") == "index":
		entries := map[string]any{"other": []any{map[string]any{"version": "1.0.0", "description": catalogCanary}}}
		if f.indexListed {
			entries["harvester-rbac"] = []any{map[string]any{"version": "110.0.0+up0.1.1", "description": catalogCanary}}
		}
		writeJSON(sw, http.StatusOK, map[string]any{"apiVersion": "v1", "entries": entries})
	case path == steve+"catalog.cattle.io.clusterrepos/rancher-charts" && r.Method == http.MethodPost && r.URL.Query().Get("action") == "install":
		op, status := f.install(body)
		f.actionAnswer(sw, op, status)
	case path == steve+"catalog.cattle.io.apps/"+catalogReleaseNamespace+"/harvester-rbac" && r.Method == http.MethodPost && r.URL.Query().Get("action") == "uninstall":
		op, status := f.uninstall()
		f.actionAnswer(sw, op, status)
	case strings.HasPrefix(path, "/k8s/clusters/"+f.cluster+"/"):
		f.kube(sw, r, strings.TrimPrefix(path, "/k8s/clusters/"+f.cluster), body)
	default:
		writeJSON(sw, http.StatusNotFound, notFoundStatus(path))
	}
}

func (f *catalogFake) actionAnswer(w http.ResponseWriter, op string, status int) {
	if status != http.StatusCreated {
		writeJSON(w, status, map[string]any{"message": catalogCanary})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"type": "chartActionOutput", "links": map[string]any{"self": f.srv.URL + "/" + catalogCanary},
		"operationName": op, "operationNamespace": catalogReleaseNamespace})
}

// statusWriter records the status written.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// kubePath splits a path below /k8s/clusters/<cluster> into its set,
// namespace and name; ok is false for a path of no set the fake serves.
func kubePath(rest string) (set, namespace, name string, ok bool) {
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case len(segs) >= 2 && segs[0] == "api":
		segs = segs[2:]
	case len(segs) >= 3 && segs[0] == "apis":
		segs = segs[3:]
	default:
		return "", "", "", false
	}
	if len(segs) >= 2 && segs[0] == "namespaces" {
		namespace = segs[1]
		segs = segs[2:]
	}
	if len(segs) == 0 || len(segs) > 2 {
		return "", "", "", false
	}
	if _, known := catalogAPIs[segs[0]]; !known {
		return "", "", "", false
	}
	set = segs[0]
	if len(segs) == 2 {
		name = segs[1]
	}
	return set, namespace, name, true
}

func (f *catalogFake) kube(w http.ResponseWriter, r *http.Request, rest, body string) {
	set, namespace, name, ok := kubePath(rest)
	if !ok {
		writeJSON(w, http.StatusNotFound, notFoundStatus(rest))
		return
	}
	table := r.Header.Get("Accept") == tableOnlyAccept
	if set == "secrets" && r.Method == http.MethodGet && !table {
		f.violations = append(f.violations, "a Secret read without the Table-only Accept: "+r.URL.Path)
		writeJSON(w, http.StatusNotAcceptable, map[string]any{"message": catalogCanary})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			f.list(w, set, namespace)
			return
		}
		o := f.find(set, namespace, name)
		switch {
		case o == nil:
			writeJSON(w, http.StatusNotFound, notFoundStatus(name))
		case table:
			f.writeTable(w, set, []*fakeObject{o})
		case set == "operations":
			writeJSON(w, http.StatusOK, f.operationBody(o))
		default:
			out := map[string]any{"apiVersion": "v1", "kind": set, "metadata": o.metadata(), "canaryText": catalogCanary}
			maps.Copy(out, o.extra)
			writeJSON(w, http.StatusOK, out)
		}
	case http.MethodDelete:
		f.delete(w, r, set, namespace, name, body)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": catalogCanary})
	}
}

func (f *catalogFake) list(w http.ResponseWriter, set, namespace string) {
	var objs []*fakeObject
	for _, o := range f.objects {
		if o.set == set && (namespace == "" || o.namespace == namespace) {
			objs = append(objs, o)
		}
	}
	slices.SortFunc(objs, func(a, b *fakeObject) int { return strings.Compare(a.name, b.name) })
	f.writeTable(w, set, objs)
	// The chart's RoleTemplates stay listed, with a deletionTimestamp, for
	// templatesGoneAfter list reads after the uninstall.
	if set == "roletemplates" && f.templatesGoneAfter > 0 {
		f.templatesGoneAfter--
		if f.templatesGoneAfter == 0 {
			for _, o := range objs {
				if o.deleting {
					delete(f.objects, o.key())
				}
			}
		}
	}
}

func (f *catalogFake) writeTable(w http.ResponseWriter, set string, objs []*fakeObject) {
	var rows []any
	for _, o := range objs {
		rows = append(rows, map[string]any{
			"cells":  []any{o.name, catalogCanary},
			"object": map[string]any{"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1", "metadata": o.metadata()},
		})
	}
	// An empty Role list answers rows: null, the others an empty array.
	if rows == nil && set != "roles" {
		rows = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": "Table", "apiVersion": "meta.k8s.io/v1", "columnDefinitions": []any{}, "rows": rows})
}

func (f *catalogFake) delete(w http.ResponseWriter, r *http.Request, set, namespace, name, body string) {
	where := set + " " + namespace + "/" + name
	if name == "" {
		f.violations = append(f.violations, "a DELETE of a collection: "+r.URL.Path)
	}
	if q := r.URL.Query(); q.Has("labelSelector") || q.Has("fieldSelector") {
		f.violations = append(f.violations, "a DELETE with a selector: "+r.URL.String())
	}
	var pre struct {
		Preconditions struct {
			UID string `json:"uid"`
		} `json:"preconditions"`
	}
	if json.Unmarshal([]byte(body), &pre) != nil || pre.Preconditions.UID == "" {
		f.violations = append(f.violations, "a DELETE without a uid precondition: "+where)
	}
	o := f.find(set, namespace, name)
	if o == nil {
		writeJSON(w, http.StatusNotFound, notFoundStatus(name))
		return
	}
	if !o.ours {
		f.violations = append(f.violations, "a DELETE of an object no probe Operation created: "+where)
	}
	if status, ok := f.deleteStatus[set+"/"+name]; ok {
		writeJSON(w, status, map[string]any{"message": catalogCanary})
		return
	}
	if pre.Preconditions.UID != o.uid {
		writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "Conflict", "message": catalogCanary})
		return
	}
	if !f.undeletable[name] {
		f.collect(o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": "Status", "status": "Success", "message": catalogCanary})
}

// recordingLog collects the lines the steps log.
type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLog) log(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *recordingLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// fastCleanup shortens every bound of a cleanup for a test.
func fastCleanup(c *catalogCleanup) {
	c.pollInterval = 10 * time.Millisecond
	c.indexWait = 300 * time.Millisecond
	c.indexStep = 100 * time.Millisecond
	c.waitBudget = 3 * time.Second
	c.requestTimeout = 2 * time.Second
	c.precheckBound = 2 * time.Second
	c.valuesBound = 2 * time.Second
	c.snapshotBound = 3 * time.Second
	c.uninstallTime = 2 * time.Second
	c.recordBound = 3 * time.Second
	c.removeBound = 3 * time.Second
	c.goneBound = 500 * time.Millisecond
	c.compareBound = 500 * time.Millisecond
}

// catalogProbe returns the catalog write probe named name with the bounds
// tune sets, after fastCleanup.
func catalogProbe(t *testing.T, name string, tune func(c *catalogCleanup)) Probe {
	t.Helper()
	for _, p := range catalogWriteProbesWith(func(c *catalogCleanup) {
		fastCleanup(c)
		if tune != nil {
			tune(c)
		}
	}) {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no catalog write probe %s", name)
	return Probe{}
}

const (
	localWriteProbe      = "catalog/install-uninstall-local"
	downstreamWriteProbe = "catalog/failing-install-downstream"
)
