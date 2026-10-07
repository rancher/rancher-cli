package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// newKindSitesServer serves every collection the shared --cluster,
// --project, --namespace and --node values and the kind positionals read:
// the management clusters and their steve collection in two pages, one
// cluster's projects, users, node drivers, OIDC clients and one client's
// secrets, provisioning clusters in two namespaces and one cluster's
// machine pools, and a downstream cluster's namespaces, nodes, pods and
// deployments.
func newKindSitesServer(t *testing.T) *pathCountingServer {
	t.Helper()
	return newPathCountingServer(t, map[string]string{
		mgmtClustersPath:   clustersBody,
		steveProjectsPath:  projectsBody,
		mgmtUsersPath:      usersBody,
		nodeDriversPath:    items(plain("digitalocean"), plain("amazonec2")),
		oidcClientsPath:    items(plain("other"), plain("my-client")),
		oidcClientPath:     `{"metadata":{"name":"my-client"},"status":{"clientSecrets":{"secret-2":{},"secret-1":{}}}}`,
		provDefaultPath:    items(plain("prod"), plain("dev")),
		provTeamPath:       items(plain("team")),
		provProdPath:       `{"metadata":{"name":"prod"},"spec":{"rkeConfig":{"machinePools":[{"name":"pool-b"},{"name":"pool-a"}]}}}`,
		downNamespacesPath: items(plain("kube-system"), plain("default")),
		downTeamNamespaces: items(plain("team")),
		downNodesPath:      items(plain("node-2"), plain("node-1")),
		downPodsPath:       items(plain("web-2"), plain("web-1")),
		downDeploysPath:    items(plain("web")),
	}, steveClustersPages)
}

// TestKindSitesFullCallback asserts, through the real TAB callback, the
// candidates and the requests of every site the shared flag and kind
// sources serve: the --cluster value on a generated command and on
// `access projects` (both the steve collection, two pages), the --namespace
// and --node values with the cluster typed by id, by display name, not
// at all, under a typed --project and with an unusable value; the name
// positional of every Completable kind on the generated verbs (management
// kinds, a cluster-namespaced kind, downstream kinds in and out of a
// namespace); the bespoke first positionals (three inspect leaves, the
// five cluster-family verbs, the three oidc-client secret verbs, create
// password-change-request); a kind that does not complete, a second
// positional, and a new-name positional. Two later sites fed by the typed
// first positional (the oidc-client secret name, the cluster-family
// --pool) stay in this table beside the positional that feeds them, so a
// change to that positional's answer shows here in the same run; the
// command-sites table holds them in full. Each row asserts the exact
// candidates and the exact requests per path. NOT
// parallel: t.Setenv mutates env globals, setStoredConfig mutates
// storedConfig.
func TestKindSitesFullCallback(t *testing.T) {
	s := newKindSitesServer(t)
	clusters := []string{"c-aaa11", "c-bbb22", "c-ccc33", "c-ddd44", "local", "prod"}
	projects := []string{"Default", "System", "p-1", "p-2"}
	projectIDs := []string{"p-1", "p-2"}
	users := []string{"sameid", "u-alice", "u-badname", "u-bob", "u-nopids"}
	provClusters := []string{"dev", "prod"}
	namespaces := []string{"default", "kube-system"}
	tests := []struct {
		name  string
		words []string
		cur   string
		want  []string
		hits  map[string]int
		noNet bool
	}{
		// the shared --cluster value
		{name: "--cluster on a generated command", words: []string{"get", "pod", "--cluster"}, want: clusters, hits: map[string]int{steveClustersPath: 2}},
		{name: "--cluster= equals form", words: []string{"get", "pod"}, cur: "--cluster=", want: []string{"--cluster=c-aaa11", "--cluster=c-bbb22", "--cluster=c-ccc33", "--cluster=c-ddd44", "--cluster=local", "--cluster=prod"}, hits: map[string]int{steveClustersPath: 2}},
		{name: "--cluster on another command", words: []string{"get", "namespace", "--cluster"}, want: clusters, hits: map[string]int{steveClustersPath: 2}},
		{name: "--cluster with a typed partial offers every cluster", words: []string{"get", "pod", "--cluster"}, cur: "c-", want: clusters, hits: map[string]int{steveClustersPath: 2}},
		{name: "--cluster= with a typed partial offers every cluster", words: []string{"get", "pod"}, cur: "--cluster=c-", want: []string{"--cluster=c-aaa11", "--cluster=c-bbb22", "--cluster=c-ccc33", "--cluster=c-ddd44", "--cluster=local", "--cluster=prod"}, hits: map[string]int{steveClustersPath: 2}},
		{name: "--project with a cluster id", words: []string{"get", "pod", "--cluster", "c-aaa11", "--project"}, want: projects, hits: map[string]int{steveProjectsPath: 1}},
		{name: "--project= equals form on a command that takes it", words: []string{"usage", "--cluster", "c-aaa11"}, cur: "--project=", want: []string{"--project=Default", "--project=System", "--project=p-1", "--project=p-2"}, hits: map[string]int{steveProjectsPath: 1}},
		{name: "--project on a command that does not take it", words: []string{"get", "node", "--cluster", "c-aaa11", "--project"}, hits: map[string]int{}},
		{name: "--project= on a command that does not take it", words: []string{"inspect", "pod", "--cluster", "c-aaa11"}, cur: "--project=", hits: map[string]int{}},
		{name: "--project on project remove-namespace", words: []string{"project", "remove-namespace", "--cluster", "c-aaa11", "--project"}, hits: map[string]int{}},
		{name: "--cluster on access projects walks steve", words: []string{"access", "projects", "--cluster"}, want: clusters, hits: map[string]int{steveClustersPath: 2}},
		{name: "--cluster= on access projects", words: []string{"access", "projects"}, cur: "--cluster=", want: []string{"--cluster=c-aaa11", "--cluster=c-bbb22", "--cluster=c-ccc33", "--cluster=c-ddd44", "--cluster=local", "--cluster=prod"}, hits: map[string]int{steveClustersPath: 2}},
		// the shared --namespace and --node values
		{name: "--namespace with a cluster id", words: []string{"get", "pod", "--cluster", "c-aaa11", "--namespace"}, want: namespaces, hits: map[string]int{downNamespacesPath: 1}},
		{name: "--namespace with a cluster display name resolves it first", words: []string{"get", "pod", "--cluster", "prod", "--namespace"}, want: namespaces, hits: map[string]int{steveClustersPath: 2, downNamespacesPath: 1}},
		{name: "--namespace= equals form", words: []string{"get", "pod", "--cluster", "c-aaa11"}, cur: "--namespace=", want: []string{"--namespace=default", "--namespace=kube-system"}, hits: map[string]int{downNamespacesPath: 1}},
		{name: "--namespace without a cluster", words: []string{"get", "pod", "--namespace"}, hits: map[string]int{}},
		{name: "--namespace under a typed project", words: []string{"get", "pod", "--cluster", "c-aaa11", "--project", "p-1", "--namespace"}, hits: map[string]int{}},
		{name: "--namespace with a display name nothing carries", words: []string{"get", "pod", "--cluster", "nope", "--namespace"}, hits: map[string]int{steveClustersPath: 2, steveClusterNopePath: 1}},
		{name: "--namespace with a spaced display name resolves it first", words: []string{"get", "pod", "--cluster", "prod cluster", "--namespace"}, want: []string{"team"}, hits: map[string]int{steveClustersPath: 2, downTeamNamespaces: 1}},
		{name: "--node with a cluster id", words: []string{"usage", "--cluster", "c-aaa11", "--node"}, want: []string{"node-1", "node-2"}, hits: map[string]int{downNodesPath: 1}},
		{name: "--node without a cluster", words: []string{"usage", "--node"}, hits: map[string]int{}},
		// management kinds on the generated verbs
		{name: "get cluster", words: []string{"get", "cluster"}, want: clusters, hits: map[string]int{mgmtClustersPath: 1}},
		// The generated project verbs address a project by id only.
		{name: "get project with a cluster id", words: []string{"get", "project", "--cluster", "c-aaa11"}, want: projectIDs, hits: map[string]int{steveProjectsPath: 1}},
		{name: "get project with a cluster display name", words: []string{"get", "project", "--cluster", "prod"}, want: projectIDs, hits: map[string]int{steveClustersPath: 2, steveProjectsPath: 1}},
		{name: "delete project with a cluster id", words: []string{"delete", "project", "--cluster", "c-aaa11"}, want: projectIDs, hits: map[string]int{steveProjectsPath: 1}},
		{name: "get project without a cluster", words: []string{"get", "project"}, hits: map[string]int{}},
		{name: "get user", words: []string{"get", "user"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "delete user", words: []string{"delete", "user"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "patch user", words: []string{"patch", "user"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "get nodedriver", words: []string{"get", "nodedriver"}, want: []string{"amazonec2", "digitalocean"}, hits: map[string]int{nodeDriversPath: 1}},
		{name: "get oidcclient", words: []string{"get", "oidcclient"}, want: []string{"my-client", "other"}, hits: map[string]int{oidcClientsPath: 1}},
		{name: "get provisioning-cluster", words: []string{"get", "provisioning-cluster"}, want: provClusters, hits: map[string]int{provDefaultPath: 1}},
		{name: "get provisioning-cluster in a typed namespace", words: []string{"get", "provisioning-cluster", "--namespace", "team-a"}, want: []string{"team"}, hits: map[string]int{provTeamPath: 1}},
		{name: "delete provisioning-cluster", words: []string{"delete", "provisioning-cluster"}, want: provClusters, hits: map[string]int{provDefaultPath: 1}},
		// downstream kinds on the generated verbs
		{name: "get namespace with a cluster id", words: []string{"get", "namespace", "--cluster", "c-aaa11"}, want: namespaces, hits: map[string]int{downNamespacesPath: 1}},
		{name: "get namespace under a typed project", words: []string{"get", "namespace", "--cluster", "c-aaa11", "--project", "p-1"}, hits: map[string]int{}},
		{name: "get node with a cluster id", words: []string{"get", "node", "--cluster", "c-aaa11"}, want: []string{"node-1", "node-2"}, hits: map[string]int{downNodesPath: 1}},
		{name: "get pod in a namespace", words: []string{"get", "pod", "--cluster", "c-aaa11", "--namespace", "default"}, want: []string{"web-1", "web-2"}, hits: map[string]int{downPodsPath: 1}},
		{name: "get pod without a namespace", words: []string{"get", "pod", "--cluster", "c-aaa11"}, hits: map[string]int{}},
		{name: "get pod with an unusable namespace", words: []string{"get", "pod", "--cluster", "c-aaa11", "--namespace", "a/b"}, hits: map[string]int{}},
		{name: "get deployment in a namespace", words: []string{"get", "deployment", "--cluster", "c-aaa11", "--namespace", "default"}, want: []string{"web"}, hits: map[string]int{downDeploysPath: 1}},
		{name: "get secret does not complete", words: []string{"get", "secret", "--cluster", "c-aaa11", "--namespace", "default"}, hits: map[string]int{}},
		{name: "a second positional completes nothing", words: []string{"get", "pod", "--cluster", "c-aaa11", "--namespace", "default", "web-1"}, hits: map[string]int{}},
		// bespoke first positionals
		{name: "inspect cluster", words: []string{"inspect", "cluster"}, want: clusters, hits: map[string]int{steveClustersPath: 2}},
		{name: "inspect project with a cluster id", words: []string{"inspect", "project", "--cluster", "c-aaa11"}, want: projects, hits: map[string]int{steveProjectsPath: 1}},
		{name: "inspect user", words: []string{"inspect", "user"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "inspect pod does not complete", words: []string{"inspect", "pod", "--cluster", "c-aaa11", "--namespace", "default"}, hits: map[string]int{}},
		{name: "cluster scale", words: []string{"cluster", "scale"}, want: provClusters, hits: map[string]int{provDefaultPath: 1}},
		{name: "cluster scale in a typed namespace", words: []string{"cluster", "scale", "--namespace", "team-a"}, want: []string{"team"}, hits: map[string]int{provTeamPath: 1}},
		{name: "cluster set-autoscaling", words: []string{"cluster", "set-autoscaling"}, want: provClusters, hits: map[string]int{provDefaultPath: 1}},
		{name: "cluster clear-autoscaling", words: []string{"cluster", "clear-autoscaling"}, want: provClusters, hits: map[string]int{provDefaultPath: 1}},
		{name: "cluster pause-autoscaling", words: []string{"cluster", "pause-autoscaling"}, want: provClusters, hits: map[string]int{provDefaultPath: 1}},
		{name: "cluster resume-autoscaling", words: []string{"cluster", "resume-autoscaling"}, want: provClusters, hits: map[string]int{provDefaultPath: 1}},
		{name: "oidc-client add-secret", words: []string{"oidc-client", "add-secret"}, want: []string{"my-client", "other"}, hits: map[string]int{oidcClientsPath: 1}},
		{name: "oidc-client rotate-secret", words: []string{"oidc-client", "rotate-secret"}, want: []string{"my-client", "other"}, hits: map[string]int{oidcClientsPath: 1}},
		{name: "oidc-client remove-secret", words: []string{"oidc-client", "remove-secret"}, want: []string{"my-client", "other"}, hits: map[string]int{oidcClientsPath: 1}},
		// Served by the user kind source: ids, system accounts left out, each
		// once - the same answer get, patch, delete and inspect user give.
		{name: "create password-change-request", words: []string{"create", "password-change-request"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "create user names a new user", words: []string{"create", "user"}, hits: map[string]int{}},
		// later sites fed by the typed first positional, each with its own source
		{name: "oidc-client rotate-secret secret name", words: []string{"oidc-client", "rotate-secret", "my-client"}, want: []string{"secret-1", "secret-2"}, hits: map[string]int{oidcClientPath: 1}},
		{name: "cluster scale --pool", words: []string{"cluster", "scale", "prod", "--pool"}, want: []string{"pool-a", "pool-b"}, hits: map[string]int{provProdPath: 1}},
		// network off
		{name: "get cluster with the network off", words: []string{"get", "cluster"}, noNet: true, hits: map[string]int{}},
		{name: "--namespace with the network off", words: []string{"get", "pod", "--cluster", "c-aaa11", "--namespace"}, noNet: true, hits: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionFixture(t, s.srv.URL, !tt.noNet)
			s.reset()
			got := runCompletion(t, tt.words, tt.cur)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.hits, s.reset())
		})
	}
}

// TestKindSitesUnderAnExecCredentialOnlyToken asserts, through the real
// callback with the only credential an exec-credential cache entry for
// one cluster, that downstream listings through that cluster still
// answer while management listings answer nothing without a request.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates
// storedConfig.
func TestKindSitesUnderAnExecCredentialOnlyToken(t *testing.T) {
	s := newKindSitesServer(t)
	tests := []struct {
		name  string
		words []string
		want  []string
		hits  map[string]int
	}{
		{name: "get pod in a namespace", words: []string{"get", "pod", "--cluster", "c-aaa11", "--namespace", "default"}, want: []string{"web-1", "web-2"}, hits: map[string]int{downPodsPath: 1}},
		{name: "get namespace", words: []string{"get", "namespace", "--cluster", "c-aaa11"}, want: []string{"default", "kube-system"}, hits: map[string]int{downNamespacesPath: 1}},
		{name: "--namespace value", words: []string{"get", "pod", "--cluster", "c-aaa11", "--namespace"}, want: []string{"default", "kube-system"}, hits: map[string]int{downNamespacesPath: 1}},
		{name: "get cluster is refused", words: []string{"get", "cluster"}, hits: map[string]int{}},
		{name: "get project is refused", words: []string{"get", "project", "--cluster", "c-aaa11"}, hits: map[string]int{}},
		{name: "get user is refused", words: []string{"get", "user"}, hits: map[string]int{}},
		{name: "--cluster value is refused", words: []string{"get", "pod", "--cluster"}, hits: map[string]int{}},
		{name: "--project value is refused", words: []string{"get", "pod", "--cluster", "c-aaa11", "--project"}, hits: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idOnlyFixture(t, s.srv, "c-aaa11")
			s.reset()
			got := runCompletion(t, tt.words, "")
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.hits, s.reset())
		})
	}
}
