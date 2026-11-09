package fidelity

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	upcomingContainerName = "rancher-cli-e2e-fidelity-v2.16"
	upcomingSubnet        = "172.30.3.0/24"
	revisionLabel         = "org.opencontainers.image.revision"
	chartPinPattern       = `^https?://[^@\s]+@sha256:[0-9a-f]{64}$`
)

// curlInsecure matches a curl flag that turns off certificate checks: -k, -sk
// and the like, or --insecure.
var curlInsecure = regexp.MustCompile(`(^|\s)(-[a-zA-Z]*k[a-zA-Z]*|--insecure)(\s|$)`)

// labeledConfig is an image config whose Labels carry the given labels.
func labeledConfig(t *testing.T, env []string, labels map[string]string) []byte {
	t.Helper()
	config := map[string]any{"Env": env}
	if len(labels) > 0 {
		config["Labels"] = labels
	}
	return mustJSON(t, map[string]any{"config": config})
}

// publishLabeled serves repo:tag like publish does, with the revision label on
// the arm64 image the script reads and decoyRevision on the amd64 image it
// must not read. An empty revision leaves the label out of the arm64 image.
func (r *fakeRegistry) publishLabeled(t *testing.T, repo, tag, serverVersion, k3sVersion, revision, decoyRevision string) publishedImage {
	t.Helper()
	env := []string{"PATH=/usr/bin", "CATTLE_SERVER_VERSION=" + serverVersion, "CATTLE_K3S_VERSION=" + k3sVersion}
	decoy := []string{"CATTLE_SERVER_VERSION=v9.9-decoy", "CATTLE_K3S_VERSION=v9.9.9+k3s9"}
	labels := map[string]string{}
	if revision != "" {
		labels[revisionLabel] = revision
	}

	armConfig := r.addBlob(labeledConfig(t, env, labels))
	amdConfig := r.addBlob(labeledConfig(t, decoy, map[string]string{revisionLabel: decoyRevision}))
	armDigest := r.addManifest(repo, imageManifest(t, armConfig))
	amdDigest := r.addManifest(repo, imageManifest(t, amdConfig))
	entry := func(digest, osName, arch string) map[string]any {
		return map[string]any{
			"mediaType": manifestMediaType, "digest": digest, "size": 1,
			"platform": map[string]any{"os": osName, "architecture": arch},
		}
	}
	list := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     listMediaType,
		"manifests": []any{
			entry(amdDigest, "linux", "amd64"),
			entry(armDigest, "linux", "arm64"),
		},
	})
	listDigest := r.addManifest(repo, list)
	r.tags[repo+":"+tag] = listDigest
	return publishedImage{ListDigest: listDigest, ArmDigest: armDigest, ArmConfigDigest: armConfig}
}

// upcomingOptions changes what newUpcomingHelmScenario publishes; the zero
// value is a main-branch build of 2.16 whose images and chart are of one commit.
type upcomingOptions struct {
	// release makes release/v2.16 exist, so the tag is v2.16-head.
	release bool
	// revision is the label of both images and the commit of the chart's name.
	revision string
	// combinedRevision is the label of rancher/rancher when it differs.
	combinedRevision string
	// serverNoLabel and combinedNoLabel leave the label out of that image.
	serverNoLabel   bool
	combinedNoLabel bool
	// noCombined does not publish rancher/rancher.
	noCombined bool
	// noK3s does not publish the k3s image CATTLE_K3S_VERSION names.
	noK3s bool
	// k3sVersion is the CATTLE_K3S_VERSION of both images.
	k3sVersion string
	// appVersion is the appVersion of the chart archive.
	appVersion string
	// assets is how the chart's values name the assets image.
	assets string
	// chartStatus is the status the chart's URL responds with; 0 responds with
	// the archive.
	chartStatus int
}

// upcomingFixture is what newUpcomingHelmScenario published.
type upcomingFixture struct {
	tag           string
	revision      string
	commit        string
	serverVersion string
	server        publishedImage
	combined      publishedImage
	k3s           publishedImage
	// chartURL is the default chart URL of the revision, pin the value the
	// script computes for it (URL and sha256), and overrideURL another URL that
	// responds with the same archive.
	chartURL    string
	pin         string
	overrideURL string
	// nodeImage is the node container's k3s image.
	nodeImage string
}

// newUpcomingHelmScenario is an upcoming 2.16 helm row before its script copy
// is written: the helm docker stub, the images of rancher/rancher-server and
// rancher/rancher with their revision labels, the k3s image, and a local HTTP
// server for the chart archive and the cert-manager manifest. The case adjusts
// s.env or the stub's files, then calls slotScript and run.
func newUpcomingHelmScenario(t *testing.T, o upcomingOptions) (*scenario, *upcomingFixture) {
	t.Helper()
	s := newScenario(t)
	f := s.installHelmDocker()
	if o.release {
		s.addBranch()
	}
	u := &upcomingFixture{
		tag:      "head",
		revision: cmp.Or(o.revision, strings.Repeat("a", 40)),
		commit:   strings.Repeat("b", 40),
	}
	if o.release {
		u.tag = "v2.16-head"
	}
	u.serverVersion = "v2.16-" + u.commit + "-head"
	k3sVersion := cmp.Or(o.k3sVersion, "v1.37.1+k3s1")
	decoy := strings.Repeat("d", 40)

	if !o.noK3s {
		u.k3s = s.reg.publish(t, "rancher/k3s", "v1.37.1-k3s1", "", "")
	}
	u.nodeImage = "rancher/k3s:v1.37.1-k3s1@" + u.k3s.ListDigest
	serverRevision := u.revision
	if o.serverNoLabel {
		serverRevision = ""
	}
	u.server = s.reg.publishLabeled(t, "rancher/rancher-server", u.tag, u.serverVersion, k3sVersion, serverRevision, decoy)
	if !o.noCombined {
		combinedRevision := cmp.Or(o.combinedRevision, u.revision)
		if o.combinedNoLabel {
			combinedRevision = ""
		}
		u.combined = s.reg.publishLabeled(t, "rancher/rancher", u.tag, u.serverVersion, k3sVersion, combinedRevision, decoy)
	}
	if o.assets != "" {
		s.reg.publish(t, "rancher/rancher-assets", "v2.16-20261027T2136Z-dev", "", "")
	}

	f.archive = chartArchive(t, cmp.Or(o.appVersion, u.serverVersion), o.assets)
	f.manifest = []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: cert-manager\n")
	f.chartSHA = sha256Hex(f.archive)
	f.manifestSHA = sha256Hex(f.manifest)
	chartPath := "/server-charts/release-2.16/rancher-2.16-" + u.revision + "-head.tgz"
	serveArchive := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.archive) }
	mux := http.NewServeMux()
	switch o.chartStatus {
	case 0:
		mux.HandleFunc(chartPath, serveArchive)
	case http.StatusNotFound:
	default:
		mux.HandleFunc(chartPath, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(o.chartStatus) })
	}
	mux.HandleFunc("/override/chart.tgz", serveArchive)
	mux.HandleFunc("/cm/cert-manager.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.manifest) })
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	u.chartURL = f.server.URL + chartPath
	u.overrideURL = f.server.URL + "/override/chart.tgz"
	u.pin = u.chartURL + "@sha256:" + f.chartSHA
	f.chartURL = u.chartURL
	f.manifestURL = f.server.URL + "/cm/cert-manager.yaml"
	f.oracleImage = "rancher/rancher-server:" + u.tag + "@" + u.server.ListDigest
	f.setPod(f.oracleImage, "docker.io/rancher/rancher-server@"+u.server.ListDigest)

	s.pinRewrites = map[string]string{
		"UPCOMING_CHART_BASE":   f.server.URL + "/server-charts",
		"CERT_MANAGER_MANIFEST": f.manifestURL + "@sha256:" + f.manifestSHA,
	}
	s.slot = &slotStub{}
	s.env = map[string]string{
		"E2E_FIDELITY_MINOR":         "2.16",
		"E2E_FIDELITY_UPCOMING_BOOT": "helm",
		"STUB_LOCK_DIR":              filepath.Join(s.stateHome, upcomingContainerName, "lock"),
	}
	return s, u
}

func (s *scenario) upcomingRoot() string {
	return filepath.Join(s.stateHome, upcomingContainerName)
}

// downloads counts the curl log lines that fetched url.
func (s *scenario) downloads(url string) int {
	s.t.Helper()
	n := 0
	for _, line := range s.curlLog() {
		if strings.HasSuffix(line, " "+url) {
			n++
		}
	}
	return n
}

func (s *scenario) callCount(log []string, prefix string) int {
	n := 0
	for _, line := range log {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestUpcomingHelmRefusesBeforeDocker asserts that every refusal of the
// upcoming boot variables exits 1 with its message before any docker call, any
// registry request, any download and any state, that a format message wins
// over the repository message, and that an empty value counts as unset.
func TestUpcomingHelmRefusesBeforeDocker(t *testing.T) {
	t.Parallel()
	hex64 := strings.TrimPrefix(sampleDigest, "sha256:")
	validChart := "https://charts.example.com/rancher-2.16.tgz@" + sampleDigest
	serverImage := "rancher/rancher-server:v2.16-abc-head@" + sampleDigest
	combinedImage := "rancher/rancher:v2.16-abc-head@" + sampleDigest
	withoutMinor := " is set without E2E_FIDELITY_MINOR; the community and Prime profiles run only their pinned images"
	type tc struct {
		name string
		env  map[string]string
		want string
	}
	var cases []tc
	for _, variable := range []struct{ name, value string }{
		{"E2E_FIDELITY_UPCOMING_BOOT", "helm"},
		{"E2E_FIDELITY_UPCOMING_CHART", validChart},
	} {
		cases = append(cases,
			tc{variable.name + " alone", map[string]string{variable.name: variable.value}, variable.name + withoutMinor},
			tc{variable.name + " with prime", map[string]string{variable.name: variable.value, "E2E_FIDELITY_PRIME": "1"}, variable.name + withoutMinor},
			tc{variable.name + " with the helm-check", map[string]string{variable.name: variable.value, "E2E_FIDELITY_HELM_CHECK": "1"}, variable.name + withoutMinor},
		)
	}
	minor := func(extra map[string]string) map[string]string {
		env := map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
		maps.Copy(env, extra)
		return env
	}
	cases = append(cases,
		tc{"a boot other than container or helm", minor(map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "docker"}),
			"error: E2E_FIDELITY_UPCOMING_BOOT=docker is not container or helm; set one of them or unset it"},
		tc{"a boot with a capital letter", minor(map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "Helm"}),
			"error: E2E_FIDELITY_UPCOMING_BOOT=Helm is not container or helm"},
		tc{"a chart without a boot", minor(map[string]string{"E2E_FIDELITY_UPCOMING_CHART": validChart}),
			"error: E2E_FIDELITY_UPCOMING_CHART is set without E2E_FIDELITY_UPCOMING_BOOT=helm; the container boot installs no chart"},
		tc{"a chart with the container boot", minor(map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "container", "E2E_FIDELITY_UPCOMING_CHART": validChart}),
			"error: E2E_FIDELITY_UPCOMING_CHART is set without E2E_FIDELITY_UPCOMING_BOOT=helm"},
		tc{"a rancher-server image with the container boot", minor(map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "container", "E2E_FIDELITY_UPCOMING_IMAGE": serverImage}),
			"error: E2E_FIDELITY_UPCOMING_IMAGE=" + serverImage + " names a rancher-server image, which the container boot cannot start; set E2E_FIDELITY_UPCOMING_BOOT=helm"},
		tc{"a rancher-server image without a boot", minor(map[string]string{"E2E_FIDELITY_UPCOMING_IMAGE": serverImage}),
			"names a rancher-server image, which the container boot cannot start"},
		tc{"a rancher/rancher image with helm", minor(map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "helm", "E2E_FIDELITY_UPCOMING_IMAGE": combinedImage}),
			"error: E2E_FIDELITY_UPCOMING_IMAGE=" + combinedImage + " is not a rancher-server image; E2E_FIDELITY_UPCOMING_BOOT=helm boots only rancher-server"},
		tc{"an image format message wins over the repository message", minor(map[string]string{"E2E_FIDELITY_UPCOMING_IMAGE": "rancher/rancher-server@sha256:" + hex64}),
			"E2E_FIDELITY_UPCOMING_IMAGE=rancher/rancher-server@sha256:" + hex64 + " has no tag; write <namespace>/<repo>:<tag>@sha256:<64 hex>"},
	)
	for _, bad := range []struct{ name, value string }{
		{"a chart without a digest", "https://charts.example.com/rancher-2.16.tgz"},
		{"a chart with a short digest", "https://charts.example.com/rancher-2.16.tgz@sha256:" + hex64[:63]},
		{"a chart with an uppercase digest", "https://charts.example.com/rancher-2.16.tgz@sha256:" + strings.ToUpper(hex64)},
		{"a chart with a scheme other than http and https", "ftp://charts.example.com/rancher-2.16.tgz@" + sampleDigest},
		{"a chart with no scheme", "charts.example.com/rancher-2.16.tgz@" + sampleDigest},
		{"a chart URL with a space", "https://charts.example.com/a b.tgz@" + sampleDigest},
		{"a chart with a trailing space", validChart + " "},
	} {
		cases = append(cases, tc{bad.name, minor(map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "helm", "E2E_FIDELITY_UPCOMING_CHART": bad.value}),
			"error: E2E_FIDELITY_UPCOMING_CHART=" + bad.value + " is not <URL>@sha256:<64 hex>"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newScenario(t)
			s.env = c.env
			res := s.run()
			s.requireRefused(res, c.want)
			assert.Empty(t, s.reg.recorded())
			assert.Empty(t, s.curlLog())
		})
	}

	t.Run("an empty value counts as unset", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		s.env = map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "", "E2E_FIDELITY_UPCOMING_CHART": ""}
		res := s.run()
		assert.NotContains(t, res.stderr, "E2E_FIDELITY_UPCOMING")
		assert.Equal(t, 97, res.code, "the run goes on to the community profile and stops at the docker stub")
	})
	t.Run("an empty boot with a minor is the container boot", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		image := s.publishDefaults()
		s.env = minor(map[string]string{"E2E_FIDELITY_UPCOMING_BOOT": "", "E2E_FIDELITY_PRINT_IMAGES": "1"})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		assert.Equal(t, "rancher/rancher:head@"+image.ListDigest, decodePrinted(t, res.stdout)["oracleImage"])
	})
	t.Run("a rancher-server image with helm and a chart is accepted", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		s.slotScript()
		s.env["E2E_FIDELITY_PRINT_IMAGES"] = "1"
		s.env["E2E_FIDELITY_UPCOMING_IMAGE"] = "rancher/rancher-server:" + u.tag + "@" + u.server.ListDigest
		s.env["E2E_FIDELITY_UPCOMING_CHART"] = u.pin
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
	})
}

// TestUpcomingBootDefaultsToContainer asserts that a run that does not set
// E2E_FIDELITY_UPCOMING_BOOT, and one that sets container, prints, requests
// and boots what it did before the helm boot existed: the same keys, the
// rancher/rancher image, no chart request and no read of the server image.
func TestUpcomingBootDefaultsToContainer(t *testing.T) {
	t.Parallel()
	today := []string{"minor", "source", "oracleImage", "serverVersion", "k3sImage", "k3sSource", "k3sVersion", "goldenOracleImage", "goldenK3sImage"}
	for _, boot := range []string{"", "container"} {
		for _, drift := range []bool{false, true} {
			t.Run(fmt.Sprintf("print with boot %q and drift %t", boot, drift), func(t *testing.T) {
				t.Parallel()
				s := newScenario(t)
				image := s.publishDefaults()
				s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "1"}
				keys := slices.Clone(today)
				if boot != "" {
					s.env["E2E_FIDELITY_UPCOMING_BOOT"] = boot
				}
				if drift {
					s.env["E2E_FIDELITY_DRIFT"] = "1"
					keys = append(keys, "driftReport", "driftKnown")
				}
				res := s.run()
				require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
				got := decodePrinted(t, res.stdout)
				assert.ElementsMatch(t, keys, slices.Collect(maps.Keys(got)))
				assert.Equal(t, "rancher/rancher:head@"+image.ListDigest, got["oracleImage"])
				assert.Equal(t, []string{"info", "version"}, res.commands())
				assert.Empty(t, s.curlLog(), "the container boot makes no chart request")
				for _, req := range s.reg.recorded() {
					assert.NotContains(t, req.Path, "rancher-server", "the container boot reads no server image")
				}
				s.requireNoStateCreated()
			})
		}
	}

	t.Run("a healthy kept pair reaches its drift run and downloads nothing", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		image := s.publishDefaults()
		oracle := "rancher/rancher:head@" + image.ListDigest
		s.present = []string{oracle, s.k3sRef}
		port := s.slotScript()
		s.installHelmDocker()
		stub := ownPair(upcomingContainerName, port, upcomingSubnet)
		s.slot = &stub
		s.writeState(s.upcomingRoot(), map[string]any{
			"containerId": "oracle-id", "adminToken": "token", "oracleImage": oracle,
			"k3sContainerId": "k3s-id", "k3sImage": s.k3sRef, "downstream": "c-m-abc",
		})
		s.keepIDs(upcomingContainerName)
		s.cannedHealthy("c-m-abc")
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_KEEP": "1", "E2E_FIDELITY_DRIFT": "1"}
		res := s.run()

		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> drift run of 2.16 against the community goldens\n")
		env, _ := s.goEnv()
		assert.Equal(t, "upcoming", env["E2E_FIDELITY_DRIFT_PROFILE"])
		for _, line := range s.curlLog() {
			assert.NotContains(t, line, "real ", "a kept container pair downloads nothing")
		}
		for _, prefix := range []string{"rm ", "run ", "pull "} {
			assert.Falsef(t, hasCall(res.log, prefix), "a healthy kept pair makes no %q call: %v", prefix, res.log)
		}
	})
}

// TestUpcomingHelmPrint asserts the print mode of the helm boot: the keys and
// values for a main-branch and a release-branch build, the chart URL built
// from the image's revision label, the overrides, and every stop. A print makes
// no docker call past the platform read, downloads the chart once and no
// cert-manager manifest, and creates nothing under the state home.
func TestUpcomingHelmPrint(t *testing.T) {
	t.Parallel()
	helmKeys := []string{
		"minor", "source", "oracleImage", "serverVersion", "k3sImage", "k3sSource", "k3sVersion", "goldenOracleImage", "goldenK3sImage",
		"boot", "chart", "chartSha256", "localK3sImage", "assetsImage", "certManager", "revision", "containerImage", "containerRevision",
	}
	printEnv := func(s *scenario, extra map[string]string) {
		s.env["E2E_FIDELITY_PRINT_IMAGES"] = "1"
		maps.Copy(s.env, extra)
	}
	requirePrintStop := func(t *testing.T, s *scenario, res runResult, message string) {
		t.Helper()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Empty(t, res.stdout, "a stopped print writes nothing to stdout")
		assert.Contains(t, res.stderr, message)
		assert.Equal(t, []string{"info", "version"}, res.commands())
		s.requireNoStateCreated()
	}

	for _, row := range []struct {
		name    string
		release bool
		source  string
		tag     string
	}{
		{"main branch", false, "main", "head"},
		{"release branch", true, "release-branch", "v2.16-head"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s, u := newUpcomingHelmScenario(t, upcomingOptions{release: row.release, assets: "block"})
			s.slotScript()
			printEnv(s, nil)
			res := s.run()
			require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
			got := decodePrinted(t, res.stdout)
			assert.ElementsMatch(t, helmKeys, slices.Collect(maps.Keys(got)))
			assert.Equal(t, "2.16", got["minor"])
			assert.Equal(t, row.source, got["source"])
			assert.Equal(t, "rancher/rancher-server:"+row.tag+"@"+u.server.ListDigest, got["oracleImage"])
			assert.Equal(t, u.serverVersion, got["serverVersion"])
			assert.Equal(t, u.nodeImage, got["k3sImage"])
			assert.Equal(t, "image", got["k3sSource"])
			assert.Equal(t, "v1.37.1+k3s1", got["k3sVersion"])
			assert.Equal(t, scriptLiteral(t, "ORACLE_IMAGE"), got["goldenOracleImage"])
			assert.Equal(t, scriptLiteral(t, "K3S_IMAGE"), got["goldenK3sImage"])
			assert.Equal(t, "helm", got["boot"])
			assert.Equal(t, u.pin, got["chart"])
			assert.Contains(t, got["chart"], "/release-2.16/rancher-2.16-"+u.revision+"-head.tgz@sha256:", "the name holds the revision label, not the commit text of CATTLE_SERVER_VERSION")
			assert.NotContains(t, got["chart"], u.commit)
			assert.Equal(t, s.helm.chartSHA, got["chartSha256"])
			assert.Equal(t, u.nodeImage, got["localK3sImage"])
			assert.Equal(t, "v2.16-20261027T2136Z-dev@"+s.reg.tags["rancher/rancher-assets:v2.16-20261027T2136Z-dev"], got["assetsImage"])
			assert.Equal(t, s.pinRewrites["CERT_MANAGER_MANIFEST"], got["certManager"])
			assert.Equal(t, u.revision, got["revision"])
			assert.Equal(t, "rancher/rancher:"+row.tag+"@"+u.combined.ListDigest, got["containerImage"])
			assert.Equal(t, u.revision, got["containerRevision"])
			assert.Regexp(t, chartPinPattern, got["chart"])

			assert.Equal(t, []string{"info", "version"}, res.commands())
			assert.Equal(t, []string{"real " + u.chartURL}, s.curlLog(), "one chart download and no cert-manager manifest")
			s.requireNoStateCreated()
		})
	}

	t.Run("a chart override is verified by its sha256", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		s.slotScript()
		pin := u.overrideURL + "@sha256:" + s.helm.chartSHA
		printEnv(s, map[string]string{"E2E_FIDELITY_UPCOMING_CHART": pin})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, pin, got["chart"])
		assert.Equal(t, s.helm.chartSHA, got["chartSha256"])
		assert.Equal(t, []string{"real " + u.overrideURL}, s.curlLog(), "the default chart is not downloaded")
		assert.Contains(t, got, "containerImage", "a chart override does not turn off the comparison of the head images")
	})
	t.Run("an image override reads no tag and compares nothing", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		image := s.reg.publishLabeled(t, "rancher/rancher-server", "v2.16-"+u.commit+"-head", u.serverVersion, "v1.37.1+k3s1", u.revision, strings.Repeat("d", 40))
		s.slotScript()
		override := "rancher/rancher-server:v2.16-" + u.commit + "-head@" + image.ListDigest
		printEnv(s, map[string]string{"E2E_FIDELITY_UPCOMING_IMAGE": override})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, "override", got["source"])
		assert.Equal(t, override, got["oracleImage"])
		assert.Equal(t, u.revision, got["revision"])
		assert.NotContains(t, got, "containerImage")
		assert.NotContains(t, got, "containerRevision")
		for _, req := range s.reg.recorded() {
			assert.NotContains(t, req.Path, "/manifests/head", "an override reads no tag")
			assert.NotContains(t, req.Path, "/v2/rancher/rancher/", "an override reads no combined image")
		}
	})
	t.Run("a downstream override leaves the node container's k3s image", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		s.slotScript()
		override := "rancher/k3s:v1.34.4-k3s1@" + sampleDigest
		printEnv(s, map[string]string{"E2E_FIDELITY_K3S_IMAGE": override})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, override, got["k3sImage"])
		assert.Equal(t, u.nodeImage, got["localK3sImage"])
	})
	t.Run("a drift run adds the drift keys to the helm keys", func(t *testing.T) {
		t.Parallel()
		s, _ := newUpcomingHelmScenario(t, upcomingOptions{})
		s.slotScript()
		printEnv(s, map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_DRIFT_REPORT": filepath.Join(s.home, "report.json")})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.ElementsMatch(t, append(slices.Clone(helmKeys), "driftReport", "driftKnown"), slices.Collect(maps.Keys(got)))
		assert.Equal(t, filepath.Join(s.home, "report.json"), got["driftReport"])
		assert.Equal(t, "helm", got["boot"])
		assert.Equal(t, "2.16", got["minor"])
	})
	t.Run("an image override of a tagged build with a chart override is accepted", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{appVersion: "v2.16.0-rc1"})
		image := s.reg.publishLabeled(t, "rancher/rancher-server", "v2.16.0-rc1", "v2.16.0-rc1", "v1.37.1+k3s1", u.revision, strings.Repeat("d", 40))
		s.slotScript()
		pin := u.overrideURL + "@sha256:" + s.helm.chartSHA
		printEnv(s, map[string]string{
			"E2E_FIDELITY_UPCOMING_IMAGE": "rancher/rancher-server:v2.16.0-rc1@" + image.ListDigest,
			"E2E_FIDELITY_UPCOMING_CHART": pin,
		})
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		got := decodePrinted(t, res.stdout)
		assert.Equal(t, pin, got["chart"])
		assert.NotContains(t, got, "containerImage")
	})
	t.Run("a chart without an assets image prints an empty assetsImage", func(t *testing.T) {
		t.Parallel()
		s, _ := newUpcomingHelmScenario(t, upcomingOptions{})
		s.slotScript()
		printEnv(s, nil)
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		assert.Empty(t, decodePrinted(t, res.stdout)["assetsImage"])
	})

	type stop struct {
		name    string
		opts    upcomingOptions
		env     func(u *upcomingFixture, s *scenario) map[string]string
		want    func(u *upcomingFixture, s *scenario) string
		noCurl  bool
		curlOne bool
	}
	noImageLabel := func(u *upcomingFixture, s *scenario) string {
		return "error: " + s.helm.oracleImage + " has no org.opencontainers.image.revision label of 40 hex digits, which names the chart; set E2E_FIDELITY_UPCOMING_CHART to the chart archive"
	}
	stops := []stop{
		{name: "the head images are of two commits", opts: upcomingOptions{combinedRevision: strings.Repeat("c", 40)}, curlOne: true,
			want: func(u *upcomingFixture, _ *scenario) string {
				return "error: rancher/rancher:head is built from commit " + strings.Repeat("c", 40) + " but rancher/rancher-server:head from commit " + u.revision +
					"; the head images are not of one commit; try again later, or set E2E_FIDELITY_UPCOMING_IMAGE and E2E_FIDELITY_UPCOMING_CHART to builds of one commit"
			}},
		{name: "the combined image has no label", opts: upcomingOptions{combinedNoLabel: true}, curlOne: true,
			want: func(*upcomingFixture, *scenario) string {
				return "error: rancher/rancher:head has no org.opencontainers.image.revision label; the head images cannot be compared"
			}},
		{name: "the server image has no label with a chart override", opts: upcomingOptions{serverNoLabel: true}, curlOne: true,
			env: func(u *upcomingFixture, s *scenario) map[string]string {
				return map[string]string{"E2E_FIDELITY_UPCOMING_CHART": u.overrideURL + "@sha256:" + s.helm.chartSHA}
			},
			want: func(*upcomingFixture, *scenario) string {
				return "error: rancher/rancher-server:head has no org.opencontainers.image.revision label; the head images cannot be compared"
			}},
		{name: "the combined image is not published", opts: upcomingOptions{noCombined: true}, curlOne: true,
			want: func(*upcomingFixture, *scenario) string {
				return "error: rancher/rancher:head is not published; the head images cannot be compared"
			}},
		{name: "the server image has no label", opts: upcomingOptions{serverNoLabel: true}, noCurl: true, want: func(u *upcomingFixture, s *scenario) string { return noImageLabel(u, s) }},
		{name: "a revision of 39 hex digits", opts: upcomingOptions{revision: strings.Repeat("a", 39)}, noCurl: true, want: func(u *upcomingFixture, s *scenario) string { return noImageLabel(u, s) }},
		{name: "a revision with an uppercase letter", opts: upcomingOptions{revision: strings.Repeat("a", 39) + "A"}, noCurl: true, want: func(u *upcomingFixture, s *scenario) string { return noImageLabel(u, s) }},
		{name: "a revision that is not hex", opts: upcomingOptions{revision: "../" + strings.Repeat("a", 37)}, noCurl: true, want: func(u *upcomingFixture, s *scenario) string { return noImageLabel(u, s) }},
		{name: "a chart that is not published yet", opts: upcomingOptions{chartStatus: http.StatusNotFound}, curlOne: true,
			want: func(u *upcomingFixture, _ *scenario) string {
				return "error: the chart for " + u.revision + " is not published yet at " + u.chartURL + "; retry in a few minutes, or once a newer head build is published, or set E2E_FIDELITY_UPCOMING_CHART"
			}},
		{name: "a chart download that fails with another status", opts: upcomingOptions{chartStatus: http.StatusInternalServerError}, curlOne: true,
			want: func(u *upcomingFixture, _ *scenario) string {
				return "error: could not download the chart archive " + u.chartURL + " (HTTP 500); nothing was removed"
			}},
		{name: "a chart of another appVersion", opts: upcomingOptions{appVersion: "v2.16-other-head"}, curlOne: true,
			want: func(u *upcomingFixture, s *scenario) string {
				return "error: the chart archive's appVersion is v2.16-other-head, but the CATTLE_SERVER_VERSION of " + s.helm.oracleImage + " is " + u.serverVersion
			}},
		{name: "a chart override with another sha256", opts: upcomingOptions{}, curlOne: true,
			env: func(u *upcomingFixture, _ *scenario) map[string]string {
				return map[string]string{"E2E_FIDELITY_UPCOMING_CHART": u.overrideURL + "@sha256:" + strings.Repeat("0", 64)}
			},
			want: func(u *upcomingFixture, s *scenario) string {
				return fmt.Sprintf("error: the chart archive %s has sha256 %s, not the pinned %s;", u.overrideURL, s.helm.chartSHA, strings.Repeat("0", 64))
			}},
		{name: "a k3s image that is not published", opts: upcomingOptions{noK3s: true}, noCurl: true,
			want: func(*upcomingFixture, *scenario) string {
				return "error: rancher/k3s:v1.37.1-k3s1 (from CATTLE_K3S_VERSION v1.37.1+k3s1) is not published; the node container of a helm boot runs that k3s"
			}},
		{name: "an image override of a tagged build without a chart override", opts: upcomingOptions{}, noCurl: true,
			env: func(u *upcomingFixture, s *scenario) map[string]string {
				image := s.reg.publishLabeled(s.t, "rancher/rancher-server", "v2.16.0-rc1", "v2.16.0-rc1", "v1.37.1+k3s1", u.revision, strings.Repeat("d", 40))
				return map[string]string{"E2E_FIDELITY_UPCOMING_IMAGE": "rancher/rancher-server:v2.16.0-rc1@" + image.ListDigest}
			},
			want: func(*upcomingFixture, *scenario) string {
				return "is not a head build; the chart of a tagged build is not on the head chart host; set E2E_FIDELITY_UPCOMING_CHART"
			}},
		{name: "an unusable CATTLE_K3S_VERSION", opts: upcomingOptions{k3sVersion: "v1.37.1"}, noCurl: true,
			want: func(*upcomingFixture, *scenario) string {
				return `has no usable CATTLE_K3S_VERSION ("v1.37.1"); the node container of a helm boot runs that k3s`
			}},
		{name: "an unusable CATTLE_K3S_VERSION with a downstream override", opts: upcomingOptions{k3sVersion: "v1.37.1"}, noCurl: true,
			env: func(*upcomingFixture, *scenario) map[string]string {
				return map[string]string{"E2E_FIDELITY_K3S_IMAGE": "rancher/k3s:v1.34.4-k3s1@" + sampleDigest}
			},
			want: func(*upcomingFixture, *scenario) string {
				return `has no usable CATTLE_K3S_VERSION ("v1.37.1"); the node container of a helm boot runs that k3s`
			}},
	}
	for _, c := range stops {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, u := newUpcomingHelmScenario(t, c.opts)
			s.slotScript()
			printEnv(s, nil)
			if c.env != nil {
				maps.Copy(s.env, c.env(u, s))
			}
			res := s.run()
			requirePrintStop(t, s, res, c.want(u, s))
			if c.noCurl {
				assert.Empty(t, s.curlLog(), "the run stops before any download")
			}
			if c.curlOne {
				assert.LessOrEqual(t, len(s.curlLog()), 1, "at most the one chart download")
				assert.Zero(t, s.downloads(s.helm.manifestURL), "no cert-manager manifest download")
			}
		})
	}
	t.Run("an unusable CATTLE_K3S_VERSION does not suggest the downstream override", func(t *testing.T) {
		t.Parallel()
		s, _ := newUpcomingHelmScenario(t, upcomingOptions{k3sVersion: "v1.37.1"})
		s.slotScript()
		printEnv(s, nil)
		res := s.run()
		require.Equal(t, 1, res.code)
		assert.NotContains(t, res.stderr, "set E2E_FIDELITY_K3S_IMAGE")
	})
}

// TestUpcomingHelmBoot asserts the steps of a helm boot of an upcoming minor
// up to the check of the running pod: the node container runs the k3s image
// CATTLE_K3S_VERSION names whatever the downstream override is, the
// HelmChart names the server image and the assets image by digest, the server
// image is not pulled by the host, the chart is downloaded twice and the
// manifest once, one platform read and one slot read are made, and a chart
// of another appVersion stops the run before anything is removed.
func TestUpcomingHelmBoot(t *testing.T) {
	t.Parallel()
	t.Run("the steps and the values", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{assets: "block"})
		f := s.helm
		port := s.slotScript()
		override := "rancher/k3s:v1.34.4-k3s1@" + sampleDigest
		s.env["E2E_FIDELITY_K3S_IMAGE"] = override
		f.setPod("rancher/rancher-server:v9.9.9", "docker.io/rancher/rancher-server@"+sampleDigest)
		res := s.run()

		require.Equalf(t, 1, res.code, "the run stops at the check of the pod; stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: the Rancher pod runs rancher/rancher-server:v9.9.9, not the pinned "+f.oracleImage)
		requireOrder(t, res.log,
			"pull -q "+u.nodeImage,
			"pull -q "+override,
			"network create --subnet "+upcomingSubnet+" "+upcomingContainerName+"-net",
			fmt.Sprintf("run -d --name %s --privileged --network %s-net --ip 172.30.3.2 -p 127.0.0.1:%d:443 --tmpfs /run --tmpfs /var/run %s server --cluster-init",
				upcomingContainerName, upcomingContainerName, port, u.nodeImage),
		)
		for _, line := range res.log {
			if strings.HasPrefix(line, "run -d --name "+upcomingContainerName+" ") {
				assert.NotContains(t, line, override, "the node container does not take the downstream's image")
			}
		}
		assert.False(t, hasCall(res.log, "pull -q "+f.oracleImage), "the node container pulls the server image, the host does not")
		assert.False(t, hasCall(res.log, "run --rm"))
		assert.Equal(t, 1, s.callCount(res.log, "ps -aq"), "one slot read")
		assert.Equal(t, 1, s.callCount(res.log, "version"), "one platform read")

		curl := s.curlLog()
		assert.Equal(t, []string{"real " + u.chartURL, "real " + u.chartURL, "real " + f.manifestURL}, curl[:3],
			"the chart is downloaded once to compute its sha256 and once to check it against that sha256; the manifest once")
		for _, line := range curl {
			assert.NotContains(t, line, "locked-real", "every download is before the lock")
		}

		records := s.stdins()
		require.Len(t, records, 3)
		secret := decodeDocuments(t, records[1].body)
		require.Len(t, secret, 2)
		pw, ok := field(t, secret[1], "stringData", "bootstrapPassword").(string)
		require.True(t, ok)
		for _, where := range []struct{ name, text string }{
			{"the docker argv log", strings.Join(res.log, "\n")},
			{"the curl log", strings.Join(s.curlLog(), "\n")},
			{"stdout", res.stdout},
			{"stderr", res.stderr},
		} {
			assert.NotContainsf(t, where.text, pw, "the password is not in %s", where.name)
		}

		chart := decodeDocuments(t, records[2].body)
		require.Len(t, chart, 1)
		spec := field(t, chart[0], "spec").(map[string]any)
		content, err := base64.StdEncoding.DecodeString(spec["chartContent"].(string))
		require.NoError(t, err)
		assert.Equal(t, f.chartSHA, sha256Hex(content))
		var values map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(spec["valuesContent"].(string)), &values))
		assert.ElementsMatch(t, []string{"replicas", "hostNetwork", "networkExposure", "image", "startupProbe", "assetsImage"}, mapKeys(values),
			"no rancherImage, rancherImageTag or bootstrapPassword")
		assert.Equal(t, map[string]any{"repository": "rancher/rancher-server", "tag": "head@" + u.server.ListDigest}, values["image"])
		assets := values["assetsImage"].(map[string]any)
		assert.Equal(t, map[string]any{"tag": "v2.16-20261027T2136Z-dev@" + s.reg.tags["rancher/rancher-assets:v2.16-20261027T2136Z-dev"]}, assets)
	})

	t.Run("a release branch image keeps its tag in the chart values", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{release: true})
		s.slotScript()
		s.helm.setPod("rancher/rancher-server:v9.9.9", "docker.io/rancher/rancher-server@"+sampleDigest)
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		records := s.stdins()
		require.Len(t, records, 3)
		spec := field(t, decodeDocuments(t, records[2].body)[0], "spec").(map[string]any)
		var values map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(spec["valuesContent"].(string)), &values))
		assert.Equal(t, map[string]any{"repository": "rancher/rancher-server", "tag": "v2.16-head@" + u.server.ListDigest}, values["image"])
	})

	t.Run("a chart of another appVersion stops before the lock", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{appVersion: "v2.16-other-head"})
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, "error: the chart archive's appVersion is v2.16-other-head, but the CATTLE_SERVER_VERSION of "+s.helm.oracleImage+" is "+u.serverVersion)
		assert.Equal(t, 1, s.callCount(res.log, "version"))
		assert.Equal(t, 1, s.callCount(res.log, "ps -aq"))
		assert.Equal(t, 1, s.downloads(s.helm.manifestURL), "the manifest is downloaded before the check, once")
	})

	t.Run("a chart that is not published stops before any pull", func(t *testing.T) {
		t.Parallel()
		s, _ := newUpcomingHelmScenario(t, upcomingOptions{chartStatus: http.StatusNotFound})
		s.slotScript()
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "is not published yet at")
		assert.Equal(t, []string{"info", "version"}, res.commands(), "no slot read, no pull and no removal")
		s.requireNoStateCreated()
	})
}

// TestUpcomingHelmKeptPair asserts how a kept pair is reused or recreated by
// the helm boot of an upcoming minor: a kept helm pair of the current image,
// chart and node container image is reused with one chart download and no
// pull, removal or boot; another chart, a kept container pair run with helm,
// and a kept helm pair run with container each recreate it.
func TestUpcomingHelmKeptPair(t *testing.T) {
	t.Parallel()
	keptState := func(s *scenario, u *upcomingFixture) map[string]any {
		return map[string]any{
			"containerId": "oracle-id", "adminToken": "token", "oracleImage": s.helm.oracleImage,
			"k3sContainerId": "k3s-id", "k3sImage": u.nodeImage, "downstream": "c-m-abc",
			"boot": "helm", "chart": u.pin, "localK3sImage": u.nodeImage, "localK3sSource": "CATTLE_K3S_VERSION", "assetsImage": "",
			"certManager": map[string]any{"manifest": s.pinRewrites["CERT_MANAGER_MANIFEST"], "images": []string{}},
		}
	}
	keep := func(s *scenario, state map[string]any) {
		port := s.slotScript()
		stub := ownPair(upcomingContainerName, port, upcomingSubnet)
		s.slot = &stub
		s.writeState(s.upcomingRoot(), state)
		s.keepIDs(upcomingContainerName)
		s.cannedHealthy("c-m-abc")
		s.env["E2E_FIDELITY_KEEP"] = "1"
	}

	t.Run("a healthy helm pair with a drift run", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		keep(s, keptState(s, u))
		s.env["E2E_FIDELITY_DRIFT"] = "1"
		res := s.run()

		require.Equalf(t, 1, res.code, "the stub writes no drift report; stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> drift run of 2.16 against the community goldens\n")
		assert.Contains(t, res.stdout, "==> reusing healthy kept container")
		env, called := s.goEnv()
		require.True(t, called)
		assert.Equal(t, "upcoming", env["E2E_FIDELITY_DRIFT_PROFILE"])
		assert.Equal(t, "2.16", env["E2E_FIDELITY_MINOR"])
		assert.Equal(t, s.helm.oracleImage, env["E2E_FIDELITY_ORACLE_IMAGE"])
		assert.Equal(t, filepath.Join(resolved(t, s.copyRoot), "internal", "testrancher", "fidelity", "testdata", "upcoming", "v2.16.json"), env["E2E_FIDELITY_DRIFT_KNOWN"])
		assert.Equal(t, 1, s.downloads(u.chartURL), "the chart is downloaded once, to compare its name with the state")
		assert.Zero(t, s.downloads(s.helm.manifestURL))
		for _, prefix := range []string{"rm ", "run ", "pull "} {
			assert.Falsef(t, hasCall(res.log, prefix), "a healthy kept pair makes no %q call: %v", prefix, res.log)
		}
		assert.Equal(t, 1, s.callCount(res.log, "version"))
	})

	t.Run("a pair of another chart runs the steps before the lock", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		state := keptState(s, u)
		state["chart"] = u.overrideURL + "@sha256:" + s.helm.chartSHA
		keep(s, state)
		s.helm.setPod("rancher/rancher-server:v9.9.9", "docker.io/rancher/rancher-server@"+sampleDigest)
		res := s.run()

		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> kept pair was created from different chart, k3s or cert-manager pins; recreating")
		assert.Equal(t, 2, s.downloads(u.chartURL))
		assert.Equal(t, 1, s.downloads(s.helm.manifestURL))
		requireOrder(t, res.log, "pull -q "+u.nodeImage, "rm -f -v")
	})

	t.Run("a kept container pair run with helm is recreated", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		state := keptState(s, u)
		delete(state, "boot")
		state["oracleImage"] = "rancher/rancher:head@" + u.combined.ListDigest
		keep(s, state)
		s.helm.setPod("rancher/rancher-server:v9.9.9", "docker.io/rancher/rancher-server@"+sampleDigest)
		res := s.run()

		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> kept container was created from a different oracle image; recreating")
		assert.Equal(t, 2, s.downloads(u.chartURL), "the steps before the lock ran")
		assert.True(t, hasCall(res.log, "rm -f -v"))
	})

	t.Run("a kept helm pair run with container is recreated", func(t *testing.T) {
		t.Parallel()
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		s.env["E2E_FIDELITY_UPCOMING_BOOT"] = "container"
		state := keptState(s, u)
		state["oracleImage"] = "rancher/rancher:head@" + u.combined.ListDigest
		keep(s, state)
		s.canned("/ping", 200, "pong")
		res := s.run()

		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> kept pair was created with a different boot; recreating")
		assert.True(t, hasCall(res.log, "rm -f -v"))
		assert.Zero(t, s.downloads(u.chartURL), "the container boot downloads no chart")
	})
}

// TestUpcomingChartBaseIsHTTPS asserts that the default chart base is an https
// URL and that the chart download resolves through curl with HTTPS-only
// redirects.
func TestUpcomingChartBaseIsHTTPS(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	assert.True(t, strings.HasPrefix(scriptLiteral(t, "UPCOMING_CHART_BASE"), "https://"))
	flags := regexp.MustCompile(`(?m)^CURL_DOWNLOAD_FLAGS=\((.*)\)$`).FindStringSubmatch(scriptText(t))
	require.Len(t, flags, 2, "CURL_DOWNLOAD_FLAGS is one line")
	assert.Contains(t, flags[1], `--proto-redir "=https"`)
	assert.Contains(t, flags[1], "--fail")
	assert.False(t, curlInsecure.MatchString(flags[1]))
	for _, name := range []string{"fetch_pinned", "resolve_upcoming_chart"} {
		assert.Contains(t, shellFunction(t, name), `"${CURL_DOWNLOAD_FLAGS[@]}"`, name)
	}
}

func scriptText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(realScriptPath(t))
	require.NoError(t, err)
	return string(raw)
}
