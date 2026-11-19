package complete

import (
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
)

const (
	upgradeProvListPath = "/v1/provisioning.cattle.io.clusters/fleet-default"
	upgradeProvBase     = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
	upgradeMgmtListPath = "/v1/management.cattle.io.clusters"
	upgradeSettingPath  = "/k8s/clusters/local/apis/management.cattle.io/v3/settings/imported-cluster-version-management"
	upgradeRKE2Path     = "/v1-rke2-release/release"
)

func provItem(name, version, rke, clusterName string) string {
	spec := fmt.Sprintf(`"spec":{"kubernetesVersion":%q`, version)
	if rke != "" {
		spec += `,"rkeConfig":` + rke
	}
	spec += "}"
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"fleet-default","generation":2},%s,"status":{"clusterName":%q,"observedGeneration":2,"conditions":[{"type":"Updated","status":"True"}]}}`, name, spec, clusterName)
}

func mgmtItem(id, driver, annotation string) string {
	ann := ""
	if annotation != "" {
		ann = fmt.Sprintf(`,"annotations":{"rancher.io/imported-cluster-version-management":%q}`, annotation)
	}
	return fmt.Sprintf(`{"id":%q,"metadata":{"name":%q%s},"spec":{"rke2Config":{"kubernetesVersion":"v1.31.4+rke2r1"}},"status":{"driver":%q,"version":{"gitVersion":"v1.31.4+rke2r1"},"conditions":[{"type":"Upgraded","status":"True"}]}}`, id, id, ann, driver)
}

const upgradeReleases = `{"data":[{"version":"v1.30.4+rke2r1","serverArgs":{}},{"version":"v1.31.4+rke2r1","serverArgs":{}},{"version":"v1.31.5+rke2r1","serverArgs":{}},{"version":"v1.32.0+rke2r1"},{"version":"v1.31.6+k3s1","serverArgs":{}},{"version":"v1.32.1+rke2r1","serverArgs":null}]}`

// TestUpgradeSourceClaims asserts the source owns the <cluster> positional
// with three requests and --k8s-version after a typed cluster with four,
// and declines every other site, --k8s-version with no cluster typed included.
func TestUpgradeSourceClaims(t *testing.T) {
	t.Parallel()
	src := UpgradeSource()
	reqs, ok := src.Claim(completion.Request{Index: 0})
	assert.True(t, ok)
	assert.Equal(t, completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceThree}, reqs)
	reqs, ok = src.Claim(completion.Request{Flag: "k8s-version", Positionals: []string{"prod"}})
	assert.True(t, ok)
	assert.Equal(t, completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceFour}, reqs)
	for _, req := range []completion.Request{
		{Flag: "k8s-version"}, {Flag: "namespace"}, {Flag: "timeout"}, {Index: 1}, {Flag: "wait"},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestUpgradeSourceClusters asserts the <cluster> site offers the clusters
// the command accepts: a provisioned cluster, an imported RKE2 cluster whose
// annotation is true, and one that leaves it to the Setting when the Setting
// is true; not the cluster Rancher runs in, one with version management off,
// a generic imported cluster or one with no management Cluster in the page.
// It sends three requests at most.
func TestUpgradeSourceClusters(t *testing.T) {
	t.Parallel()
	items := []string{
		provItem("prod", "v1.31.4+rke2r1", "{}", "c-m-prod1"),
		provItem("edge", "", "", "c-m-edge1"),
		provItem("edge-default", "", "", "c-m-edge2"),
		provItem("edge-off", "", "", "c-m-edge3"),
		provItem("edge-imported", "", "", "c-m-edge4"),
		provItem("edge-beyond-page", "", "", "c-m-edge5"),
		provItem("local", "", "", "local"),
	}
	list := func(parts ...string) string {
		out := "["
		for i, p := range parts {
			if i > 0 {
				out += ","
			}
			out += p
		}
		return out + "]"
	}
	s := completiontest.New(t, map[string]string{
		upgradeProvListPath: `{"data":` + list(items...) + `}`,
		upgradeMgmtListPath: `{"data":` + list(
			mgmtItem("c-m-edge1", "rke2", "true"), mgmtItem("c-m-edge2", "rke2", "system-default"),
			mgmtItem("c-m-edge3", "rke2", "false"), mgmtItem("c-m-edge4", "imported", "true"), mgmtItem("local", "k3s", "true")) + `}`,
		upgradeSettingPath: `{"value":"true"}`,
	})
	conn := s.Conn(3)
	got := UpgradeSource().Complete(t.Context(), conn, completion.Request{Index: 0})
	assert.ElementsMatch(t, []string{"prod", "edge", "edge-default"}, got.Names)
	assert.LessOrEqual(t, conn.Used(), 3)
	assert.False(t, conn.Exceeded())

	off := completiontest.New(t, map[string]string{
		upgradeProvListPath: `{"data":` + list(items[2]) + `}`,
		upgradeMgmtListPath: `{"data":` + list(mgmtItem("c-m-edge2", "rke2", "system-default")) + `}`,
		upgradeSettingPath:  `{"value":"false"}`,
	})
	assert.Empty(t, UpgradeSource().Complete(t.Context(), off.Conn(3), completion.Request{Index: 0}).Names, "the Setting is off")
}

// TestUpgradeSourceClustersOnAFailedRead asserts a failed list offers no
// candidates: no provisioning Cluster list, no management Cluster page.
func TestUpgradeSourceClustersOnAFailedRead(t *testing.T) {
	t.Parallel()
	noProv := completiontest.New(t, map[string]string{upgradeMgmtListPath: `{"data":[]}`})
	assert.Empty(t, UpgradeSource().Complete(t.Context(), noProv.Conn(3), completion.Request{Index: 0}).Names)
	noMgmt := completiontest.New(t, map[string]string{upgradeProvListPath: `{"data":[` + provItem("prod", "v1.31.4+rke2r1", "{}", "c-m-prod1") + `]}`})
	assert.Empty(t, UpgradeSource().Complete(t.Context(), noMgmt.Conn(3), completion.Request{Index: 0}).Names)
}

// TestUpgradeSourceVersions asserts --k8s-version offers the releases of the
// cluster's distribution that have serverArgs and are not lower than the
// current one, without the current one; an imported cluster costs four
// requests, a provisioned one two.
func TestUpgradeSourceVersions(t *testing.T) {
	t.Parallel()
	prod := completiontest.New(t, map[string]string{
		upgradeProvBase + "/prod": provItem("prod", "v1.31.4+rke2r1", "{}", "c-m-prod1"),
		upgradeRKE2Path:           upgradeReleases,
	})
	conn := prod.Conn(4)
	got := UpgradeSource().Complete(t.Context(), conn, completion.Request{Flag: "k8s-version", Positionals: []string{"prod"}})
	assert.ElementsMatch(t, []string{"v1.31.5+rke2r1"}, got.Names)
	assert.Equal(t, 2, conn.Used())

	edge := completiontest.New(t, map[string]string{
		upgradeProvBase + "/edge":          provItem("edge", "", "", "c-m-edge1"),
		upgradeMgmtListPath + "/c-m-edge1": mgmtItem("c-m-edge1", "rke2", "system-default"),
		upgradeSettingPath:                 `{"value":"true"}`,
		upgradeRKE2Path:                    upgradeReleases,
	})
	conn = edge.Conn(4)
	got = UpgradeSource().Complete(t.Context(), conn, completion.Request{Flag: "k8s-version", Positionals: []string{"edge"}})
	assert.ElementsMatch(t, []string{"v1.31.5+rke2r1"}, got.Names)
	assert.Equal(t, 4, conn.Used())
	assert.False(t, conn.Exceeded())
}

// importedMgmtItem is a management Cluster of an imported RKE2 cluster whose
// config has configVersion, that runs running, with the given conditions.
func importedMgmtItem(id, configVersion, running, conditions string) string {
	return fmt.Sprintf(`{"id":%q,"metadata":{"name":%q,"annotations":{"rancher.io/imported-cluster-version-management":"true"}},"spec":{"rke2Config":{"kubernetesVersion":%q}},"status":{"driver":"rke2","version":{"gitVersion":%q},"conditions":%s}}`, id, id, configVersion, running, conditions)
}

// TestUpgradeSourceVersionsNeverOffersTheCurrentVersion asserts --k8s-version
// offers what clusterops accepts and nothing after it: never the version the
// cluster runs, whether or not its upgrade is finished, and a stored version
// above the running one once.
func TestUpgradeSourceVersionsNeverOffersTheCurrentVersion(t *testing.T) {
	t.Parallel()
	unfinished := `{"metadata":{"name":"prod","namespace":"fleet-default","generation":3},"spec":{"kubernetesVersion":"v1.31.5+rke2r1","rkeConfig":{}},"status":{"clusterName":"c-m-prod1","observedGeneration":2,"conditions":[{"type":"Updated","status":"Unknown"}]}}`
	req := func(name string) completion.Request {
		return completion.Request{Flag: "k8s-version", Positionals: []string{name}}
	}

	t.Run("provisioned, upgrade not finished", func(t *testing.T) {
		t.Parallel()
		s := completiontest.New(t, map[string]string{upgradeProvBase + "/prod": unfinished, upgradeRKE2Path: upgradeReleases})
		assert.Empty(t, UpgradeSource().Complete(t.Context(), s.Conn(4), req("prod")).Names, "the stored version is the current one")
	})

	t.Run("provisioned, upgrade finished", func(t *testing.T) {
		t.Parallel()
		s := completiontest.New(t, map[string]string{upgradeProvBase + "/prod": provItem("prod", "v1.31.5+rke2r1", "{}", "c-m-prod1"), upgradeRKE2Path: upgradeReleases})
		assert.Empty(t, UpgradeSource().Complete(t.Context(), s.Conn(4), req("prod")).Names, "nothing newer, and the current version is not offered")
	})

	t.Run("imported, stored version above the running one, upgrade not finished", func(t *testing.T) {
		t.Parallel()
		s := completiontest.New(t, map[string]string{
			upgradeProvBase + "/edge":          provItem("edge", "", "", "c-m-edge1"),
			upgradeMgmtListPath + "/c-m-edge1": importedMgmtItem("c-m-edge1", "v1.31.5+rke2r1", "v1.31.4+rke2r1", `[{"type":"Upgraded","status":"Unknown"}]`),
			upgradeRKE2Path:                    upgradeReleases,
		})
		assert.Equal(t, []string{"v1.31.5+rke2r1"}, UpgradeSource().Complete(t.Context(), s.Conn(4), req("edge")).Names, "the stored version once")
	})

	t.Run("imported, no Upgraded condition", func(t *testing.T) {
		t.Parallel()
		s := completiontest.New(t, map[string]string{
			upgradeProvBase + "/edge":          provItem("edge", "", "", "c-m-edge1"),
			upgradeMgmtListPath + "/c-m-edge1": importedMgmtItem("c-m-edge1", "v1.31.5+rke2r1", "v1.31.5+rke2r1", `[]`),
			upgradeRKE2Path:                    upgradeReleases,
		})
		assert.Empty(t, UpgradeSource().Complete(t.Context(), s.Conn(4), req("edge")).Names, "the running version is not offered")
	})
}

// TestUpgradeSourceVersionsOnARefusedCluster asserts a cluster the command
// refuses, and any failed read offers no candidates.
func TestUpgradeSourceVersionsOnARefusedCluster(t *testing.T) {
	t.Parallel()
	off := completiontest.New(t, map[string]string{
		upgradeProvBase + "/edge":          provItem("edge", "", "", "c-m-edge1"),
		upgradeMgmtListPath + "/c-m-edge1": mgmtItem("c-m-edge1", "rke2", "false"),
		upgradeRKE2Path:                    upgradeReleases,
	})
	req := completion.Request{Flag: "k8s-version", Positionals: []string{"edge"}}
	assert.Empty(t, UpgradeSource().Complete(t.Context(), off.Conn(4), req).Names)
	assert.Equal(t, []string{upgradeProvBase + "/edge", upgradeMgmtListPath + "/c-m-edge1"}, off.Requested(), "no release list for a refused cluster")

	noReleases := completiontest.New(t, map[string]string{upgradeProvBase + "/prod": provItem("prod", "v1.31.4+rke2r1", "{}", "c-m-prod1")})
	assert.Empty(t, UpgradeSource().Complete(t.Context(), noReleases.Conn(4), completion.Request{Flag: "k8s-version", Positionals: []string{"prod"}}).Names)
	assert.Empty(t, UpgradeSource().Complete(t.Context(), completiontest.New(t, nil).Conn(4), completion.Request{Flag: "k8s-version", Positionals: []string{"prod"}}).Names)
}
