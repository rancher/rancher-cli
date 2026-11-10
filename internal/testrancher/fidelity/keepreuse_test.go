package fidelity

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
// call. It also answers a docker container inspect with success, as a
// container that exists, when STUB_PRE_RENAME is set. Everything else goes to
// the stub it wraps.
const keepDockerWrapper = `#!/bin/sh
case "$*" in
"container inspect "*)
  if [ -n "$STUB_PRE_RENAME" ]; then printf '%s\n' "$*" >> "$STUB_LOG"; exit 0; fi ;;
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

// keepFindOutWhyLine is the second line of the refusal: the checks it names and
// the commands that show why, which name the oracle container for a check of
// the oracle and the downstream container for a check of the downstream.
func keepFindOutWhyLine(container, check string) string {
	named := container
	if slices.Contains([]string{"k3s-container", "k3s-container-id", "downstream"}, check) {
		named = container + "-k3s"
	}
	return fmt.Sprintf("error: with E2E_FIDELITY_KEEP=1 a kept pair whose container, container id, bind address, /ping, admin token, state file, k3s container, k3s container id or downstream cluster check fails is never removed. Find out why (docker ps -a --filter name=%s; docker logs --tail 50 %s) and run again.", named, named)
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

const unnamedDetail = "reuse_state or reuse_downstream failed at a check that sets no name; this is a defect in scripts/e2e-fidelity"

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
				const anchor = "reuse_state() {\n  REUSE_RECREATE=\"\"\n"
				text := string(raw)
				require.Equal(k.t, 1, strings.Count(text, anchor))
				text = strings.Replace(text, anchor, anchor+"  [ -n \"\" ] || return 1\n", 1)
				testfs.WriteExecutable(k.t, k.s.script, []byte(text))
			},
			detail: func(*keepRun) string { return unnamedDetail },
		},
	}
}

// keepFailureRows names one failure row for each kind of failure that depends
// on where the check runs: a state file check, which a helm pair fails before the
// lock; another check that a helm pair fails before the lock; and a check after
// the lock that adds a hint line to the refusal.
var keepFailureRows = []string{
	"state file has no containerId",
	"container does not exist",
	"container is published on another address",
}

// keepFullSlots are the slots that run every failure row. One is a container
// boot with no drift run and one is a helm boot with a drift run, so every row
// meets each boot and the drift reports line.
var keepFullSlots = []string{"community", "helm-check"}

// keepOtherSlots are the slots that run only the rows of keepFailureRows.
var keepOtherSlots = []string{"prime", "2.16", "2.17", "2.16-helm"}

// keepDriftSlot reports whether the slot allows a drift run, so its refusal
// names the drift reports it deletes.
func keepDriftSlot(slot keepSlot) bool {
	return slot.kind == "upcoming" || slot.kind == "helm" || slot.kind == "upcominghelm"
}

// keepStopCase is one failure row run on one slot.
type keepStopCase struct {
	slot keepSlot
	row  keepStop
}

// keepStopCases lists the cases of TestKeepRunStopsOnAFailedPairCheck: every
// row on each slot of keepFullSlots and the rows of keepFailureRows on each slot
// of keepOtherSlots. It fails the test when a row of keepFailureRows is missing
// from keepStopRows or no longer has the properties that make it the row for
// its kind of failure, when keepFullSlots lacks a container boot without a
// drift run or a helm boot with one, and when a slot of keepSlots gets fewer
// cases than it needs.
func keepStopCases(t *testing.T) []keepStopCase {
	t.Helper()
	require.Len(t, keepFailureRows, 3, "one row for each of the three kinds of failure")
	rows := keepStopRows()
	var named []keepStop
	for _, name := range keepFailureRows {
		i := slices.IndexFunc(rows, func(r keepStop) bool { return r.name == name })
		require.GreaterOrEqualf(t, i, 0, "keepStopRows has no row named %q", name)
		named = append(named, rows[i])
	}
	require.True(t, named[0].check == "state-file" && named[0].keptStateFails && !named[0].unreadablePins, "%q is the state file row that a helm pair fails before the lock", named[0].name)
	require.True(t, named[1].check != "state-file" && named[1].keptStateFails, "%q is the other row that a helm pair fails before the lock", named[1].name)
	require.True(t, named[2].hint != nil && !named[2].keptStateFails, "%q is the row after the lock with a hint line", named[2].name)

	var fullBoots []string
	for _, name := range keepFullSlots {
		i := slices.IndexFunc(keepSlots, func(slot keepSlot) bool { return slot.name == name })
		require.GreaterOrEqualf(t, i, 0, "keepSlots has no slot named %q", name)
		slot := keepSlots[i]
		fullBoots = append(fullBoots, slot.boot+" boot, drift run: "+strconv.FormatBool(keepDriftSlot(slot)))
	}
	require.Contains(t, fullBoots, "container boot, drift run: false", "keepFullSlots holds a container boot with no drift run")
	require.Contains(t, fullBoots, "helm boot, drift run: true", "keepFullSlots holds a helm boot with a drift run")

	var cases []keepStopCase
	for _, slot := range keepSlots {
		switch {
		case slices.Contains(keepFullSlots, slot.name):
			for _, row := range rows {
				cases = append(cases, keepStopCase{slot, row})
			}
		case slices.Contains(keepOtherSlots, slot.name):
			for _, row := range named {
				cases = append(cases, keepStopCase{slot, row})
			}
		}
	}

	perSlot := map[string][]string{}
	for _, tc := range cases {
		perSlot[tc.slot.name] = append(perSlot[tc.slot.name], tc.row.name)
	}
	for _, slot := range keepSlots {
		for _, name := range keepFailureRows {
			assert.Containsf(t, perSlot[slot.name], name, "slot %s gets no case for the row %q", slot.name, name)
		}
	}
	for _, name := range keepFullSlots {
		var all []string
		for _, row := range rows {
			all = append(all, row.name)
		}
		assert.Equalf(t, all, perSlot[name], "slot %s runs every row", name)
	}
	return cases
}

// TestKeepRunStopsOnAFailedPairCheck asserts that under E2E_FIDELITY_KEEP=1 a
// kept pair that fails its container, container id, bind address, /ping, admin
// token or state file check stops the run with exit 4: the refusal names the
// failed check and the commands that remove the pair, and the run removes
// nothing, boots nothing, writes no state and sends no request beyond the
// checks themselves (and, on a helm slot whose kept state cannot be read, the
// downloads before the lock; an upcoming minor booted with helm always
// downloads its chart before the lock). A failure that sets no check name stops
// the run too.
//
// Each failure row runs on the community slot and on helm-check. Every other
// slot runs one row for each kind of failure: the state file, the container,
// and the bind address with its hint. That covers the other slots because the
// checks depend on the slot only through the boot, the drift reports line, the
// chart, k3s and cert-manager pins, the oracle image, and the URL and paths
// derived from the port, state root and container name:
//   - the boot and the drift reports line differ between the two full slots;
//   - the two rows that fail before the lock run on both helm slots, which
//     covers the values a helm slot computes before that point;
//   - the pins and the oracle image of every slot are compared in
//     TestKeepRunRecreatesOnAChangedPinOrNoState and
//     TestKeepRunReusesAHealthyPairWithoutRemoval;
//   - the URL and the CA file of the /ping and admin token checks are used on
//     every slot by TestKeepRunReusesAHealthyPairWithoutRemoval, whose success
//     path runs both checks;
//   - the names and paths in the refusal lines are asserted on every case.
func TestKeepRunStopsOnAFailedPairCheck(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, tc := range keepStopCases(t) {
		slot, row := tc.slot, tc.row
		t.Run(slot.name+"/"+row.name, func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, slot)
			row.setup(k)
			requests := k.recordRequestOptions()
			res := k.run()

			require.NotContains(t, res.stdout, keepToken, "the admin token must never be printed")
			require.NotContains(t, res.stderr, keepToken, "the admin token must never be printed")
			require.Equalf(t, 4, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			c := slot.container
			block := []string{
				fmt.Sprintf("error: E2E_FIDELITY_KEEP=1: the kept pair %s on port %d failed its %s check: %s; nothing was removed", c, k.port, row.check, row.detail(k)),
				keepFindOutWhyLine(c, row.check),
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
			driftSlot := keepDriftSlot(slot)
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

			requireKeptRunDockerCalls(t, slot.container, res.log)
			requireNoRequestOptions(t, requests)
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
		t.Parallel()
		for _, slot := range keepSlots {
			for _, pin := range keepPinChanges() {
				if !slices.Contains(pin.slots, slot.name) || pin.computed {
					continue
				}
				t.Run(slot.name+"/"+pin.name, func(t *testing.T) {
					t.Parallel()
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
		t.Parallel()
		for _, slot := range keepSlots {
			t.Run(slot.name, func(t *testing.T) {
				t.Parallel()
				k := newKeepRun(t, slot)
				k.noState = true
				res := k.run()
				assert.Contains(t, res.stdout, "==> no state file at "+k.stateFile()+"; booting a new pair")
				requireRecreated(t, k, res)
			})
		}
	})

	t.Run("a changed pin recreates the pair whatever else fails", func(t *testing.T) {
		t.Parallel()
		for _, slot := range keepSlots {
			if slot.name != "community" && slot.name != "helm-check" {
				continue
			}
			for _, row := range keepStopRows() {
				if row.unreadablePins || row.check == "unnamed" {
					continue
				}
				t.Run(slot.name+"/"+row.name, func(t *testing.T) {
					t.Parallel()
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
		t.Parallel()
		for _, slot := range keepSlots {
			for _, pin := range keepPinChanges() {
				if !slices.Contains(pin.slots, slot.name) {
					continue
				}
				t.Run(slot.name+"/"+pin.name, func(t *testing.T) {
					t.Parallel()
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
		t.Parallel()
		for _, slot := range keepSlots {
			if slot.name != "community" && slot.name != "helm-check" {
				continue
			}
			t.Run(slot.name, func(t *testing.T) {
				t.Parallel()
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
		t.Parallel()
		k := newKeepRun(t, keepSlots[0])
		k.state["k3sImage"] = "rancher/k3s:v0.0.1@sha256:" + strings.Repeat("0", 64)
		k.canned("/v1/provisioning.cattle.io.clusters/fleet-default/downstream", "404", "{}")
		res := k.run()

		assert.Contains(t, res.stdout, "==> reusing healthy kept container")
		assert.Contains(t, res.stdout, "==> kept downstream was created from a different k3s image; rebuilding it")
		assert.Contains(t, res.stdout, "==> rebuilding the kept downstream against the running oracle (a different k3s image)")
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
		t.Parallel()
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
			t.Parallel()
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
			// The update call is forbidden as a guard for a future change: the
			// update that turns the restart policy off sits behind a test for a
			// run without E2E_FIDELITY_KEEP, so a reused pair never reaches it.
			for _, prefix := range []string{"rm ", "run ", "start ", "network rm", "network create", "container rm", "update "} {
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

// groupOpener is the index of the line that opens the group the line at index
// i sits in: the nearest earlier line with less indentation, or -1.
func groupOpener(lines []string, i int) int {
	for j := i - 1; j >= 0; j-- {
		if indentOf(lines[j]) < indentOf(lines[i]) {
			return j
		}
	}
	return -1
}

// requireFailureGroupsSetAReason asserts, from the lines of one function of the
// script, that no check fails by a trailing "|| return 1" and that every
// "return 1" sits in a group that sets reasonVar, or sets both
// REUSE_FAILED_CHECK and REUSE_FAILED_DETAIL. It returns the number of
// "return 1" lines and the check names the function sets.
func requireFailureGroupsSetAReason(t *testing.T, lines []string, reasonVar string) (returns int, names map[string]bool) {
	t.Helper()
	orReturn := regexp.MustCompile(`\|\|\s*return 1`)
	setsCheck := regexp.MustCompile(`REUSE_FAILED_CHECK="([^"]+)"`)
	names = map[string]bool{}
	for i, line := range lines {
		assert.Falsef(t, orReturn.MatchString(line), "a check fails with a bare \"|| return 1\" at line %d: %s", i, line)
		if m := setsCheck.FindStringSubmatch(line); m != nil {
			names[m[1]] = true
		}
		if strings.TrimSpace(line) != "return 1" {
			continue
		}
		returns++
		opener := groupOpener(lines, i)
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
		reason := strings.Contains(group, reasonVar+"=")
		stops := strings.Contains(group, "REUSE_FAILED_CHECK=") && strings.Contains(group, "REUSE_FAILED_DETAIL=")
		assert.Truef(t, reason || stops, "the group of the return 1 at line %d sets neither %s nor a failed check and its detail:\n%s", i, reasonVar, group)
	}
	return returns, names
}

// reviewedLine is a line of the script that is allowed to hold a word a scan
// looks for, with the function it sits in (empty for the top level) and why.
type reviewedLine struct{ owner, text, reason string }

// requireReviewedLines asserts that every line of the script for which match
// is true (continuation lines joined, comments skipped, runs of white space
// read as one space) equals its own entry of list with the same owner function,
// and that every entry equals one line: a second copy of a listed line fails,
// and so does a stale entry. A new spelling of what the scan looks for, in any
// function, fails until its line is reviewed and listed.
func requireReviewedLines(t *testing.T, match func(line string) bool, list []reviewedLine) {
	t.Helper()
	funcStart := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\) \{$`)
	squash := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	used := make([]bool, len(list))
	for _, entry := range list {
		assert.NotEmptyf(t, entry.reason, "the entry needs its reason: %s", entry.text)
	}
	owner := ""
	for _, line := range joinedScriptLines(t) {
		if m := funcStart.FindStringSubmatch(line); m != nil {
			owner = m[1]
		} else if line == "}" {
			owner = ""
		}
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "#") || !match(text) {
			continue
		}
		found := false
		for i, entry := range list {
			if !used[i] && entry.owner == owner && squash(entry.text) == squash(text) {
				used[i] = true
				found = true
				break
			}
		}
		assert.Truef(t, found, "a line that is not reviewed, or a second copy of a listed line, in %q: %s", owner, text)
	}
	for i, entry := range list {
		assert.Truef(t, used[i], "the entry matches no line of its function; remove it: %q in %q", entry.text, entry.owner)
	}
}

// wordLines returns a match function for requireReviewedLines: a line that holds
// the word.
func wordLines(word *regexp.Regexp) func(string) bool { return word.MatchString }

// TestDownstreamPinChangedIsReviewedEverywhere asserts that every line of the
// script that names downstream_pin_changed is one of the reviewed lines: the
// variable says that the kept downstream is rebuilt, which only a changed k3s
// image may cause, so no other function and no other spelling may set it.
func TestDownstreamPinChangedIsReviewedEverywhere(t *testing.T) {
	requireReviewedLines(t, wordLines(regexp.MustCompile(`downstream_pin_changed`)), []reviewedLine{
		{"reuse_downstream", `downstream_pin_changed=""`, "clears the result first, so an exported value never forces a rebuild"},
		{"reuse_downstream", `downstream_pin_changed="a different k3s image"`, "the one place that sets it: the k3s image compare"},
		{"", `[ -n "${downstream_pin_changed:-}" ] || refuse_reuse`, "the call site stops the run unless the k3s image changed"},
		{"rebuild_downstream", `echo "==> rebuilding the kept downstream against the running oracle (${downstream_pin_changed:-a different k3s image})"`, "names the reason when the rebuild starts"},
	})
}

// loginOwner is the name of the function that logs in to a new oracle. It is a
// function that loops on the clock, which no test file outside oracleminor_test.go
// may name, so it is read from the table of clock functions.
func loginOwner(t *testing.T) string {
	t.Helper()
	for _, c := range clockFunctions {
		if strings.HasPrefix(c.fn, "login") {
			return c.fn
		}
	}
	require.Fail(t, "no clock function that logs in")
	return ""
}

// methodOptionLines are the lines of the script that give curl a request method,
// a config file or a body, each with the function it sits in and why.
func methodOptionLines(t *testing.T) []reviewedLine {
	t.Helper()
	return []reviewedLine{
		{"import_downstream", `api -o /dev/null -X POST "${BASE_URL}/v1/provisioning.cattle.io.clusters" -d @- <<JSON`, "creates the imported cluster"},
		{"rebuild_downstream", `code="$(api -o /dev/null -w '%{http_code}' -X DELETE "${BASE_URL}/v1/provisioning.cattle.io.clusters/fleet-default/${DOWNSTREAM_NAME}")" || true`, "removes the imported cluster of a downstream whose k3s image changed"},
		{loginOwner(t), `out="$(curl -s --max-time 30 -w '\n%{http_code}' --cacert "${CA_FILE}" "${BASE_URL}/v3-public/localProviders/local?action=login" -H 'Content-Type: application/json' -d @- <<JSON || true`, "logs in to a new oracle with the bootstrap password"},
		{"", `curl -s --cacert "${CA_FILE}" -o /dev/null "${BASE_URL}/v3/settings/server-url" -X PUT -H @<(printf 'Authorization: Bearer %s\n' "${LOGIN_TOKEN}") -H 'Content-Type: application/json' -d "{\"value\":\"https://${ORACLE_IP}\"}"`, "sets the server URL of a new oracle"},
		{"", `ADMIN_TOKEN="$(curl -s --cacert "${CA_FILE}" "${BASE_URL}/v3/tokens" -H @<(printf 'Authorization: Bearer %s\n' "${LOGIN_TOKEN}") -H 'Content-Type: application/json' -d @- <<'JSON' | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])'`, "creates the admin token of a new oracle"},
	}
}

// TestMethodOptionsOfTheScriptAreReviewed asserts that every line of the script
// that gives curl a request method (-X, alone or at the end of a cluster of
// short options, --request), a config file (-K, --config, which can set the
// method) or a request body or upload (-d, -F, -T, -I and --data, --json, --form,
// --upload-file, --head on a line that runs curl or api; each makes curl send a
// POST, a PUT or a HEAD) is one of the reviewed lines, so a request that creates
// or removes something is listed with the function that sends it. No line uses a
// config file.
func TestMethodOptionsOfTheScriptAreReviewed(t *testing.T) {
	method := regexp.MustCompile(`(?:^|\s)(?:-[A-Za-z]*[XK]|--request|--config)`)
	body := regexp.MustCompile(`(?:^|\s)(?:-[A-Za-z]*[dFTI]|--data|--json|--form|--upload-file|--head)`)
	runsCurl := regexp.MustCompile(`\b(?:curl|api)\b`)
	match := func(line string) bool {
		return method.MatchString(line) || runsCurl.MatchString(line) && body.MatchString(line)
	}
	requireReviewedLines(t, match, methodOptionLines(t))
}

// TestPythonRequestsOfTheScriptAreReviewed asserts that every line of the script
// that names a python HTTP client (urllib, http.client, requests) is one of the
// reviewed lines, so a request sent by python3 cannot be added without review.
// The only client is the registry reader.
func TestPythonRequestsOfTheScriptAreReviewed(t *testing.T) {
	const reason = "the registry reader of read_registry_image: reads image metadata from the registry, sends no request to the oracle"
	var list []reviewedLine
	for _, text := range []string{
		`import urllib.error`,
		`import urllib.parse`,
		`import urllib.request`,
		`req = urllib.request.Request(path, method=method)`,
		`with urllib.request.urlopen(req, timeout=timeout) as resp:`,
		`except urllib.error.HTTPError as err:`,
		`query = urllib.parse.urlencode(`,
		`if isinstance(err, urllib.error.URLError):`,
	} {
		list = append(list, reviewedLine{"read_registry_image", text, reason})
	}
	requireReviewedLines(t, wordLines(regexp.MustCompile(`urllib|http\.client|requests\.`)), list)
}

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

	_, names := requireFailureGroupsSetAReason(t, lines, "REUSE_RECREATE")

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

// pairRemoval matches a command of the script that removes part of a kept pair:
// a docker removal or prune, a curl request with the DELETE method in any
// spelling in the list of TestPairRemovalPatternMatchesEveryDeleteSpelling (-X
// DELETE, -XDELETE, -X "DELETE", -X 'DELETE', -sX DELETE, --request DELETE,
// --request=DELETE, --request "DELETE", in any letter case), and the removal of
// the state file, state root or CA file. A method that no text names is caught
// by the reviewed list of method options in
// TestMethodOptionsOfTheScriptAreReviewed.
var pairRemoval = regexp.MustCompile(`\bdocker\s+(container\s+|network\s+|volume\s+)?rm\b|\bdocker\s+(container|system|network|volume)\s+prune\b|(?:(?:^|\s)-[A-Za-z]*X\s*|--request(?:\s+|=))["']?(?i:DELETE)\b|\brm\b.*\$\{(STATE_FILE|STATE_ROOT|CA_FILE)\}`)

// TestPairRemovalPatternMatchesEveryDeleteSpelling asserts that the pattern of
// the removal scan matches each spelling of a DELETE request and of a docker
// removal, and none of the reads and writes that are no removal.
func TestPairRemovalPatternMatchesEveryDeleteSpelling(t *testing.T) {
	for _, line := range []string{
		`api -X DELETE "${URL}"`,
		`api -XDELETE "${URL}"`,
		`api -X "DELETE" "${URL}"`,
		`api -X 'DELETE' "${URL}"`,
		`api -X delete "${URL}"`,
		`api -sX DELETE "${URL}"`,
		`api -sSX DELETE "${URL}"`,
		`api -fsSXDELETE "${URL}"`,
		`api -sX "DELETE" "${URL}"`,
		`api --request DELETE "${URL}"`,
		`api --request=DELETE "${URL}"`,
		`api --request "DELETE" "${URL}"`,
		`api --request 'delete' "${URL}"`,
		`docker rm -f -v "${K3S_CONTAINER}"`,
		`docker network rm "${NETWORK}"`,
		`rm -f "${STATE_FILE}"`,
	} {
		assert.Truef(t, pairRemoval.MatchString(line), "the removal scan misses %s", line)
	}
	for _, line := range []string{
		`api -X POST "${URL}"`,
		`api -X PATCH -d '{"deleted":true}' "${URL}"`,
		`api --request GET "${URL}"`,
		`api -sX POST "${URL}"`,
		`api -o /dev/null "${BASE_URL}/v3/clusters/${DOWNSTREAM_ID}"`,
		`docker ps -a --filter name="${CONTAINER}"`,
	} {
		assert.Falsef(t, pairRemoval.MatchString(line), "the removal scan flags %s", line)
	}
}

var (
	keepBracedRead = regexp.MustCompile(`\$\{[#!]?([A-Za-z_][A-Za-z0-9_]*)`)
	keepPlainRead  = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	keepAssignment = regexp.MustCompile(`(?:^\s*|\|\|\s+|&&\s+|;\s*|\bthen\s+)([A-Za-z_][A-Za-z0-9_]*)=`)
	keepLocalNames = regexp.MustCompile(`^\s*local\s+(.+)$`)
	keepComment    = regexp.MustCompile(`^\s*#`)
	keepFuncStart  = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\) \{$`)
	keepCallPrefix = `(?:^\s*|\$\(|` + "`" + `|\|\|\s*|&&\s*|;\s*|\|\s*|!\s+|\{\s+|\b(?:then|if|elif|while|until|do|else)\s+)`
)

// keepCallPattern matches a line that calls one of the functions: its name in
// the position of a command, after the start of the line, a command
// substitution or backtick, a list operator, a negation, a brace, or one of
// then, if, elif, while, until, do and else. A name in a quoted text matches
// only after one of those prefixes; a match there makes the set of called
// functions differ, so the scan test fails and the line is looked at.
func keepCallPattern(functions []string) *regexp.Regexp {
	quoted := make([]string, len(functions))
	for i, fn := range functions {
		quoted[i] = regexp.QuoteMeta(fn)
	}
	return regexp.MustCompile(keepCallPrefix + `(` + strings.Join(quoted, "|") + `)\b`)
}

// TestKeepCallPatternFindsEveryFormOfACall asserts the lines that
// keepCallPattern reads as a call of new_helper and the lines it does not.
func TestKeepCallPatternFindsEveryFormOfACall(t *testing.T) {
	call := keepCallPattern([]string{"other_helper", "new_helper", "new_helper_two"})
	for _, line := range []string{
		"new_helper",
		"  new_helper arg",
		"  if new_helper; then",
		"  if new_helper arg; then",
		"  if ! new_helper; then",
		"  elif new_helper; then",
		"  while new_helper; do",
		"  until new_helper; do",
		"  do new_helper",
		"  else new_helper",
		"  then new_helper",
		"  x=\"$(new_helper)\"",
		"  x=`new_helper`",
		"  cmd || new_helper",
		"  cmd && new_helper",
		"  cmd; new_helper",
		"  cmd | new_helper",
		"  { new_helper; }",
		"  if other_helper; then",
		"  new_helper_two",
	} {
		assert.Truef(t, call.MatchString(line), "a call of a helper: %s", line)
	}
	assert.Equal(t, "new_helper_two", call.FindStringSubmatch("  new_helper_two arg")[1], "a name that starts another name matches as the longer name")
	assert.Equal(t, "new_helper", call.FindStringSubmatch("  new_helper arg")[1])
	for _, line := range []string{
		`  echo "new_helper failed"`,
		`  detail="new_helper failed at a check"`,
		"  new_helper_three",
		"  x=${new_helper}",
		"  # new_helper",
		"  echo done",
	} {
		if keepComment.MatchString(line) {
			continue
		}
		assert.Falsef(t, call.MatchString(line), "not a call of a helper: %s", line)
	}
}

// TestKeepReadPatternsFindTheVariablesALineReads asserts the names that
// keepBracedRead and keepPlainRead read from a line of the script.
func TestKeepReadPatternsFindTheVariablesALineReads(t *testing.T) {
	for line, want := range map[string]string{
		`echo "${X}"`:          "X",
		`echo "${#X}"`:         "X",
		`echo "${!X}"`:         "X",
		`echo "${X:-y}"`:       "X",
		`echo "${X_1}"`:        "X_1",
		`echo "$X"`:            "X",
		`echo "$X_1 and more"`: "X_1",
	} {
		var got []string
		for _, re := range []*regexp.Regexp{keepBracedRead, keepPlainRead} {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				got = append(got, m[1])
			}
		}
		assert.Equalf(t, []string{want}, got, "the variables read by %s", line)
	}
	for _, line := range []string{`echo "$1"`, `echo "$?"`, `echo "$(date)"`, `echo "$#"`, `echo "${#}"`, `echo done`} {
		for _, re := range []*regexp.Regexp{keepBracedRead, keepPlainRead} {
			assert.Emptyf(t, re.FindAllStringSubmatch(line, -1), "no variable is read by %s", line)
		}
	}
}

// TestKeepChecksDependOnTheSlotOnlyThroughKnownVariables asserts that the
// functions that check a kept pair (reuse_state, refuse_reuse and
// read_kept_state, with the state_value and load_helm_state they call) read no
// variable outside the list below, and call no function of the script besides
// those two. TestKeepRunStopsOnAFailedPairCheck runs each failure row on two
// slots only, which covers every slot while these functions read no other
// variable that differs by slot. A new variable fails this test until the cases
// of that test cover it and its name is added here. The branch of
// refuse_reuse that names the downstream container for a downstream check is
// reached by no failure row; the rows of
// TestKeepRunStopsOnAFailedDownstreamCheck that run on every slot reach it.
//
// It is a scan of names, not a parse of the shell: a name the function assigns
// itself (name= at the start of a statement, or local) is not a read of a slot
// variable, and ${#NAME} and ${!NAME} count as reads. It does not see a read in
// arithmetic without a dollar sign, a [[ -v NAME ]] test, or an environment
// read in the inline python. It does not see the code before the lock (the helm
// block, check_slot, helm_prepare and resolve_upcoming_chart), which the rows on
// every slot guard, or an allowed name whose value starts to differ by slot.
func TestKeepChecksDependOnTheSlotOnlyThroughKnownVariables(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	allowed := map[string]string{
		"BOOT":                  "container or helm, by slot",
		"DRIFT_SLOT":            "set on the slots that allow a drift run",
		"CONTAINER":             "container name, by slot",
		"K3S_CONTAINER":         "derived from the container name",
		"NETWORK":               "derived from the container name",
		"HOST_PORT":             "port, by slot (the prime slot has its own)",
		"STATE_ROOT":            "state root, by slot",
		"STATE_FILE":            "under the state root",
		"CA_FILE":               "under the state root",
		"BASE_URL":              "derived from the port",
		"ORACLE_IMAGE":          "oracle image, by slot",
		"CHART_PIN":             "chart of a helm slot",
		"LOCAL_K3S_PIN":         "node container k3s image of a helm slot",
		"CERT_MANAGER_MANIFEST": "cert-manager manifest of a helm slot",
		"HELM_COMPUTED":         "set by helm_prepare, which only a helm slot runs",
		"HELM_ASSETS_IMAGE":     "computed by helm_prepare or read from the state file, on a helm slot",
		"LOCAL_K3S_SOURCE":      "computed by helm_prepare or read from the state file, on a helm slot",
		"BIND_ADDR":             "the same on every slot",
		"REUSE_FAILED_CHECK":    "result of reuse_state or reuse_downstream, the same on every slot",
		"REUSE_FAILED_DETAIL":   "result of reuse_state or reuse_downstream, the same on every slot",
		"REUSE_FAILED_HINT":     "result of reuse_state or reuse_downstream, the same on every slot",
		"DOWNSTREAM_LAST_STATE": "what the last read of the downstream gave, the same on every slot",
	}
	checks := []string{"reuse_state", "refuse_reuse", "read_kept_state"}
	helpers := []string{"state_value", "load_helm_state"}

	used := map[string]bool{}
	for _, name := range slices.Concat(checks, helpers) {
		body := functionBody(t, name)
		assigned := map[string]bool{}
		for _, line := range body {
			if keepComment.MatchString(line) {
				continue
			}
			for _, m := range keepAssignment.FindAllStringSubmatch(line, -1) {
				assigned[m[1]] = true
			}
			if m := keepLocalNames.FindStringSubmatch(line); m != nil {
				for field := range strings.FieldsSeq(m[1]) {
					local, _, _ := strings.Cut(field, "=")
					assigned[local] = true
				}
			}
		}
		for i, line := range body {
			if keepComment.MatchString(line) {
				continue
			}
			for _, re := range []*regexp.Regexp{keepBracedRead, keepPlainRead} {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					if assigned[m[1]] {
						continue
					}
					_, ok := allowed[m[1]]
					assert.Truef(t, ok, "%s line %d reads %s, which differs by slot or is not in the list of allowed variables: %s", name, i+1, m[1], strings.TrimSpace(line))
					used[m[1]] = true
				}
			}
		}
	}
	for name := range allowed {
		assert.Truef(t, used[name], "no check reads %s; remove it from the list", name)
	}

	var functions []string
	for _, line := range scriptLines(t) {
		if m := keepFuncStart.FindStringSubmatch(line); m != nil {
			functions = append(functions, m[1])
		}
	}
	call := keepCallPattern(functions)
	called := map[string]bool{}
	for _, name := range slices.Concat(checks, helpers) {
		for _, line := range functionBody(t, name)[1:] {
			if keepComment.MatchString(line) {
				continue
			}
			for _, m := range call.FindAllStringSubmatch(line, -1) {
				if m[1] != name {
					called[m[1]] = true
				}
			}
		}
	}
	assert.ElementsMatch(t, helpers, slices.Sorted(maps.Keys(called)), "the functions of the script that the checks and their helpers call")
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
	removal := pairRemoval
	funcStart := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\) \{$`)

	// printed lists the lines that name a removal command in text only. Every
	// other line that matches is a command.
	printed := map[string]string{
		`printf 'error:   docker rm -f -v %q %q\n' "${CONTAINER}" "${K3S_CONTAINER}"`: "the refusal prints the command that removes the pair",
		`printf 'error:   docker network rm %q\n' "${NETWORK}"`:                       "the refusal prints the command that removes the network",
		`printf 'error:   rm -rf %q\n' "${STATE_ROOT}"`:                               "the refusal prints the command that removes the state root",
		`printf 'error:   docker rm -f -v %q\n' "${CONTAINER}" >&2`:                   "the report of an oracle container that restarted more than 3 times prints the command that removes it",
	}
	printed[`echo "error: the suite was renamed from e2e-real to e2e-fidelity; remove them (docker rm -f -v, docker network rm) together with the old state root ${STATE_ROOT/e2e-fidelity/e2e-real}, or rename them, then rerun" >&2`] = "the refusal for a pair from before the rename names the commands in text"
	printedUsed := map[string]bool{}
	allowed := map[string]string{
		"boot_k3s":           "removes the downstream container before a new one; its callers are the fresh boot and rebuild_downstream",
		"rebuild_downstream": "rebuilds the downstream only after a k3s pin change: a changed k3sImage",
	}
	used := map[string]bool{}

	// The refusal has two calls: the fresh boot's, and the reuse branch's after
	// a failed downstream check that is no pin change. Both run only under
	// E2E_FIDELITY_KEEP=1.
	const reuseRefusal = `[ -n "${downstream_pin_changed:-}" ] || refuse_reuse`
	refuseWord := regexp.MustCompile(`\brefuse_reuse\b`)
	callLine, reuseCall, refuseCalls := -1, -1, 0
	for i, line := range lines {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "#") || text == "refuse_reuse() {" || !refuseWord.MatchString(text) {
			continue
		}
		refuseCalls++
		switch text {
		case "refuse_reuse":
			require.Equal(t, -1, callLine, "the fresh boot calls the refusal once")
			callLine = i
		case reuseRefusal:
			require.Equal(t, -1, reuseCall, "the reuse branch calls the refusal once")
			reuseCall = i
		}
	}
	require.Equal(t, 2, refuseCalls, "the refusal has two calls")
	require.GreaterOrEqual(t, callLine, 0, "the fresh boot calls the refusal")
	require.GreaterOrEqual(t, reuseCall, 0, "the reuse branch calls the refusal")
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

	// The reuse branch's call sits in the block of a failed reuse_downstream,
	// after the branch's own KEEP=1 test, and the downstream is rebuilt after
	// it in the same block.
	failedIf := slices.Index(lines[reuseIf:], "  if ! reuse_downstream; then") + reuseIf
	require.Greater(t, failedIf, reuseIf, "the reuse branch tests reuse_downstream with an if")
	failedFi := slices.Index(lines[failedIf:], "  fi") + failedIf
	require.Greater(t, failedFi, failedIf)
	require.Less(t, failedFi, elseLine)
	require.Greater(t, reuseCall, failedIf, "the refusal call comes after the test of reuse_downstream")
	require.Less(t, reuseCall, failedFi, "the refusal call is inside the block of a failed reuse_downstream")

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
	// block of a failed reuse_downstream in the reuse branch, after the refusal
	// call.
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
			assert.Truef(t, owner == "" && text == "rebuild_downstream" && i > reuseCall && i < failedFi,
				"line %d calls rebuild_downstream anywhere but after the refusal call in the block of a failed reuse_downstream: %s", i+1, text)
		}
	}
	assert.Equal(t, 2, bootCalls, "boot_k3s has two call sites")
	assert.Equal(t, 1, rebuildCalls, "rebuild_downstream has one call site")
}

// fleetAgentReady is the body the Deployment read of the fleet agent answers
// when the agent is ready.
const fleetAgentReady = `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`

// cannedImport adds the rows an import of the downstream cluster c-m-abc
// reads, and the two fleet agent reads answering ready: the pair's downstream is
// rebuilt and imported against the stubs.
func (k *keepRun) cannedImport() {
	k.t.Helper()
	k.canned("/v1/provisioning.cattle.io.clusters", "200", "{}")
	k.canned("/v1/provisioning.cattle.io.clusters/fleet-default/downstream", "404", `{"status":{"clusterName":"c-m-abc"}}`)
	k.canned("/v3/clusterregistrationtokens?clusterId=c-m-abc", "200", `{"data":[{"token":"regtoken"}]}`)
	k.canned("/v3/import/regtoken_c-m-abc.yaml", "200", "apiVersion: v1")
	k.canned("/k8s/clusters/c-m-abc/api/v1/namespaces/cattle-fleet-system", "200", "{}")
	k.canned("/k8s/clusters/c-m-abc/apis/apps/v1/namespaces/cattle-fleet-system/deployments/fleet-agent", "200", fleetAgentReady)
}

// moveK3sImage makes the kept downstream come from another k3s image than the
// run's, so the run rebuilds and imports the downstream, and lets docker run
// and the TLS name check succeed.
func (k *keepRun) moveK3sImage() {
	k.t.Helper()
	k.state["k3sImage"] = "rancher/k3s:v0.0.1@sha256:" + strings.Repeat("0", 64)
	delete(k.s.env, "STUB_FAIL_RUN")
	testfs.WriteExecutable(k.t, filepath.Join(k.s.stubDir, "openssl"),
		[]byte("#!/bin/sh\nprintf 'IP Address:172.30.%d.2\\n' 1 2 3 4 5 6 7 8 9\n"))
}

// TestEverySlotWaitsForTheFleetAgentAfterAnImport asserts, on every slot of
// the script, that the import of the downstream cluster that a moved k3s image
// causes reads the cattle-fleet-system namespace and then the fleet-agent
// Deployment, after the proxy path has answered steadily and before the smoke
// check of the slot. The fleet paths are the only rows that make the run
// wait: a path without a row would spin on the real clock.
func TestEverySlotWaitsForTheFleetAgentAfterAnImport(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		t.Run(slot.name, func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, slot)
			k.moveK3sImage()
			k.cannedImport()
			start := time.Now()
			res := k.run()
			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Less(t, time.Since(start), 60*time.Second, "a fleet path without a canned row spins on the real clock")
			assert.Contains(t, res.stdout, "==> waiting for the fleet agent on the downstream cluster c-m-abc")

			namespace, agent, versions, smoke := -1, -1, 0, -1
			for i, line := range k.s.curlLog() {
				switch {
				case strings.HasSuffix(line, "/k8s/clusters/c-m-abc/api/v1/namespaces/cattle-fleet-system"):
					namespace = i
				case strings.HasSuffix(line, "/k8s/clusters/c-m-abc/apis/apps/v1/namespaces/cattle-fleet-system/deployments/fleet-agent"):
					agent = i
				case strings.HasSuffix(line, "/k8s/clusters/c-m-abc/version") && namespace < 0:
					versions++
				case strings.HasSuffix(line, "/rancherversion") && smoke < 0:
					smoke = i
				}
			}
			require.GreaterOrEqual(t, namespace, 0, "the namespace is read")
			assert.Greater(t, agent, namespace, "the Deployment is read after the namespace")
			assert.GreaterOrEqual(t, versions, 6, "the proxy path answered steadily before the fleet reads")
			if smoke >= 0 {
				assert.Less(t, agent, smoke, "the fleet reads come before the smoke check")
			}
		})
	}
}

// TestKeepRunStoppedOracleExitsFive asserts that a run that fails after it
// reached its own oracle container, whose container Docker reports as exited,
// exits 5 with the container state first in the diagnostics: a reused pair
// whose downstream rebuild fails, and a new pair whose server never answers.
// A run that fails before it reached the container keeps its exit code and
// still prints the state.
func TestKeepRunStoppedOracleExitsFive(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)

	t.Run("a reused pair", func(t *testing.T) {
		t.Parallel()
		k := newKeepRun(t, keepSlots[0])
		k.state["k3sImage"] = "rancher/k3s:v0.0.1@sha256:" + strings.Repeat("0", 64)
		k.canned("/v1/provisioning.cattle.io.clusters/fleet-default/downstream", "404", "{}")
		k.s.helm.files["node_state"] = "exited"
		res := k.run()
		require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" is not running (status exited, exit code 0, OOMKilled false)")
		assert.Contains(t, res.stderr, "the oracle's certificate did not gain the SAN")
		assert.Contains(t, res.stdout, "==> container state:\n[rancher] status=exited exitCode=0 oomKilled=false finishedAt=")
		assert.Contains(t, res.stdout, "\n[k3s] status=running exitCode=0 oomKilled=false finishedAt=")
	})

	t.Run("a new pair that never answers", func(t *testing.T) {
		t.Parallel()
		k := newKeepRun(t, keepSlots[0])
		k.noState = true
		k.ping = "no reply"
		delete(k.s.env, "STUB_FAIL_RUN")
		k.s.helm.files["node_state"] = "exited"
		k.s.helm.files["exit_code"] = "137"
		k.s.helm.files["oom"] = "true"
		res := k.run()
		require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.True(t, hasCall(res.log, "run -d --name "+k.slot.container), "the run reached its container: %v", res.log)
		assert.Contains(t, res.stderr, "(status exited, exit code 137, OOMKilled true)")
		assert.Contains(t, res.stdout, "[rancher] status=exited exitCode=137 oomKilled=true")
		for _, line := range k.s.curlLog() {
			assert.NotContains(t, line, "/ping", "the wait reads the container before it reads /ping, and stops")
		}
	})

	t.Run("a failure before the run reached its container", func(t *testing.T) {
		t.Parallel()
		k := newKeepRun(t, keepSlots[0])
		k.noState = true
		k.s.env["STUB_PRE_RENAME"] = "1"
		k.s.helm.files["node_state"] = "exited"
		res := k.run()
		require.Equalf(t, 1, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Contains(t, res.stderr, "a pre-rename oracle pair is still present")
		assert.NotContains(t, res.stderr, "is not running")
		assert.Contains(t, res.stdout, "[rancher] status=exited exitCode=0 oomKilled=false")
	})
}

// keptRunDockerCalls are the docker calls a refused kept run makes, as prefixes
// of the stub's log lines, each with why. Any other call fails a stop row: a
// removal, a network change, a container start or a command run in a
// container that changes anything. A name %s is a container of the slot's pair.
var keptRunDockerCalls = []struct{ prefix, reason string }{
	{"version --format ", "the check of the Docker platform before the run starts"},
	{"info", "the Docker root directory and the daemon's information"},
	{"image inspect ", "the check that a pinned image is present"},
	{"pull -q ", "a helm pair whose kept state cannot be read pulls its pinned images before the lock"},
	{"run --rm --entrypoint k3s ", "a helm pair reads the k3s version from an image in a container that removes itself"},
	{"inspect --format ", "the container id and published address of the kept containers"},
	{"inspect -f ", "the state of the containers in the diagnostics, and the port bindings of every container"},
	{"network ls -q", "the check for a network that overlaps the slot's"},
	{"network inspect -f ", "the check for a network that overlaps the slot's"},
	{"ps -aq", "the check for a container that holds the slot's port"},
	{"stats --no-stream", "the host's container load in the diagnostics"},
	{"logs --tail ", "the last log lines of the containers in the diagnostics, through the redaction filter"},
	{"exec %s kubectl get ", "a read of the oracle's nodes, pods and events in the diagnostics"},
	{"exec %s kubectl describe ", "a read of the oracle's nodes in the diagnostics"},
	{"exec %s kubectl -n cattle-system logs ", "a read of the Rancher or cluster agent log in the diagnostics"},
	{"exec %s kubectl -n cattle-system get pods ", "a read of the Rancher pods in the diagnostics"},
	{"exec %s kubectl -n cert-manager get pods ", "a read of the cert-manager pods in the diagnostics"},
	{"exec %s kubectl -n kube-system logs ", "a read of the chart install job log in the diagnostics"},
}

// requireKeptRunDockerCalls asserts that every docker call of a refused run is
// one of keptRunDockerCalls, where a name %s stands for the oracle container or
// the downstream container of the slot.
func requireKeptRunDockerCalls(t *testing.T, container string, log []string) {
	t.Helper()
	for _, line := range log {
		allowed := false
		for _, call := range keptRunDockerCalls {
			if !strings.Contains(call.prefix, "%s") {
				allowed = allowed || strings.HasPrefix(line, call.prefix)
				continue
			}
			for _, name := range []string{container, container + "-k3s"} {
				allowed = allowed || strings.HasPrefix(line, fmt.Sprintf(call.prefix, name))
			}
		}
		assert.Truef(t, allowed, "docker call %q is not one a refused run makes: %v", line, log)
	}
}

// keepCurlRecorder stands in for curl in front of whatever curl the case has
// (saved as curl-next). It writes the arguments of every call that gives curl
// a request method, a config file or a body (-X, --request, -d, --data*,
// --json, -F, --form, -T, --upload-file, -I, --head, -K, --config, alone or in
// a cluster of short options) to the file STUB_REQUEST_LOG, then runs the next
// curl.
const keepCurlRecorder = `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
  --request|--request=*|--data|--data-*|--json|--json=*|--form|--form=*|--upload-file|--upload-file=*|--head|--config|--config=*|-[XdFTKI]*|-[!-]*[XdFTKI]*)
    printf '%s\n' "$*" >> "$STUB_REQUEST_LOG"
    break ;;
  esac
done
exec "$STUB_NEXT_CURL" "$@"
`

// recordRequestOptions puts the recorder in front of the case's curl and
// returns the path of the file it writes. Call it after the other setup.
func (k *keepRun) recordRequestOptions() string {
	k.t.Helper()
	next := filepath.Join(k.s.stubDir, "curl-next")
	require.NoError(k.t, os.Rename(filepath.Join(k.s.stubDir, "curl"), next))
	testfs.WriteExecutable(k.t, filepath.Join(k.s.stubDir, "curl"), []byte(keepCurlRecorder))
	log := filepath.Join(k.s.stubDir, "curl.requests")
	k.s.env["STUB_NEXT_CURL"] = next
	k.s.env["STUB_REQUEST_LOG"] = log
	return log
}

// requireNoRequestOptions asserts that no call of the run gave curl a request
// method, a config file or a body: a refused run reads and sends nothing.
func requireNoRequestOptions(t *testing.T, log string) {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	assert.Emptyf(t, string(raw), "a refused run sends no request that has a method or a body")
}

// keepDownstreamStop is a way the kept downstream fails a check after the
// kept pair passed its own, and what the refusal says about it.
type keepDownstreamStop struct {
	name   string
	check  string
	setup  func(k *keepRun)
	detail func(k *keepRun) string
	// hint is the extra refusal line the case adds, or empty.
	hint string
	// waitLast is the text in the parentheses of the wait line when the case
	// reads a downstream that is not active, or empty when the run prints no
	// wait line.
	waitLast string
	// limit is the time in seconds the wait line names: 1 or 0 for a case that
	// shortens the wait, 120 for the script's own.
	limit string
	// everySlot is true for the one row of each check that runs on every slot;
	// the other rows run on the community and helm-check slots only, because
	// what they vary (the shape of a state file value, the words for the last
	// read) does not depend on the slot.
	everySlot bool
	// proxyRead is true when the downstream turns active and the run then reads
	// the proxy path to it; fleetRead when it then reads the fleet agent too.
	proxyRead, fleetRead bool
	// watchSleep is true for a case whose wait runs on the real clock with a
	// limit the case shortens: the sleep stub ends a run that loses its bound.
	watchSleep bool
	// stderr is a text the run prints on stderr before the refusal, or empty.
	stderr string
}

const keepDownstreamHint = "a downstream that reconnects after the host slept can take a few minutes; run again"

// keepClusterRead is the path of the read of the kept downstream cluster.
const keepClusterRead = "/v3/clusters/c-m-abc"

// editScript replaces the one place in this case's copy of the script that
// holds old with replacement.
func (k *keepRun) editScript(old, replacement string) {
	k.t.Helper()
	require.NotEqual(k.t, realScriptPath(k.t), k.s.script, "the case changes the copy of the script only")
	raw, err := os.ReadFile(k.s.script)
	require.NoError(k.t, err)
	text := string(raw)
	require.Equal(k.t, 1, strings.Count(text, old))
	testfs.WriteExecutable(k.t, k.s.script, []byte(strings.Replace(text, old, replacement, 1)))
}

// setDownstreamLimit gives the copy of the script a limit in seconds for a kept
// downstream cluster to turn active. With 0 the wait reads once and stops. With
// 1 it reads until the real clock passes the next second, because the stubbed
// sleep takes no time.
func (k *keepRun) setDownstreamLimit(seconds string) {
	k.t.Helper()
	k.editScript("\nDOWNSTREAM_ACTIVE_LIMIT_SECONDS=120\n", "\nDOWNSTREAM_ACTIVE_LIMIT_SECONDS="+seconds+"\n")
}

// setFleetLimit gives the copy of the script a limit in seconds for the fleet
// agent to be ready. The wait reads bash's SECONDS, so it ends after the next
// second passes whatever the stubbed sleep does.
func (k *keepRun) setFleetLimit(seconds string) {
	k.t.Helper()
	k.editScript("\nFLEET_LIMIT_SECONDS=600\n", "\nFLEET_LIMIT_SECONDS="+seconds+"\n")
}

// keepRunawaySeconds is the real time a run may keep calling sleep before the
// sleep stub of a case with a short wait ends the script.
const keepRunawaySeconds = 30

// keepSleepWatchdog stands in for sleep. It takes no time. When the run has
// called it for STUB_SLEEP_SECONDS of real time it writes a marker file and ends
// the script with a termination signal, so a wait that lost its bound fails the
// case within that time and never hangs.
const keepSleepWatchdog = `#!/bin/sh
start="$STUB_SLEEP_START"
now=$(date +%s)
[ -f "$start" ] || echo "$now" > "$start"
if [ $((now - $(cat "$start"))) -ge "$STUB_SLEEP_SECONDS" ]; then
  echo runaway > "$start.runaway"
  kill -TERM "$PPID"
fi
exit 0
`

// keepCurlWatchdog stands in for curl in front of the curl stub (saved as
// curl-stub). It sends every call to the stub, except that once STUB_SLEEP_SECONDS
// of real time have passed since the first read of the kept downstream cluster
// it writes a marker file and answers active, which ends a wait that has no
// sleep to watch. The answer carries the status line when the call asks for it.
const keepCurlWatchdog = `#!/bin/sh
case "$*" in
*/v3/clusters/c-m-abc)
  start="$STUB_CURL_START"
  now=$(date +%s)
  [ -f "$start" ] || echo "$now" > "$start"
  if [ $((now - $(cat "$start"))) -ge "$STUB_SLEEP_SECONDS" ]; then
    echo runaway > "$start.runaway"
    case "$*" in
    *http_code*) printf '{"state":"active"}\n200' ;;
    *) printf '{"state":"active"}' ;;
    esac
    exit 0
  fi ;;
esac
exec "$STUB_REAL_CURL" "$@"
`

// watchSleep replaces the sleep stub with the watchdog, which ends the script
// after keepRunawaySeconds of real time in sleep calls, and returns the path of
// the marker file it writes then.
func (k *keepRun) watchSleep() string {
	k.t.Helper()
	testfs.WriteExecutable(k.t, filepath.Join(k.s.stubDir, "sleep"), []byte(keepSleepWatchdog))
	start := filepath.Join(k.s.stubDir, "sleep.start")
	k.s.env["STUB_SLEEP_START"] = start
	k.s.env["STUB_SLEEP_SECONDS"] = strconv.Itoa(keepRunawaySeconds)
	return start + ".runaway"
}

// watchWait puts a real-time deadline on a case that waits for the kept
// downstream cluster: the sleep stub and the curl stub each write a marker file
// and end the wait after keepRunawaySeconds, whichever the wait calls. It
// returns the paths of the two marker files, which exist only when the wait did
// not stop at its limit. The fleet agent rows let the run go on to its end after
// a marker. A wait that makes no call to either stub is not covered; the test
// timeout ends it.
func (k *keepRun) watchWait() []string {
	k.t.Helper()
	sleepMarker := k.watchSleep()
	realCurl := filepath.Join(k.s.stubDir, "curl-stub")
	require.NoError(k.t, os.Rename(filepath.Join(k.s.stubDir, "curl"), realCurl))
	testfs.WriteExecutable(k.t, filepath.Join(k.s.stubDir, "curl"), []byte(keepCurlWatchdog))
	curlStart := filepath.Join(k.s.stubDir, "curl.start")
	k.s.env["STUB_CURL_START"] = curlStart
	k.s.env["STUB_REAL_CURL"] = realCurl
	k.cannedFleetAgent()
	return []string{sleepMarker, curlStart + ".runaway"}
}

// keepCurlCounter stands in for curl in front of the curl stub (saved as
// curl-stub). The first STUB_INACTIVE_READS reads of the kept downstream
// cluster are answered from the table STUB_CURL_TABLE.inactive, every other
// call from the normal table.
const keepCurlCounter = `#!/bin/sh
case "$*" in
*/v3/clusters/c-m-abc)
  n=$(cat "$STUB_CURL_TABLE.count" 2>/dev/null || echo 0)
  n=$((n + 1))
  echo "$n" > "$STUB_CURL_TABLE.count"
  if [ "$n" -le "$STUB_INACTIVE_READS" ]; then
    exec env STUB_CURL_TABLE="$STUB_CURL_TABLE.inactive" "$STUB_REAL_CURL" "$@"
  fi ;;
esac
exec "$STUB_REAL_CURL" "$@"
`

// inactiveFirstReads makes the first reads of the kept downstream cluster
// answer with the given state, and every read after them with active.
func (k *keepRun) inactiveFirstReads(reads int, state string) {
	k.t.Helper()
	realCurl := filepath.Join(k.s.stubDir, "curl-stub")
	require.NoError(k.t, os.Rename(filepath.Join(k.s.stubDir, "curl"), realCurl))
	testfs.WriteExecutable(k.t, filepath.Join(k.s.stubDir, "curl"), []byte(keepCurlCounter))
	row := keepClusterRead + "\t200\t" + `{"state":"` + state + `"}` + "\n"
	require.NoError(k.t, os.WriteFile(filepath.Join(k.s.stubDir, "curl.table.inactive"), []byte(row), 0o600))
	k.s.env["STUB_REAL_CURL"] = realCurl
	k.s.env["STUB_INACTIVE_READS"] = strconv.Itoa(reads)
}

// keepCurlOneActive stands in for curl in front of the curl stub (saved as
// curl-stub). Only read number STUB_ACTIVE_READ of the kept downstream cluster
// is answered from the normal table (active); every other read is answered from
// the table STUB_CURL_TABLE.inactive.
const keepCurlOneActive = `#!/bin/sh
case "$*" in
*/v3/clusters/c-m-abc)
  n=$(cat "$STUB_CURL_TABLE.count" 2>/dev/null || echo 0)
  n=$((n + 1))
  echo "$n" > "$STUB_CURL_TABLE.count"
  if [ "$n" -ne "$STUB_ACTIVE_READ" ]; then
    exec env STUB_CURL_TABLE="$STUB_CURL_TABLE.inactive" "$STUB_REAL_CURL" "$@"
  fi ;;
esac
exec "$STUB_REAL_CURL" "$@"
`

// activeOnlyOnRead makes the kept downstream cluster answer with the given state
// on every read except read number n, which answers active.
func (k *keepRun) activeOnlyOnRead(n int, state string) {
	k.t.Helper()
	realCurl := filepath.Join(k.s.stubDir, "curl-stub")
	require.NoError(k.t, os.Rename(filepath.Join(k.s.stubDir, "curl"), realCurl))
	testfs.WriteExecutable(k.t, filepath.Join(k.s.stubDir, "curl"), []byte(keepCurlOneActive))
	row := keepClusterRead + "\t200\t" + `{"state":"` + state + `"}` + "\n"
	require.NoError(k.t, os.WriteFile(filepath.Join(k.s.stubDir, "curl.table.inactive"), []byte(row), 0o600))
	k.s.env["STUB_REAL_CURL"] = realCurl
	k.s.env["STUB_ACTIVE_READ"] = strconv.Itoa(n)
}

// cannedFleetAgent adds the two reads of the fleet agent, answering ready.
func (k *keepRun) cannedFleetAgent() {
	k.t.Helper()
	k.canned("/k8s/clusters/c-m-abc/api/v1/namespaces/cattle-fleet-system", "200", "{}")
	k.canned("/k8s/clusters/c-m-abc/apis/apps/v1/namespaces/cattle-fleet-system/deployments/fleet-agent", "200", fleetAgentReady)
}

func keepDownstreamStopRows() []keepDownstreamStop {
	var rows []keepDownstreamStop
	for _, field := range []string{"k3sImage", "k3sContainerId", "downstream"} {
		for _, variant := range []struct {
			name  string
			value any
		}{{"is missing", nil}, {"is null", nil}, {"is a number", 5}} {
			rows = append(rows, keepDownstreamStop{
				name: "state file " + field + " " + variant.name, check: "state-file", everySlot: variant.name == "is missing",
				setup: func(k *keepRun) {
					if variant.name == "is missing" {
						delete(k.state, field)
						return
					}
					k.state[field] = variant.value
				},
				detail: func(k *keepRun) string { return "the state file " + k.stateFile() + " has no " + field },
			})
		}
	}
	missingContainer := func(k *keepRun) { k.ids = k.slot.container + " oracle-id\n" }
	rows = append(rows,
		keepDownstreamStop{
			name: "k3s container does not exist", check: "k3s-container", setup: missingContainer, everySlot: true,
			detail: func(k *keepRun) string { return "container " + k.slot.container + "-k3s does not exist" },
		},
		keepDownstreamStop{
			name: "k3s container does not exist and downstream_pin_changed is exported", check: "k3s-container",
			setup: func(k *keepRun) {
				missingContainer(k)
				k.s.env["downstream_pin_changed"] = "1"
				k.s.env["DOWNSTREAM_PIN_CHANGED"] = "1"
			},
			detail: func(k *keepRun) string { return "container " + k.slot.container + "-k3s does not exist" },
		},
		keepDownstreamStop{
			name: "k3s container has another id", check: "k3s-container-id", everySlot: true,
			setup: func(k *keepRun) {
				k.ids = k.slot.container + " oracle-id\n" + k.slot.container + "-k3s another-id\n"
			},
			detail: func(k *keepRun) string {
				return "container " + k.slot.container + "-k3s has id another-id, but the state file records k3s-id"
			},
		},
	)
	// The case that stays provisioning keeps a limit of 1 s, so the wait loop
	// runs more than once on every slot; the others read once and stop.
	inactive := func(name, limit, code, body, last string, everySlot bool) keepDownstreamStop {
		return keepDownstreamStop{
			name: name, check: "downstream", hint: keepDownstreamHint, waitLast: last, limit: limit, everySlot: everySlot,
			setup: func(k *keepRun) {
				k.setDownstreamLimit(limit)
				k.canned(keepClusterRead, code, body)
			},
			detail: func(*keepRun) string {
				return "the downstream cluster c-m-abc was not active within " + limit + " s (" + last + ")"
			},
		}
	}
	rows = append(rows,
		inactive("downstream stays provisioning", "1", "200", `{"state":"provisioning"}`, "last state provisioning", true),
		inactive("downstream gives no answer", "0", "000", "", "no HTTP answer", false),
		inactive("downstream answers 404", "0", "404", `{"type":"error","code":"NotFound"}`, "HTTP 404", false),
		inactive("downstream answer has no state", "0", "200", "{}", "no state in the answer", false),
		keepDownstreamStop{
			name: "downstream turns active and its proxy path does not settle", check: "downstream",
			waitLast: "last state provisioning", limit: "120", proxyRead: true, watchSleep: true,
			setup: func(k *keepRun) {
				k.inactiveFirstReads(2, "provisioning")
				k.canned("/k8s/clusters/c-m-abc/version", "503", "")
			},
			detail: func(*keepRun) string {
				return "the downstream cluster c-m-abc turned active, but a later check of it failed (see the error above)"
			},
		},
		keepDownstreamStop{
			name: "downstream turns active and then is not active again", check: "downstream",
			waitLast: "last state provisioning", limit: "120", watchSleep: true, stderr: "did not become active within 10 minutes",
			setup: func(k *keepRun) { k.activeOnlyOnRead(3, "provisioning") },
			detail: func(*keepRun) string {
				return "the downstream cluster c-m-abc turned active, but a later check of it failed (see the error above)"
			},
		},
		keepDownstreamStop{
			name: "downstream turns active and its fleet agent is never ready", check: "downstream",
			waitLast: "last state provisioning", limit: "120", proxyRead: true, fleetRead: true, watchSleep: true,
			stderr: "deployment cattle-fleet-system/fleet-agent on the downstream cluster c-m-abc was not ready within 1 s",
			setup: func(k *keepRun) {
				k.inactiveFirstReads(2, "provisioning")
				k.setFleetLimit("1")
				k.canned("/k8s/clusters/c-m-abc/api/v1/namespaces/cattle-fleet-system", "200", "{}")
				k.canned("/k8s/clusters/c-m-abc/apis/apps/v1/namespaces/cattle-fleet-system/deployments/fleet-agent", "200", `{"spec":{"replicas":1},"status":{"readyReplicas":0}}`)
			},
			detail: func(*keepRun) string {
				return "the downstream cluster c-m-abc turned active, but a later check of it failed (see the error above)"
			},
		},
		keepDownstreamStop{
			name: "a failure that sets no check", check: "unnamed", everySlot: true,
			setup: func(k *keepRun) {
				const anchor = "reuse_downstream() {\n  downstream_pin_changed=\"\"\n"
				k.editScript(anchor, anchor+"  [ -n \"\" ] || return 1\n")
			},
			detail: func(*keepRun) string { return unnamedDetail },
		},
	)
	return rows
}

// requireKeepRefusal asserts the refusal a run printed for a failed check: the
// first line with the check and its detail, the line that names the checks and
// the commands that show why, for a downstream read that got no answer the line
// that names the oracle container too, the hint when there is one, and the
// commands that remove the whole pair.
func requireKeepRefusal(t *testing.T, k *keepRun, stderr, check, detail, hint string) {
	t.Helper()
	c := k.slot.container
	block := []string{
		fmt.Sprintf("error: E2E_FIDELITY_KEEP=1: the kept pair %s on port %d failed its %s check: %s; nothing was removed", c, k.port, check, detail),
		keepFindOutWhyLine(c, check),
	}
	if check == "downstream" && strings.HasSuffix(detail, "(no HTTP answer)") {
		block = append(block, fmt.Sprintf("error: the last read of the downstream cluster got no HTTP answer; also find out why the oracle did not answer (docker ps -a --filter name=%s; docker logs --tail 50 %s)", c, c))
	}
	if hint != "" {
		block = append(block, "error: "+hint)
	}
	if check == "state-file" {
		block = append(block, "error: "+detail+"; remove the pair with:")
	} else {
		block = append(block, "error: to boot a new pair instead, remove this one first, then run again:")
	}
	block = append(block,
		"error:   docker rm -f -v "+shellWord(t, c)+" "+shellWord(t, c+"-k3s"),
		"error:   docker network rm "+shellWord(t, c+"-net"),
		"error:   rm -rf "+shellWord(t, k.root),
	)
	if k.slot.kind == "upcoming" || k.slot.kind == "helm" || k.slot.kind == "upcominghelm" {
		block = append(block, "error: this also deletes drift reports under "+k.root)
	}
	require.Contains(t, stderr, strings.Join(block, "\n")+"\n")
	if hint == "" {
		assert.NotContains(t, stderr, "can take a few minutes; run again")
	}
}

// TestKeepRunStopsOnAFailedDownstreamCheck asserts that under
// E2E_FIDELITY_KEEP=1 a kept downstream that fails its state file, k3s
// container, k3s container id or downstream cluster check stops the run with
// exit 4 on every slot: the refusal names the failed check, the commands that
// show why for the downstream container, and the commands that remove the whole
// pair, and the run removes nothing, imports nothing, writes no state and sends
// no request beyond the checks themselves. A downstream cluster that is not
// active is waited for first, and one that turns active but fails a later check
// stops the run too. A failure that sets no check name stops the run, and a
// variable named downstream_pin_changed in the environment changes nothing.
// Each check has one row on every slot; the variants of a state file value, the
// other words for the last read, the proxy path and the exported variable run on
// the community and helm-check slots. A wait that loses its bound ends the case
// after keepRunawaySeconds with a message that says so.
func TestKeepRunStopsOnAFailedDownstreamCheck(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	var slotNames []string
	for _, slot := range keepSlots {
		slotNames = append(slotNames, slot.name)
	}
	require.Subset(t, slotNames, []string{"community", "helm-check"}, "the slots that run every row are slots of the script")
	for _, slot := range keepSlots {
		for _, row := range keepDownstreamStopRows() {
			if !row.everySlot && slot.name != "community" && slot.name != "helm-check" {
				continue
			}
			t.Run(slot.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				k := newKeepRun(t, slot)
				row.setup(k)
				var runaway []string
				switch {
				case row.limit == "0" || row.limit == "1":
					runaway = k.watchWait()
				case row.watchSleep:
					runaway = []string{k.watchSleep()}
				}
				requests := k.recordRequestOptions()
				res := k.run()

				for _, marker := range runaway {
					_, err := os.Stat(marker)
					require.Truef(t, os.IsNotExist(err), "the wait did not stop at its limit of %s s: the run kept waiting for %d s of real time and was ended\nstdout: %s\nstderr: %s", row.limit, keepRunawaySeconds, res.stdout, res.stderr)
				}
				require.NotContains(t, res.stdout, keepToken, "the admin token must never be printed")
				require.NotContains(t, res.stderr, keepToken, "the admin token must never be printed")
				require.Equalf(t, 4, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
				requireKeepRefusal(t, k, res.stderr, row.check, row.detail(k), row.hint)
				if slot.kind == "community" || slot.kind == "prime" {
					assert.NotContains(t, res.stderr, "drift reports", "a slot that allows no drift run has no drift reports")
				}

				assert.Contains(t, res.stdout, "==> reusing healthy kept container")
				if row.waitLast != "" {
					assert.Contains(t, res.stdout, "==> waiting up to "+row.limit+" s for the kept downstream cluster c-m-abc to be active ("+row.waitLast+")")
				} else {
					assert.NotContains(t, res.stdout, "==> waiting up to")
				}
				assert.NotContains(t, res.stdout, "rebuilding the kept downstream")
				assert.NotContains(t, res.stdout, "recreating")

				requireKeptRunDockerCalls(t, slot.container, res.log)
				requireNoRequestOptions(t, requests)
				allowed := []string{k.base() + "/ping", k.base() + "/v3/users?me=true", k.base() + keepClusterRead}
				if row.proxyRead {
					allowed = append(allowed, k.base()+"/k8s/clusters/c-m-abc/version")
				}
				if row.fleetRead {
					allowed = append(allowed,
						k.base()+"/k8s/clusters/c-m-abc/api/v1/namespaces/cattle-fleet-system",
						k.base()+"/k8s/clusters/c-m-abc/apis/apps/v1/namespaces/cattle-fleet-system/deployments/fleet-agent")
				}
				if row.stderr != "" {
					assert.Contains(t, res.stderr, row.stderr)
				}
				for _, line := range k.s.curlLog() {
					if rest, ok := strings.CutPrefix(line, "canned "); ok {
						assert.Containsf(t, allowed, rest, "the curl log: %v", k.s.curlLog())
						continue
					}
					chartOnly := slot.kind == "upcominghelm" && line == "real "+k.s.helm.chartURL
					assert.Truef(t, chartOnly, "request %q must not happen: %v", line, k.s.curlLog())
				}
				_, called := k.s.goEnv()
				assert.False(t, called, "no test run starts after a refusal")
				assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file are not written")
			})
		}
	}
}

// TestKeepRunWaitsForAKeptDownstreamToTurnActive asserts that on every slot a
// kept downstream cluster that is not active on its first read is waited for:
// the run prints the wait line, reads the cluster until it is active, then reads
// the proxy path until it answers steadily and the fleet agent, in that order,
// and goes on as with a reused downstream, with no rebuild and no removal. A
// downstream that is active on its first read makes no wait, no proxy read and
// no fleet read.
func TestKeepRunWaitsForAKeptDownstreamToTurnActive(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		t.Run(slot.name+"/not active on the first reads", func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, slot)
			k.inactiveFirstReads(3, "provisioning")
			k.cannedFleetAgent()
			start := time.Now()
			res := k.run()

			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Less(t, time.Since(start), 60*time.Second, "a read without a canned row spins on the real clock")
			requireOrder(t, strings.Split(res.stdout, "\n"),
				"==> reusing healthy kept container",
				"==> waiting up to 120 s for the kept downstream cluster c-m-abc to be active (last state provisioning)",
				"==> waiting for the downstream cluster c-m-abc to be active",
				"==> waiting for the proxy path to /k8s/clusters/c-m-abc to settle",
				"==> waiting for the fleet agent on the downstream cluster c-m-abc",
			)
			assert.NotContains(t, res.stdout, "rebuilding the kept downstream")
			for _, prefix := range []string{"rm ", "run ", "start ", "network rm", "network create", "container rm"} {
				assert.Falsef(t, hasCall(res.log, prefix), "a reused pair makes no %q call: %v", prefix, res.log)
			}

			clusterReads, lastCluster, namespace, agent, versions := 0, -1, -1, -1, 0
			for i, line := range k.s.curlLog() {
				switch {
				case strings.HasSuffix(line, keepClusterRead) && namespace < 0:
					clusterReads++
					lastCluster = i
				case strings.HasSuffix(line, "/k8s/clusters/c-m-abc/version") && namespace < 0:
					versions++
					assert.Greater(t, lastCluster, -1, "the proxy path is read after the cluster")
				case strings.HasSuffix(line, "/k8s/clusters/c-m-abc/api/v1/namespaces/cattle-fleet-system"):
					namespace = i
				case strings.HasSuffix(line, "/k8s/clusters/c-m-abc/apis/apps/v1/namespaces/cattle-fleet-system/deployments/fleet-agent"):
					agent = i
				case strings.Contains(line, "/v1/provisioning.cattle.io.clusters") || strings.Contains(line, "/v3/clusterregistrationtokens") || strings.Contains(line, "/v3/import"):
					assert.Failf(t, "no import", "request %q must not happen", line)
				}
			}
			assert.Equal(t, 5, clusterReads, "three reads that are not active, the read that is, and the read of the wait for the downstream")
			assert.GreaterOrEqual(t, versions, 6, "the proxy path answered steadily before the fleet reads")
			require.Greater(t, namespace, lastCluster, "the namespace is read after the last cluster read")
			assert.Greater(t, agent, namespace, "the Deployment is read after the namespace")
			assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file stay")
		})

		t.Run(slot.name+"/active on the first read", func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, slot)
			res := k.run()

			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			for _, text := range []string{
				"==> waiting up to", "==> waiting for the downstream cluster", "==> waiting for the proxy path", "==> waiting for the fleet agent",
			} {
				assert.NotContainsf(t, res.stdout, text, "an active downstream is reused as it is")
			}
			for _, line := range k.s.curlLog() {
				assert.NotContains(t, line, "cattle-fleet-system", "an active downstream makes no fleet read")
			}
		})
	}
}

// TestKeepRunRebuildsAMovedK3sImageBeforeCheckingItsContainer asserts that on
// every slot a downstream whose k3s image moved is rebuilt even when its
// container is gone: the image is compared before the container, so the case
// is a pin change and not a failed check.
func TestKeepRunRebuildsAMovedK3sImageBeforeCheckingItsContainer(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		t.Run(slot.name, func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, slot)
			k.moveK3sImage()
			k.cannedImport()
			k.ids = slot.container + " oracle-id\n"
			res := k.run()

			assert.NotEqualf(t, 4, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.NotContains(t, res.stderr, "failed its")
			assert.Contains(t, res.stdout, "==> kept downstream was created from a different k3s image; rebuilding it")
			assert.Contains(t, res.stdout, "==> rebuilding the kept downstream against the running oracle (a different k3s image)")
			assert.True(t, hasCall(res.log, "run -d --name "+slot.container+"-k3s --privileged --restart unless-stopped --network"), "the downstream container is created again and keeps its restart policy: %v", res.log)
			assert.Contains(t, k.s.curlLog(), "canned "+k.base()+"/v1/provisioning.cattle.io.clusters/fleet-default/downstream")
		})
	}
}

// TestReuseDownstreamFailuresSetAReason asserts, from the text of
// reuse_downstream, that no failure leaves it without saying which kind it is:
// it starts by clearing its result variables, every return of a failure sits in
// a group that either reports a changed k3s image or names the failed check and
// its detail, no check fails by a trailing "|| return 1", and it ends with an
// explicit success. The check names it sets are the ones the refusal tests
// cover.
func TestReuseDownstreamFailuresSetAReason(t *testing.T) {
	lines := functionBody(t, "reuse_downstream")
	body := lines[1 : len(lines)-1]

	require.GreaterOrEqual(t, len(body), 5)
	for i, want := range []string{`downstream_pin_changed=""`, `REUSE_FAILED_CHECK=""`, `REUSE_FAILED_DETAIL=""`, `REUSE_FAILED_HINT=""`} {
		assert.Equal(t, want, strings.TrimSpace(body[i]), "the first statements clear the result variables")
	}
	var last string
	for _, line := range body {
		if strings.TrimSpace(line) != "" {
			last = strings.TrimSpace(line)
		}
	}
	assert.Equal(t, "return 0", last, "the last statement is an explicit success")

	returns, names := requireFailureGroupsSetAReason(t, lines, "downstream_pin_changed")
	assert.Greater(t, returns, 5, "the scan reads the failure groups")

	// Only a changed k3s image rebuilds the downstream: the one non-empty
	// assignment of downstream_pin_changed sits in the group that compares the
	// k3s image of the state file with the pinned one.
	assignsPin := regexp.MustCompile(`downstream_pin_changed="[^"]+"`)
	var assigned []int
	for i, line := range lines {
		if assignsPin.MatchString(line) {
			assigned = append(assigned, i)
		}
	}
	require.Len(t, assigned, 1, "downstream_pin_changed is set to a reason in exactly one place")
	opener := groupOpener(lines, assigned[0])
	require.Greater(t, opener, 0)
	assert.Equal(t, `[ "${saved_k3s_image}" = "${K3S_IMAGE}" ] || {`, strings.TrimSpace(lines[opener]), "only a changed k3s image rebuilds the downstream")

	var want []string
	for _, row := range keepDownstreamStopRows() {
		if row.check != "unnamed" {
			want = append(want, row.check)
		}
	}
	assert.ElementsMatch(t, slices.Compact(slices.Sorted(slices.Values(want))), slices.Sorted(maps.Keys(names)), "the check names reuse_downstream sets and the names the refusal tests cover")

	// Each check keeps one row that runs on every slot.
	for _, check := range want {
		onEverySlot := slices.ContainsFunc(keepDownstreamStopRows(), func(row keepDownstreamStop) bool {
			return row.check == check && row.everySlot
		})
		assert.Truef(t, onEverySlot, "the check %s has no row that runs on every slot", check)
	}
}
