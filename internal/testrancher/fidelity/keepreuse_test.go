package fidelity

import (
	"fmt"
	"maps"
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
)

// keepToken is the admin token every state file of these tests holds. It is
// distinctive so a test can assert that no output ever repeats it.
const keepToken = "keep-token-4f9c2a71"

// keepDockerWrapper stands in for docker in front of the helm docker stub
// (saved as docker-helm). It fails the host address read when STUB_NO_HOSTIP is
// set, which the helm stub never does, and fails every docker run -d when
// STUB_FAIL_RUN is set, so a run that boots a new pair ends right after the
// call. Everything else goes to the stub it wraps.
const keepDockerWrapper = `#!/bin/sh
case "$*" in
*HostIp*)
  if [ -n "$STUB_NO_HOSTIP" ]; then printf '%s\n' "$*" >> "$STUB_LOG"; exit 1; fi ;;
"run -d "*)
  if [ -n "$STUB_FAIL_RUN" ]; then printf '%s\n' "$*" >> "$STUB_LOG"; exit 1; fi ;;
esac
exec "$STUB_REAL_DOCKER" "$@"
`

// keepSlot is one oracle slot of the script: its container, how its test setup
// is built, and the boot the script gives it.
type keepSlot struct {
	name      string
	container string
	kind      string
	minor     string
	subnet    string
	boot      string
}

var keepSlots = []keepSlot{
	{"community", "rancher-cli-e2e-fidelity", "community", "", "", "container"},
	{"prime", "rancher-cli-e2e-fidelity-prime", "prime", "", "", "container"},
	{"2.16", "rancher-cli-e2e-fidelity-v2.16", "upcoming", "2.16", "172.30.3.0/24", "container"},
	{"2.17", "rancher-cli-e2e-fidelity-v2.17", "upcoming", "2.17", "172.30.4.0/24", "container"},
	{"helm-check", helmCheckName, "helm", "", helmCheckSubnet, "helm"},
	{"2.16-helm", upcomingContainerName, "upcominghelm", "2.16", upcomingSubnet, "helm"},
}

var (
	keepTopAssign    = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)="([^"]*)"$`)
	keepContainerRow = regexp.MustCompile(`^\s+CONTAINER="([^"]*)"$`)
	keepBootRow      = regexp.MustCompile(`^\s+BOOT="([^"]*)"$`)
	keepMinorArm     = regexp.MustCompile(`^\s+(\d+\.\d+)\) `)
	keepBootPlain    = regexp.MustCompile(`^\$\{([A-Z_]+)\}$`)
	keepBootDefault  = regexp.MustCompile(`^\$\{([A-Z_]+):-([^}]*)\}$`)
)

func scriptLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(realScriptPath(t))
	require.NoError(t, err)
	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		lines = append(lines, strings.TrimSuffix(line, "\r"))
	}
	return lines
}

// scriptSlotBoots reads the slots of the script's profile block and the boot
// of each: the container name of every arm, expanded for each minor of the
// minor arm, with the arm's BOOT line, or the default BOOT line before the
// block when the arm has none. A value of the form ${NAME} is the top-level
// NAME line; a value of the form ${NAME:-value} is NAME in env when set, else
// value. A boot other than container or helm fails the test.
func scriptSlotBoots(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	lines := scriptLines(t)
	start := slices.Index(lines, `if [ "${E2E_FIDELITY_PRIME:-}" = "1" ]; then`)
	require.GreaterOrEqual(t, start, 0, "the profile block changed shape")
	end := slices.Index(lines[start:], "fi")
	require.GreaterOrEqual(t, end, 0, "the end of the profile block was not found")
	end += start

	top := map[string]string{}
	defaultBoot := ""
	for _, line := range lines[:start] {
		m := keepTopAssign.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, seen := top[m[1]]; !seen {
			top[m[1]] = m[2]
		}
		if m[1] == "BOOT" {
			defaultBoot = m[2]
		}
	}
	require.NotEmpty(t, defaultBoot, "no default BOOT line before the profile block")

	type arm struct {
		container, boot string
		hasBoot         bool
		minors          []string
	}
	var arms []arm
	for _, line := range lines[start : end+1] {
		if strings.HasPrefix(line, "if ") || strings.HasPrefix(line, "elif ") || line == "else" {
			arms = append(arms, arm{})
			continue
		}
		if len(arms) == 0 {
			continue
		}
		cur := &arms[len(arms)-1]
		if m := keepContainerRow.FindStringSubmatch(line); m != nil {
			cur.container = m[1]
		}
		if m := keepBootRow.FindStringSubmatch(line); m != nil {
			cur.boot, cur.hasBoot = m[1], true
		}
		if m := keepMinorArm.FindStringSubmatch(line); m != nil {
			cur.minors = append(cur.minors, m[1])
		}
	}

	resolve := func(value string) string {
		if m := keepBootPlain.FindStringSubmatch(value); m != nil {
			v, ok := top[m[1]]
			require.Truef(t, ok, "no top-level %s line for the boot value %s", m[1], value)
			return v
		}
		if m := keepBootDefault.FindStringSubmatch(value); m != nil {
			if v, ok := env[m[1]]; ok {
				return v
			}
			return m[2]
		}
		return value
	}

	boots := map[string]string{}
	for _, a := range arms {
		require.NotEmptyf(t, a.container, "an arm of the profile block has no CONTAINER line: %+v", a)
		boot := defaultBoot
		if a.hasBoot {
			boot = resolve(a.boot)
		}
		require.Containsf(t, []string{"container", "helm"}, boot, "the boot of %s", a.container)
		names := []string{a.container}
		if strings.Contains(a.container, "${MINOR}") {
			require.NotEmpty(t, a.minors, "the minor arm lists no minor")
			names = nil
			for _, minor := range a.minors {
				names = append(names, strings.ReplaceAll(a.container, "${MINOR}", minor))
			}
		}
		for _, name := range names {
			require.NotContainsf(t, name, "$", "the container name %s is not fully expanded", name)
			boots[name] = boot
		}
	}
	return boots
}

// requireKeepSlotsMatchTheScript asserts that the rows of keepSlots are the
// slots of the script, each with the boot the script gives it, so a slot that
// is added or changes its boot fails until it has rows here.
func requireKeepSlotsMatchTheScript(t *testing.T) {
	t.Helper()
	boots := scriptSlotBoots(t, nil)
	helmMinorBoots := scriptSlotBoots(t, map[string]string{"UPCOMING_BOOT": "helm"})
	var want []string
	for _, slot := range keepSlots {
		if slot.kind != "upcominghelm" {
			want = append(want, slot.container)
		}
	}
	require.ElementsMatch(t, slices.Sorted(maps.Keys(boots)), want, "the slots of the script and the slots of keepSlots differ")
	for _, slot := range keepSlots {
		got := boots
		if slot.kind == "upcominghelm" {
			got = helmMinorBoots
		}
		require.Equalf(t, slot.boot, got[slot.container], "the script gives %s another boot", slot.container)
	}
}

// shellWord is s as the printf %q conversion of bash writes it. bash writes
// it, so the word is the one the script prints whatever characters s holds.
func shellWord(t *testing.T, s string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "bash", "-c", `printf %q "$1"`, "_", s).Output()
	require.NoError(t, err)
	return strings.TrimSuffix(string(out), "\n")
}

// keepRun is one run of the script under E2E_FIDELITY_KEEP=1 against a kept
// pair that is healthy until a case changes it.
type keepRun struct {
	t        *testing.T
	s        *scenario
	slot     keepSlot
	root     string
	port     int
	state    map[string]any
	rawState string
	noState  bool
	noKeep   bool
	ids      string
	ping     string
	// tokenCode is the status the server answers the admin token check with.
	tokenCode string
	// oldImage is an oracle image reference that no run names.
	oldImage string
	before   map[string][]byte
}

func (k *keepRun) base() string      { return fmt.Sprintf("https://127.0.0.1:%d", k.port) }
func (k *keepRun) stateFile() string { return filepath.Join(k.root, "state.json") }

func keepContainerState(oracle, k3s string) map[string]any {
	return map[string]any{
		"containerId": "oracle-id", "adminToken": keepToken, "oracleImage": oracle,
		"k3sContainerId": "k3s-id", "k3sImage": k3s, "downstream": "c-m-abc", "boot": "container",
	}
}

// upcomingHelmKeptState is the state file of a healthy kept pair of the
// upcoming minor booted with helm: the fields write_state records for the
// image, chart and node container image of the scenario newUpcomingHelmScenario
// published.
func upcomingHelmKeptState(s *scenario, u *upcomingFixture) map[string]any {
	state := keepContainerState(s.helm.oracleImage, u.nodeImage)
	state["boot"] = "helm"
	state["chart"] = u.pin
	state["localK3sImage"] = u.nodeImage
	state["localK3sSource"] = "CATTLE_K3S_VERSION"
	state["assetsImage"] = ""
	state["certManager"] = map[string]any{"manifest": s.pinRewrites["CERT_MANAGER_MANIFEST"], "images": []string{}}
	return state
}

func newKeepRun(t *testing.T, slot keepSlot) *keepRun {
	t.Helper()
	return newKeepRunIn(t, slot, "state")
}

// newKeepRunIn is newKeepRun with the state home directory named home, so a
// case can give it a name with a space.
func newKeepRunIn(t *testing.T, slot keepSlot, home string) *keepRun {
	t.Helper()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	moveHome := func(s *scenario) {
		s.stateHome = filepath.Join(filepath.Dir(s.stateHome), home)
		require.NoError(t, os.MkdirAll(s.stateHome, 0o755))
	}
	zeros := strings.Repeat("0", 64)
	k := &keepRun{t: t, slot: slot, ping: "pong", tokenCode: "200", oldImage: "rancher/rancher:v0.0.1@sha256:" + zeros}
	k.ids = slot.container + " oracle-id\n" + slot.container + "-k3s k3s-id\n"
	switch slot.kind {
	case "helm":
		s := newHelmCheckScenario(t, helmRowOptions{})
		moveHome(s)
		k.s = s
		k.port = s.slotScript()
		stub := ownPair(slot.container, k.port, slot.subnet)
		s.slot = &stub
		k.root = s.helmCheckRoot()
		k.state = s.helmState(t)
		k.state["adminToken"] = keepToken
	case "upcominghelm":
		require.Equal(t, "state", home, "the upcoming helm scenario fixes its state home")
		s, u := newUpcomingHelmScenario(t, upcomingOptions{})
		k.s = s
		k.port = s.slotScript()
		stub := ownPair(slot.container, k.port, slot.subnet)
		s.slot = &stub
		k.root = s.upcomingRoot()
		k.state = upcomingHelmKeptState(s, u)
		k.oldImage = "rancher/rancher-server:head@sha256:" + zeros
	case "upcoming":
		s := newScenario(t)
		moveHome(s)
		k.s = s
		s.minor = slot.minor
		image := s.publishDefaults()
		oracle := "rancher/rancher:head@" + image.ListDigest
		s.present = []string{oracle, s.k3sRef}
		k.port = s.slotScript()
		s.installHelmDocker()
		stub := ownPair(slot.container, k.port, slot.subnet)
		s.slot = &stub
		s.env = map[string]string{"E2E_FIDELITY_MINOR": slot.minor}
		k.root = filepath.Join(s.stateHome, slot.container)
		k.state = keepContainerState(oracle, s.k3sRef)
		k.oldImage = "rancher/rancher:head@sha256:" + zeros
	default:
		s := newScenario(t)
		moveHome(s)
		k.s = s
		s.minor = ""
		s.installHelmDocker()
		k.port = s.slotScript()
		oracle := scriptLiteral(t, "ORACLE_IMAGE")
		if slot.kind == "prime" {
			oracle = scriptLiteral(t, "PRIME_ORACLE_IMAGE")
			s.env["E2E_FIDELITY_PRIME"] = "1"
			k.port = 8444
		}
		k.root = filepath.Join(s.stateHome, slot.container)
		k.state = keepContainerState(oracle, scriptLiteral(t, "K3S_IMAGE"))
	}
	realDocker := filepath.Join(k.s.stubDir, "docker-helm")
	require.NoError(t, os.Rename(filepath.Join(k.s.stubDir, "docker"), realDocker))
	testfs.WriteExecutable(t, filepath.Join(k.s.stubDir, "docker"), []byte(keepDockerWrapper))
	k.s.env["STUB_REAL_DOCKER"] = realDocker
	k.s.env["STUB_FAIL_RUN"] = "1"
	return k
}

// canned adds a row to the table the curl stub answers from; the code is
// written as given, so a case can answer 000.
func (k *keepRun) canned(path, code, body string) {
	k.t.Helper()
	file, err := os.OpenFile(filepath.Join(k.s.stubDir, "curl.table"), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(k.t, err)
	_, err = fmt.Fprintf(file, "%s\t%s\t%s\n", path, code, body)
	require.NoError(k.t, err)
	require.NoError(k.t, file.Close())
}

func (k *keepRun) snapshot() map[string][]byte {
	k.t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"state.json", "ca.pem"} {
		raw, err := os.ReadFile(filepath.Join(k.root, name))
		require.NoError(k.t, err)
		out[name] = raw
	}
	return out
}

func (k *keepRun) run() runResult {
	k.t.Helper()
	s := k.s
	switch {
	case k.noState:
	case k.rawState != "":
		require.NoError(k.t, os.MkdirAll(k.root, 0o700))
		require.NoError(k.t, os.WriteFile(k.stateFile(), []byte(k.rawState), 0o600))
		require.NoError(k.t, os.WriteFile(filepath.Join(k.root, "ca.pem"), []byte("-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n"), 0o600))
	default:
		s.writeState(k.root, k.state)
	}
	if !k.noState {
		k.before = k.snapshot()
	}
	s.helm.files["ids"] = k.ids
	k.canned("/ping", "200", k.ping)
	k.canned("/v3/users?me=true", k.tokenCode, "{}")
	k.canned("/v3/clusters/c-m-abc", "200", `{"state":"active"}`)
	k.canned("/rancherversion", "200", `{"Version":"v2.15.2","GitCommit":"abc"}`)
	k.canned("/k8s/clusters/c-m-abc/version", "200", "{}")
	if !k.noKeep {
		s.env["E2E_FIDELITY_KEEP"] = "1"
	}
	return s.run()
}

// keepStop is a way a kept pair fails a check of its own state, and what the
// refusal says about it.
type keepStop struct {
	name  string
	check string
	setup func(k *keepRun)
	// detail is the text after the check name in the first refusal line.
	detail func(k *keepRun) string
	// hint is the extra refusal line a case adds, or empty.
	hint func(k *keepRun) string
	// keptStateFails is true when a helm pair fails the read of its kept state
	// before the lock, so the steps before the lock run before the stop.
	keptStateFails bool
	// unreadablePins is true when the state file cannot give the image to
	// compare, so no pin is compared and a changed pin cannot recreate the pair.
	unreadablePins bool
}

const unnamedDetail = "reuse_state failed at a check that sets no name; this is a defect in scripts/e2e-fidelity"

func keepStopRows() []keepStop {
	missingField := func(field string) func(k *keepRun) string {
		return func(k *keepRun) string {
			return "the state file " + k.stateFile() + " has no " + field
		}
	}
	return []keepStop{
		{
			name: "state file is not JSON", check: "state-file", keptStateFails: true, unreadablePins: true,
			setup:  func(k *keepRun) { k.rawState = "this is not JSON\n" },
			detail: func(k *keepRun) string { return "the state file " + k.stateFile() + " could not be read" },
		},
		{
			name: "state file has a null oracleImage", check: "state-file", keptStateFails: true, unreadablePins: true,
			setup:  func(k *keepRun) { k.state["oracleImage"] = nil },
			detail: missingField("oracleImage"),
		},
		{
			name: "state file has a number for oracleImage", check: "state-file", keptStateFails: true, unreadablePins: true,
			setup:  func(k *keepRun) { k.state["oracleImage"] = 5 },
			detail: missingField("oracleImage"),
		},
		{
			name: "state file has no oracleImage", check: "state-file", keptStateFails: true, unreadablePins: true,
			setup:  func(k *keepRun) { delete(k.state, "oracleImage") },
			detail: missingField("oracleImage"),
		},
		{
			name: "state file has no containerId", check: "state-file", keptStateFails: true,
			setup:  func(k *keepRun) { delete(k.state, "containerId") },
			detail: missingField("containerId"),
		},
		{
			name: "container does not exist", check: "container", keptStateFails: true,
			setup:  func(k *keepRun) { k.ids = "" },
			detail: func(k *keepRun) string { return "container " + k.slot.container + " does not exist" },
		},
		{
			name: "container has another id", check: "container-id", keptStateFails: true,
			setup: func(k *keepRun) {
				k.ids = k.slot.container + " another-id\n" + k.slot.container + "-k3s k3s-id\n"
			},
			detail: func(k *keepRun) string {
				return "container " + k.slot.container + " has id another-id, but the state file records oracle-id"
			},
		},
		{
			name: "container publishes no host address", check: "bind-address",
			setup: func(k *keepRun) { k.s.env["STUB_NO_HOSTIP"] = "1" },
			detail: func(k *keepRun) string {
				return "container " + k.slot.container + " publishes no host address for 443/tcp; it may be stopped or restarting"
			},
		},
		{
			name: "container is published on another address", check: "bind-address",
			setup: func(k *keepRun) { k.s.env["STUB_BOUND_IP"] = "10.1.2.3" },
			detail: func(k *keepRun) string {
				return "container " + k.slot.container + " is published on 10.1.2.3, but this run asks for 127.0.0.1 (E2E_FIDELITY_BIND_ADDR)"
			},
			hint: func(*keepRun) string { return "to reuse the pair as it is, run with E2E_FIDELITY_BIND_ADDR=10.1.2.3" },
		},
		{
			name: "container reports an empty address", check: "bind-address",
			setup: func(k *keepRun) { k.s.env["STUB_BOUND_IP"] = "" },
			detail: func(k *keepRun) string {
				return "container " + k.slot.container + " is published on (none), but this run asks for 127.0.0.1 (E2E_FIDELITY_BIND_ADDR)"
			},
		},
		{
			name: "ping gives no pong", check: "ping",
			setup: func(k *keepRun) { k.ping = "no reply" },
			detail: func(k *keepRun) string {
				return k.base() + "/ping did not answer pong within 5 s; a kept server restarts itself after some feature changes and can take a few minutes to answer"
			},
		},
		{
			name: "state file has no adminToken", check: "state-file",
			setup:  func(k *keepRun) { delete(k.state, "adminToken") },
			detail: missingField("adminToken"),
		},
		{
			name: "admin token is rejected", check: "admin-token",
			setup: func(k *keepRun) { k.tokenCode = "401" },
			detail: func(k *keepRun) string {
				return "the admin token in the state file got HTTP 401 from " + k.base() + "/v3/users?me=true"
			},
		},
		{
			name: "admin token check gets no answer", check: "admin-token",
			setup: func(k *keepRun) { k.tokenCode = "000" },
			detail: func(k *keepRun) string {
				return "the admin token in the state file got HTTP 000 from " + k.base() + "/v3/users?me=true; a kept server restarts itself after some feature changes and can take a few minutes to answer"
			},
		},
		{
			name: "a failure that sets no check", check: "unnamed",
			setup: func(k *keepRun) {
				require.NotEqual(k.t, realScriptPath(k.t), k.s.script, "the case changes the copy of the script only")
				raw, err := os.ReadFile(k.s.script)
				require.NoError(k.t, err)
				const anchor = "  REUSE_FAILED_HINT=\"\"\n"
				text := string(raw)
				require.Equal(k.t, 1, strings.Count(text, anchor))
				text = strings.Replace(text, anchor, anchor+"  [ -n \"\" ] || return 1\n", 1)
				testfs.WriteExecutable(k.t, k.s.script, []byte(text))
			},
			detail: func(*keepRun) string { return unnamedDetail },
		},
	}
}

// TestKeepRunStopsOnAFailedPairCheck asserts that under E2E_FIDELITY_KEEP=1 a
// kept pair that fails its container, container id, bind address, /ping, admin
// token or state file check stops the run with exit 4 on every slot: the
// refusal names the failed check and the commands that remove the pair, and
// the run removes nothing, boots nothing, writes no state and sends no request
// beyond the checks themselves (and, on a helm slot whose kept state cannot be
// read, the downloads before the lock; an upcoming minor booted with helm
// always downloads its chart before the lock). A failure that sets no check
// name stops the run too.
func TestKeepRunStopsOnAFailedPairCheck(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		for _, row := range keepStopRows() {
			t.Run(slot.name+"/"+row.name, func(t *testing.T) {
				k := newKeepRun(t, slot)
				row.setup(k)
				res := k.run()

				require.NotContains(t, res.stdout, keepToken, "the admin token must never be printed")
				require.NotContains(t, res.stderr, keepToken, "the admin token must never be printed")
				require.Equalf(t, 4, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
				c := slot.container
				block := []string{
					fmt.Sprintf("error: E2E_FIDELITY_KEEP=1: the kept pair %s on port %d failed its %s check: %s; nothing was removed", c, k.port, row.check, row.detail(k)),
					fmt.Sprintf("error: with E2E_FIDELITY_KEEP=1 a kept pair whose container, container id, bind address, /ping, admin token or state file check fails is never removed. Find out why (docker ps -a --filter name=%s; docker logs --tail 50 %s) and run again.", c, c),
				}
				if row.hint != nil {
					block = append(block, "error: "+row.hint(k))
				}
				if row.check == "state-file" {
					block = append(block, "error: "+row.detail(k)+"; remove the pair with:")
				} else {
					block = append(block, "error: to boot a new pair instead, remove this one first, then run again:")
				}
				block = append(block,
					"error:   docker rm -f -v "+shellWord(t, c)+" "+shellWord(t, c+"-k3s"),
					"error:   docker network rm "+shellWord(t, c+"-net"),
					"error:   rm -rf "+shellWord(t, k.root),
				)
				driftSlot := slot.kind == "upcoming" || slot.kind == "helm" || slot.kind == "upcominghelm"
				if driftSlot {
					block = append(block, "error: this also deletes drift reports under "+k.root)
				}
				require.Contains(t, res.stderr, strings.Join(block, "\n")+"\n")
				if !driftSlot {
					assert.NotContains(t, res.stderr, "drift reports", "a slot that allows no drift run has no drift reports")
				}
				if row.hint == nil {
					assert.NotContains(t, res.stderr, "to reuse the pair as it is", "only an address that differs offers to reuse it")
				}

				for _, line := range res.log {
					for _, prefix := range []string{"rm ", "network rm", "network create", "run -d", "exec -i", "container rm"} {
						assert.Falsef(t, strings.HasPrefix(line, prefix), "docker call %q must not happen: %v", line, res.log)
					}
				}
				helmBoot := slot.kind == "helm" || slot.kind == "upcominghelm"
				downloads := helmBoot && row.keptStateFails
				// A kept pair of the upcoming minor booted with helm downloads the
				// chart before the lock to compare its name with the state file.
				chartAlways := slot.kind == "upcominghelm"
				for _, line := range k.s.curlLog() {
					if rest, ok := strings.CutPrefix(line, "canned "); ok {
						assert.Containsf(t, []string{k.base() + "/ping", k.base() + "/v3/users?me=true"}, rest, "the curl log: %v", k.s.curlLog())
						continue
					}
					chartOnly := chartAlways && line == "real "+k.s.helm.chartURL
					assert.Truef(t, chartOnly || downloads && (line == "real "+k.s.helm.chartURL || line == "real "+k.s.helm.manifestURL),
						"request %q must not happen: %v", line, k.s.curlLog())
				}
				if downloads {
					assert.Contains(t, k.s.curlLog(), "real "+k.s.helm.chartURL)
					assert.Contains(t, k.s.curlLog(), "real "+k.s.helm.manifestURL)
				}
				_, called := k.s.goEnv()
				assert.False(t, called, "no test run starts after a refusal")
				assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file are not written")
				assert.NotContains(t, res.stdout, "reusing healthy kept container")
				assert.NotContains(t, res.stdout, "recreating")
			})
		}
	}
}

// TestKeepRefusalCommandsRunVerbatim asserts that the commands the refusal
// prints name each path and container as one shell word, so a state home with
// a space, a single quote, a dollar sign and a backtick in its name is copied
// and run as printed: the shell reads the printed words back to the real
// names.
func TestKeepRefusalCommandsRunVerbatim(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	for _, slot := range []keepSlot{keepSlots[0], keepSlots[4]} {
		t.Run(slot.name, func(t *testing.T) {
			k := newKeepRunIn(t, slot, "state it's a $HOME `home`")
			require.Contains(t, k.root, " ")
			k.ids = ""
			res := k.run()
			require.Equalf(t, 4, res.code, "stderr: %s", res.stderr)

			c := slot.container
			want := map[string][]string{
				"error:   docker rm -f -v ":   {"docker", "rm", "-f", "-v", c, c + "-k3s"},
				"error:   docker network rm ": {"docker", "network", "rm", c + "-net"},
				"error:   rm -rf ":            {"rm", "-rf", k.root},
			}
			for prefix, words := range want {
				var line string
				for l := range strings.SplitSeq(res.stderr, "\n") {
					if strings.HasPrefix(l, prefix) {
						line = l
					}
				}
				require.NotEmptyf(t, line, "no line starting with %q in: %s", prefix, res.stderr)
				command := strings.TrimPrefix(line, "error:   ")
				read, err := exec.CommandContext(t.Context(), "bash", "-c", `for w in `+command+`; do printf '%s\n' "$w"; done`).Output()
				require.NoError(t, err)
				assert.Equal(t, words, strings.Split(strings.TrimSuffix(string(read), "\n"), "\n"), line)
			}
		})
	}
}

// requireRecreated asserts that the run removed the pair and went on to boot a
// new one: the removal of both containers, the network removal and the start
// of the oracle container, in that order, with no refusal.
func requireRecreated(t *testing.T, k *keepRun, res runResult) {
	t.Helper()
	c := k.slot.container
	require.NotEqualf(t, 4, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
	assert.NotContains(t, res.stderr, "failed its")
	requireOrder(t, res.log, "rm -f -v "+c+"-k3s "+c, "network rm "+c+"-net", "run -d --name "+c)
}

// pinChange is a change of the state file that makes one of the pair's pins
// differ from the run's.
type pinChange struct {
	name   string
	slots  []string
	change func(k *keepRun)
	line   string
	// computed is true for a field that a run compares only when it computed
	// the value itself, which a run does when the kept state cannot be reused
	// before the lock.
	computed bool
}

func keepPinChanges() []pinChange {
	zeros := strings.Repeat("0", 64)
	helmOnly := []string{"helm-check"}
	return []pinChange{
		{"oracleImage", []string{"community", "prime", "2.16", "2.17", "helm-check", "2.16-helm"}, func(k *keepRun) { k.state["oracleImage"] = k.oldImage },
			"==> kept container was created from a different oracle image; recreating", false},
		{"boot", []string{"community"}, func(k *keepRun) { k.state["boot"] = "helm" },
			"==> kept pair was created with a different boot; recreating", false},
		{"boot", helmOnly, func(k *keepRun) { k.state["boot"] = "container" },
			"==> kept pair was created with a different boot; recreating", false},
		{"chart", helmOnly, func(k *keepRun) { k.state["chart"] = "https://example.com/rancher-2.14.0.tgz@sha256:" + zeros },
			"==> kept pair was created from different chart, k3s or cert-manager pins; recreating", false},
		{"localK3sImage", helmOnly, func(k *keepRun) { k.state["localK3sImage"] = "rancher/k3s:v1.36.1-k3s1@sha256:" + zeros },
			"==> kept pair was created from different chart, k3s or cert-manager pins; recreating", false},
		{"certManager.manifest", helmOnly, func(k *keepRun) {
			k.state["certManager"] = map[string]any{"manifest": "https://example.com/cert-manager.yaml@sha256:" + zeros, "images": []string{}}
		}, "==> kept pair was created from different chart, k3s or cert-manager pins; recreating", false},
		{"assetsImage", helmOnly, func(k *keepRun) { k.state["assetsImage"] = "other@sha256:" + zeros },
			"==> kept pair has a different assets image or a different source of its k3s version (the server image or CATTLE_K3S_VERSION); recreating", true},
		{"localK3sSource", helmOnly, func(k *keepRun) { k.state["localK3sSource"] = "CATTLE_K3S_VERSION" },
			"==> kept pair has a different assets image or a different source of its k3s version (the server image or CATTLE_K3S_VERSION); recreating", true},
	}
}

// TestKeepRunRecreatesOnAChangedPinOrNoState asserts what stays as it was
// under E2E_FIDELITY_KEEP=1: a kept pair whose pin differs from the run's (an
// image, including a moved upcoming-minor head, the boot, or a helm pair's
// chart, node k3s image, cert-manager manifest, assets image or source of the
// k3s version) is removed and booted again, and so is a run with no state
// file. A changed pin recreates the pair whatever else is wrong with it: the
// pin checks come before the container, bind address, /ping and token checks,
// and the state file needs only a readable image to compare. A moved k3s
// image goes to the downstream rebuild and removes neither the oracle
// container, the network nor the state file. A pair that is not a helm pair
// compares no helm field.
func TestKeepRunRecreatesOnAChangedPinOrNoState(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)

	t.Run("a changed pin recreates the pair", func(t *testing.T) {
		for _, slot := range keepSlots {
			for _, pin := range keepPinChanges() {
				if !slices.Contains(pin.slots, slot.name) || pin.computed {
					continue
				}
				t.Run(slot.name+"/"+pin.name, func(t *testing.T) {
					k := newKeepRun(t, slot)
					pin.change(k)
					res := k.run()
					assert.Contains(t, res.stdout, pin.line)
					requireRecreated(t, k, res)
				})
			}
		}
	})

	t.Run("no state file creates a new pair", func(t *testing.T) {
		for _, slot := range keepSlots {
			t.Run(slot.name, func(t *testing.T) {
				k := newKeepRun(t, slot)
				k.noState = true
				res := k.run()
				assert.Contains(t, res.stdout, "==> no state file at "+k.stateFile()+"; booting a new pair")
				requireRecreated(t, k, res)
			})
		}
	})

	t.Run("a changed pin recreates the pair whatever else fails", func(t *testing.T) {
		for _, slot := range keepSlots {
			if slot.name != "community" && slot.name != "helm-check" {
				continue
			}
			for _, row := range keepStopRows() {
				if row.unreadablePins || row.check == "unnamed" {
					continue
				}
				t.Run(slot.name+"/"+row.name, func(t *testing.T) {
					k := newKeepRun(t, slot)
					row.setup(k)
					k.state["oracleImage"] = k.oldImage
					res := k.run()
					assert.Contains(t, res.stdout, "==> kept container was created from a different oracle image; recreating")
					requireRecreated(t, k, res)
				})
			}
		}
	})

	t.Run("every kind of changed pin recreates a pair whose container is missing", func(t *testing.T) {
		for _, slot := range keepSlots {
			for _, pin := range keepPinChanges() {
				if !slices.Contains(pin.slots, slot.name) {
					continue
				}
				t.Run(slot.name+"/"+pin.name, func(t *testing.T) {
					k := newKeepRun(t, slot)
					pin.change(k)
					k.ids = ""
					res := k.run()
					assert.Contains(t, res.stdout, pin.line)
					requireRecreated(t, k, res)
				})
			}
		}
	})

	t.Run("a state file with no readable image stops the run before any pin is compared", func(t *testing.T) {
		for _, slot := range keepSlots {
			if slot.name != "community" && slot.name != "helm-check" {
				continue
			}
			t.Run(slot.name, func(t *testing.T) {
				k := newKeepRun(t, slot)
				delete(k.state, "oracleImage")
				k.state["boot"] = map[string]string{"community": "helm", "helm-check": "container"}[slot.name]
				res := k.run()
				require.Equalf(t, 4, res.code, "stderr: %s", res.stderr)
				assert.Contains(t, res.stderr, "failed its state-file check: the state file "+k.stateFile()+" has no oracleImage")
				assert.False(t, hasCall(res.log, "rm "), "nothing is removed: %v", res.log)
			})
		}
	})

	t.Run("a moved k3s image rebuilds the downstream and leaves the rest", func(t *testing.T) {
		k := newKeepRun(t, keepSlots[0])
		k.state["k3sImage"] = "rancher/k3s:v0.0.1@sha256:" + strings.Repeat("0", 64)
		k.canned("/v1/provisioning.cattle.io.clusters/fleet-default/downstream", "404", "{}")
		res := k.run()

		assert.Contains(t, res.stdout, "==> reusing healthy kept container")
		assert.Contains(t, res.stdout, "==> kept downstream was created from a different k3s image; rebuilding it")
		assert.Contains(t, res.stdout, "==> kept downstream is unhealthy; rebuilding it against the running oracle")
		assert.NotContains(t, res.stderr, "failed its")
		for _, line := range res.log {
			assert.Falsef(t, strings.HasPrefix(line, "network rm"), "the network stays: %v", res.log)
			if strings.HasPrefix(line, "rm ") {
				assert.Equalf(t, "rm -f -v "+k.slot.container+"-k3s", line, "only the downstream container is removed: %v", res.log)
			}
		}
		assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file stay")
	})

	t.Run("a pair that is not a helm pair compares no helm field", func(t *testing.T) {
		k := newKeepRun(t, keepSlots[0])
		k.state["chart"] = "left over from another pin"
		k.state["localK3sImage"] = "left over"
		k.state["assetsImage"] = "left over"
		k.state["localK3sSource"] = "left over"
		k.state["certManager"] = map[string]any{"manifest": "left over", "images": []string{}}
		res := k.run()

		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		assert.Contains(t, res.stdout, "==> reusing healthy kept container")
		for _, prefix := range []string{"rm ", "run ", "network rm"} {
			assert.Falsef(t, hasCall(res.log, prefix), "a reused pair makes no %q call: %v", prefix, res.log)
		}
	})
}

// TestNoKeepRunStillRemovesAndBoots asserts that a run without
// E2E_FIDELITY_KEEP=1 never checks a kept pair: it removes the pair and boots
// a new one, whatever state the pair is in, with no refusal.
func TestNoKeepRunStillRemovesAndBoots(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	k := newKeepRun(t, keepSlots[0])
	k.noKeep = true
	k.ping = "no reply"
	res := k.run()

	requireRecreated(t, k, res)
	assert.NotContains(t, res.stdout, "reusing healthy kept container")
	assert.Empty(t, k.s.curlLog(), "no request is sent to the kept pair")
}

// TestKeepRunReusesAHealthyPairWithoutRemoval asserts that a healthy kept pair
// is reused on every slot, the smoke slots included: the run reaches its end
// (the bootstrap smoke on a smoke slot, the test run on the community slot)
// and makes no removal, no docker run or start, and no network creation or
// removal.
func TestKeepRunReusesAHealthyPairWithoutRemoval(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		t.Run(slot.name, func(t *testing.T) {
			k := newKeepRun(t, slot)
			res := k.run()

			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Contains(t, res.stdout, "==> reusing healthy kept container")
			if slot.name == "community" {
				_, called := k.s.goEnv()
				assert.True(t, called, "the test run starts after the reuse")
			} else {
				assert.Contains(t, res.stdout, "oracle bootstrap smoke passed")
			}
			for _, prefix := range []string{"rm ", "run ", "start ", "network rm", "network create", "container rm"} {
				assert.Falsef(t, hasCall(res.log, prefix), "a reused pair makes no %q call: %v", prefix, res.log)
			}
			assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file stay")
		})
	}
}

// functionBody is the lines of a top-level function of the script, from its
// first line to its closing brace.
func functionBody(t *testing.T, name string) []string {
	t.Helper()
	var lines []string
	for line := range strings.SplitSeq(strings.TrimRight(shellFunction(t, name), "\n"), "\n") {
		lines = append(lines, strings.TrimSuffix(line, "\r"))
	}
	require.GreaterOrEqual(t, len(lines), 3)
	require.Equal(t, name+"() {", lines[0])
	require.Equal(t, "}", lines[len(lines)-1])
	return lines
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// TestReuseStateFailuresSetAReason asserts, from the text of reuse_state, that
// no failure leaves it without saying which kind it is: it starts by clearing
// its result variables, every return of a failure sits in a group that either
// allows a new pair or names the failed check and its detail, no check fails
// by a bare test or by a trailing "|| return 1", and it ends with an explicit
// success. The check names it sets are the ones the refusal tests cover. The
// refusal function ends the run with exit 4 and never returns.
func TestReuseStateFailuresSetAReason(t *testing.T) {
	lines := functionBody(t, "reuse_state")
	body := lines[1 : len(lines)-1]

	require.GreaterOrEqual(t, len(body), 5)
	for i, want := range []string{`REUSE_RECREATE=""`, `REUSE_FAILED_CHECK=""`, `REUSE_FAILED_DETAIL=""`, `REUSE_FAILED_HINT=""`} {
		assert.Equal(t, want, strings.TrimSpace(body[i]), "the first statements clear the result variables")
	}
	var last string
	for _, line := range body {
		if strings.TrimSpace(line) != "" {
			last = strings.TrimSpace(line)
		}
	}
	assert.Equal(t, "return 0", last, "the last statement is an explicit success")

	orReturn := regexp.MustCompile(`\|\|\s*return 1`)
	names := map[string]bool{}
	setsCheck := regexp.MustCompile(`REUSE_FAILED_CHECK="([^"]+)"`)
	for i, line := range lines {
		assert.Falsef(t, orReturn.MatchString(line), "a check fails with a bare \"|| return 1\" at line %d: %s", i, line)
		if m := setsCheck.FindStringSubmatch(line); m != nil {
			names[m[1]] = true
		}
		if strings.TrimSpace(line) != "return 1" {
			continue
		}
		opener := -1
		for j := i - 1; j >= 0; j-- {
			if indentOf(lines[j]) < indentOf(line) {
				opener = j
				break
			}
		}
		require.Greaterf(t, opener, 0, "a return 1 outside any group at line %d", i)
		closer := -1
		for j := i + 1; j < len(lines); j++ {
			text := strings.TrimSpace(lines[j])
			if indentOf(lines[j]) == indentOf(lines[opener]) && (strings.HasPrefix(text, "}") || text == "fi") {
				closer = j
				break
			}
		}
		require.Greaterf(t, closer, i, "no end of the group of the return 1 at line %d", i)
		group := strings.Join(lines[opener:closer+1], "\n")
		recreates := strings.Contains(group, "REUSE_RECREATE=")
		stops := strings.Contains(group, "REUSE_FAILED_CHECK=") && strings.Contains(group, "REUSE_FAILED_DETAIL=")
		assert.Truef(t, recreates || stops, "the group of the return 1 at line %d sets neither a reason to recreate nor a failed check and its detail:\n%s", i, group)
	}

	var want []string
	for _, row := range keepStopRows() {
		if row.check != "unnamed" {
			want = append(want, row.check)
		}
	}
	assert.ElementsMatch(t, slices.Compact(slices.Sorted(slices.Values(want))), slices.Sorted(maps.Keys(names)), "the check names reuse_state sets and the names the refusal tests cover")

	refuse := functionBody(t, "refuse_reuse")
	var refuseLast string
	for _, line := range refuse[1 : len(refuse)-1] {
		if strings.TrimSpace(line) != "" {
			refuseLast = strings.TrimSpace(line)
		}
	}
	assert.Equal(t, "exit 4", refuseLast, "the refusal ends the run with exit 4")
	noReturn := regexp.MustCompile(`\breturn\b`)
	for _, line := range refuse {
		assert.Falsef(t, noReturn.MatchString(line), "the refusal never returns: %s", line)
	}
}

// TestPairRemovalsHappenOnlyAtTheKnownSites asserts that every command of the
// script that removes part of a kept pair (a container, the network, the state
// file or state root, or the imported cluster) sits at a site that is allowed
// to: the cleanup of a run without E2E_FIDELITY_KEEP=1, the fresh boot after
// the refusal call, or one of the sites in the list below. A removal anywhere
// else fails: in the fresh boot before the refusal call, in any code that runs
// after the fresh boot (it also runs for a reused pair), or in a function that
// is not listed. Docker volume and prune commands and the removal of the CA
// file count as removals.
func TestPairRemovalsHappenOnlyAtTheKnownSites(t *testing.T) {
	lines := scriptLines(t)
	removal := regexp.MustCompile(`\bdocker\s+(container\s+|network\s+|volume\s+)?rm\b|\bdocker\s+(container|system|network|volume)\s+prune\b|-X\s+DELETE|\brm\b.*\$\{(STATE_FILE|STATE_ROOT|CA_FILE)\}`)
	funcStart := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\) \{$`)

	// printed lists the lines that name a removal command in text only. Every
	// other line that matches is a command.
	printed := map[string]string{
		`printf 'error:   docker rm -f -v %q %q\n' "${CONTAINER}" "${K3S_CONTAINER}"`: "the refusal prints the command that removes the pair",
		`printf 'error:   docker network rm %q\n' "${NETWORK}"`:                       "the refusal prints the command that removes the network",
		`printf 'error:   rm -rf %q\n' "${STATE_ROOT}"`:                               "the refusal prints the command that removes the state root",
	}
	printed[`echo "error: the suite was renamed from e2e-real to e2e-fidelity; remove them (docker rm -f -v, docker network rm) together with the old state root ${STATE_ROOT/e2e-fidelity/e2e-real}, or rename them, then rerun" >&2`] = "the refusal for a pair from before the rename names the commands in text"
	printedUsed := map[string]bool{}
	allowed := map[string]string{
		"boot_k3s":           "removes the downstream container before a new one; its callers are the fresh boot and rebuild_downstream",
		"rebuild_downstream": "out of scope: a failed downstream check rebuilds the downstream, which the rule leaves as it is",
	}
	used := map[string]bool{}

	callLine := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "refuse_reuse" {
			require.Equal(t, -1, callLine, "the refusal is called once")
			callLine = i
		}
	}
	require.GreaterOrEqual(t, callLine, 0, "the fresh boot calls the refusal")
	require.Contains(t, lines[callLine-1], `"${E2E_FIDELITY_KEEP:-}" = "1"`, "the refusal call is gated on E2E_FIDELITY_KEEP=1")

	// The fresh boot is the else branch of the reuse test; its removals sit
	// between the else and the fi that closes the branch.
	reuseIf := slices.Index(lines, `if [ "${E2E_FIDELITY_KEEP:-}" = "1" ] && reuse_state; then`)
	require.GreaterOrEqual(t, reuseIf, 0, "the reuse test of the fresh boot changed shape")
	elseLine := slices.Index(lines[reuseIf:], "else") + reuseIf
	fiLine := slices.Index(lines[reuseIf:], "fi") + reuseIf
	require.Less(t, reuseIf, elseLine)
	require.Less(t, elseLine, callLine)
	require.Less(t, callLine, fiLine)

	owner := ""
	cleanupSites, freshBootSites := 0, 0
	for i, line := range lines {
		if m := funcStart.FindStringSubmatch(line); m != nil {
			owner = m[1]
		} else if line == "}" {
			owner = ""
		}
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "#") || !removal.MatchString(text) {
			continue
		}
		if _, ok := printed[text]; ok {
			printedUsed[text] = true
			continue
		}
		switch {
		case owner == "cleanup":
			gated := false
			for j := i - 1; j >= 0; j-- {
				if indentOf(lines[j]) < indentOf(line) {
					opener := strings.TrimSpace(lines[j])
					gated = strings.Contains(opener, `"${E2E_FIDELITY_KEEP:-}" != "1"`) && strings.HasSuffix(opener, "then")
					break
				}
			}
			assert.Truef(t, gated, "line %d removes part of the pair in cleanup outside the block that a kept pair skips: %s", i+1, text)
			cleanupSites++
		case allowed[owner] != "":
			used[owner] = true
		case owner == "":
			assert.Truef(t, i > callLine && i < fiLine, "line %d removes part of the pair outside the fresh boot after the refusal call: %s", i+1, text)
			freshBootSites++
		default:
			assert.Failf(t, "removal at an unknown site", "line %d in %s: %s", i+1, owner, text)
		}
	}
	assert.GreaterOrEqual(t, cleanupSites, 3, "cleanup removes both containers, the network and the state file")
	assert.GreaterOrEqual(t, freshBootSites, 3, "the fresh boot removes both containers, the network and the state file")
	for name := range allowed {
		assert.Truef(t, used[name], "the allowed site %s has no removal; remove it from the list", name)
	}
	for text := range printed {
		assert.Truef(t, printedUsed[text], "the printed line has no match in the script; remove it from the list: %s", text)
	}

	// The allowed functions run only where they are meant to: boot_k3s from
	// rebuild_downstream and from the fresh boot, rebuild_downstream from the
	// one line in the reuse branch.
	bootCall := regexp.MustCompile(`\bboot_k3s\b`)
	rebuildCall := regexp.MustCompile(`\brebuild_downstream\b`)
	bootCalls, rebuildCalls := 0, 0
	owner = ""
	for i, line := range lines {
		if m := funcStart.FindStringSubmatch(line); m != nil {
			owner = m[1]
			continue
		} else if line == "}" {
			owner = ""
		}
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "#") {
			continue
		}
		if bootCall.MatchString(text) {
			bootCalls++
			assert.Truef(t, owner == "rebuild_downstream" || owner == "" && i > callLine && i < fiLine,
				"line %d calls boot_k3s outside rebuild_downstream and the fresh boot: %s", i+1, text)
		}
		if rebuildCall.MatchString(text) {
			rebuildCalls++
			assert.Truef(t, owner == "" && text == "reuse_downstream || rebuild_downstream" && i > reuseIf && i < elseLine,
				"line %d calls rebuild_downstream anywhere but the one line of the reuse branch: %s", i+1, text)
		}
	}
	assert.Equal(t, 2, bootCalls, "boot_k3s has two call sites")
	assert.Equal(t, 1, rebuildCalls, "rebuild_downstream has one call site")
}
