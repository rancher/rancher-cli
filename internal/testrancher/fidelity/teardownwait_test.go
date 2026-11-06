package fidelity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const backingNamespacePrefix = "/k8s/clusters/local/api/v1/namespaces/"

// projectRef is what projectPathOf reports about a request path that names
// a project, the project collection or a project in a form the guard does
// not know.
type projectRef struct {
	// collection is true for a path with no project name.
	collection bool
	// unknown is true for a project path with a name but no cluster to
	// derive a backing namespace from.
	unknown bool
	// backing is the path of the project's backing namespace on the local
	// cluster, derived as <cluster>-<name>, the name Rancher gives the
	// namespace of a project created today; empty for a collection or an
	// unknown form.
	backing string
}

// isProjectType reports whether typ, lower-cased, is the resource type of a
// project in a path rooted at root: steve names the type with its group,
// Norman and the Kubernetes API by the singular or plural name.
func isProjectType(root, typ string) bool {
	if root == "v1" {
		return typ == "management.cattle.io.project" || typ == "management.cattle.io.projects"
	}
	return typ == "project" || typ == "projects"
}

// projectPathOf reports whether a decoded request path names a project or
// the project collection. After an optional /k8s/clusters/<id> prefix, the
// path is read by its root: /v3 (Norman: /v3/projects/<cluster>:<name>, or
// under /v3/clusters/<cluster>), /v1 (steve:
// /v1/management.cattle.io.projects/<cluster>/<name>) or
// /apis/management.cattle.io/<version> (the Kubernetes API:
// namespaces/<cluster>/projects/<name>). The resource type must sit at the
// position its root gives it: a project role template binding path, a
// namespace or a user named projects, and a sub-resource of a project are
// not projects. A project path with a name and no cluster, such as the
// cluster-wide /apis/management.cattle.io/v3/projects/<name>, is unknown: a
// shape the guard rejects because no backing namespace can be derived. A
// Norman id with no colon is read as a collection.
func projectPathOf(path string) (projectRef, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) >= 3 && segs[0] == "k8s" && segs[1] == "clusters" {
		segs = segs[3:]
	}
	if len(segs) == 0 {
		return projectRef{}, false
	}
	lower := func(i int) string {
		if i < len(segs) {
			return strings.ToLower(segs[i])
		}
		return ""
	}
	var typeIdx, maxTail int
	switch segs[0] {
	case "v3":
		typeIdx, maxTail = 1, 1
		if c := lower(1); (c == "cluster" || c == "clusters") && len(segs) >= 4 {
			typeIdx = 3
		}
	case "v1":
		typeIdx, maxTail = 1, 2
	case "apis":
		if lower(1) != "management.cattle.io" || len(segs) < 4 {
			return projectRef{}, false
		}
		typeIdx, maxTail = 3, 1
		if lower(3) == "namespaces" && len(segs) >= 6 {
			typeIdx = 5
		}
	default:
		return projectRef{}, false
	}
	if typeIdx >= len(segs) || !isProjectType(segs[0], lower(typeIdx)) {
		return projectRef{}, false
	}
	tail := segs[typeIdx+1:]
	if len(tail) > maxTail {
		return projectRef{}, false
	}
	named := func(cluster, name string) (projectRef, bool) {
		if cluster == "" || name == "" {
			return projectRef{unknown: true}, true
		}
		return projectRef{backing: backingNamespacePrefix + cluster + "-" + name}, true
	}
	collection := projectRef{collection: true}
	switch {
	case segs[0] == "v3":
		if len(tail) == 0 {
			return collection, true
		}
		cluster, name, ok := strings.Cut(tail[0], ":")
		if !ok || cluster == "" || name == "" {
			return collection, true
		}
		return named(cluster, name)
	case segs[0] == "v1":
		if len(tail) < 2 {
			return collection, true
		}
		return named(tail[0], tail[1])
	case typeIdx == 5:
		if len(tail) == 0 {
			return collection, true
		}
		return named(segs[4], tail[0])
	default:
		if len(tail) == 0 {
			return collection, true
		}
		return projectRef{unknown: true}, true
	}
}

// teardownRequest is one request a teardown step sent: its method, decoded
// path and the status the test server answered.
type teardownRequest struct {
	method string
	path   string
	status int
}

// answeredGone reports whether reqs holds a GET of path that answered 404
// after a GET of it that answered 200.
func answeredGone(reqs []teardownRequest, path string) bool {
	seen := false
	for _, r := range reqs {
		if r.method != http.MethodGet || r.path != path {
			continue
		}
		if r.status == http.StatusOK {
			seen = true
		}
		if r.status == http.StatusNotFound && seen {
			return true
		}
	}
	return false
}

// noProjectWait is the message projectDeletesWithoutWait reports for a
// project DELETE with no wait for the project; noBackingWait, notOneProject
// and unknownProjectForm below are its other messages.
func noProjectWait(path string) string {
	return fmt.Sprintf("DELETE %s is not followed by GETs of the project that answer 200, then 404; wait for the project to answer 404 after the delete", path)
}

func noBackingWait(path, backing string) string {
	return fmt.Sprintf("DELETE %s is not followed by GETs of its backing namespace %s that answer 200, then 404; wait for the backing namespace to answer 404 after the delete", path, backing)
}

func notOneProject(path string) string {
	return fmt.Sprintf("DELETE %s does not name one project; delete a project by its name", path)
}

func unknownProjectForm(path string) string {
	return fmt.Sprintf("DELETE %s is a project delete in a form the guard does not know; add the form to projectPathOf", path)
}

// projectDeletesWithoutWait returns one message for every project delete
// in a teardown's requests that does not wait for it: a collection delete,
// a delete in a form the guard does not know, and a delete of one project
// that no later GET of the project path and of its backing namespace path
// shows going from 200 to 404.
func projectDeletesWithoutWait(reqs []teardownRequest) []string {
	var out []string
	for i, r := range reqs {
		if r.method != http.MethodDelete {
			continue
		}
		ref, ok := projectPathOf(r.path)
		if !ok {
			continue
		}
		if ref.collection {
			out = append(out, notOneProject(r.path))
			continue
		}
		if ref.unknown {
			out = append(out, unknownProjectForm(r.path))
			continue
		}
		later := reqs[i+1:]
		if !answeredGone(later, r.path) {
			out = append(out, noProjectWait(r.path))
		}
		if !answeredGone(later, ref.backing) {
			out = append(out, noBackingWait(r.path, ref.backing))
		}
	}
	return out
}

// TestProjectDeletesWithoutWait asserts the check reports a project delete
// unless later GETs show the project and its backing namespace each going
// from 200 to 404, for each project path form, names in each message the
// path it is about, and never reports a path that only starts like a
// project.
func TestProjectDeletesWithoutWait(t *testing.T) {
	t.Parallel()
	const (
		kube        = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/projects/p-x"
		kubeCluster = "/k8s/clusters/c-m-x/apis/management.cattle.io/v3/namespaces/c-m-x/projects/p-x"
		kubeBare    = "/apis/management.cattle.io/v3/namespaces/c-m-x/projects/p-x"
		norman      = "/v3/projects/c-m-x:p-x"
		normanSub   = "/v3/clusters/c-m-x/projects/c-m-x:p-x"
		normanCase  = "/v3/Project/c-m-x:p-x"
		steve       = "/v1/management.cattle.io.projects/c-m-x/p-x"
		steveProxy  = "/k8s/clusters/local/v1/management.cattle.io.projects/c-m-x/p-x"
		steveSingle = "/v1/management.cattle.io.project/c-m-x/p-x"
		backing     = "/k8s/clusters/local/api/v1/namespaces/c-m-x-p-x"
		wide        = "/k8s/clusters/local/apis/management.cattle.io/v3/projects"
		wideNamed   = "/k8s/clusters/local/apis/management.cattle.io/v3/projects/p-x"
	)
	del := func(path string) teardownRequest { return teardownRequest{http.MethodDelete, path, http.StatusOK} }
	get := func(path string, status int) teardownRequest { return teardownRequest{http.MethodGet, path, status} }
	clean := func(project string) []teardownRequest {
		return []teardownRequest{
			del(project),
			get(project, 200), get(project, 503), get(project, 404),
			get(backing, 200), get(backing, 404),
		}
	}
	both := func(project string) []string {
		return []string{noProjectWait(project), noBackingWait(project, backing)}
	}
	tests := []struct {
		name string
		reqs []teardownRequest
		want []string
	}{
		{"delete with no GETs", []teardownRequest{del(kube)}, both(kube)},
		{"GETs only before the delete", []teardownRequest{get(kube, 200), get(kube, 404), get(backing, 200), get(backing, 404), del(kube)}, both(kube)},
		{"project GETs only", []teardownRequest{del(kube), get(kube, 200), get(kube, 404)}, []string{noBackingWait(kube, backing)}},
		{"backing namespace GETs only", []teardownRequest{del(kube), get(backing, 200), get(backing, 404)}, []string{noProjectWait(kube)}},
		{"a single 404 GET of each", []teardownRequest{del(kube), get(kube, 404), get(backing, 404)}, both(kube)},
		{"200 with no later 404", []teardownRequest{del(kube), get(kube, 200), get(backing, 200)}, both(kube)},
		{"404 then 200", []teardownRequest{del(kube), get(kube, 404), get(kube, 200), get(backing, 404), get(backing, 200)}, both(kube)},
		{"200 then 404 for both", clean(kube), nil},
		{"a wait that stops on the 503", []teardownRequest{del(kube), get(kube, 200), get(kube, 503), get(backing, 200), get(backing, 404)}, []string{noProjectWait(kube)}},
		{"the project clean and the backing namespace GETs before the delete", []teardownRequest{get(backing, 200), get(backing, 404), del(kube), get(kube, 200), get(kube, 404)}, []string{noBackingWait(kube, backing)}},
		{"kubernetes form clean", clean(kube), nil},
		{"kubernetes form violating", []teardownRequest{del(kube)}, both(kube)},
		{"kubernetes form through another cluster id clean", clean(kubeCluster), nil},
		{"kubernetes form through another cluster id violating", []teardownRequest{del(kubeCluster)}, both(kubeCluster)},
		{"kubernetes form without the proxy prefix violating", []teardownRequest{del(kubeBare)}, both(kubeBare)},
		{"norman form clean", clean(norman), nil},
		{"norman form violating", []teardownRequest{del(norman)}, both(norman)},
		{"norman cluster form clean", clean(normanSub), nil},
		{"norman cluster form violating", []teardownRequest{del(normanSub)}, both(normanSub)},
		{"norman singular upper case form violating", []teardownRequest{del(normanCase)}, both(normanCase)},
		{"steve form clean", clean(steve), nil},
		{"steve form violating", []teardownRequest{del(steve)}, both(steve)},
		{"steve form through the cluster proxy clean", clean(steveProxy), nil},
		{"steve form through the cluster proxy violating", []teardownRequest{del(steveProxy)}, both(steveProxy)},
		{"steve singular form violating", []teardownRequest{del(steveSingle)}, both(steveSingle)},
		{"kubernetes collection delete", []teardownRequest{del("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/projects")}, []string{notOneProject("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/projects")}},
		{"cluster-wide kubernetes collection delete", []teardownRequest{del(wide)}, []string{notOneProject(wide)}},
		{"norman collection delete", []teardownRequest{del("/v3/projects")}, []string{notOneProject("/v3/projects")}},
		{"norman cluster collection delete", []teardownRequest{del("/v3/clusters/c-m-x/projects")}, []string{notOneProject("/v3/clusters/c-m-x/projects")}},
		{"norman delete without a project name", []teardownRequest{del("/v3/projects/c-m-x")}, []string{notOneProject("/v3/projects/c-m-x")}},
		{"steve collection delete", []teardownRequest{del("/v1/management.cattle.io.projects")}, []string{notOneProject("/v1/management.cattle.io.projects")}},
		{"steve collection delete through the cluster proxy", []teardownRequest{del("/k8s/clusters/local/v1/management.cattle.io.projects")}, []string{notOneProject("/k8s/clusters/local/v1/management.cattle.io.projects")}},
		{"collection delete after a clean project delete", append(clean(kube), del("/v3/projects")), []string{notOneProject("/v3/projects")}},
		{"cluster-wide kubernetes name has no backing namespace", []teardownRequest{del(wideNamed)}, []string{unknownProjectForm(wideNamed)}},
		{"cluster-wide kubernetes name with waits is still rejected", append(clean(wideNamed), get(wideNamed, 404)), []string{unknownProjectForm(wideNamed)}},
		{"kubernetes binding delete", []teardownRequest{del("/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x-p-x/projectroletemplatebindings/prtb")}, nil},
		{"norman binding delete", []teardownRequest{del("/v3/projectroletemplatebindings/c-m-x:prtb")}, nil},
		{"norman binding under a project delete", []teardownRequest{del("/v3/projects/c-m-x:p-x/projectroletemplatebindings/prtb")}, nil},
		{"steve binding delete", []teardownRequest{del("/v1/management.cattle.io.projectroletemplatebindings/c-m-x-p-x/prtb")}, nil},
		{"a namespace named projects", []teardownRequest{del("/k8s/clusters/local/api/v1/namespaces/projects")}, nil},
		{"a user named projects", []teardownRequest{del("/k8s/clusters/local/apis/management.cattle.io/v3/users/projects")}, nil},
		{"a steve secret named projects", []teardownRequest{del("/v1/secrets/projects/x")}, nil},
		{"another path delete", []teardownRequest{del("/v3/users/u-x")}, nil},
		{"a GET of a project", []teardownRequest{get(kube, 200)}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, projectDeletesWithoutWait(tc.reqs))
		})
	}
}

// TestProjectPathOf asserts the matcher names the backing namespace of each
// named form, tells a collection from a project and from a project path it
// cannot derive a backing namespace from, and reads the resource type only
// where its root puts it.
func TestProjectPathOf(t *testing.T) {
	t.Parallel()
	const backing = "/k8s/clusters/local/api/v1/namespaces/c-m-x-p-x"
	for path, want := range map[string]projectRef{
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/projects/p-x": {backing: backing},
		"/k8s/clusters/c-m-x/apis/management.cattle.io/v3/namespaces/c-m-x/projects/p-x": {backing: backing},
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/projects":     {collection: true},
		"/k8s/clusters/local/apis/management.cattle.io/v3/projects":                      {collection: true},
		"/k8s/clusters/local/apis/management.cattle.io/v3/projects/p-x":                  {unknown: true},
		"/v3/projects/c-m-x:p-x":                      {backing: backing},
		"/v3/Projects/c-m-x:p-x":                      {backing: backing},
		"/v3/project/c-m-x:p-x":                       {backing: backing},
		"/v3/clusters/c-m-x/projects/c-m-x:p-x":       {backing: backing},
		"/v3/clusters/c-m-x/projects":                 {collection: true},
		"/v3/projects":                                {collection: true},
		"/v3/projects/p-x":                            {collection: true},
		"/v1/management.cattle.io.projects/c-m-x/p-x": {backing: backing},
		"/k8s/clusters/local/v1/management.cattle.io.projects/c-m-x/p-x": {backing: backing},
		"/k8s/clusters/c-m-x/v1/management.cattle.io.projects/c-m-x/p-x": {backing: backing},
		"/v1/management.cattle.io.project/c-m-x/p-x":                     {backing: backing},
		"/v1/management.cattle.io.projects":                              {collection: true},
		"/v1/management.cattle.io.projects/c-m-x":                        {collection: true},
		"/k8s/clusters/local/v1/management.cattle.io.projects":           {collection: true},
	} {
		got, ok := projectPathOf(path)
		assert.True(t, ok, path)
		assert.Equal(t, want, got, path)
	}
	for _, path := range []string{
		"/v3/projectroletemplatebindings/c-m-x:prtb",
		"/v3/projects/c-m-x:p-x/projectroletemplatebindings",
		"/v3/projects/c-m-x:p-x/projectroletemplatebindings/prtb",
		"/v3/users/u-x",
		"/v3/clusters/projects",
		"/v1/secrets/projects/x",
		"/v1/management.cattle.io.projects/c-m-x/p-x/extra",
		"/k8s/clusters/local/api/v1/namespaces/projects",
		"/k8s/clusters/local/apis/management.cattle.io/v3/users/projects",
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/projectroletemplatebindings/prtb",
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/clusterroletemplatebindings/projects",
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-m-x/clusterroletemplatebindings/p-x",
		"/k8s/clusters/local/apis/other.cattle.io/v3/namespaces/c-m-x/projects/p-x",
	} {
		_, ok := projectPathOf(path)
		assert.False(t, ok, path)
	}
}

// stepKey names one teardown step of one probe.
type stepKey struct{ probe, step string }

// templateParam matches one {name} placeholder in a path template.
var templateParam = regexp.MustCompile(`\{[^{}/]*\}`)

// unresolvedProjectTemplate returns the path template an unresolved-capture
// error from deleteFixture quotes when that template matches a project
// path form, with each {name} read as one path segment.
func unresolvedProjectTemplate(err error) (string, bool) {
	_, rest, ok := strings.Cut(err.Error(), "capture in ")
	if !ok {
		return "", false
	}
	quoted, qerr := strconv.QuotedPrefix(rest)
	if qerr != nil {
		return "", false
	}
	template, uerr := strconv.Unquote(quoted)
	if uerr != nil {
		return "", false
	}
	_, isProject := projectPathOf(templateParam.ReplaceAllString(template, "x"))
	return template, isProject
}

// unusedAllowEntries returns the allowlist entries no step needed.
func unusedAllowEntries(allow map[stepKey]string, used []stepKey) []stepKey {
	var unused []stepKey
	for key := range allow {
		if !slices.Contains(used, key) {
			unused = append(unused, key)
		}
	}
	slices.SortFunc(unused, func(a, b stepKey) int {
		return strings.Compare(a.probe+"\x00"+a.step, b.probe+"\x00"+b.step)
	})
	return unused
}

// checkTeardown runs each of p.Teardown's steps against a test server
// through the package's fake target, records every request, and reports
// how many project deletes the teardown sent, the problems it found and
// the allowlist entries it needed. The server answers every DELETE 200;
// after a project DELETE the GETs of that project's path and of its
// backing namespace's path answer 200, then 503, then 404; every other GET
// answers 404. Every capture an observation declares is seeded with the
// Norman id c-m-x:p-x; a path built from one names a project.
func checkTeardown(t *testing.T, p Probe, allow map[stepKey]string) (projectDeletes int, problems []string, used []stepKey) {
	t.Helper()
	return checkTeardownWithin(t, p, allow, 15*time.Second)
}

// checkTeardownWithin is checkTeardown with the context each step gets.
func checkTeardownWithin(t *testing.T, p Probe, allow map[stepKey]string, budget time.Duration) (projectDeletes int, problems []string, used []stepKey) {
	t.Helper()
	if len(p.Teardown) == 0 {
		return 0, nil, nil
	}
	var mu sync.Mutex
	var reqs []teardownRequest
	armed := map[string]bool{}
	gets := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		mu.Lock()
		status := http.StatusNotFound
		switch r.Method {
		case http.MethodDelete:
			status = http.StatusOK
			if ref, ok := projectPathOf(path); ok && !ref.collection && !ref.unknown {
				armed[path] = true
				armed[ref.backing] = true
			}
		case http.MethodGet:
			if armed[path] {
				switch gets[path] {
				case 0:
					status = http.StatusOK
				case 1:
					status = http.StatusServiceUnavailable
				}
				gets[path]++
			}
		}
		reqs = append(reqs, teardownRequest{r.Method, path, status})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tgt := adminTarget(srv)

	captured := map[string]string{}
	if p.Downstream {
		captured["downstream"] = testDownstream
	}
	for _, o := range p.Observations {
		for name := range o.Captures {
			captured[name] = "c-m-x:p-x"
		}
	}
	for _, s := range p.Teardown {
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		mu.Lock()
		before := len(reqs)
		mu.Unlock()
		err := s.Do(ctx, tgt, captured)
		expired := ctx.Err() != nil
		cancel()
		mu.Lock()
		sent := len(reqs) - before
		mu.Unlock()

		key := stepKey{p.Name, s.Name}
		if err != nil && !expired {
			if template, ok := unresolvedProjectTemplate(err); ok {
				problems = append(problems, fmt.Sprintf("%s: step %q has an unresolved capture in the project path %q", p.Name, s.Name, template))
				continue
			}
		}
		reason := ""
		switch {
		case expired:
			reason = "ran past its context"
		case err != nil:
			reason = "returned an error: " + err.Error()
		case sent == 0:
			reason = "returned nil and sent no request"
		}
		if reason == "" {
			continue
		}
		if _, ok := allow[key]; ok {
			if !slices.Contains(used, key) {
				used = append(used, key)
			}
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: step %q %s", p.Name, s.Name, reason))
	}

	mu.Lock()
	defer mu.Unlock()
	for _, r := range reqs {
		if ref, ok := projectPathOf(r.path); ok && !ref.unknown && r.method == http.MethodDelete {
			projectDeletes++
		}
	}
	for _, v := range projectDeletesWithoutWait(reqs) {
		problems = append(problems, p.Name+": "+v)
	}
	return projectDeletes, problems, used
}

// teardownAllowlist holds the teardown steps that return an error or send
// no request when run against the test server with only the captures the
// guard can seed: a probe's observations declare some, and its setup steps
// store the rest, which the guard never runs. Every other teardown step
// sends its requests and succeeds. Each entry names the capture its setup
// step stores and what the step deletes.
var teardownAllowlist = map[stepKey]string{
	{"access-downstream/member-cluster-token", "member cluster token"}:                                        "token is stored by the setup step that creates the member's cluster token; the step deletes that token",
	{"access-downstream/owner-management-gate", "cluster owner login token"}:                                  "clusterownerlogin is stored by the cluster owner setup step; the step deletes that login token",
	{"access-downstream/owner-management-gate", "cluster owner user"}:                                         "owneruid is stored by the cluster owner setup step; the step deletes that user",
	{"access-downstream/owner-steve", "cluster owner login token"}:                                            "clusterownerlogin is stored by the cluster owner setup step; the step deletes that login token",
	{"access-downstream/owner-steve", "cluster owner user"}:                                                   "owneruid is stored by the cluster owner setup step; the step deletes that user",
	{"access/standard-cloud-credential-by-name", "standard user's cloud credential"}:                          "ccid is stored by the setup step that creates the cloud credential; the step deletes that credential",
	{"owner-downstream/cluster-login-and-review", "owner user"}:                                               "owneruid is stored by the owner user setup step; the step deletes that user",
	{"owner/login-body-names-no-owner", "owner user"}:                                                         "owneruid is stored by the owner user setup step; the step deletes that user",
	{"login/user-base-user-creates-an-ext-token", "only one global role binding after the login"}:             "owneruid and ownergrb are stored by the owner user setup step; the step reads that user's bindings",
	{"login/clusters-create-user-cannot-create-an-ext-token", "only one global role binding after the login"}: "owneruid and ownergrb are stored by the owner user setup step; the step reads that user's bindings",
	{"login/user-base-user-creates-an-ext-token", "owner user"}:                                               "owneruid is stored by the owner user setup step; the step deletes that user",
	{"login/clusters-create-user-cannot-create-an-ext-token", "owner user"}:                                   "owneruid is stored by the owner user setup step; the step deletes that user",
	{"revoke-downstream/cluster-scoped-v3-token-logout-401", "owner user"}:                                    "owneruid is stored by the owner user setup step; the step deletes that user",
	{"revoke/standard-user-login-token-logout-deletes-itself", "owner user"}:                                  "owneruid is stored by the owner user setup step; the step deletes that user",
	{"serverfacts/install-uuid-no-global-role", "role-less user's login token"}:                               "rolelesslogin is stored by the role-less user setup step; the step deletes that login token",
	{"serverfacts/install-uuid-no-global-role", "role-less user"}:                                             "rolelessuid is stored by the role-less user setup step; the step deletes that user",
	{"authproviders/external-providers-enabled", "restore auth config azuread"}:                               "the marker unrecorded.authconfig.azuread.wrote is stored by the step that enables it and the original by the step that reads the originals; the guard seeds none; the step restores that config",
	{"authproviders/external-providers-enabled", "restore auth config okta"}:                                  "the marker unrecorded.authconfig.okta.wrote is stored by the step that enables it and the original by the step that reads the originals; the guard seeds none; the step restores that config",
	{"authproviders/external-providers-enabled", "restore auth config openldap"}:                              "the marker unrecorded.authconfig.openldap.wrote is stored by the step that enables it and the original by the step that reads the originals; the guard seeds none; the step restores that config",
	{"authproviders/external-providers-enabled", "read back the auth configs"}:                                "the markers unrecorded.authconfig.<name>.wrote are stored by the steps that enable the configs; the guard seeds none; the step reads the written configs back",
	{"authproviders/external-providers-enabled", "wait for the auth provider list to hold local alone"}:       "the markers unrecorded.authconfig.<name>.wrote are stored by the steps that enable the configs; the guard seeds none; the step waits for the list after the restores",
}

// assertGuardTotals asserts the project delete count the probes' teardowns
// sent equals want, that every allowlist entry was used, and that every
// entry has a reason.
func assertGuardTotals(t assert.TestingT, want, got int, allow map[stepKey]string, used []stepKey) {
	assert.Equal(t, want, got, "the teardowns send %d project deletes, want %d; update wantProjectDeletes when a teardown adds or removes a project delete", got, want)
	assert.Empty(t, unusedAllowEntries(allow, used), "allowlist entries no teardown step needed; remove them")
	for key, reason := range allow {
		assert.NotEmpty(t, reason, "allowlist entry %v needs a reason", key)
	}
}

// TestEveryTeardownWaitsForTheProjectsItDeletes asserts no probe's teardown
// deletes a project without waiting until the project and its backing
// namespace are gone, runs every teardown step of every probe against a
// test server, and reports each step that fails or sends nothing unless
// the allowlist holds it with a reason. A teardown's delete outlives its
// probe and races the next one; a setup's delete is followed by that
// setup's own waits before any observation. Setups are out of scope, as is
// a project removed indirectly by a cluster object delete. The count below
// is the number of project deletes the teardowns send today; an allowlist
// entry no step needed fails the test. The test server answers every
// DELETE 200: a step that waits only for another DELETE status, such as
// 202, passes, and so does a step that sends exactly three unconditional
// GETs per path. A run against Rancher (the fidelity suite) catches both.
func TestEveryTeardownWaitsForTheProjectsItDeletes(t *testing.T) {
	t.Parallel()
	const wantProjectDeletes = 4
	var mu sync.Mutex
	total := 0
	var used []stepKey
	t.Run("probes", func(t *testing.T) {
		for _, p := range All() {
			t.Run(p.Name, func(t *testing.T) {
				t.Parallel()
				deletes, problems, u := checkTeardown(t, p, teardownAllowlist)
				assert.Empty(t, problems)
				mu.Lock()
				defer mu.Unlock()
				total += deletes
				used = append(used, u...)
			})
		}
	})
	assertGuardTotals(t, wantProjectDeletes, total, teardownAllowlist, used)
}

// recordingT records the failures an assertion reports.
type recordingT struct{ failures []string }

func (r *recordingT) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// TestAssertGuardTotals asserts the closing checks of the guard report a
// wrong project delete count, an allowlist entry no step used and an entry
// with no reason, and report nothing for correct totals.
func TestAssertGuardTotals(t *testing.T) {
	t.Parallel()
	key := stepKey{"fam/probe", "step"}
	allow := map[stepKey]string{key: "owneruid is stored by a setup step; the step deletes a user"}
	for _, tc := range []struct {
		name  string
		want  int
		got   int
		allow map[stepKey]string
		used  []stepKey
		fails int
	}{
		{"correct totals", 4, 4, allow, []stepKey{key}, 0},
		{"a wrong count", 4, 3, allow, []stepKey{key}, 1},
		{"an unused entry", 4, 4, allow, nil, 1},
		{"an entry with no reason", 4, 4, map[stepKey]string{key: ""}, []stepKey{key}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec recordingT
			assertGuardTotals(&rec, tc.want, tc.got, tc.allow, tc.used)
			assert.Len(t, rec.failures, tc.fails)
		})
	}
}

// TestDeleteProjectFixtureWaitsForTheProjectAndItsBackingNamespace asserts
// the helper deletes the project, polls the project through a 503 until it
// answers 404, then polls its backing namespace, with {downstream}
// resolved.
func TestDeleteProjectFixtureWaitsForTheProjectAndItsBackingNamespace(t *testing.T) {
	t.Parallel()
	const (
		project = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/" + testDownstream + "/projects/p-x"
		backing = "/k8s/clusters/local/api/v1/namespaces/" + testDownstream + "-p-x"
	)
	var mu sync.Mutex
	var got []recordedRequest
	projectGets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, recordedRequest{r.Method, r.URL.Path, ""})
		status := http.StatusOK
		switch {
		case r.Method == http.MethodGet && r.URL.Path == project:
			projectGets++
			switch projectGets {
			case 2:
				status = http.StatusServiceUnavailable
			case 3:
				status = http.StatusNotFound
			}
		case r.Method == http.MethodGet:
			status = http.StatusNotFound
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tgt := adminTarget(srv)
	captured := map[string]string{"downstream": testDownstream}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, s := range deleteProjectFixture("p-x") {
		require.NoError(t, s.Do(ctx, tgt, captured), s.Name)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []recordedRequest{
		{http.MethodDelete, project, ""},
		{http.MethodGet, project, ""},
		{http.MethodGet, project, ""},
		{http.MethodGet, project, ""},
		{http.MethodGet, backing, ""},
	}, got)
}

// TestCheckTeardownRules asserts the rules of the check the real probes
// cannot show: a project path left unresolved is always a problem, a step
// that sends nothing, runs past its context or needs a capture the guard
// cannot seed is a problem unless allowlisted, a capture only a downstream
// probe has is not seeded for another probe, an allowlist entry no step
// needed is reported, every capture an observation declares is seeded, and
// a seeded capture in a project path names a project.
func TestCheckTeardownRules(t *testing.T) {
	t.Parallel()
	t.Run("unresolved project path is a problem even when allowlisted", func(t *testing.T) {
		t.Parallel()
		p := Probe{Name: "fam/unresolved", Teardown: []Step{deleteFixture("project", "/v3/projects/{pid}")}}
		allow := map[stepKey]string{{"fam/unresolved", "project"}: "test"}
		_, problems, used := checkTeardown(t, p, allow)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "unresolved capture")
		assert.Empty(t, used)
	})
	t.Run("a step that sends nothing is a problem unless allowlisted", func(t *testing.T) {
		t.Parallel()
		p := Probe{Name: "fam/silent", Teardown: []Step{{Name: "skip", Do: func(context.Context, Target, map[string]string) error { return nil }}}}
		_, problems, used := checkTeardown(t, p, nil)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "sent no request")
		assert.Empty(t, used)

		key := stepKey{"fam/silent", "skip"}
		_, problems, used = checkTeardown(t, p, map[stepKey]string{key: "test"})
		assert.Empty(t, problems)
		assert.Equal(t, []stepKey{key}, used)
	})
	t.Run("a probe without Downstream has no downstream capture", func(t *testing.T) {
		t.Parallel()
		p := Probe{Name: "fam/local", Teardown: deleteProjectFixture("p-x")}
		_, problems, _ := checkTeardown(t, p, nil)
		require.NotEmpty(t, problems)
		assert.Contains(t, problems[0], "unresolved capture")
	})
	t.Run("a step that outlives its context is a problem", func(t *testing.T) {
		t.Parallel()
		blocked := Step{Name: "block", Do: func(ctx context.Context, tgt Target, _ map[string]string) error {
			if _, err := adminDo(ctx, tgt, http.MethodGet, "/v3/users/u-x", ""); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		}}
		p := Probe{Name: "fam/blocked", Teardown: []Step{blocked}}
		_, problems, _ := checkTeardownWithin(t, p, nil, 200*time.Millisecond)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "past its context")
	})
	t.Run("an allowlist entry no step needed is reported", func(t *testing.T) {
		t.Parallel()
		p := Probe{Name: "fam/clean", Teardown: []Step{deleteFixture("secret", "/v3/users/u-x")}}
		key := stepKey{"fam/clean", "secret"}
		allow := map[stepKey]string{key: "test"}
		_, problems, used := checkTeardown(t, p, allow)
		assert.Empty(t, problems)
		assert.Equal(t, []stepKey{key}, unusedAllowEntries(allow, used))
	})
	t.Run("a captured value is seeded from the observations", func(t *testing.T) {
		t.Parallel()
		p := Probe{
			Name:         "fam/captured",
			Observations: []Observation{{Name: "create", Request: RequestSpec{Method: http.MethodPost, Path: "/v3/tokens", Identity: IdentityAdmin}, Captures: map[string]Capture{"tokid": {Path: "id"}}}},
			Teardown:     []Step{deleteFixture("token", "/v3/tokens/{tokid}")},
		}
		_, problems, _ := checkTeardown(t, p, nil)
		assert.Empty(t, problems)
	})
	t.Run("a seeded capture in a project path names a project", func(t *testing.T) {
		t.Parallel()
		const (
			project = "/v3/projects/c-m-x:p-x"
			backing = "/k8s/clusters/local/api/v1/namespaces/c-m-x-p-x"
		)
		observations := []Observation{{Name: "create", Request: RequestSpec{Method: http.MethodPost, Path: "/v3/projects", Identity: IdentityAdmin}, Captures: map[string]Capture{"pid": {Path: "id"}}}}
		gone := func(_ map[string]any, status int) bool { return status == http.StatusNotFound }

		bare := Probe{Name: "fam/named", Observations: observations, Teardown: []Step{deleteFixture("project", "/v3/projects/{pid}")}}
		deletes, problems, _ := checkTeardown(t, bare, nil)
		assert.Equal(t, 1, deletes)
		assert.Equal(t, []string{"fam/named: " + noProjectWait(project), "fam/named: " + noBackingWait(project, backing)}, problems)

		waiting := Probe{Name: "fam/named-waits", Observations: observations, Teardown: []Step{
			deleteFixture("project", "/v3/projects/{pid}"),
			waitFixture("project gone", http.MethodGet, "/v3/projects/{pid}", "", gone),
			waitFixture("backing namespace gone", http.MethodGet, backing, "", gone),
		}}
		deletes, problems, _ = checkTeardown(t, waiting, nil)
		assert.Equal(t, 1, deletes)
		assert.Empty(t, problems)
	})
}
