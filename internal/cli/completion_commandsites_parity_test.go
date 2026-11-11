package cli

import (
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
)

// newCommandSitesServer serves every collection the command-shaped
// completion sites read: the amazonec2 machine configs of fleet-default,
// the cloud credential Secrets of cattle-global-data, the management
// users, global roles and clusters, one cluster's two aggregated discovery roots, the
// cattle-system pods of the local and of a downstream cluster, one
// provisioning cluster in two namespaces, and one OIDC client.
func newCommandSitesServer(t *testing.T) *pathCountingServer {
	t.Helper()
	return newPathCountingServer(t, addAppCompletionBodies(map[string]string{
		machineConfigsPath: items(plain("cfg-b"), plain("cfg-a"), plain("bad name!")),
		cloudCredsPath:     completiontest.CloudCredentialsBody,
		mgmtUsersPath:      usersBody,
		globalRolesPath:    items(`{"metadata":{"name":"user-base"},"displayName":"User-Base"}`, `{"metadata":{"name":"admin"},"displayName":"Administrator"}`),
		mgmtClustersPath:   clustersBody,
		discoveryCorePath:  completiontest.DiscoveryCoreBody,
		discoveryGroupPath: completiontest.DiscoveryGroupsBody,
		localCattlePods:    items(plain("rancher-webhook-2"), plain("rancher-webhook-1")),
		downCattlePods:     items(plain("cattle-cluster-agent-1"), plain("cattle-cluster-agent-2")),
		provProdPath:       `{"metadata":{"name":"prod"},"spec":{"rkeConfig":{"machinePools":[{"name":"pool-b"},{"name":"pool-a"}]}}}`,
		provTeamProdPath:   `{"metadata":{"name":"prod"},"spec":{"rkeConfig":{"machinePools":[{"name":"team-pool"}]}}}`,
		oidcClientPath:     `{"metadata":{"name":"my-client"},"status":{"clientSecrets":{"secret-2":{},"secret-1":{}}}}`,
		localRepos:         reposBody,
		downRepos:          reposBody,
	}), steveClustersPages)
}

// machineConfigPrefixes returns the `<kind>/` tokens of every
// machine-config kind, sorted, behind spelling ("" for the space form).
func machineConfigPrefixes(spelling string) []string {
	var out []string
	for _, k := range kinds.All() {
		if k.GVR.Group == kinds.MachineConfigGroup {
			out = append(out, spelling+k.Name+"/")
		}
	}
	slices.Sort(out)
	return out
}

// TestCommandSitesFullCallback asserts, through the real TAB callback,
// the candidates and the requests of every command-shaped completion
// site: create cluster's --machine-config (the static `<kind>/` prefixes
// with the network on and off, the live `<kind>/name` tokens, an unknown
// kind) and --cloud-credential; the access questions' resource
// positional, --group and --subresource under the command's own
// resolution rules, with the cluster typed by id and by display name,
// absent, and beside --non-resource-url; --as on every access command
// that declares it; the cluster-family verbs' --pool from the typed
// cluster in the default and a typed namespace, and the two verbs that
// declare no --pool; logs --pod for a local-only target, a
// downstream-only target by cluster id and display name, a target
// without a static namespace and an unknown one; the oidc-client secret
// name of the typed client; charts --repo and app install's --repo, --chart and
// --version (the ClusterRepo and index reads, the charts the index allows, the
// versions that are not deprecated); create user's --global-role. Each row asserts the exact
// candidates and the exact requests per path. NOT parallel: t.Setenv
// mutates env globals, setStoredConfig mutates storedConfig.
func TestCommandSitesFullCallback(t *testing.T) {
	s := newCommandSitesServer(t)
	globalRoles := []string{"admin:Administrator", "user-base:User-Base"}
	users := []string{"alice", "bob", "sameid:sameid (Same Id)", "u-alice:alice (Alice Smith)", "u-badname:bad name!", "u-bob:bob"}
	credentials := []string{"aws-prod", "cc-abc12", "cc-collide", "cc-dup1", "cc-dup2", "cc-hostile", "cc-noannotation", "marker-only"}
	resources := []string{"deployments", "gauges", "ingresses", "nodes", "pods", "services"}
	groups := []string{"apps", "gateway.example.io", "metrics.example.io", "networking.k8s.io"}
	podSubresources := []string{"exec", "log", "status"}
	discovery := map[string]int{discoveryCorePath: 1, discoveryGroupPath: 1}
	webhookPods := []string{"rancher-webhook-1", "rancher-webhook-2"}
	agentPods := []string{"cattle-cluster-agent-1", "cattle-cluster-agent-2"}
	tests := []struct {
		name  string
		words []string
		cur   string
		want  []string
		hits  map[string]int
		noNet bool
	}{
		// create cluster --machine-config: the static half
		{name: "--machine-config bare offers the kind prefixes", words: []string{"create", "cluster", "node-1", "--machine-config"}, want: machineConfigPrefixes(""), hits: map[string]int{}},
		{name: "--machine-config bare offers the kind prefixes with the network off", words: []string{"create", "cluster", "node-1", "--machine-config"}, noNet: true, want: machineConfigPrefixes(""), hits: map[string]int{}},
		{name: "--machine-config= bare offers the kind prefixes", words: []string{"create", "cluster", "node-1"}, cur: "--machine-config=", want: machineConfigPrefixes("--machine-config="), hits: map[string]int{}},
		{name: "--machine-config partial kind offers every prefix", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "ama", want: machineConfigPrefixes(""), hits: map[string]int{}},
		// create cluster --machine-config: the live half
		{name: "--machine-config kind prefix lists the kind", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "amazonec2config/", want: []string{"amazonec2config/cfg-a", "amazonec2config/cfg-b"}, hits: map[string]int{machineConfigsPath: 1}},
		{name: "--machine-config kind prefix and a partial name", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "amazonec2config/cf", want: []string{"amazonec2config/cfg-a", "amazonec2config/cfg-b"}, hits: map[string]int{machineConfigsPath: 1}},
		{name: "--machine-config= kind prefix equals form", words: []string{"create", "cluster", "node-1"}, cur: "--machine-config=amazonec2config/", want: []string{"--machine-config=amazonec2config/cfg-a", "--machine-config=amazonec2config/cfg-b"}, hits: map[string]int{machineConfigsPath: 1}},
		{name: "--machine-config kind alias prefix answers the kind name", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "amazonec2configs/", want: []string{"amazonec2config/cfg-a", "amazonec2config/cfg-b"}, hits: map[string]int{machineConfigsPath: 1}},
		{name: "--machine-config kind prefix with the network off", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "amazonec2config/", noNet: true, hits: map[string]int{}},
		{name: "--machine-config unknown kind", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "nonesuch/x", hits: map[string]int{}},
		{name: "--machine-config kind outside the group", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "pod/x", hits: map[string]int{}},
		// create cluster --cloud-credential
		{name: "--cloud-credential", words: []string{"create", "cluster", "node-1", "--cloud-credential"}, want: credentials, hits: map[string]int{cloudCredsPath: 1}},
		{name: "--cloud-credential= equals form", words: []string{"create", "cluster", "node-1"}, cur: "--cloud-credential=", want: []string{"--cloud-credential=aws-prod", "--cloud-credential=cc-abc12", "--cloud-credential=cc-collide", "--cloud-credential=cc-dup1", "--cloud-credential=cc-dup2", "--cloud-credential=cc-hostile", "--cloud-credential=cc-noannotation", "--cloud-credential=marker-only"}, hits: map[string]int{cloudCredsPath: 1}},
		{name: "--cloud-credential with the network off", words: []string{"create", "cluster", "node-1", "--cloud-credential"}, noNet: true, hits: map[string]int{}},
		// the access questions' resource positional
		{name: "access can resource with a cluster id", words: []string{"access", "can", "get", "--cluster", "c-aaa11"}, want: resources, hits: discovery},
		{name: "access can resource with a cluster display name resolves it first", words: []string{"access", "can", "get", "--cluster", "prod"}, want: resources, hits: map[string]int{steveClustersPath: 2, discoveryCorePath: 1, discoveryGroupPath: 1}},
		{name: "access can resource with a partial", words: []string{"access", "can", "get", "--cluster", "c-aaa11"}, cur: "po", want: resources, hits: discovery},
		{name: "access can resource without a cluster", words: []string{"access", "can", "get"}, hits: map[string]int{}},
		{name: "access can resource beside --non-resource-url", words: []string{"access", "can", "get", "--cluster", "c-aaa11", "--non-resource-url", "/healthz"}, hits: map[string]int{}},
		{name: "access can resource with the network off", words: []string{"access", "can", "get", "--cluster", "c-aaa11"}, noNet: true, hits: map[string]int{}},
		{name: "access who-can resource", words: []string{"access", "who-can", "get", "--cluster", "c-aaa11"}, want: resources, hits: discovery},
		{name: "access explain resource", words: []string{"access", "explain", "get", "--cluster", "c-aaa11"}, want: resources, hits: discovery},
		// the access questions' --group
		{name: "access can --group", words: []string{"access", "can", "get", "pods", "--cluster", "c-aaa11", "--group"}, want: groups, hits: discovery},
		{name: "access can --group= equals form", words: []string{"access", "can", "get", "pods", "--cluster", "c-aaa11"}, cur: "--group=", want: []string{"--group=apps", "--group=gateway.example.io", "--group=metrics.example.io", "--group=networking.k8s.io"}, hits: discovery},
		{name: "access can --group with a display name resolves it first", words: []string{"access", "can", "get", "pods", "--cluster", "prod", "--group"}, want: groups, hits: map[string]int{steveClustersPath: 2, discoveryCorePath: 1, discoveryGroupPath: 1}},
		{name: "access can --group without a cluster", words: []string{"access", "can", "get", "pods", "--group"}, hits: map[string]int{}},
		// The question source declines --group and --subresource beside
		// --non-resource-url, a combination the command rejects; the
		// surface's own path answered them.
		{name: "access can --group beside --non-resource-url", words: []string{"access", "can", "get", "--cluster", "c-aaa11", "--non-resource-url", "/healthz", "--group"}, hits: map[string]int{}},
		{name: "access can --subresource beside --non-resource-url", words: []string{"access", "can", "get", "pods", "--cluster", "c-aaa11", "--non-resource-url", "/healthz", "--subresource"}, hits: map[string]int{}},
		{name: "access who-can --group", words: []string{"access", "who-can", "get", "pods", "--cluster", "c-aaa11", "--group"}, want: groups, hits: discovery},
		{name: "access explain --group", words: []string{"access", "explain", "get", "pods", "--cluster", "c-aaa11", "--group"}, want: groups, hits: discovery},
		// the access questions' --subresource, resolved by the command's rules
		{name: "access can --subresource of a plural", words: []string{"access", "can", "get", "pods", "--cluster", "c-aaa11", "--subresource"}, want: podSubresources, hits: discovery},
		{name: "access can --subresource of a singular", words: []string{"access", "can", "get", "pod", "--cluster", "c-aaa11", "--subresource"}, want: podSubresources, hits: discovery},
		{name: "access can --subresource of a short name", words: []string{"access", "can", "get", "po", "--cluster", "c-aaa11", "--subresource"}, want: podSubresources, hits: discovery},
		{name: "access can --subresource= equals form", words: []string{"access", "can", "get", "pods", "--cluster", "c-aaa11"}, cur: "--subresource=", want: []string{"--subresource=exec", "--subresource=log", "--subresource=status"}, hits: discovery},
		{name: "access can --subresource with the cluster before the verb", words: []string{"access", "can", "--cluster", "c-aaa11", "get", "pods", "--subresource"}, want: podSubresources, hits: discovery},
		{name: "access can --subresource with a boolean flag between", words: []string{"access", "can", "get", "--no-input", "pods", "--cluster", "c-aaa11", "--subresource"}, want: podSubresources, hits: discovery},
		{name: "access can --subresource of a cluster-scoped resource", words: []string{"access", "can", "get", "nodes", "--cluster", "c-aaa11", "--subresource"}, want: []string{"proxy", "status"}, hits: discovery},
		{name: "access can --subresource of a qualified name", words: []string{"access", "can", "get", "deployments.apps", "--cluster", "c-aaa11", "--subresource"}, want: []string{"scale", "status"}, hits: discovery},
		{name: "access can --subresource under --group", words: []string{"access", "can", "get", "deployments", "--group", "apps", "--cluster", "c-aaa11", "--subresource"}, want: []string{"scale", "status"}, hits: discovery},
		{name: "access can --subresource of an ambiguous bare name", words: []string{"access", "can", "get", "ingresses", "--cluster", "c-aaa11", "--subresource"}, hits: discovery},
		{name: "access can --subresource of a qualified ambiguous name", words: []string{"access", "can", "get", "ingresses.networking.k8s.io", "--cluster", "c-aaa11", "--subresource"}, want: []string{"status"}, hits: discovery},
		{name: "access can --subresource of a stale group's resource", words: []string{"access", "can", "get", "widgets", "--cluster", "c-aaa11", "--subresource"}, hits: discovery},
		{name: "access can --subresource of a partly stale group's resource", words: []string{"access", "can", "get", "gauges", "--cluster", "c-aaa11", "--subresource"}, want: []string{"status"}, hits: discovery},
		{name: "access can --subresource without a resource typed", words: []string{"access", "can", "get", "--cluster", "c-aaa11", "--subresource"}, hits: map[string]int{}},
		// The command reads <resource>/<name> and resolves the resource, as
		// the question source does; the surface's own path resolved the
		// whole token and found nothing.
		{name: "access can --subresource of a resource with a name", words: []string{"access", "can", "get", "pods/web", "--cluster", "c-aaa11", "--subresource"}, want: podSubresources, hits: discovery},
		{name: "access can --subresource with a display name resolves it first", words: []string{"access", "can", "get", "pods", "--cluster", "prod", "--subresource"}, want: podSubresources, hits: map[string]int{steveClustersPath: 2, discoveryCorePath: 1, discoveryGroupPath: 1}},
		{name: "access who-can --subresource", words: []string{"access", "who-can", "get", "pods", "--cluster", "c-aaa11", "--subresource"}, want: podSubresources, hits: discovery},
		{name: "access explain --subresource", words: []string{"access", "explain", "get", "pods", "--cluster", "c-aaa11", "--subresource"}, want: podSubresources, hits: discovery},
		// --as on the access commands that declare it
		{name: "access can --as", words: []string{"access", "can", "get", "pods", "--as"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "access can --as= equals form", words: []string{"access", "can", "get", "pods"}, cur: "--as=", want: []string{"--as=alice", "--as=bob", "--as=sameid:sameid (Same Id)", "--as=u-alice:alice (Alice Smith)", "--as=u-badname:bad name!", "--as=u-bob:bob"}, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "access can --as with an unusable cluster value still answers", words: []string{"access", "can", "get", "pods", "--cluster", "prod cluster", "--as"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "access can --as with the network off", words: []string{"access", "can", "get", "pods", "--as"}, noNet: true, hits: map[string]int{}},
		// Under --type group a bare --as value is a Kubernetes group, which
		// no collection lists; the surface's own path offered users there.
		{name: "access can --as under --type group", words: []string{"access", "can", "get", "pods", "--type", "group", "--as"}, hits: map[string]int{}},
		// A flag typed twice takes its last spelling, as the command reads it.
		{name: "access can --as under --type group then --type user", words: []string{"access", "can", "get", "pods", "--type", "group", "--type", "user", "--as"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "access can --as under --type user then --type group", words: []string{"access", "can", "get", "pods", "--type", "user", "--type", "group", "--as"}, hits: map[string]int{}},
		{name: "access list --as", words: []string{"access", "list", "--as"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "access explain --as", words: []string{"access", "explain", "get", "pods", "--as"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "access clusters --as", words: []string{"access", "clusters", "--as"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		{name: "access projects --as", words: []string{"access", "projects", "--as"}, want: users, hits: map[string]int{mgmtUsersPath: 1}},
		// the cluster-family verbs' --pool
		{name: "cluster scale --pool", words: []string{"cluster", "scale", "prod", "--pool"}, want: []string{"pool-a", "pool-b"}, hits: map[string]int{provProdPath: 1}},
		{name: "cluster scale --pool= equals form", words: []string{"cluster", "scale", "prod"}, cur: "--pool=", want: []string{"--pool=pool-a", "--pool=pool-b"}, hits: map[string]int{provProdPath: 1}},
		{name: "cluster scale --pool in a typed namespace", words: []string{"cluster", "scale", "prod", "--namespace", "team-a", "--pool"}, want: []string{"team-pool"}, hits: map[string]int{provTeamProdPath: 1}},
		{name: "cluster scale --pool in a namespace typed as -n", words: []string{"cluster", "scale", "prod", "-n", "team-a", "--pool"}, want: []string{"team-pool"}, hits: map[string]int{provTeamProdPath: 1}},
		{name: "cluster scale --pool with the cluster after the flag", words: []string{"cluster", "scale", "--nodes", "3", "prod", "--pool"}, want: []string{"pool-a", "pool-b"}, hits: map[string]int{provProdPath: 1}},
		{name: "cluster scale --pool without a cluster typed", words: []string{"cluster", "scale", "--pool"}, hits: map[string]int{}},
		{name: "cluster scale --pool with an unusable cluster name", words: []string{"cluster", "scale", "a/b", "--pool"}, hits: map[string]int{}},
		{name: "cluster scale --pool with the network off", words: []string{"cluster", "scale", "prod", "--pool"}, noNet: true, hits: map[string]int{}},
		{name: "cluster set-autoscaling --pool", words: []string{"cluster", "set-autoscaling", "prod", "--pool"}, want: []string{"pool-a", "pool-b"}, hits: map[string]int{provProdPath: 1}},
		{name: "cluster clear-autoscaling --pool", words: []string{"cluster", "clear-autoscaling", "prod", "--pool"}, want: []string{"pool-a", "pool-b"}, hits: map[string]int{provProdPath: 1}},
		// pause-autoscaling and resume-autoscaling act on the whole cluster
		// and declare no --pool: the token is no flag-value site.
		{name: "cluster pause-autoscaling has no --pool", words: []string{"cluster", "pause-autoscaling", "prod", "--pool"}, hits: map[string]int{}},
		{name: "cluster resume-autoscaling has no --pool", words: []string{"cluster", "resume-autoscaling", "prod", "--pool"}, hits: map[string]int{}},
		// logs --pod
		{name: "logs --pod of a local-only target", words: []string{"logs", "webhook", "--pod"}, want: webhookPods, hits: map[string]int{localCattlePods: 1}},
		{name: "logs --pod= equals form", words: []string{"logs", "webhook"}, cur: "--pod=", want: []string{"--pod=rancher-webhook-1", "--pod=rancher-webhook-2"}, hits: map[string]int{localCattlePods: 1}},
		{name: "logs --pod of a local-only target ignores a typed cluster", words: []string{"logs", "webhook", "--cluster", "prod cluster", "--pod"}, want: webhookPods, hits: map[string]int{localCattlePods: 1}},
		// Served by the pod source: an id-shaped cluster costs no resolution
		// request, and a display name nothing carries stops before the
		// listing (the surface's own path resolved every downstream cluster
		// and listed under the unresolved name).
		{name: "logs --pod of a downstream-only target with a cluster id", words: []string{"logs", "agent", "--cluster", "c-aaa11", "--pod"}, want: agentPods, hits: map[string]int{downCattlePods: 1}},
		{name: "logs --pod of a downstream-only target with a display name resolves it first", words: []string{"logs", "agent", "--cluster", "prod", "--pod"}, want: agentPods, hits: map[string]int{steveClustersPath: 2, downCattlePods: 1}},
		{name: "logs --pod of a downstream-only target with a display name nothing carries", words: []string{"logs", "agent", "--cluster", "nope", "--pod"}, hits: map[string]int{steveClustersPath: 2}},
		{name: "logs --pod of a downstream-only target without a cluster", words: []string{"logs", "agent", "--pod"}, hits: map[string]int{}},
		{name: "logs --pod of a target without a static namespace", words: []string{"logs", "fleet-agent", "--pod"}, hits: map[string]int{}},
		{name: "logs --pod of an unknown target", words: []string{"logs", "nope", "--pod"}, hits: map[string]int{}},
		{name: "logs --pod without a target", words: []string{"logs", "--pod"}, hits: map[string]int{}},
		{name: "logs --pod with the network off", words: []string{"logs", "webhook", "--pod"}, noNet: true, hits: map[string]int{}},
		// charts --repo: the enabled ClusterRepos of the typed cluster
		{name: "charts --repo offers the enabled repositories and not the disabled one", words: []string{"charts", "--cluster", "c-aaa11", "--repo"}, want: []string{"no-enabled-field", "rancher-charts"}, hits: map[string]int{downRepos: 1}},
		{name: "charts --repo= equals form", words: []string{"charts", "--cluster", "c-aaa11"}, cur: "--repo=", want: []string{"--repo=no-enabled-field", "--repo=rancher-charts"}, hits: map[string]int{downRepos: 1}},
		{name: "charts --repo of the local cluster", words: []string{"charts", "--cluster", "local", "--repo"}, want: []string{"no-enabled-field", "rancher-charts"}, hits: map[string]int{localRepos: 1}},
		{name: "charts --repo with a display name resolves it first", words: []string{"charts", "--cluster", "prod", "--repo"}, want: []string{"no-enabled-field", "rancher-charts"}, hits: map[string]int{steveClustersPath: 2, downRepos: 1}},
		{name: "charts --repo without --cluster offers no names", words: []string{"charts", "--repo"}, hits: map[string]int{}},
		{name: "charts --repo with the network off", words: []string{"charts", "--cluster", "c-aaa11", "--repo"}, noNet: true, hits: map[string]int{}},
		// app install --repo: the same enabled ClusterRepos charts offers
		{name: "app install --repo offers the enabled repositories and not the disabled one", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo"}, want: []string{"no-enabled-field", "rancher-charts"}, hits: map[string]int{downRepos: 1}},
		{name: "app install --repo= equals form", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11"}, cur: "--repo=", want: []string{"--repo=no-enabled-field", "--repo=rancher-charts"}, hits: map[string]int{downRepos: 1}},
		{name: "app install --repo of the local cluster", words: []string{"app", "install", "my-rel", "--cluster", "local", "--repo"}, want: []string{"no-enabled-field", "rancher-charts"}, hits: map[string]int{localRepos: 1}},
		{name: "app install --repo without --cluster offers no names", words: []string{"app", "install", "my-rel", "--repo"}, hits: map[string]int{}},
		{name: "app install --repo with the network off", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo"}, noNet: true, hits: map[string]int{}},
		// app install --chart: the charts the index allows, one read of the
		// ClusterRepo and one of its index per TAB
		{name: "app install --chart offers the charts the index allows and not a hidden, auto-install or all-deprecated chart", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-namespace-chart", "fixed-release-chart", "nginx"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart= equals form", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts"}, cur: "--chart=", want: []string{"--chart=fixed-namespace-chart", "--chart=fixed-release-chart", "--chart=nginx"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart does not offer a fixed namespace that differs from --namespace", words: []string{"app", "install", "--cluster", "c-aaa11", "--namespace", "default", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-release-chart", "nginx"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart offers a fixed namespace that matches --namespace", words: []string{"app", "install", "--cluster", "c-aaa11", "--namespace", "cattle-monitoring-system", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-namespace-chart", "fixed-release-chart", "nginx"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart does not offer a fixed release name that differs from the release", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-namespace-chart", "nginx"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart offers a fixed release name that matches the release", words: []string{"app", "install", "fleet", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-namespace-chart", "fixed-release-chart", "nginx"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart of the local cluster", words: []string{"app", "install", "--cluster", "local", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-namespace-chart", "fixed-release-chart", "nginx"}, hits: map[string]int{localRepoObject: 1, localRepoIndex: 1}},
		{name: "app install --chart with a display name resolves it first", words: []string{"app", "install", "--cluster", "prod", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-namespace-chart", "fixed-release-chart", "nginx"}, hits: map[string]int{steveClustersPath: 2, downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart without --repo offers no names", words: []string{"app", "install", "--cluster", "c-aaa11", "--chart"}, hits: map[string]int{}},
		{name: "app install --chart without --cluster offers no names", words: []string{"app", "install", "--repo", "rancher-charts", "--chart"}, hits: map[string]int{}},
		{name: "app install --chart of a disabled repository offers no names and reads no index", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "disabled-charts", "--chart"}, hits: map[string]int{downDisabledRepoObject: 1}},
		{name: "app install --chart of an unknown repository", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "nope", "--chart"}, hits: map[string]int{"/k8s/clusters/c-aaa11/apis/catalog.cattle.io/v1/clusterrepos/nope": 1}},
		{name: "app install --chart with the network off", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart"}, noNet: true, hits: map[string]int{}},
		// app install --version: the versions of --chart in the index, the
		// surface sorts the answer, so the order Rancher serves is asserted
		// in the source's own tests
		{name: "app install --version offers a prerelease and not a deprecated version", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart", "nginx", "--version"}, want: []string{"0.8.0", "1.1.0", "2.0.0-rc.1"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --version= equals form", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart", "nginx"}, cur: "--version=", want: []string{"--version=0.8.0", "--version=1.1.0", "--version=2.0.0-rc.1"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --version without --chart offers no versions", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--version"}, hits: map[string]int{}},
		{name: "app install --version without --repo offers no versions", words: []string{"app", "install", "--cluster", "c-aaa11", "--chart", "nginx", "--version"}, hits: map[string]int{}},
		{name: "app install --version of a chart the index lacks", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart", "nope", "--version"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --version of a chart with only deprecated versions", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart", "deprecated-chart", "--version"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --version of a disabled repository", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "disabled-charts", "--chart", "nginx", "--version"}, hits: map[string]int{downDisabledRepoObject: 1}},
		{name: "app install --version with the network off", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart", "nginx", "--version"}, noNet: true, hits: map[string]int{}},
		// the rest of app install and app uninstall
		{name: "app install --values is left to the shell", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--values"}, hits: map[string]int{}},
		{name: "app install --helm-timeout offers nothing", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--helm-timeout"}, hits: map[string]int{}},
		{name: "app install --timeout offers nothing", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--timeout"}, hits: map[string]int{}},
		{name: "app install --description offers nothing", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--description"}, hits: map[string]int{}},
		{name: "app install --system-default-registry offers nothing", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--system-default-registry"}, hits: map[string]int{}},
		{name: "app install release names a new release", words: []string{"app", "install"}, hits: map[string]int{}},
		{name: "app uninstall --helm-timeout offers nothing", words: []string{"app", "uninstall", "my-app", "--cluster", "c-aaa11", "--helm-timeout"}, hits: map[string]int{}},
		{name: "app uninstall --timeout offers nothing", words: []string{"app", "uninstall", "my-app", "--cluster", "c-aaa11", "--timeout"}, hits: map[string]int{}},
		// create user --global-role
		{name: "create user --global-role", words: []string{"create", "user", "alice", "--global-role"}, want: globalRoles, hits: map[string]int{globalRolesPath: 1}},
		{name: "create user --global-role= equals form", words: []string{"create", "user", "alice"}, cur: "--global-role=", want: []string{"--global-role=admin:Administrator", "--global-role=user-base:User-Base"}, hits: map[string]int{globalRolesPath: 1}},
		{name: "create user --global-role repeated", words: []string{"create", "user", "alice", "--global-role", "admin", "--global-role"}, want: globalRoles, hits: map[string]int{globalRolesPath: 1}},
		{name: "create user --global-role with the network off", words: []string{"create", "user", "alice", "--global-role"}, noNet: true, hits: map[string]int{}},
		{name: "create user --description is no global role site", words: []string{"create", "user", "alice", "--description"}, hits: map[string]int{}},
		{name: "create user username is no completion site", words: []string{"create", "user"}, hits: map[string]int{}},
		// the oidc-client secret name
		{name: "oidc-client rotate-secret secret name", words: []string{"oidc-client", "rotate-secret", "my-client"}, want: []string{"secret-1", "secret-2"}, hits: map[string]int{oidcClientPath: 1}},
		{name: "oidc-client remove-secret secret name", words: []string{"oidc-client", "remove-secret", "my-client"}, want: []string{"secret-1", "secret-2"}, hits: map[string]int{oidcClientPath: 1}},
		{name: "oidc-client rotate-secret secret name with a partial", words: []string{"oidc-client", "rotate-secret", "my-client"}, cur: "sec", want: []string{"secret-1", "secret-2"}, hits: map[string]int{oidcClientPath: 1}},
		{name: "oidc-client rotate-secret secret name of an unusable client", words: []string{"oidc-client", "rotate-secret", "a/b"}, hits: map[string]int{}},
		{name: "oidc-client rotate-secret secret name with the network off", words: []string{"oidc-client", "rotate-secret", "my-client"}, noNet: true, hits: map[string]int{}},
		// add-secret declares one positional and implements no source; the
		// surface's own path used to offer the client's existing secret names
		// for a second one, a site the verb does not have.
		{name: "oidc-client add-secret second positional", words: []string{"oidc-client", "add-secret", "my-client"}, hits: map[string]int{}},
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

// TestCommandSitesUnderAnExecCredentialOnlyToken asserts, through the
// real callback with the only credentials exec-credential cache entries
// for the local and one downstream cluster, that logs --pod still lists
// through the cluster its target names while every management-backed
// command site answers nothing without a request. NOT parallel: t.Setenv
// mutates env globals, setStoredConfig mutates storedConfig.
func TestCommandSitesUnderAnExecCredentialOnlyToken(t *testing.T) {
	s := newCommandSitesServer(t)
	tests := []struct {
		name  string
		words []string
		cur   string
		want  []string
		hits  map[string]int
	}{
		{name: "logs --pod of a local-only target", words: []string{"logs", "webhook", "--pod"}, want: []string{"rancher-webhook-1", "rancher-webhook-2"}, hits: map[string]int{localCattlePods: 1}},
		{name: "logs --pod of a downstream-only target with a cluster id", words: []string{"logs", "agent", "--cluster", "c-aaa11", "--pod"}, want: []string{"cattle-cluster-agent-1", "cattle-cluster-agent-2"}, hits: map[string]int{downCattlePods: 1}},
		{name: "charts --repo of a cluster id", words: []string{"charts", "--cluster", "c-aaa11", "--repo"}, want: []string{"no-enabled-field", "rancher-charts"}, hits: map[string]int{downRepos: 1}},
		{name: "app install --repo of a cluster id", words: []string{"app", "install", "my-rel", "--cluster", "c-aaa11", "--repo"}, want: []string{"no-enabled-field", "rancher-charts"}, hits: map[string]int{downRepos: 1}},
		{name: "app install --chart of a cluster id", words: []string{"app", "install", "--cluster", "c-aaa11", "--repo", "rancher-charts", "--chart"}, want: []string{"fixed-namespace-chart", "fixed-release-chart", "nginx"}, hits: map[string]int{downRepoObject: 1, downRepoIndex: 1}},
		{name: "app install --chart of a display name is refused", words: []string{"app", "install", "--cluster", "prod", "--repo", "rancher-charts", "--chart"}, hits: map[string]int{}},
		{name: "logs --pod of a downstream-only target with a display name is refused", words: []string{"logs", "agent", "--cluster", "prod", "--pod"}, hits: map[string]int{}},
		{name: "--machine-config kind prefix is refused", words: []string{"create", "cluster", "node-1", "--machine-config"}, cur: "amazonec2config/", hits: map[string]int{}},
		{name: "--cloud-credential is refused", words: []string{"create", "cluster", "node-1", "--cloud-credential"}, hits: map[string]int{}},
		{name: "access can resource is refused", words: []string{"access", "can", "get", "--cluster", "c-aaa11"}, hits: map[string]int{}},
		{name: "access can --group is refused", words: []string{"access", "can", "get", "pods", "--cluster", "c-aaa11", "--group"}, hits: map[string]int{}},
		{name: "access can --as is refused", words: []string{"access", "can", "get", "pods", "--as"}, hits: map[string]int{}},
		{name: "cluster scale --pool is refused", words: []string{"cluster", "scale", "prod", "--pool"}, hits: map[string]int{}},
		{name: "oidc-client rotate-secret secret name is refused", words: []string{"oidc-client", "rotate-secret", "my-client"}, hits: map[string]int{}},
		{name: "create user --global-role is refused", words: []string{"create", "user", "alice", "--global-role"}, hits: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idOnlyFixture(t, s.srv, "local", "c-aaa11")
			s.reset()
			got := runCompletion(t, tt.words, tt.cur)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.hits, s.reset())
		})
	}
}
