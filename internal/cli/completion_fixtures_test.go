package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The paths and bodies the live-completion parity tables share: every
// management collection a shared flag or a kind positional reads. A row
// added here is added for every table that lists the collection.
const (
	mgmtClustersPath   = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	mgmtProjectsPath   = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-aaa11/projects"
	mgmtUsersPath      = "/k8s/clusters/local/apis/management.cattle.io/v3/users"
	roleTemplatesPath  = "/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates"
	globalRolesPath    = "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"
	steveClustersPath  = "/v1/management.cattle.io.clusters"
	nodeDriversPath    = "/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers"
	oidcClientsPath    = "/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients"
	oidcClientPath     = "/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients/my-client"
	provDefaultPath    = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
	provTeamPath       = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/team-a/clusters"
	provProdPath       = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod"
	downNamespacesPath = "/k8s/clusters/c-aaa11/api/v1/namespaces"
	downTeamNamespaces = "/k8s/clusters/c-ddd44/api/v1/namespaces"
	downNodesPath      = "/k8s/clusters/c-aaa11/api/v1/nodes"
	downPodsPath       = "/k8s/clusters/c-aaa11/api/v1/namespaces/default/pods"
	downDeploysPath    = "/k8s/clusters/c-aaa11/apis/apps/v1/namespaces/default/deployments"
)

// usersBody carries one of every user shape the projections must handle:
// a plain user, a repeated row, a system account, a username outside the
// shell-safe charset, a user without principals, and one whose id equals
// its username.
const usersBody = `{"items":[
	{"metadata":{"name":"u-alice"},"username":"alice","principalIds":["local://u-alice"]},
	{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
	{"metadata":{"name":"u-bob"},"username":"bob","principalIds":["local://u-bob"]},
	{"metadata":{"name":"u-sys"},"username":"system-svc","principalIds":["system://u-sys"]},
	{"metadata":{"name":"u-badname"},"username":"bad name!","principalIds":["local://u-badname"]},
	{"metadata":{"name":"u-nopids"},"username":"nopids"},
	{"metadata":{"name":"sameid"},"username":"sameid","principalIds":["local://sameid"]}
]}`

// clustersBody carries a cluster with a unique display name, the local
// cluster, two clusters sharing a display name, and one whose display
// name carries a space (never a candidate; resolves when typed quoted).
const clustersBody = `{"items":[
	{"metadata":{"name":"c-aaa11"},"spec":{"displayName":"prod"}},
	{"metadata":{"name":"local"},"spec":{"displayName":"local"}},
	{"metadata":{"name":"c-bbb22"},"spec":{"displayName":"dup"}},
	{"metadata":{"name":"c-ccc33"},"spec":{"displayName":"dup"}},
	{"metadata":{"name":"c-ddd44"},"spec":{"displayName":"prod cluster"}}
]}`

// projectsBody carries two projects with unique display names.
const projectsBody = `{"items":[
	{"metadata":{"name":"p-1"},"spec":{"displayName":"Default"}},
	{"metadata":{"name":"p-2"},"spec":{"displayName":"System"}}
]}`

// steveFixturePage is one page of a steve collection as a fixture serves
// it, keyed by the continue token the previous page announced.
type steveFixturePage struct {
	Data     []map[string]any `json:"data"`
	Count    int              `json:"count"`
	Continue string           `json:"continue,omitempty"`
	Revision string           `json:"revision"`
}

func steveFixtureItem(id, display string) map[string]any {
	return map[string]any{"id": id, "metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display}}
}

// pathCountingServer serves fixed bodies by path and steve pages by
// continue token, and counts the requests per path, so a parity row can
// assert both the candidates and the requests behind them.
type pathCountingServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newPathCountingServer(t *testing.T, bodies map[string]string, pages map[string]steveFixturePage) *pathCountingServer {
	t.Helper()
	s := &pathCountingServer{hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		if r.URL.Path == steveClustersPath && pages != nil {
			page, ok := pages[r.URL.Query().Get("continue")]
			if !ok {
				http.Error(w, "no such page", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// reset clears the per-path counts and returns them as they were.
func (s *pathCountingServer) reset() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.hits
	s.hits = map[string]int{}
	return old
}

// items wraps object literals into a Kubernetes list body.
func items(rows ...string) string { return `{"items":[` + strings.Join(rows, ",") + `]}` }

// plain is an object literal with a name alone.
func plain(name string) string { return `{"metadata":{"name":"` + name + `"}}` }
