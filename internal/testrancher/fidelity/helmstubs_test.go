package fidelity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// curlStubScript stands in for curl. Every request is logged with what the stub
// did with it: "real" for a URL under STUB_HTTP_PREFIX (the test's own HTTP
// server), which goes to the real curl; "canned" for a URL whose path is a row
// of STUB_CURL_TABLE (path, status and body, tab separated); "denied" for any
// other URL, which exits 97. A real request made while the directory
// STUB_LOCK_DIR exists (the run holds its lock) is logged as "locked-real", and
// the full argument list of every real request goes to STUB_CURL_ARGV.
const curlStubScript = `#!/bin/sh
url=""
out=""
fmt=""
for arg in "$@"; do
  if [ "$take" = out ]; then out="$arg"; take=""; continue; fi
  if [ "$take" = fmt ]; then fmt="$arg"; take=""; continue; fi
  case "$arg" in
  -o|--output) take=out ;;
  -w|--write-out) take=fmt ;;
  http://*|https://*) url="$arg" ;;
  esac
done
case "$url" in
"$STUB_HTTP_PREFIX"/*)
  if [ -n "$STUB_LOCK_DIR" ] && [ -d "$STUB_LOCK_DIR" ]; then
    printf 'locked-real %s\n' "$url" >> "$STUB_CURL_LOG"
  else
    printf 'real %s\n' "$url" >> "$STUB_CURL_LOG"
  fi
  printf '%s\n' "$*" >> "$STUB_CURL_ARGV"
  exec "$REAL_CURL" "$@" ;;
esac
path="/${url#*://*/}"
tab=$(printf '\t')
while IFS="$tab" read -r p code body; do
  if [ "$p" = "$path" ]; then
    printf 'canned %s\n' "$url" >> "$STUB_CURL_LOG"
    if [ -z "$out" ]; then printf '%s' "$body"; fi
    case "$fmt" in
    '%{http_code}') printf '%s' "$code" ;;
    '\n%{http_code}') printf '\n%s' "$code" ;;
    esac
    exit 0
  fi
done < "$STUB_CURL_TABLE"
printf 'denied %s\n' "$url" >> "$STUB_CURL_LOG"
exit 97
`

// goStubScript stands in for go: it logs its arguments and the
// E2E_FIDELITY_* variables of its environment, and exits 0 without writing a
// drift report.
const goStubScript = `#!/bin/sh
printf 'args %s\n' "$*" >> "$STUB_GO_LOG"
env | grep '^E2E_FIDELITY_[A-Z_]*=' | sort | awk '{print "env " $0}' >> "$STUB_GO_LOG"
exit 0
`

// sleepStubScript stands in for sleep so no wait takes time.
const sleepStubScript = "#!/bin/sh\nexit 0\n"

// opensslStubScript stands in for openssl; nothing a run reaches reads its
// output.
const opensslStubScript = "#!/bin/sh\nexit 0\n"

// dfStubScript stands in for df with ample free space, so no run depends on
// the host's disk.
const dfStubScript = `#!/bin/sh
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "stub 999999999 1 999999999 1% /"
`

// helmDockerStubScript stands in for docker in the helm rows. Every call is
// logged. It returns output for the calls of a boot, a kept-pair read and the
// slot reads; any other call exits 97. The stdin of each docker exec -i is
// saved as stdin.<n> in STUB_DIR with an index line "<n> <kubectl arguments>",
// and the output of kubectl comes from files in STUB_DIR. The node container's
// state is the file node_state (running, restarting or exited; running when
// absent), returned as Docker returns it: State.Status is the value,
// State.Running is true unless exited and State.Restarting is true only while
// restarting.
const helmDockerStubScript = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
node_state() {
  if [ -s "$STUB_DIR/node_state" ]; then cat "$STUB_DIR/node_state"; else echo running; fi
}
case "$1" in
info) exit 0 ;;
version)
  if [ "$2" = "--format" ]; then printf '%s\n' "$STUB_PLATFORM"; exit 0; fi ;;
ps)
  if [ "$*" = "ps -aq" ] && [ -n "$STUB_PS" ]; then cat "$STUB_PS"; exit 0; fi ;;
inspect)
  if [ "$2" = "-f" ] && [ "$#" -ge 4 ] && [ -n "$STUB_INSPECT" ]; then cat "$STUB_INSPECT"; exit 0; fi
  if [ "$2" = "--format" ]; then
    case "$3" in
    *HostIp*) printf '%s\n' "$STUB_BOUND_IP"; exit 0 ;;
    *State.Status*) node_state; exit 0 ;;
    *State.Running*) if [ "$(node_state)" = exited ]; then echo false; else echo true; fi; exit 0 ;;
    *State.Restarting*) if [ "$(node_state)" = restarting ]; then echo true; else echo false; fi; exit 0 ;;
    *State.ExitCode*) echo 3; exit 0 ;;
    esac
    id=$(awk -v n="$4" '$1 == n {print $2}' "$STUB_DIR/ids")
    if [ -n "$id" ]; then printf '%s\n' "$id"; exit 0; fi
    exit 1
  fi ;;
image)
  if [ "$2" = "inspect" ]; then
    for ref in $STUB_PRESENT; do
      if [ "$3" = "$ref" ]; then exit 0; fi
    done
  fi ;;
pull)
  for ref in $(cat "$STUB_DIR/pull_fail"); do
    if [ "$3" = "$ref" ]; then exit 1; fi
  done
  exit 0 ;;
network)
  case "$2" in
  ls)
    if [ "$3" = "-q" ] && [ -n "$STUB_NETLS" ]; then cat "$STUB_NETLS"; exit 0; fi ;;
  inspect)
    if [ "$3" = "-f" ] && [ "$#" -ge 5 ] && [ -n "$STUB_NETINSPECT" ]; then cat "$STUB_NETINSPECT"; exit 0; fi ;;
  create|rm) exit 0 ;;
  esac ;;
rm) exit 0 ;;
logs) echo "stub node container log line"; exit 0 ;;
run)
  case "$*" in
  "run --rm --entrypoint k3s "*) cat "$STUB_DIR/k3s_version"; exit 0 ;;
  "run -d "*) echo stub-container-id; exit 0 ;;
  esac ;;
exec)
  shift
  stdin=""
  if [ "$1" = "-i" ]; then stdin=1; shift; fi
  shift
  if [ "$1" != kubectl ]; then exit 97; fi
  shift
  if [ -n "$stdin" ]; then
    n=$(( $(cat "$STUB_DIR/stdin_count" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$STUB_DIR/stdin_count"
    cat > "$STUB_DIR/stdin.$n"
    printf '%s\t%s\n' "$n" "$*" >> "$STUB_DIR/stdin_index"
  fi
  case "$*" in
  "get nodes")
    if [ -s "$STUB_DIR/not_ready" ]; then echo "local-node   NotReady   control-plane,etcd,master   1s   v1.36.2+k3s1"; exit 0; fi
    echo "local-node   Ready   control-plane,etcd,master   1s   v1.36.2+k3s1"; exit 0 ;;
  "apply -f -") exit 0 ;;
  "create -f -") exit 0 ;;
  "-n cert-manager rollout status "*) exit 0 ;;
  "get validatingwebhookconfiguration cert-manager-webhook "*) cat "$STUB_DIR/ca_bundle"; exit 0 ;;
  "wait --for=condition=Established crd/issuers.cert-manager.io "*)
    if [ -s "$STUB_DIR/crd_not_established" ]; then exit 1; fi
    exit 0 ;;
  "-n cert-manager get pods "*) cat "$STUB_DIR/cm_imageids"; exit 0 ;;
  "-n cattle-system get deploy rancher -o name")
    if [ -s "$STUB_DIR/deploy_missing" ]; then exit 1; fi
    echo deployment.apps/rancher; exit 0 ;;
  "-n cattle-system rollout status deploy/rancher "*) exit 0 ;;
  "-n cattle-system get pods -l app=rancher "*) cat "$STUB_DIR/pod_lines"; exit 0 ;;
  "-n cattle-system get deploy rancher-webhook "*) cat "$STUB_DIR/webhook_ready"; exit 0 ;;
  esac ;;
esac
exit 97
`

// helmFixture is what the helm rows use: the files the docker stub reads and,
// when serveCharts ran, the chart archive and cert-manager manifest on a local
// HTTP server.
type helmFixture struct {
	dir string
	// server returns the archive and the manifest; nil until serveCharts.
	server      *httptest.Server
	archive     []byte
	manifest    []byte
	chartURL    string
	chartSHA    string
	manifestURL string
	manifestSHA string
	// oracleImage is the server image reference the copy of the script names.
	oracleImage string
	// files are what the docker stub returns, by file name in dir.
	files map[string]string
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// chartValuesYAML is the shape of the chart's values.yaml that the script
// reads: comments, several blocks with a tag key, and the assetsImage key as
// a block under its two comment lines (assets "block"), as an inline map
// (assets "inline") or not at all.
func chartValuesYAML(assets string) string {
	values := `# Default values for rancher.
# This is a YAML-formatted file.

image:
  repository: "rancher/rancher"
  tag: ""
  pullPolicy: IfNotPresent

hostNetwork: false

startupProbe:
  failureThreshold: 12
  periodSeconds: 30

`
	switch assets {
	case "block":
		values += `# Specify the image used to fill the catalog directory of the Rancher pod.
# The init container copies the charts from this image.
assetsImage:
  repository: rancher/rancher-assets
  tag: v2.16-20261027T2136Z-dev

`
	case "inline":
		values += "assetsImage: {repository: rancher/rancher-assets, tag: v2.16-20261027T2136Z-dev}\n\n"
	}
	values += `busyboxImage:
  repository: busybox
  tag: "1.36"
`
	return values
}

// chartArchive builds a chart archive with a Chart.yaml naming appVersion and
// the values of chartValuesYAML.
func chartArchive(t *testing.T, appVersion, assets string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, file := range []struct{ name, body string }{
		{"rancher/Chart.yaml", "apiVersion: v2\nname: rancher\nversion: 2.15.2\nappVersion: " + appVersion + "\nkubeVersion: '< 1.37.0-0'\n"},
		{"rancher/values.yaml", chartValuesYAML(assets)},
		{"rancher/templates/NOTES.txt", "notes\n"},
	} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(file.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// installHelmDocker puts the helm docker stub in place of the plain one and
// creates the files it returns from with the defaults of a healthy boot of
// the pinned image: a Rancher pod that runs it, a k3s binary that matches the
// pin, no ready webhook.
func (s *scenario) installHelmDocker() *helmFixture {
	s.t.Helper()
	f := &helmFixture{dir: filepath.Join(s.stubDir, "helm"), files: map[string]string{}}
	require.NoError(s.t, os.MkdirAll(f.dir, 0o755))
	require.NoError(s.t, os.WriteFile(filepath.Join(s.stubDir, "docker"), []byte(helmDockerStubScript), 0o755))
	f.files["ids"] = ""
	f.files["pull_fail"] = ""
	f.files["k3s_version"] = "k3s version v1.36.2+k3s1 (0000000)\ngo version go1.24.0\n"
	f.files["cm_imageids"] = "quay.io/jetstack/cert-manager-controller@sha256:" + strings.Repeat("c", 64) + "\n" +
		"quay.io/jetstack/cert-manager-cainjector@sha256:" + strings.Repeat("d", 64) + "\n" +
		"quay.io/jetstack/cert-manager-webhook@sha256:" + strings.Repeat("e", 64) + "\n"
	f.files["webhook_ready"] = ""
	f.files["ca_bundle"] = "Y2EgYnVuZGxl"
	s.helm = f
	return f
}

// serveCharts publishes the oracle image of a helm-check row (the combined
// image, or with server set an image of the rancher-server repository),
// starts a local HTTP server that returns a chart archive and the
// cert-manager manifest, and rewrites the pin lines of the script copy to name
// them. It returns the image reference the copy names.
func (s *scenario) serveCharts(appVersion, assets string, server bool) string {
	s.t.Helper()
	f := s.helm
	require.NotNil(s.t, f, "installHelmDocker comes first")
	repo, tag, k3s := "rancher/rancher", "v2.15.2", "v1.36.1+k3s1"
	if server {
		repo, tag, k3s = "rancher/rancher-server", "v2.16.0", "v1.37.1+k3s1"
	}
	image := s.reg.publish(s.t, repo, tag, tag, k3s)
	f.oracleImage = repo + ":" + tag + "@" + image.ListDigest
	if assets != "" {
		s.reg.publish(s.t, "rancher/rancher-assets", "v2.16-20261027T2136Z-dev", "", "")
	}

	f.archive = chartArchive(s.t, appVersion, assets)
	f.manifest = []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: cert-manager\n")
	f.chartSHA = sha256Hex(f.archive)
	f.manifestSHA = sha256Hex(f.manifest)
	mux := http.NewServeMux()
	mux.HandleFunc("/charts/rancher.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.archive) })
	mux.HandleFunc("/cm/cert-manager.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.manifest) })
	f.server = httptest.NewServer(mux)
	s.t.Cleanup(f.server.Close)
	f.chartURL = f.server.URL + "/charts/rancher.tgz"
	f.manifestURL = f.server.URL + "/cm/cert-manager.yaml"

	s.pinRewrites = map[string]string{
		"ORACLE_IMAGE":          f.oracleImage,
		"HELMCHECK_CHART":       f.chartURL + "@sha256:" + f.chartSHA,
		"CERT_MANAGER_MANIFEST": f.manifestURL + "@sha256:" + f.manifestSHA,
	}
	if server {
		s.pinRewrites["HELMCHECK_LOCAL_K3S_IMAGE"] = "rancher/k3s:v1.37.1-k3s1@sha256:" + strings.Repeat("a", 64)
	}
	f.setPod(f.oracleImage, "docker.io/"+repo+"@"+image.ListDigest)
	return f.oracleImage
}

// setPod sets the image and imageID of the Rancher pod the stub reports.
func (f *helmFixture) setPod(image, imageID string) {
	f.files["pod_lines"] = "image=" + image + " id=" + imageID + "\n"
}

// helmEnviron writes the docker stub's files and returns its
// environment, and the curl stub's, when the scenario is a helm row.
func (s *scenario) helmEnviron() []string {
	f := s.helm
	if f == nil {
		return nil
	}
	for name, content := range f.files {
		require.NoError(s.t, os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600))
	}
	env := []string{
		"STUB_DIR=" + f.dir, "STUB_BOUND_IP=127.0.0.1",
		"STUB_LOCK_DIR=" + filepath.Join(s.stateHome, helmCheckName, "lock"),
	}
	if f.server != nil {
		realCurl, err := exec.LookPath("curl")
		if err != nil {
			s.t.Skip("curl is not installed; the rows that download through it cannot run")
		}
		env = append(env, "STUB_HTTP_PREFIX="+f.server.URL, "REAL_CURL="+realCurl)
	}
	return env
}

// canned adds a row to the table the curl stub returns responses from: the
// request path with its query, the status and the body.
func (s *scenario) canned(path string, code int, body string) {
	s.t.Helper()
	file, err := os.OpenFile(filepath.Join(s.stubDir, "curl.table"), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(s.t, err)
	defer func() { require.NoError(s.t, file.Close()) }()
	_, err = fmt.Fprintf(file, "%s\t%d\t%s\n", path, code, body)
	require.NoError(s.t, err)
}

// cannedHealthy adds the responses of a reused, healthy pair.
func (s *scenario) cannedHealthy(downstream string) {
	s.t.Helper()
	s.canned("/ping", 200, "pong")
	s.canned("/v3/users?me=true", 200, "{}")
	s.canned("/v3/clusters/"+downstream, 200, `{"state":"active"}`)
	s.canned("/rancherversion", 200, `{"Version":"v2.15.2","GitCommit":"abc"}`)
	s.canned("/k8s/clusters/"+downstream+"/version", 200, "{}")
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// curlLog is the curl stub's log, one "real|canned|denied <url>" line per
// request.
func (s *scenario) curlLog() []string {
	s.t.Helper()
	return readLines(s.t, filepath.Join(s.stubDir, "curl.log"))
}

// goEnv is the E2E_FIDELITY_* environment the go stub saw, by name, and
// whether it was called at all.
func (s *scenario) goEnv() (map[string]string, bool) {
	s.t.Helper()
	env := map[string]string{}
	called := false
	for _, line := range readLines(s.t, filepath.Join(s.stubDir, "go.log")) {
		if strings.HasPrefix(line, "args ") {
			called = true
		}
		if rest, ok := strings.CutPrefix(line, "env "); ok {
			name, value, _ := strings.Cut(rest, "=")
			env[name] = value
		}
	}
	return env, called
}

// stdinRecord is the stdin of one docker exec -i, with the kubectl arguments
// of its call.
type stdinRecord struct {
	args string
	body []byte
}

// stdins are the stdin of every docker exec -i the stub saw, in call order.
func (s *scenario) stdins() []stdinRecord {
	s.t.Helper()
	if s.helm == nil {
		return nil
	}
	index := filepath.Join(s.helm.dir, "stdin_index")
	if _, err := os.Stat(index); err != nil {
		return nil
	}
	var records []stdinRecord
	for _, line := range readLines(s.t, index) {
		n, args, _ := strings.Cut(line, "\t")
		body, err := os.ReadFile(filepath.Join(s.helm.dir, "stdin."+n))
		require.NoError(s.t, err)
		records = append(records, stdinRecord{args: args, body: body})
	}
	return records
}

// ownPair is the slot stub of a profile whose own pair already exists: its
// container, its downstream and its network are not holders of its slot.
func ownPair(container string, port int, subnet string) slotStub {
	return slotStub{
		ps:         "c1\nc2\n",
		inspect:    fmt.Sprintf("/%s %d \n/%s-k3s \n", container, port, container),
		netLS:      "n1\n",
		netInspect: fmt.Sprintf("%s-net %s \n", container, subnet),
	}
}
