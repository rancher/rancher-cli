package fidelity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPairInstallUUID is the install id the default row of the curl stub gives
// for the install-uuid setting of a pair.
const testPairInstallUUID = "5d3c1c8e-0000-4000-8000-000000000001"

// otherPairInstallUUID differs from testPairInstallUUID, so a test can tell
// the value of a state file or of an earlier row from the default row.
const otherPairInstallUUID = "5d3c1c8e-0000-4000-8000-000000000002"

const (
	installUUIDFailure = "error: cannot read the install-uuid setting of the pair"
	installUUIDEnv     = "E2E_FIDELITY_PAIR_INSTALL_UUID"
)

// installUUIDReads is the number of requests of the run for the install-uuid
// setting.
func (k *keepRun) installUUIDReads() int {
	k.t.Helper()
	n := 0
	for _, line := range k.s.curlLog() {
		if strings.HasSuffix(line, installUUIDPath) {
			n++
		}
	}
	return n
}

// decodeStateFile is the JSON object of a state file's content.
func decodeStateFile(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var state map[string]any
	require.NoError(t, json.Unmarshal(raw, &state))
	return state
}

// TestKeepRunReadsTheInstallIdOfAStateFileWithoutOne asserts that on every slot
// a reused pair whose state file has no installUuid key reads the install-uuid
// setting once, leaves the state file and the CA file byte for byte as they
// were, and on the community slot, the one whose run reaches the test run,
// gives the value to it in E2E_FIDELITY_PAIR_INSTALL_UUID.
func TestKeepRunReadsTheInstallIdOfAStateFileWithoutOne(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		t.Run(slot.name, func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, slot)
			require.NotContains(t, k.state, "installUuid")
			res := k.run()

			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Equal(t, 1, k.installUUIDReads(), "the value is read once: %v", k.s.curlLog())
			assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file stay")
			assert.NotContains(t, decodeStateFile(t, k.before["state.json"]), "installUuid")
			if slot.name == "community" {
				env, called := k.s.goEnv()
				require.True(t, called, "the test run starts after the reuse")
				assert.Equal(t, testPairInstallUUID, env[installUUIDEnv])
			}
		})
	}
}

// TestKeepRunRebuildRecordsTheInstallId asserts that on every slot a run that
// rebuilds the kept downstream records the install id it read in the state
// file, and changes nothing else in it but the k3s image the rebuild is for.
func TestKeepRunRebuildRecordsTheInstallId(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		t.Run(slot.name, func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, slot)
			k.moveK3sImage()
			k.cannedImport()
			res := k.run()

			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Contains(t, res.stdout, "==> rebuilding the kept downstream against the running oracle (a different k3s image)")
			assert.Equal(t, 1, k.installUUIDReads(), "the value is read once: %v", k.s.curlLog())

			raw, err := os.ReadFile(k.stateFile())
			require.NoError(t, err)
			after := decodeStateFile(t, raw)
			assert.Equal(t, testPairInstallUUID, after["installUuid"])
			before := decodeStateFile(t, k.before["state.json"])
			delete(after, "installUuid")
			assert.NotEqual(t, before["k3sImage"], after["k3sImage"], "the rebuild records the k3s image of the run")
			after["k3sImage"] = before["k3sImage"]
			assert.Equal(t, before, after, "a rebuild changes the k3s image and nothing else")
			if slot.name == "community" {
				env, called := k.s.goEnv()
				require.True(t, called, "the test run starts after the rebuild")
				assert.Equal(t, testPairInstallUUID, env[installUUIDEnv])
			}
		})
	}
}

// TestKeepRunUsesTheInstallIdOfTheStateFile asserts that on every slot a kept
// pair whose state file records an install id sends no request for the
// install-uuid setting, reused or with a rebuilt downstream, keeps the value in
// the state file, and on the community slot gives it to the test run.
func TestKeepRunUsesTheInstallIdOfTheStateFile(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		for _, rebuild := range []bool{false, true} {
			name := slot.name + "/reused"
			if rebuild {
				name = slot.name + "/rebuilt"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				k := newKeepRun(t, slot)
				k.state["installUuid"] = otherPairInstallUUID
				if rebuild {
					k.moveK3sImage()
					k.cannedImport()
				}
				res := k.run()

				require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
				for _, line := range k.s.curlLog() {
					assert.NotContains(t, line, "install-uuid", "a state file with the value makes no read")
				}
				raw, err := os.ReadFile(k.stateFile())
				require.NoError(t, err)
				assert.Equal(t, otherPairInstallUUID, decodeStateFile(t, raw)["installUuid"])
				if !rebuild {
					assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file stay")
				}
				if slot.name == "community" {
					env, called := k.s.goEnv()
					require.True(t, called, "the test run starts")
					assert.Equal(t, otherPairInstallUUID, env[installUUIDEnv])
				}
			})
		}
	}
}

// TestKeepRunStopsOnAnInstallIdThatCannotBeRead asserts that on every slot a
// reused pair whose state file has no install id, and whose install-uuid
// setting answers with an error body, an empty or missing value, a value that
// is not a string, an empty body or a body that is not a JSON object, stops
// with exit 1 and the message that names the setting, before the downstream
// is checked and before the test run, and leaves the state file as it was. A
// row added before the run answers before the default row of the case, so each
// row stops the run only if the first matching row wins.
func TestKeepRunStopsOnAnInstallIdThatCannotBeRead(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	rows := []struct{ name, code, body string }{
		{"error status with a text body", "500", "internal server error"},
		{"empty value without a default", "200", `{"value":""}`},
		{"empty value and an empty default", "200", `{"value":"","default":""}`},
		{"no value and no default", "200", `{}`},
		{"value that is not a string", "200", `{"value":5}`},
		{"body that is not JSON", "200", "not json"},
		{"body that is a JSON array", "200", `[]`},
		{"empty body", "200", ""},
	}
	for _, slot := range keepSlots {
		for _, row := range rows {
			t.Run(slot.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				k := newKeepRun(t, slot)
				k.canned(installUUIDPath, row.code, row.body)
				res := k.run()

				require.Equalf(t, 1, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
				assert.Contains(t, res.stderr, installUUIDFailure)
				assert.NotContains(t, res.stdout, keepToken, "the admin token must never be printed")
				assert.NotContains(t, res.stderr, keepToken, "the admin token must never be printed")
				assert.Contains(t, res.stdout, "==> reusing healthy kept container")
				assert.Equal(t, 1, k.installUUIDReads(), "the value is read once: %v", k.s.curlLog())
				for _, line := range k.s.curlLog() {
					assert.False(t, strings.HasSuffix(line, keepClusterRead), "the downstream is not checked: %v", k.s.curlLog())
				}
				_, called := k.s.goEnv()
				assert.False(t, called, "no test run starts after the stop")
				assert.Equal(t, k.before, k.snapshot(), "the state file and the CA file are not written")
				for _, prefix := range []string{"rm ", "run ", "start ", "network rm", "network create", "container rm"} {
					assert.Falsef(t, hasCall(res.log, prefix), "a stop removes and starts nothing, no %q call: %v", prefix, res.log)
				}
			})
		}
	}
}

// TestKeepRunGivesTheTestRunTheInstallIdItRead asserts that on the community
// slot the test run gets, in E2E_FIDELITY_PAIR_INSTALL_UUID, the value of the
// install-uuid setting, else its default when the value is null, empty or
// missing. Each row is added before the run, so each also asserts that the
// first matching row of the curl stub wins over the default row of the case.
func TestKeepRunGivesTheTestRunTheInstallIdItRead(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	const value, fallback = "5d3c1c8e-0000-4000-8000-0000000000a1", "5d3c1c8e-0000-4000-8000-0000000000b2"
	rows := []struct{ name, body, want string }{
		{"the value", `{"value":"` + value + `"}`, value},
		{"the value over the default", `{"value":"` + value + `","default":"` + fallback + `"}`, value},
		{"the default for an empty value", `{"value":"","default":"` + fallback + `"}`, fallback},
		{"the default for a null value", `{"value":null,"default":"` + fallback + `"}`, fallback},
		{"the default for a missing value", `{"default":"` + fallback + `"}`, fallback},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			k := newKeepRun(t, keepSlots[0])
			require.Equal(t, "community", k.slot.name)
			k.canned(installUUIDPath, "200", row.body)
			res := k.run()

			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			env, called := k.s.goEnv()
			require.True(t, called, "the test run starts")
			assert.Equal(t, row.want, env[installUUIDEnv])
			assert.NotEqual(t, testPairInstallUUID, env[installUUIDEnv], "the row of the case wins over the default row")
		})
	}
}

// installIDSiteProblems lists what is wrong with the lines of
// scripts/e2e-fidelity at the places that carry the install id of the pair: the
// read in the reuse branch is after the flag that marks the run as holding its
// container and before the downstream check, the read of a fresh boot is after
// the admin token is created and before the state file is written, the
// variable is exported to the test run after the oracle image, nothing else
// calls the read, and both test runs of the suite carry the same time limit.
func installIDSiteProblems(lines []string) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	text := func(i int) string {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "#") {
			return ""
		}
		return line
	}
	find := func(from, to int, match func(string) bool) int {
		for i := from; i < to; i++ {
			if match(text(i)) {
				return i
			}
		}
		return -1
	}
	equals := func(want string) func(string) bool { return func(line string) bool { return line == want } }

	reuseIf := slices.Index(lines, `if [ "${E2E_FIDELITY_KEEP:-}" = "1" ] && reuse_state; then`)
	if reuseIf < 0 {
		return append(problems, "the reuse test changed shape")
	}
	elseLine := slices.Index(lines[reuseIf:], "else") + reuseIf
	fiLine := slices.Index(lines[reuseIf:], "fi") + reuseIf
	if elseLine <= reuseIf || fiLine <= elseLine {
		return append(problems, "the reuse branch and the fresh boot changed shape")
	}

	booted := find(reuseIf+1, elseLine, equals("ORACLE_BOOTED=1"))
	fromState := find(reuseIf+1, elseLine, equals(`PAIR_INSTALL_UUID="$(state_value installUuid)"`))
	reuseRead := find(reuseIf+1, elseLine, equals(`[ -n "${PAIR_INSTALL_UUID}" ] || read_pair_install_uuid`))
	downstream := find(reuseIf+1, elseLine, equals("if ! reuse_downstream; then"))
	switch {
	case min(booted, fromState, reuseRead, downstream) < 0:
		add("the reuse branch lacks one of the flag (%d), the read of the state file (%d), the read of the setting for a state file without it (%d) or the downstream check (%d)", booted, fromState, reuseRead, downstream)
	case booted >= fromState || fromState >= reuseRead || reuseRead >= downstream:
		add("the reuse branch must read the install id after the flag and before the downstream check: flag %d, state file %d, setting %d, downstream %d", booted, fromState, reuseRead, downstream)
	}

	adminToken := find(elseLine+1, fiLine, func(line string) bool { return strings.HasPrefix(line, `ADMIN_TOKEN="$(`) })
	freshRead := find(elseLine+1, fiLine, equals("read_pair_install_uuid"))
	writeState := find(elseLine+1, fiLine, equals(`[ "${E2E_FIDELITY_KEEP:-}" != "1" ] || write_state`))
	switch {
	case min(adminToken, freshRead, writeState) < 0:
		add("the fresh boot lacks one of the admin token (%d), the read of the setting (%d) or the write of the state file (%d)", adminToken, freshRead, writeState)
	case adminToken >= freshRead || freshRead >= writeState:
		add("a fresh boot must read the install id after the admin token and before the state file is written: token %d, setting %d, state file %d", adminToken, freshRead, writeState)
	}

	calls := 0
	for i := range lines {
		if strings.Contains(text(i), "read_pair_install_uuid") {
			calls++
		}
	}
	if calls != 3 {
		add("read_pair_install_uuid is named on %d lines, want its definition and its two calls", calls)
	}

	oracleExport := find(fiLine+1, len(lines), equals(`export E2E_FIDELITY_ORACLE_IMAGE="${ORACLE_IMAGE}"`))
	pairExport := find(fiLine+1, len(lines), equals(`export E2E_FIDELITY_PAIR_INSTALL_UUID="${PAIR_INSTALL_UUID}"`))
	switch {
	case min(oracleExport, pairExport) < 0:
		add("the exports after the boot lack the oracle image (%d) or the install id of the pair (%d)", oracleExport, pairExport)
	case pairExport < oracleExport:
		add("the install id of the pair is exported before the oracle image: %d, %d", pairExport, oracleExport)
	}
	named := 0
	for i := range lines {
		if strings.Contains(text(i), installUUIDEnv) {
			named++
		}
	}
	if named != 1 {
		add("%s is named on %d lines, want its export alone", installUUIDEnv, named)
	}

	suiteRuns := 0
	for i := range lines {
		line := text(i)
		if !strings.Contains(line, "go test ") || !strings.Contains(line, "./test/e2e/fidelity/") {
			continue
		}
		suiteRuns++
		if !strings.Contains(line, " -timeout=20m ") {
			add("a run of the fidelity suite has no -timeout=20m: %s", line)
		}
	}
	if suiteRuns != 2 {
		add("the script runs the fidelity suite on %d lines, want 2", suiteRuns)
	}
	return problems
}

// TestInstallIdSitesOfTheScript asserts, from the lines of scripts/e2e-fidelity,
// where the install id of the pair is read and exported: a fresh boot reads it
// after the admin token and before the state file is written, the reuse branch
// reads it, only when the state file has none, after the flag that marks the
// run as holding its container and before the downstream check, the variable is
// exported after the oracle image, no other line calls the read, and both runs
// of the fidelity suite carry -timeout=20m. No fixture reaches a fresh boot, so
// the order is read from the text. Each row of a table of changed copies of the
// lines must fail the scan.
func TestInstallIdSitesOfTheScript(t *testing.T) {
	t.Parallel()
	lines := scriptLines(t)
	assert.Empty(t, installIDSiteProblems(lines))

	lineOf := func(t *testing.T, lines []string, want string) int {
		t.Helper()
		var found []int
		for i, line := range lines {
			if strings.TrimSpace(line) == want {
				found = append(found, i)
			}
		}
		require.Lenf(t, found, 1, "the line %q appears once", want)
		return found[0]
	}
	remove := func(want string) func(t *testing.T, lines []string) []string {
		return func(t *testing.T, lines []string) []string {
			t.Helper()
			return slices.Delete(lines, lineOf(t, lines, want), lineOf(t, lines, want)+1)
		}
	}
	swap := func(a, b string) func(t *testing.T, lines []string) []string {
		return func(t *testing.T, lines []string) []string {
			t.Helper()
			i, j := lineOf(t, lines, a), lineOf(t, lines, b)
			lines[i], lines[j] = lines[j], lines[i]
			return lines
		}
	}
	const (
		reuseRead  = `[ -n "${PAIR_INSTALL_UUID}" ] || read_pair_install_uuid`
		freshRead  = "read_pair_install_uuid"
		fromState  = `PAIR_INSTALL_UUID="$(state_value installUuid)"`
		downstream = "if ! reuse_downstream; then"
		writeState = `[ "${E2E_FIDELITY_KEEP:-}" != "1" ] || write_state`
		oracleExp  = `export E2E_FIDELITY_ORACLE_IMAGE="${ORACLE_IMAGE}"`
		pairExp    = `export E2E_FIDELITY_PAIR_INSTALL_UUID="${PAIR_INSTALL_UUID}"`
	)
	mutations := []struct {
		name   string
		change func(t *testing.T, lines []string) []string
	}{
		{"the read of the reuse branch is removed", remove(reuseRead)},
		{"the read of the reuse branch runs after the downstream check", swap(reuseRead, downstream)},
		{"the read of the reuse branch is unconditional", func(t *testing.T, lines []string) []string {
			t.Helper()
			lines[lineOf(t, lines, reuseRead)] = "  read_pair_install_uuid"
			return lines
		}},
		{"the state file is not read in the reuse branch", remove(fromState)},
		{"the read of a fresh boot is removed", remove(freshRead)},
		{"the read of a fresh boot runs after the state file is written", swap(freshRead, writeState)},
		{"the read of a fresh boot runs before the admin token", func(t *testing.T, lines []string) []string {
			t.Helper()
			i := lineOf(t, lines, freshRead)
			lines = slices.Delete(lines, i, i+1)
			at := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), `ADMIN_TOKEN="$(curl`) })
			require.GreaterOrEqual(t, at, 0)
			return slices.Insert(lines, at, "  read_pair_install_uuid")
		}},
		{"a third call of the read", func(_ *testing.T, lines []string) []string {
			return append(lines, "read_pair_install_uuid")
		}},
		{"the export is removed", remove(pairExp)},
		{"the export is before the oracle image", swap(pairExp, oracleExp)},
		{"the export is named twice", func(_ *testing.T, lines []string) []string {
			return append(lines, "export "+installUUIDEnv+"=x")
		}},
		{"a run of the suite has no time limit", func(_ *testing.T, lines []string) []string {
			i := slices.IndexFunc(lines, func(line string) bool {
				return strings.Contains(line, "go test -tags e2e_fidelity -count=1 -timeout=20m ./test/e2e/fidelity/ ${UPDATE_FLAG}")
			})
			require.GreaterOrEqual(t, i, 0)
			lines[i] = strings.Replace(lines[i], " -timeout=20m", "", 1)
			return lines
		}},
		{"a third run of the suite", func(_ *testing.T, lines []string) []string {
			return append(lines, "go test -tags e2e_fidelity -count=1 -timeout=20m ./test/e2e/fidelity/")
		}},
	}
	for _, row := range mutations {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			changed := row.change(t, slices.Clone(lines))
			assert.NotEmpty(t, installIDSiteProblems(changed), "the scan accepts the changed lines")
		})
	}
}

// handRunPairVariableLines lists the lines of a script's text that assign or
// export E2E_FIDELITY_PAIR_INSTALL_UUID or E2E_FIDELITY_ORACLE_IMAGE: a
// plain or exported assignment, a declaration, a prefix of a command, and the
// read and printf -v builtins. Comments are skipped and a backslash
// continuation is one line.
func handRunPairVariableLines(text string) []string {
	const name = `E2E_FIDELITY_(?:PAIR_INSTALL_UUID|ORACLE_IMAGE)`
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + name + `\+?=`),
		regexp.MustCompile(`\b(?:export|declare|typeset|readonly|local)\b[^#]*\b` + name + `\b`),
		regexp.MustCompile(`\b(?:read|printf\s+-v)\b[^#]*\b` + name + `\b`),
	}
	var found []string
	for _, line := range joinContinuations(strings.Split(text, "\n")) {
		if isCommentLine(line) {
			continue
		}
		if slices.ContainsFunc(patterns, func(re *regexp.Regexp) bool { return re.MatchString(line) }) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// TestHandRunComparisonNeverSetsThePairVariables asserts that
// scripts/e2e-fidelity-downstream and scripts/e2e-fidelity-downstream-update,
// which compare a server the caller runs, never assign or export
// E2E_FIDELITY_PAIR_INSTALL_UUID or E2E_FIDELITY_ORACLE_IMAGE: the probes that
// write only to the pair scripts/e2e-fidelity boots take the pair for the
// server of a run that has the first variable set, and the recorded image comes
// from the second. A table of changed copies of the text must fail the check, and
// a line that only names the variables, in a comment or a list, passes.
func TestHandRunComparisonNeverSetsThePairVariables(t *testing.T) {
	t.Parallel()
	dir, _ := scriptFiles(t)
	texts := map[string]string{}
	for _, name := range []string{"e2e-fidelity-downstream", "e2e-fidelity-downstream-update"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		texts[name] = string(raw)
		assert.Emptyf(t, handRunPairVariableLines(string(raw)), "scripts/%s sets a variable of the pair", name)
	}

	mutations := []struct{ name, add string }{
		{"export of the install id", "export E2E_FIDELITY_PAIR_INSTALL_UUID=x"},
		{"export of the install id from a variable", `export E2E_FIDELITY_PAIR_INSTALL_UUID="${PAIR_INSTALL_UUID}"`},
		{"assignment of the install id", "E2E_FIDELITY_PAIR_INSTALL_UUID=x"},
		{"assignment of the oracle image", "E2E_FIDELITY_ORACLE_IMAGE=y"},
		{"export of the oracle image", `export E2E_FIDELITY_ORACLE_IMAGE="${ORACLE_IMAGE}"`},
		{"export of the oracle image with an assignment", "export E2E_FIDELITY_ORACLE_IMAGE=y"},
		{"export of a name set before", "export E2E_FIDELITY_ORACLE_IMAGE"},
		{"declaration with the export flag", "declare -x E2E_FIDELITY_PAIR_INSTALL_UUID=x"},
		{"prefix of the suite run", "E2E_FIDELITY_PAIR_INSTALL_UUID=x go test ./test/e2e/fidelity/"},
		{"prefix of env", "env E2E_FIDELITY_ORACLE_IMAGE=y go test ./test/e2e/fidelity/"},
		{"assignment in a continued command", "go test \\\n  E2E_FIDELITY_ORACLE_IMAGE=y ./test/e2e/fidelity/"},
		{"read into the variable", "read -r E2E_FIDELITY_PAIR_INSTALL_UUID"},
		{"printf into the variable", "printf -v E2E_FIDELITY_ORACLE_IMAGE %s y"},
		{"appended value", "E2E_FIDELITY_ORACLE_IMAGE+=y"},
	}
	for name, text := range texts {
		for _, row := range mutations {
			t.Run(name+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				assert.NotEmpty(t, handRunPairVariableLines(text+"\n"+row.add+"\n"), "the check accepts the added line")
			})
		}
		t.Run(name+"/names only", func(t *testing.T) {
			t.Parallel()
			names := text + "\n# E2E_FIDELITY_PAIR_INSTALL_UUID=x is set by scripts/e2e-fidelity\n" +
				"for var in E2E_FIDELITY_PAIR_INSTALL_UUID E2E_FIDELITY_ORACLE_IMAGE; do\n" +
				"  [ -z \"${!var:-}\" ] || echo \"error: ${var} is set\" >&2\n" +
				"done\n" +
				"[ -z \"${E2E_FIDELITY_ORACLE_IMAGE:-}\" ] || exit 1\n"
			assert.Empty(t, handRunPairVariableLines(names))
		})
	}
}

// installIDUnsetProblems checks the text of scripts/e2e-fidelity-downstream:
// a line unsets E2E_FIDELITY_PAIR_INSTALL_UUID before the line that runs the
// suite with go test. It returns one problem per rule broken.
func installIDUnsetProblems(text string) []string {
	unset := regexp.MustCompile(`^\s*unset\b[^#]*\bE2E_FIDELITY_PAIR_INSTALL_UUID\b`)
	goTest := regexp.MustCompile(`^\s*go test\b`)
	unsetAt, goTestAt := -1, -1
	for i, line := range joinContinuations(strings.Split(text, "\n")) {
		if isCommentLine(line) {
			continue
		}
		if unsetAt < 0 && unset.MatchString(line) {
			unsetAt = i
		}
		if goTestAt < 0 && goTest.MatchString(line) {
			goTestAt = i
		}
	}
	var problems []string
	switch {
	case goTestAt < 0:
		problems = append(problems, "no go test line")
	case unsetAt < 0:
		problems = append(problems, "no line unsets E2E_FIDELITY_PAIR_INSTALL_UUID")
	case unsetAt > goTestAt:
		problems = append(problems, "E2E_FIDELITY_PAIR_INSTALL_UUID is unset after the go test line")
	}
	return problems
}

// TestHandRunComparisonUnsetsTheInstallID asserts that
// scripts/e2e-fidelity-downstream unsets E2E_FIDELITY_PAIR_INSTALL_UUID before
// it runs the suite, so a value the caller's shell holds never lets the
// catalog write probes write to the server it compares. A copy without the
// unset, and one with the unset after the go test line, must fail the check.
func TestHandRunComparisonUnsetsTheInstallID(t *testing.T) {
	t.Parallel()
	dir, _ := scriptFiles(t)
	raw, err := os.ReadFile(filepath.Join(dir, "e2e-fidelity-downstream"))
	require.NoError(t, err)
	text := string(raw)
	assert.Empty(t, installIDUnsetProblems(text))

	const unsetLine = "unset E2E_FIDELITY_PAIR_INSTALL_UUID\n"
	require.Equal(t, 1, strings.Count(text, unsetLine), "the script unsets the variable on one line")
	dropped := strings.Replace(text, unsetLine, "", 1)
	assert.Equal(t, []string{"no line unsets E2E_FIDELITY_PAIR_INSTALL_UUID"}, installIDUnsetProblems(dropped))
	moved := strings.TrimRight(dropped, "\n") + "\n" + unsetLine
	assert.Equal(t, []string{"E2E_FIDELITY_PAIR_INSTALL_UUID is unset after the go test line"}, installIDUnsetProblems(moved))
}
