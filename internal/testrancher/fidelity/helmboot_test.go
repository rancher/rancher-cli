package fidelity

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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
	helmCheckName   = "rancher-cli-e2e-fidelity-helm-check"
	helmCheckSubnet = "172.30.5.0/24"
)

type helmRowOptions struct {
	// appVersion is the appVersion of the chart archive the test server
	// returns; the default is the version of the pinned image.
	appVersion string
	// assets is how the chart's values name the assets image: "block", "inline"
	// or empty for not at all.
	assets string
	// server names an image of the rancher-server repository instead of the
	// combined image.
	server bool
}

// newHelmCheckScenario is a helm-check row before its script copy is written:
// the helm docker stub, a published oracle image, and the chart archive
// and manifest a test server returns. The case adjusts pinRewrites or the
// stub's files, then calls slotScript and run.
func newHelmCheckScenario(t *testing.T, o helmRowOptions) *scenario {
	t.Helper()
	s := newScenario(t)
	s.minor = ""
	s.installHelmDocker()
	if o.appVersion == "" {
		o.appVersion = "v2.15.2"
		if o.server {
			o.appVersion = "v2.16.0"
		}
	}
	s.serveCharts(o.appVersion, o.assets, o.server)
	s.slot = &slotStub{}
	s.env = map[string]string{"E2E_FIDELITY_HELM_CHECK": "1"}
	return s
}

func (s *scenario) helmCheckRoot() string {
	return filepath.Join(s.stateHome, helmCheckName)
}

// requireOrder asserts that the log has a line starting with each prefix, in
// the order given.
func requireOrder(t *testing.T, log []string, prefixes ...string) {
	t.Helper()
	from := 0
	for _, prefix := range prefixes {
		i := slices.IndexFunc(log[from:], func(line string) bool { return strings.HasPrefix(line, prefix) })
		require.GreaterOrEqualf(t, i, 0, "no docker call starting with %q after position %d: %v", prefix, from, log)
		from += i + 1
	}
}

func hasCall(log []string, prefix string) bool {
	return slices.ContainsFunc(log, func(line string) bool { return strings.HasPrefix(line, prefix) })
}

func decodeDocuments(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(body))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		require.NoError(t, err)
		docs = append(docs, doc)
	}
}

func field(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, key := range path {
		m, ok := cur.(map[string]any)
		require.Truef(t, ok, "%v: %v is not an object", path, cur)
		cur, ok = m[key]
		require.Truef(t, ok, "%v: no %s", path, key)
	}
	return cur
}

// TestHelmCheckRefusesBeforeDocker asserts that every refusal of the
// helm-check variables exits 1 with its message before any docker call and
// before any state is created, and that an empty value counts as unset.
func TestHelmCheckRefusesBeforeDocker(t *testing.T) {
	const flag = "E2E_FIDELITY_HELM_CHECK"
	pinnedOnly := " is set without E2E_FIDELITY_MINOR; the community and Prime profiles run only their pinned images"
	type tc struct {
		name string
		env  map[string]string
		want string
	}
	cases := []tc{
		{"a value other than 1", map[string]string{flag: "2"}, "error: E2E_FIDELITY_HELM_CHECK=2 is not 1; set E2E_FIDELITY_HELM_CHECK=1 or unset it"},
		{"the word true", map[string]string{flag: "true"}, "error: E2E_FIDELITY_HELM_CHECK=true is not 1; set E2E_FIDELITY_HELM_CHECK=1 or unset it"},
		{"a bad value with prime", map[string]string{flag: "0", "E2E_FIDELITY_PRIME": "1"}, "error: E2E_FIDELITY_HELM_CHECK=0 is not 1"},
		{"prime", map[string]string{flag: "1", "E2E_FIDELITY_PRIME": "1"}, "error: E2E_FIDELITY_HELM_CHECK and E2E_FIDELITY_PRIME select different oracles; set one"},
		{"a minor", map[string]string{flag: "1", "E2E_FIDELITY_MINOR": "2.16"}, "error: E2E_FIDELITY_HELM_CHECK and E2E_FIDELITY_MINOR select different oracles; set one"},
		{"the record flag", map[string]string{flag: "1", "E2E_FIDELITY_UPDATE_FLAG": "-update"}, "error: E2E_FIDELITY_HELM_CHECK cannot record goldens; it only compares a run with them"},
		{"drift and the record flag", map[string]string{flag: "1", "E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_UPDATE_FLAG": "-update"}, "error: E2E_FIDELITY_HELM_CHECK cannot record goldens; it only compares a run with them"},
		{"an upcoming image override", map[string]string{flag: "1", "E2E_FIDELITY_UPCOMING_IMAGE": "rancher/rancher:v2.16-abc-head@" + sampleDigest}, "E2E_FIDELITY_UPCOMING_IMAGE" + pinnedOnly},
		{"a k3s image override", map[string]string{flag: "1", "E2E_FIDELITY_K3S_IMAGE": "rancher/k3s:v1.34.4-k3s1@" + sampleDigest}, "E2E_FIDELITY_K3S_IMAGE" + pinnedOnly},
		{"print images", map[string]string{flag: "1", "E2E_FIDELITY_PRINT_IMAGES": "1"}, "E2E_FIDELITY_PRINT_IMAGES" + pinnedOnly},
		{"a drift profile set by the caller", map[string]string{flag: "1", "E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_DRIFT_PROFILE": "upcoming"}, "error: E2E_FIDELITY_DRIFT_PROFILE is set by scripts/e2e-fidelity; unset it"},
		{"a drift profile set by the caller in an upcoming run", map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_DRIFT_PROFILE": "helm-check"}, "error: E2E_FIDELITY_DRIFT_PROFILE is set by scripts/e2e-fidelity; unset it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.slotScript()
			s.env = c.env
			res := s.run()
			s.requireRefused(res, c.want)
			assert.Empty(t, s.reg.recorded())
			assert.Empty(t, s.curlLog())
		})
	}

	t.Run("a helm pin of the community image", func(t *testing.T) {
		s := newScenario(t)
		s.pinRewrites = map[string]string{"ORACLE_BOOT": "helm"}
		s.slotScript()
		s.env = map[string]string{flag: "1"}
		res := s.run()
		s.requireRefused(res, "error: E2E_FIDELITY_HELM_CHECK compares a helm boot with goldens recorded under the container boot, but ORACLE_BOOT is helm")
	})

	t.Run("a helm boot of an image with a registry host", func(t *testing.T) {
		s := newScenario(t)
		const image = "registry.example.com/rancher/rancher:v2.15.2@" + sampleDigest
		s.pinRewrites = map[string]string{"ORACLE_IMAGE": image}
		s.slotScript()
		s.env = map[string]string{flag: "1"}
		res := s.run()
		s.requireRefused(res, "error: a helm boot reads images only from Docker Hub; "+image+" includes a registry host")
	})

	t.Run("an empty value counts as unset", func(t *testing.T) {
		s := newScenario(t)
		s.slotScript()
		s.env = map[string]string{flag: ""}
		res := s.run()
		assert.NotContains(t, res.stderr, flag)
		assert.Equal(t, 97, res.code, "the run goes on to the community profile and stops at the docker stub")
	})
}

// TestHelmCheckRefusesAHeldSlot asserts that a helm-check run names the
// profile in the refusal of a port or subnet another container or network
// holds, and that another profile's refusal names the helm-check pair when it
// holds the port.
func TestHelmCheckRefusesAHeldSlot(t *testing.T) {
	cases := []struct {
		name string
		stub func(port int) slotStub
		want func(port int) string
	}{
		{"port held by a container", func(port int) slotStub {
			return slotStub{ps: "c1\n", inspect: fmt.Sprintf("/web %d \n", port)}
		}, func(port int) string {
			return fmt.Sprintf("error: port %d for helm-check is held by container web\n", port)
		}},
		{"subnet held by a network", func(int) slotStub {
			return slotStub{netLS: "n1\n", netInspect: "lab-net 172.30.5.128/25 \n"}
		}, func(int) string {
			return "error: subnet 172.30.5.0/24 for helm-check overlaps network lab-net (172.30.5.128/25)\n"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHelmCheckScenario(t, helmRowOptions{})
			port := s.slotScript()
			stub := c.stub(port)
			s.slot = &stub
			res := s.run()
			require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
			assert.Contains(t, res.stderr, c.want(port))
			assert.NotContains(t, res.stderr, "that is the")
			assertNoRemovalOrBoot(t, res)
			s.requireNoStateCreated()
			assert.Empty(t, s.curlLog(), "nothing is downloaded before the slot is checked")
		})
	}

	t.Run("port outside docker", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		port := s.slotScript()
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, fmt.Sprintf(
			"error: port %d for helm-check is in use on 127.0.0.1 outside Docker (checked twice, 2 s apart); a pair removed in the last minute can leave the port in TIME_WAIT", port))
		assertNoRemovalOrBoot(t, res)
	})

	t.Run("the helm-check pair holds the port of an upcoming minor", func(t *testing.T) {
		s := newScenario(t)
		s.publishDefaults()
		port := s.slotScript()
		s.slot = &slotStub{ps: "c1\n", inspect: fmt.Sprintf("/%s %d \n", helmCheckName, port)}
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16"}
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, fmt.Sprintf("error: port %d for 2.16 is held by container %s, the helm-check pair\n", port, helmCheckName))
		assertNoRemovalOrBoot(t, res)
	})

	t.Run("the helm-check's own pair is not a holder", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		port := s.slotScript()
		stub := ownPair(helmCheckName, port, helmCheckSubnet)
		s.slot = &stub
		s.helm.setPod("rancher/rancher:v9.9.9", "docker.io/rancher/rancher@"+sampleDigest)
		res := s.run()
		assert.NotContains(t, res.stderr, "is held by")
		assert.NotContains(t, res.stderr, "overlaps network")
		assert.Contains(t, res.stderr, "the Rancher pod runs rancher/rancher:v9.9.9")
	})
}

// stopsBeforeTheLock asserts what every failure before the lock has in
// common: exit 1, the message, nothing removed, created or booted, and no
// state under the state home.
func stopsBeforeTheLock(t *testing.T, s *scenario, res runResult, message string) {
	t.Helper()
	require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
	assert.Contains(t, res.stderr, message)
	assert.Contains(t, res.stderr, "nothing was removed")
	for _, line := range res.log {
		for _, prefix := range []string{"rm ", "run -d", "network create", "network rm", "exec"} {
			assert.Falsef(t, strings.HasPrefix(line, prefix), "docker call %q must not happen before the lock: %v", line, res.log)
		}
	}
	s.requireNoStateCreated()
	_, err := os.Stat(s.helmCheckRoot())
	assert.True(t, os.IsNotExist(err))
}

// TestHelmCheckStopsBeforeTheLock asserts that a chart archive or manifest
// with another sha256, an archive of another appVersion, a k3s image that
// does not match the pin, a failed download and a failed pull each end the
// run before the lock, with their message, so nothing is removed.
func TestHelmCheckStopsBeforeTheLock(t *testing.T) {
	zeros := strings.Repeat("0", 64)
	k3sPin := scriptLiteral(t, "HELMCHECK_LOCAL_K3S_IMAGE")

	t.Run("archive with another sha256", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		f := s.helm
		s.pinRewrites["HELMCHECK_CHART"] = f.chartURL + "@sha256:" + zeros
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, fmt.Sprintf("error: the chart archive %s has sha256 %s, not the pinned %s;", f.chartURL, f.chartSHA, zeros))
		assert.Equal(t, []string{"real " + f.chartURL}, s.curlLog(), "the manifest is not downloaded after a bad archive")
	})
	t.Run("manifest with another sha256", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		f := s.helm
		s.pinRewrites["CERT_MANAGER_MANIFEST"] = f.manifestURL + "@sha256:" + zeros
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, fmt.Sprintf("error: the cert-manager manifest %s has sha256 %s, not the pinned %s;", f.manifestURL, f.manifestSHA, zeros))
	})
	t.Run("archive of another appVersion", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{appVersion: "v2.15.3"})
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, "error: the chart archive's appVersion is v2.15.3, but the CATTLE_SERVER_VERSION of "+s.helm.oracleImage+" is v2.15.2")
	})
	t.Run("archive of the same appVersion passes the check", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.setPod("rancher/rancher:v9.9.9", "docker.io/rancher/rancher@"+sampleDigest)
		s.slotScript()
		res := s.run()
		assert.NotContains(t, res.stderr, "appVersion")
		assert.Contains(t, res.stderr, "the Rancher pod runs rancher/rancher:v9.9.9", "the run got past the checks before the lock to the boot")
	})
	t.Run("image that embeds another k3s", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["k3s_version"] = "k3s version v1.36.3+k3s1 (0000000)\n"
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, "error: "+s.helm.oracleImage+" embeds k3s v1.36.3-k3s1, but the k3s image of the node container is tagged v1.36.2-k3s1;")
		assert.Contains(t, res.log, "run --rm --entrypoint k3s "+s.helm.oracleImage+" --version")
	})
	t.Run("image whose k3s version line names no version", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["k3s_version"] = "unknown\n"
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, "shows no k3s version")
	})
	t.Run("rancher-server image that names another k3s", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{server: true, assets: "block"})
		s.pinRewrites["HELMCHECK_LOCAL_K3S_IMAGE"] = "rancher/k3s:v1.37.2-k3s1@sha256:" + strings.Repeat("a", 64)
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, "names CATTLE_K3S_VERSION v1.37.1+k3s1, but the k3s image of the node container is tagged v1.37.2-k3s1;")
		assert.False(t, hasCall(res.log, "run --rm"), "the k3s version of a rancher-server image is read from the registry")
	})
	t.Run("archive that cannot be downloaded", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		f := s.helm
		s.pinRewrites["HELMCHECK_CHART"] = f.server.URL + "/missing.tgz@sha256:" + f.chartSHA
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, "error: could not download the chart archive "+f.server.URL+"/missing.tgz;")
	})
	t.Run("image that cannot be pulled", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["pull_fail"] = k3sPin + "\n"
		s.slotScript()
		res := s.run()
		stopsBeforeTheLock(t, s, res, "error: could not pull "+k3sPin+"; nothing was removed")
		assert.Empty(t, s.curlLog(), "nothing is downloaded after a failed pull")
	})
}

// TestHelmCheckBoot asserts the boot from the node container to the check of
// the running pod: the docker run line, the order of the steps, the password
// only on the stdin of the secret manifest, the HelmChart object with exactly
// the chart values, the post-rollout check against the pinned image, and that
// a check that passes goes on to the wait for Rancher.
func TestHelmCheckBoot(t *testing.T) {
	k3sPin := scriptLiteral(t, "HELMCHECK_LOCAL_K3S_IMAGE")
	password := regexp.MustCompile(`^[a-zA-Z0-9]{20}$`)

	t.Run("the steps, the password and the HelmChart", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		f := s.helm
		port := s.slotScript()
		s.canned("/ping", 200, "pong")
		res := s.run()

		require.Equalf(t, 1, res.code, "the run goes on to the wait for the webhook and stops there; stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: rancher-webhook not ready within 10 minutes")
		assert.Contains(t, res.stdout, "==> rancher-webhook wait: 60 tries, 10 s apart")
		image := f.oracleImage
		requireOrder(t, res.log,
			"network create --subnet "+helmCheckSubnet+" "+helmCheckName+"-net",
			fmt.Sprintf("run -d --name %s --privileged --network %s-net --ip 172.30.5.2 -p 127.0.0.1:%d:443 --tmpfs /run --tmpfs /var/run %s server --cluster-init --disable=traefik,servicelb,metrics-server,local-storage --node-name=local-node",
				helmCheckName, helmCheckName, port, k3sPin),
			"exec "+helmCheckName+" kubectl get nodes",
			"exec -i "+helmCheckName+" kubectl apply -f -",
			"exec "+helmCheckName+" kubectl -n cert-manager rollout status deploy/cert-manager --timeout=300s",
			"exec "+helmCheckName+" kubectl -n cert-manager rollout status deploy/cert-manager-cainjector",
			"exec "+helmCheckName+" kubectl -n cert-manager rollout status deploy/cert-manager-webhook",
			"exec "+helmCheckName+" kubectl get validatingwebhookconfiguration cert-manager-webhook",
			"exec "+helmCheckName+" kubectl wait --for=condition=Established crd/issuers.cert-manager.io",
			"exec "+helmCheckName+" kubectl -n cert-manager get pods",
			"exec -i "+helmCheckName+" kubectl create -f -",
			"exec -i "+helmCheckName+" kubectl apply -f -",
			"exec "+helmCheckName+" kubectl -n cattle-system get deploy rancher -o name",
			"exec "+helmCheckName+" kubectl -n cattle-system rollout status deploy/rancher",
			"exec "+helmCheckName+" kubectl -n cattle-system get pods -l app=rancher",
			"exec "+helmCheckName+" kubectl -n cattle-system get deploy rancher-webhook",
		)
		for _, ref := range []string{k3sPin, scriptLiteral(t, "K3S_IMAGE"), image} {
			assert.Contains(t, res.log, "pull -q "+ref)
		}
		require.GreaterOrEqual(t, len(s.curlLog()), 2)
		assert.Equal(t, []string{"real " + f.chartURL, "real " + f.manifestURL}, s.curlLog()[:2])
		argv := readLines(t, filepath.Join(s.stubDir, "curl.argv"))
		require.Len(t, argv, 2)
		for i, url := range []string{f.chartURL, f.manifestURL} {
			assert.Regexp(t, "^--fail --location --proto-redir =https --silent --show-error --max-time 60 --output "+regexp.QuoteMeta(s.tmpDir)+"/[^ ]+ "+regexp.QuoteMeta(url)+"$", argv[i])
		}

		records := s.stdins()
		require.Len(t, records, 3)
		assert.Equal(t, f.manifest, records[0].body, "the checked manifest goes to kubectl apply as it was downloaded")

		assert.Equal(t, "create -f -", records[1].args, "the secret is created, not applied: nothing keeps the password in an annotation")
		secret := decodeDocuments(t, records[1].body)
		require.Len(t, secret, 2)
		assert.Equal(t, "Namespace", field(t, secret[0], "kind"))
		assert.Equal(t, "cattle-system", field(t, secret[0], "metadata", "name"))
		assert.Equal(t, "Secret", field(t, secret[1], "kind"))
		assert.Equal(t, "bootstrap-secret", field(t, secret[1], "metadata", "name"))
		assert.Equal(t, "cattle-system", field(t, secret[1], "metadata", "namespace"))
		pw, ok := field(t, secret[1], "stringData", "bootstrapPassword").(string)
		require.True(t, ok)
		assert.Regexp(t, password, pw)
		assert.Equal(t, 1, bytes.Count(records[1].body, []byte(pw)), "the password is in the secret manifest once")

		for _, where := range []struct {
			name string
			text string
		}{
			{"the docker argv log", strings.Join(res.log, "\n")},
			{"the manifest apply", string(records[0].body)},
			{"the HelmChart apply", string(records[2].body)},
			{"the curl log", strings.Join(s.curlLog(), "\n")},
			{"stdout", res.stdout},
			{"stderr", res.stderr},
		} {
			assert.NotContainsf(t, where.text, pw, "the password is not in %s", where.name)
		}

		chart := decodeDocuments(t, records[2].body)
		require.Len(t, chart, 1)
		assert.Equal(t, "helm.cattle.io/v1", field(t, chart[0], "apiVersion"))
		assert.Equal(t, "HelmChart", field(t, chart[0], "kind"))
		assert.Equal(t, "rancher", field(t, chart[0], "metadata", "name"))
		assert.Equal(t, "kube-system", field(t, chart[0], "metadata", "namespace"))
		spec, ok := field(t, chart[0], "spec").(map[string]any)
		require.True(t, ok)
		assert.ElementsMatch(t, []string{"targetNamespace", "failurePolicy", "chartContent", "valuesContent"}, mapKeys(spec), "no spec.chart and no spec.repo")
		assert.Equal(t, "cattle-system", spec["targetNamespace"])
		assert.Equal(t, "abort", spec["failurePolicy"], "a failed install is not uninstalled and installed again, which would delete the bootstrap secret")
		content, err := base64.StdEncoding.DecodeString(spec["chartContent"].(string))
		require.NoError(t, err)
		assert.Equal(t, f.chartSHA, sha256Hex(content), "chartContent is the archive whose sha256 was checked")
		var values map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(spec["valuesContent"].(string)), &values))
		_, tag, _ := strings.Cut(strings.SplitN(image, "@", 2)[0], ":")
		assert.Equal(t, map[string]any{
			"replicas":        1,
			"hostNetwork":     true,
			"networkExposure": map[string]any{"type": "none"},
			"image":           map[string]any{"repository": "rancher/rancher", "tag": tag + "@" + strings.SplitN(image, "@", 2)[1]},
			"startupProbe":    map[string]any{"failureThreshold": 30},
		}, values, "exactly the chart values; no rancherImage, rancherImageTag, bootstrapPassword or assetsImage")
	})

	t.Run("a kept pair starts with a restart policy", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.setPod("rancher/rancher:v9.9.9", "docker.io/rancher/rancher@"+sampleDigest)
		s.slotScript()
		s.env["E2E_FIDELITY_KEEP"] = "1"
		res := s.run()
		assert.True(t, hasCall(res.log, "run -d --name "+helmCheckName+" --privileged --restart unless-stopped --network"), "log: %v", res.log)
		removals := 0
		for _, line := range res.log {
			if strings.HasPrefix(line, "rm ") {
				removals++
			}
		}
		assert.Equal(t, 1, removals, "the removal before the boot only; a kept pair is not removed on failure")
	})

	for _, c := range []struct {
		name    string
		image   string
		imageID string
		want    string
	}{
		{"a pod that runs another image", "rancher/rancher:v2.14.0", "docker.io/rancher/rancher@IMAGEDIGEST",
			"error: the Rancher pod runs rancher/rancher:v2.14.0, not the pinned IMAGE"},
		{"a pod built with rancherImage set", "example.com/team/rancher:custom", "docker.io/rancher/rancher@IMAGEDIGEST",
			"error: the Rancher pod runs example.com/team/rancher:custom, not the pinned IMAGE"},
		{"a pod whose imageID names another digest", "IMAGE", "docker.io/rancher/rancher@" + sampleDigest,
			"does not end in the pinned digest IMAGEDIGEST"},
		{"a pod with no imageID", "IMAGE", "", "error: the Rancher pod's imageID <empty> does not end in the pinned digest IMAGEDIGEST"},
		{"a pod without a container named rancher", "", "docker.io/rancher/rancher@IMAGEDIGEST", "error: the Rancher pod has no container named rancher"},
		{"no pod", "", "", "error: the cluster in the node container has no Rancher pod"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newHelmCheckScenario(t, helmRowOptions{})
			f := s.helm
			digest := f.oracleImage[strings.Index(f.oracleImage, "@")+1:]
			expand := func(text string) string {
				return strings.NewReplacer("IMAGEDIGEST", digest, "IMAGE", f.oracleImage).Replace(text)
			}
			switch {
			case c.image == "" && c.imageID != "":
				f.setPod("", expand(c.imageID))
			case c.image == "":
				f.files["pod_lines"] = ""
			default:
				f.setPod(expand(c.image), expand(c.imageID))
			}
			s.slotScript()
			res := s.run()
			require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
			assert.Contains(t, res.stderr, expand(c.want))
			assert.NotContains(t, res.stdout, "waiting for https://", "the run stops before the wait for Rancher")
			for _, line := range s.curlLog() {
				assert.NotContains(t, line, "/ping")
			}
			assert.True(t, hasCall(res.log, "rm -f -v"), "the failed pair is removed")
		})
	}
}

// TestHelmCheckBootOfARancherServerImage asserts the paths the helm-check run
// does not reach, because it boots the combined image: an image of the
// rancher-server repository takes the k3s version of the node container's
// image from CATTLE_K3S_VERSION and no run of the image, and a chart with the
// assets image gets that image's digest in its values.
func TestHelmCheckBootOfARancherServerImage(t *testing.T) {
	s := newHelmCheckScenario(t, helmRowOptions{server: true, assets: "block"})
	f := s.helm
	s.helm.setPod("rancher/rancher-server:v9.9.9", "docker.io/rancher/rancher-server@"+sampleDigest)
	s.slotScript()
	res := s.run()
	require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
	assert.False(t, hasCall(res.log, "run --rm"))
	assert.False(t, hasCall(res.log, "pull -q "+f.oracleImage), "the node container pulls the server image, the host does not")
	assert.Contains(t, res.log, "pull -q rancher/k3s:v1.37.1-k3s1@sha256:"+strings.Repeat("a", 64))

	records := s.stdins()
	require.Len(t, records, 3)
	chart := decodeDocuments(t, records[2].body)
	spec := field(t, chart[0], "spec").(map[string]any)
	var values map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(spec["valuesContent"].(string)), &values))
	digest := f.oracleImage[strings.Index(f.oracleImage, "@")+1:]
	assert.Equal(t, map[string]any{"repository": "rancher/rancher-server", "tag": "v2.16.0@" + digest}, values["image"])
	assets := values["assetsImage"].(map[string]any)
	assert.Regexp(t, `^v2\.16-20261027T2136Z-dev@sha256:[0-9a-f]{64}$`, assets["tag"])
	assert.Equal(t, s.reg.tags["rancher/rancher-assets:v2.16-20261027T2136Z-dev"], strings.SplitN(assets["tag"].(string), "@", 2)[1])
	assert.Contains(t, res.stderr, "error: the Rancher pod runs rancher/rancher-server:v9.9.9")
}

// writeState writes a kept pair's state file and CA under the state root, as
// a boot with E2E_FIDELITY_KEEP leaves them.
func (s *scenario) writeState(root string, state map[string]any) {
	s.t.Helper()
	require.NoError(s.t, os.MkdirAll(root, 0o700))
	raw, err := json.Marshal(state)
	require.NoError(s.t, err)
	require.NoError(s.t, os.WriteFile(filepath.Join(root, "state.json"), raw, 0o600))
	require.NoError(s.t, os.WriteFile(filepath.Join(root, "ca.pem"), []byte("-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n"), 0o600))
}

// helmState is the state file of a kept helm-check pair that names the pins of
// the row's script copy.
func (s *scenario) helmState(t *testing.T) map[string]any {
	t.Helper()
	f := s.helm
	return map[string]any{
		"containerId": "oracle-id", "adminToken": "token", "oracleImage": f.oracleImage,
		"k3sContainerId": "k3s-id", "k3sImage": scriptLiteral(t, "K3S_IMAGE"), "downstream": "c-m-abc",
		"boot": "helm", "chart": s.pinRewrites["HELMCHECK_CHART"],
		"localK3sImage":  scriptLiteral(t, "HELMCHECK_LOCAL_K3S_IMAGE"),
		"localK3sSource": "embedded", "assetsImage": "",
		"certManager": map[string]any{"manifest": s.pinRewrites["CERT_MANAGER_MANIFEST"], "images": []string{}},
	}
}

func (s *scenario) keepIDs(container string) {
	s.t.Helper()
	s.helm.files["ids"] = container + " oracle-id\n" + container + "-k3s k3s-id\n"
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return out
}

// TestHelmCheckKeptPair asserts how a kept pair is reused or rebuilt: a
// healthy kept helm-check pair makes no pull, download or removal and runs
// its drift run with the helm-check minor, profile and known file; an
// upcoming pair's drift run is unchanged apart from its profile; a pair of
// another chart runs the steps before the lock; a pair that fails its health
// check runs them after the lock and before any removal.
func TestHelmCheckKeptPair(t *testing.T) {
	t.Run("a healthy helm-check pair with a drift run", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		port := s.slotScript()
		stub := ownPair(helmCheckName, port, helmCheckSubnet)
		s.slot = &stub
		s.writeState(s.helmCheckRoot(), s.helmState(t))
		s.keepIDs(helmCheckName)
		s.cannedHealthy("c-m-abc")
		s.env["E2E_FIDELITY_KEEP"] = "1"
		s.env["E2E_FIDELITY_DRIFT"] = "1"
		res := s.run()

		require.Equalf(t, 1, res.code, "the stub writes no drift report; stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: the drift run did not finish; no report at ")
		assert.Contains(t, res.stdout, "==> drift run of helm-check (2.15) against the community goldens")
		assert.Contains(t, res.stdout, "==> rancher-webhook wait: 60 tries, 10 s apart")
		env, called := s.goEnv()
		require.True(t, called)
		assert.Equal(t, "2.15", env["E2E_FIDELITY_MINOR"])
		assert.Equal(t, "helm-check", env["E2E_FIDELITY_DRIFT_PROFILE"])
		assert.Equal(t, filepath.Join(resolved(t, s.copyRoot), "internal", "testrancher", "fidelity", "testdata", "install", "v2.15.json"), env["E2E_FIDELITY_DRIFT_KNOWN"])
		assert.Equal(t, s.helm.oracleImage, env["E2E_FIDELITY_ORACLE_IMAGE"])
		for _, prefix := range []string{"rm ", "run ", "pull "} {
			assert.Falsef(t, hasCall(res.log, prefix), "a healthy kept pair makes no %q call: %v", prefix, res.log)
		}
		for _, line := range s.curlLog() {
			assert.NotContains(t, line, "real ", "a healthy kept pair downloads nothing")
		}
	})

	t.Run("a healthy upcoming pair with a drift run", func(t *testing.T) {
		s := newScenario(t)
		image := s.publishDefaults()
		oracle := "rancher/rancher:head@" + image.ListDigest
		s.present = []string{oracle, s.k3sRef}
		port := s.slotScript()
		s.installHelmDocker()
		const container = "rancher-cli-e2e-fidelity-v2.16"
		stub := ownPair(container, port, "172.30.3.0/24")
		s.slot = &stub
		s.writeState(filepath.Join(s.stateHome, container), map[string]any{
			"containerId": "oracle-id", "adminToken": "token", "oracleImage": oracle,
			"k3sContainerId": "k3s-id", "k3sImage": s.k3sRef, "downstream": "c-m-abc",
		})
		s.keepIDs(container)
		s.cannedHealthy("c-m-abc")
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_KEEP": "1", "E2E_FIDELITY_DRIFT": "1"}
		res := s.run()

		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> drift run of 2.16 against the community goldens\n")
		assert.Contains(t, res.stdout, "==> rancher-webhook wait: 60 tries, 10 s apart")
		env, _ := s.goEnv()
		assert.Equal(t, "upcoming", env["E2E_FIDELITY_DRIFT_PROFILE"])
		assert.Equal(t, "2.16", env["E2E_FIDELITY_MINOR"])
		assert.Equal(t, filepath.Join(resolved(t, s.copyRoot), "internal", "testrancher", "fidelity", "testdata", "upcoming", "v2.16.json"), env["E2E_FIDELITY_DRIFT_KNOWN"])
	})

	t.Run("a community pair whose state has no boot is reused as a container boot", func(t *testing.T) {
		s := newScenario(t)
		s.minor = ""
		s.installHelmDocker()
		s.slotScript()
		state := map[string]any{
			"containerId": "oracle-id", "adminToken": "token", "oracleImage": scriptLiteral(t, "ORACLE_IMAGE"),
			"k3sContainerId": "k3s-id", "k3sImage": scriptLiteral(t, "K3S_IMAGE"), "downstream": "c-m-abc",
			"chart": "left over from another pin",
		}
		root := filepath.Join(s.stateHome, "rancher-cli-e2e-fidelity")
		s.writeState(root, state)
		s.keepIDs("rancher-cli-e2e-fidelity")
		s.cannedHealthy("c-m-abc")
		s.env = map[string]string{"E2E_FIDELITY_KEEP": "1"}
		res := s.run()

		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> reusing healthy kept container")
		assert.Contains(t, res.stdout, "==> rancher-webhook wait: 30 tries, 10 s apart")
		for _, prefix := range []string{"rm ", "run ", "pull "} {
			assert.Falsef(t, hasCall(res.log, prefix), "a community pair whose state file has no boot is reused: %v", res.log)
		}
		for _, line := range s.curlLog() {
			assert.NotContains(t, line, "real ")
		}
		env, called := s.goEnv()
		require.True(t, called)
		assert.NotContains(t, env, "E2E_FIDELITY_DRIFT_PROFILE")
		raw, err := os.ReadFile(filepath.Join(root, "state.json"))
		require.NoError(t, err)
		var after map[string]any
		require.NoError(t, json.Unmarshal(raw, &after))
		assert.Equal(t, state, after, "a reused pair's state file is not rewritten")
	})

	t.Run("a kept pair of another chart runs the steps before the lock", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.setPod("rancher/rancher:v9.9.9", "docker.io/rancher/rancher@"+sampleDigest)
		s.slotScript()
		state := s.helmState(t)
		state["chart"] = "https://example.com/rancher-2.14.0.tgz@sha256:" + strings.Repeat("b", 64)
		s.writeState(s.helmCheckRoot(), state)
		s.keepIDs(helmCheckName)
		s.cannedHealthy("c-m-abc")
		s.env["E2E_FIDELITY_KEEP"] = "1"
		res := s.run()

		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> kept pair was created from different chart, k3s or cert-manager pins; recreating")
		assert.Equal(t, []string{"real " + s.helm.chartURL, "real " + s.helm.manifestURL}, s.curlLog())
		requireOrder(t, res.log, "pull -q ", "rm -f -v", "run -d --name "+helmCheckName)
	})

	t.Run("a kept pair that fails its health check does the work before any removal", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.setPod("rancher/rancher:v9.9.9", "docker.io/rancher/rancher@"+sampleDigest)
		port := s.slotScript()
		s.writeState(s.helmCheckRoot(), s.helmState(t))
		s.keepIDs(helmCheckName)
		s.canned("/ping", 200, "no reply")
		s.env["E2E_FIDELITY_KEEP"] = "1"
		res := s.run()

		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.NotContains(t, res.stdout, "==> reusing healthy kept container")
		assert.Equal(t, []string{"canned https://127.0.0.1:" + fmt.Sprint(port) + "/ping", "locked-real " + s.helm.chartURL, "locked-real " + s.helm.manifestURL}, s.curlLog(), "the work runs after the lock")
		requireOrder(t, res.log, "pull -q ", "rm -f -v", "run -d --name "+helmCheckName)
		assert.True(t, slices.IndexFunc(res.log, func(l string) bool { return strings.HasPrefix(l, "rm ") }) > slices.IndexFunc(res.log, func(l string) bool { return strings.HasPrefix(l, "pull ") }))
	})
}

func mapKeys(m map[string]any) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// stateProgram runs the state functions of the script against a docker stub
// that knows two containers.
func stateProgram(t *testing.T, body string) (stateFile string, stdout string) {
	t.Helper()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	dir := t.TempDir()
	stateFile = filepath.Join(dir, "state.json")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(helmDockerStubScript), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ids"), []byte("pair oracle-id\npair-k3s k3s-id\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "log"), nil, 0o600))
	program := "set -euo pipefail\n" +
		"STATE_FILE=" + stateFile + " CONTAINER=pair K3S_CONTAINER=pair-k3s ORACLE_IMAGE=oracle K3S_IMAGE=k3s ADMIN_TOKEN=token DOWNSTREAM_ID=c-1\n" +
		"CHART_PIN=chart LOCAL_K3S_PIN=node-k3s CERT_MANAGER_MANIFEST=manifest\n" +
		shellFunction(t, "state_value") + shellFunction(t, "load_helm_state") + shellFunction(t, "write_state") + body + "\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", program)
	cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "STUB_LOG=" + filepath.Join(dir, "log"), "STUB_DIR=" + dir}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "output: %s", out)
	return stateFile, string(out)
}

// TestWriteStateRecordsTheBoot asserts that write_state records the boot, that
// a container pair's state has none of the helm fields, and that a reused helm
// pair's state keeps the assets image, the source of the k3s version and the
// cert-manager image digests it read from the state file, which the run did
// not compute.
func TestWriteStateRecordsTheBoot(t *testing.T) {
	read := func(t *testing.T, path string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		var state map[string]any
		require.NoError(t, json.Unmarshal(raw, &state))
		return state
	}

	t.Run("container", func(t *testing.T) {
		path, _ := stateProgram(t, `BOOT=container; HELM_ASSETS_IMAGE=""; LOCAL_K3S_SOURCE=""; CERT_MANAGER_IMAGES="[]"; write_state`)
		assert.Equal(t, map[string]any{
			"containerId": "oracle-id", "adminToken": "token", "oracleImage": "oracle",
			"k3sContainerId": "k3s-id", "k3sImage": "k3s", "downstream": "c-1", "boot": "container",
		}, read(t, path))
	})

	t.Run("a computed helm pair", func(t *testing.T) {
		path, _ := stateProgram(t, `BOOT=helm; HELM_ASSETS_IMAGE="tag@sha256:aa"; LOCAL_K3S_SOURCE=embedded; CERT_MANAGER_IMAGES='["quay.io/x@sha256:bb"]'; write_state`)
		assert.Equal(t, map[string]any{
			"containerId": "oracle-id", "adminToken": "token", "oracleImage": "oracle",
			"k3sContainerId": "k3s-id", "k3sImage": "k3s", "downstream": "c-1", "boot": "helm",
			"chart": "chart", "localK3sImage": "node-k3s", "localK3sSource": "embedded", "assetsImage": "tag@sha256:aa",
			"certManager": map[string]any{"manifest": "manifest", "images": []any{"quay.io/x@sha256:bb"}},
		}, read(t, path))
	})

	t.Run("a reused helm pair keeps the values it did not compute", func(t *testing.T) {
		dir := t.TempDir()
		previous := filepath.Join(dir, "previous.json")
		require.NoError(t, os.WriteFile(previous, []byte(`{"assetsImage":"old@sha256:cc","localK3sSource":"CATTLE_K3S_VERSION","certManager":{"manifest":"manifest","images":["quay.io/y@sha256:dd"]}}`), 0o600))
		path, _ := stateProgram(t, `cp `+previous+` "${STATE_FILE}"; BOOT=helm; HELM_ASSETS_IMAGE=""; LOCAL_K3S_SOURCE=""; CERT_MANAGER_IMAGES="[]"; load_helm_state; write_state`)
		state := read(t, path)
		assert.Equal(t, "old@sha256:cc", state["assetsImage"])
		assert.Equal(t, "CATTLE_K3S_VERSION", state["localK3sSource"])
		assert.Equal(t, map[string]any{"manifest": "manifest", "images": []any{"quay.io/y@sha256:dd"}}, state["certManager"])
	})
}

// TestHelmCheckKeptPairFields asserts that a kept pair is recreated, and the
// checks and downloads of helm_prepare run before the lock, when any one of
// the state file fields it is compared by differs: the boot, the image, the
// chart, the k3s image of the node container, the cert-manager manifest, and
// the container id the state file records against the container Docker holds.
// The pair would otherwise pass every health check, so a comparison that is
// missing shows as a reused pair or as work done after the lock.
func TestHelmCheckKeptPairFields(t *testing.T) {
	zeros := strings.Repeat("0", 64)
	rows := []struct {
		name   string
		change func(s *scenario, state map[string]any)
	}{
		{"boot", func(_ *scenario, state map[string]any) { state["boot"] = "container" }},
		{"oracleImage", func(_ *scenario, state map[string]any) {
			state["oracleImage"] = "rancher/rancher:v2.15.1@sha256:" + zeros
		}},
		{"chart", func(_ *scenario, state map[string]any) {
			state["chart"] = "https://example.com/rancher-2.14.0.tgz@sha256:" + zeros
		}},
		{"localK3sImage", func(_ *scenario, state map[string]any) {
			state["localK3sImage"] = "rancher/k3s:v1.36.1-k3s1@sha256:" + zeros
		}},
		{"certManager.manifest", func(_ *scenario, state map[string]any) {
			state["certManager"] = map[string]any{"manifest": "https://example.com/cert-manager.yaml@sha256:" + zeros, "images": []string{}}
		}},
		{"containerId", func(s *scenario, _ map[string]any) {
			s.helm.files["ids"] = helmCheckName + " another-id\n" + helmCheckName + "-k3s k3s-id\n"
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newHelmCheckScenario(t, helmRowOptions{})
			s.helm.setPod("rancher/rancher:v9.9.9", "docker.io/rancher/rancher@"+sampleDigest)
			s.slotScript()
			state := s.helmState(t)
			s.keepIDs(helmCheckName)
			row.change(s, state)
			s.writeState(s.helmCheckRoot(), state)
			s.cannedHealthy("c-m-abc")
			s.env["E2E_FIDELITY_KEEP"] = "1"
			res := s.run()

			require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
			assert.NotContains(t, res.stdout, "==> reusing healthy kept container")
			assert.Equal(t, []string{"real " + s.helm.chartURL, "real " + s.helm.manifestURL}, s.curlLog(), "the work runs before the lock, and the pair does not pass the health check")
			requireOrder(t, res.log, "pull -q ", "rm -f -v", "run -d --name "+helmCheckName)
		})
	}
}

// TestHelmCheckKeepStopsBeforeTheLockWithoutState asserts that a failure
// before the lock in a run with E2E_FIDELITY_KEEP=1 and no state file creates
// no state root and removes nothing.
func TestHelmCheckKeepStopsBeforeTheLockWithoutState(t *testing.T) {
	s := newHelmCheckScenario(t, helmRowOptions{})
	zeros := strings.Repeat("0", 64)
	s.pinRewrites["HELMCHECK_CHART"] = s.helm.chartURL + "@sha256:" + zeros
	s.slotScript()
	s.env["E2E_FIDELITY_KEEP"] = "1"
	res := s.run()
	stopsBeforeTheLock(t, s, res, "error: the chart archive "+s.helm.chartURL+" has sha256 ")
}

// reuseProgram runs reuse_state against a state file and a docker stub that
// knows the pair's container, and prints its status and the values it loaded.
func reuseProgram(t *testing.T, state map[string]any, computed bool) string {
	t.Helper()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(stateFile, raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(helmDockerStubScript), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ids"), []byte("pair oracle-id\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "log"), nil, 0o600))
	program := "set -uo pipefail\n" +
		"STATE_FILE=" + stateFile + " CONTAINER=pair ORACLE_IMAGE=oracle BIND_ADDR=127.0.0.1 BOOT=helm\n" +
		"CHART_PIN=chart LOCAL_K3S_PIN=node-k3s CERT_MANAGER_MANIFEST=manifest\n"
	if computed {
		program += "HELM_COMPUTED=1 HELM_ASSETS_IMAGE=computed@sha256:aa LOCAL_K3S_SOURCE=embedded\n"
	} else {
		program += "HELM_COMPUTED='' HELM_ASSETS_IMAGE='' LOCAL_K3S_SOURCE=''\n"
	}
	program += shellFunction(t, "state_value") + shellFunction(t, "load_helm_state") + shellFunction(t, "reuse_state") +
		"reuse_state\necho \"status=$? assets=${HELM_ASSETS_IMAGE} source=${LOCAL_K3S_SOURCE}\"\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", program)
	cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "STUB_LOG=" + filepath.Join(dir, "log"), "STUB_DIR=" + dir}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "output: %s", out)
	return string(out)
}

// TestReuseStateComparesWhatARunComputed asserts that reuse_state compares the
// assets image and the source of the k3s version in the state file with the
// values the run computed, and that a run which computed nothing reads them
// from the state file.
func TestReuseStateComparesWhatARunComputed(t *testing.T) {
	const mismatch = "==> kept pair has a different assets image or a different source of its k3s version (the server image or CATTLE_K3S_VERSION); recreating"
	base := func() map[string]any {
		return map[string]any{
			"containerId": "oracle-id", "oracleImage": "oracle", "boot": "helm", "chart": "chart",
			"localK3sImage": "node-k3s", "localK3sSource": "embedded", "assetsImage": "computed@sha256:aa",
			"certManager": map[string]any{"manifest": "manifest", "images": []string{}},
		}
	}
	t.Run("a run that computed other values than the state file's", func(t *testing.T) {
		for key, value := range map[string]string{"assetsImage": "other@sha256:bb", "localK3sSource": "CATTLE_K3S_VERSION"} {
			state := base()
			state[key] = value
			out := reuseProgram(t, state, true)
			assert.Contains(t, out, mismatch, key)
			assert.Contains(t, out, "status=1", key)
		}
	})
	t.Run("a run whose values equal the state file's goes on to the next check", func(t *testing.T) {
		out := reuseProgram(t, base(), true)
		assert.NotContains(t, out, mismatch)
		assert.Contains(t, out, "==> kept container bind address changed; recreating", "the stub reports no bind address, which is the next check")
	})
	t.Run("a run that computed nothing reads the values from the state file", func(t *testing.T) {
		state := base()
		state["assetsImage"] = "recorded@sha256:cc"
		state["localK3sSource"] = "CATTLE_K3S_VERSION"
		out := reuseProgram(t, state, false)
		assert.NotContains(t, out, mismatch)
		assert.Contains(t, out, "assets=recorded@sha256:cc source=CATTLE_K3S_VERSION")
	})
}

// TestFetchPinnedChecksCertificates asserts that the download functions of the
// helm boot never turn off certificate checks: no -k, -sk or --insecure.
func TestFetchPinnedChecksCertificates(t *testing.T) {
	insecure := curlInsecure
	assert.False(t, insecure.MatchString(shellFunction(t, "fetch_pinned")))
	assert.False(t, insecure.MatchString(shellFunction(t, "resolve_upcoming_chart")))
	for _, bad := range []string{"curl -sk http://x", "curl -k http://x", "curl --insecure http://x", "curl -fsSk http://x"} {
		assert.Truef(t, insecure.MatchString(bad), "%s", bad)
	}
	for _, good := range []string{"curl --fail --location --silent --max-time 60 --output f http://x", "curl --show-error http://x"} {
		assert.Falsef(t, insecure.MatchString(good), "%s", good)
	}
}

// TestHelmCheckBootFailures asserts the failures of the boot after the lock:
// a node container that stopped or that Docker is restarting is reported at
// once with its exit code and log, a cert-manager webhook with no CA bundle or
// an Issuer CRD that is not established stops the run before the secret and
// the chart, and a chart install that creates no deploy/rancher is named as
// that.
func TestHelmCheckBootFailures(t *testing.T) {
	t.Run("a node container that stopped", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["not_ready"] = "1"
		s.helm.files["node_state"] = "exited"
		s.slotScript()
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: the node container stopped (exit code 3); its last log lines:")
		assert.Contains(t, res.stderr, "stub node container log line")
		assert.Empty(t, s.stdins(), "nothing is applied to a node container that stopped")
	})
	t.Run("a node container that Docker is restarting", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["not_ready"] = "1"
		s.helm.files["node_state"] = "restarting"
		s.slotScript()
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: the node container stopped (exit code 3); its last log lines:")
		assert.Contains(t, res.stderr, "stub node container log line")
		assert.Empty(t, s.stdins(), "nothing is applied to a node container that is restarting")
	})
	t.Run("a cert-manager webhook with no CA bundle", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["ca_bundle"] = ""
		s.slotScript()
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: the cert-manager webhook configuration still had no CA bundle after 2 minutes")
		assert.Len(t, s.stdins(), 1, "only the cert-manager manifest was applied")
		assert.False(t, hasCall(res.log, "exec -i "+helmCheckName+" kubectl create"))
		assert.False(t, hasCall(res.log, "exec "+helmCheckName+" kubectl wait"), "the CRD is waited for after the CA bundle")
	})
	t.Run("a cert-manager Issuer CRD that is not established", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["crd_not_established"] = "1"
		s.slotScript()
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: the cert-manager Issuer CRD was not established within 1 minute")
		assert.Len(t, s.stdins(), 1, "only the cert-manager manifest was applied")
		assert.False(t, hasCall(res.log, "exec -i "+helmCheckName+" kubectl create"), "no secret is created")
		assert.False(t, hasCall(res.log, "exec "+helmCheckName+" kubectl -n cattle-system get deploy"), "no wait for the chart's deployment")
		assert.False(t, hasCall(res.log, "exec "+helmCheckName+" kubectl -n cattle-system rollout"), "no wait for the chart's rollout")
	})
	t.Run("a chart install that creates no deploy/rancher", func(t *testing.T) {
		s := newHelmCheckScenario(t, helmRowOptions{})
		s.helm.files["deploy_missing"] = "1"
		s.slotScript()
		res := s.run()
		require.Equalf(t, 1, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stderr, "error: the chart install created no deploy/rancher within 10 minutes")
		assert.NotContains(t, res.stderr, "did not roll out")
		assert.False(t, hasCall(res.log, "exec "+helmCheckName+" kubectl -n cattle-system rollout status deploy/rancher"))
	})
}

// TestHelmCheckReadsTheAssetsImageForm asserts that a chart whose values name
// assetsImage in a form the script cannot read stops the run before the lock
// instead of booting with the chart's default assets image.
func TestHelmCheckReadsTheAssetsImageForm(t *testing.T) {
	s := newHelmCheckScenario(t, helmRowOptions{assets: "inline"})
	s.slotScript()
	res := s.run()
	stopsBeforeTheLock(t, s, res, "error: the chart's values.yaml names assetsImage in a form this script cannot read")
}
