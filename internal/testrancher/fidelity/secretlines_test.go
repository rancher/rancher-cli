package fidelity

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const approvedAdminHeader = `-H @<(printf 'Authorization: Bearer %s\n' "${ADMIN_TOKEN}")`
const approvedLoginHeader = `-H @<(printf 'Authorization: Bearer %s\n' "${LOGIN_TOKEN}")`

// secretVariableLines are the lines of scripts/e2e-fidelity that expand
// ADMIN_TOKEN, LOGIN_TOKEN, BOOTSTRAP_PW or saved_token (the local variable of
// reuse_state that holds the admin token read from the state file), each with
// the function it sits in (empty for the main flow) and why it is allowed.
func secretVariableLines(t *testing.T) []reviewedLine {
	t.Helper()
	return []reviewedLine{
		{"redact_log", `if out="$(BOOTSTRAP_PW="${BOOTSTRAP_PW:-}" LOGIN_TOKEN="${LOGIN_TOKEN:-}" ADMIN_TOKEN="${ADMIN_TOKEN:-}" python3 -I -c "${program}" "${prefix}")"; then`,
			"prefix assignments: the three values are in the environment of that one command, and its arguments are the program text and the log prefix"},
		{"api", `curl -s --cacert "${CA_FILE}" --max-time 30 ` + approvedAdminHeader + ` -H 'Content-Type: application/json' "$@"`,
			"printf is a builtin that writes a pipe, and curl's arguments hold only the name of the pipe"},
		{"reuse_state", `[ -n "${ADMIN_TOKEN}" ] || {`,
			"[ is a builtin: no process runs and no argument list is built"},
		{"reuse_state", `code="$(curl -s --cacert "${CA_FILE}" -o /dev/null -w '%{http_code}' ` + approvedAdminHeader + ` "${BASE_URL}/v3/users?me=true")" || true`,
			"the approved header form: the token is written to a pipe by the builtin printf"},
		{"reuse_state", `ADMIN_TOKEN="${saved_token}"`,
			"an assignment: no process runs and no argument list is built"},
		{"write_state", `ADMIN_TOKEN="${ADMIN_TOKEN}" python3 -c '`,
			"a prefix assignment: the token is in the environment of that one command, and the single-quoted program has no expansion"},
		{"create_bootstrap_secret", `bootstrapPassword: "${BOOTSTRAP_PW}"`,
			"a here-document body: it is the standard input of kubectl create -f -, not an argument"},
		{loginOwner(t), `{"username":"admin","password":"${BOOTSTRAP_PW}"}`,
			"a here-document body: it is the standard input of curl -d @-, not an argument"},
		{"", `( umask 177; printf 'CATTLE_BOOTSTRAP_PASSWORD=%s\n' "${BOOTSTRAP_PW}" >"${ENV_FILE}" )`,
			"the builtin printf writes a file that only its owner can read, and docker reads it with --env-file"},
		{"", `curl -s --cacert "${CA_FILE}" -o /dev/null "${BASE_URL}/v3/settings/server-url" -X PUT ` + approvedLoginHeader + ` -H 'Content-Type: application/json' -d "{\"value\":\"https://${ORACLE_IP}\"}"`,
			"the approved header form: the token is written to a pipe by the builtin printf"},
		{"", `ADMIN_TOKEN="$(curl -s --cacert "${CA_FILE}" "${BASE_URL}/v3/tokens" ` + approvedLoginHeader + ` -H 'Content-Type: application/json' -d @- <<'JSON' | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])'`,
			"the approved header form: the token is written to a pipe by the builtin printf"},
		{"", `export E2E_FIDELITY_ADMIN_TOKEN="${ADMIN_TOKEN}"`,
			"export is a builtin: the value goes into the environment of later commands, not an argument list"},
		{"", `code="$(curl -s --cacert "${CA_FILE}" -o /dev/null -w '%{http_code}' ` + approvedAdminHeader + ` "${BASE_URL}/v3/users?me=true")"`,
			"the approved header form: the token is written to a pipe by the builtin printf"},
		{"", `code="$(curl -s --cacert "${CA_FILE}" -o /dev/null -w '%{http_code}' ` + approvedAdminHeader + ` "${BASE_URL}/k8s/clusters/${DOWNSTREAM_ID}/version")"`,
			"the approved header form: the token is written to a pipe by the builtin printf"},
	}
}

// TestSecretVariableLinesOfTheScriptAreReviewed asserts that every line of
// scripts/e2e-fidelity that expands ADMIN_TOKEN, LOGIN_TOKEN, BOOTSTRAP_PW or
// saved_token ($NAME, ${NAME} or ${NAME:-...}) equals one reviewed entry with
// the function it sits in, and that no script under scripts/ other than that
// one has such a line. Continuation lines are joined, comments are skipped and
// white space is squashed. A new line, a second copy of a listed line and an
// entry that no line matches each fail. The rule is that every line that puts
// the admin token, the login token or the bootstrap password on a command line
// is on the list. A scan cannot tell a builtin from an external command: every
// line that expands one of them is listed with the reason it is allowed. Limits: an indirect expansion (${!name}) and a copy of the value
// under another name are not seen, the list does not prove a line allowed, it
// makes a new line need an entry, and E2E_FIDELITY_ADMIN_TOKEN is not in
// the set: the script only exports it, and scripts/e2e-fidelity-downstream reads
// it as ${!var:-} in a test.
func TestSecretVariableLinesOfTheScriptAreReviewed(t *testing.T) {
	secret := regexp.MustCompile(`\$\{?(?:ADMIN_TOKEN|LOGIN_TOKEN|BOOTSTRAP_PW|saved_token)\b`)
	requireReviewedLines(t, wordLines(secret), secretVariableLines(t))

	dir, names := scriptFiles(t)
	for _, name := range names {
		if name == "e2e-fidelity" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		for _, line := range joinContinuations(strings.Split(string(raw), "\n")) {
			if isCommentLine(line) {
				continue
			}
			assert.Falsef(t, secret.MatchString(line), "scripts/%s expands a secret variable on a line that has no reviewed entry: %s", name, line)
		}
	}
}

// TestWriteStateTakesTheAdminTokenFromTheEnvironment asserts that write_state
// still records the admin token in the state file, as the second field, in a
// file only its owner can read, and that the token is on no argument list of
// python3: a shell function in front of python3 records every argument list and
// then runs the real one.
func TestWriteStateTakesTheAdminTokenFromTheEnvironment(t *testing.T) {
	t.Parallel()
	const token = "token-secret-7f3a"
	stateFile, _ := stateProgram(t, `python3() { printf '%s\n' "$@" >> "$(dirname "${STATE_FILE}")/python.argv"; command python3 "$@"; }
ADMIN_TOKEN=`+token+`; BOOT=container; HELM_ASSETS_IMAGE=""; LOCAL_K3S_SOURCE=""; CERT_MANAGER_IMAGES="[]"; write_state`)

	raw, err := os.ReadFile(stateFile)
	require.NoError(t, err)
	info, err := os.Stat(stateFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	var state map[string]any
	require.NoError(t, json.Unmarshal(raw, &state))
	assert.Equal(t, map[string]any{
		"containerId": "oracle-id", "adminToken": token, "oracleImage": "oracle",
		"k3sContainerId": "k3s-id", "k3sImage": "k3s", "downstream": "c-1", "boot": "container",
		"installUuid": testPairInstallUUID,
	}, state)

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	opening, err := decoder.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), opening)
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		require.NoError(t, err)
		name, ok := key.(string)
		require.True(t, ok)
		keys = append(keys, name)
		var value any
		require.NoError(t, decoder.Decode(&value))
	}
	require.GreaterOrEqual(t, len(keys), 3)
	assert.Equal(t, []string{"containerId", "adminToken", "oracleImage"}, keys[:3])

	argv, err := os.ReadFile(filepath.Join(filepath.Dir(stateFile), "python.argv"))
	require.NoError(t, err)
	assert.NotEmpty(t, argv)
	assert.Contains(t, string(argv), "oracle-id")
	assert.NotContains(t, string(argv), token)
}

// stateReadProgram runs functions of the script that read the state file, with
// shell functions in front of python3, docker and curl. The python3 function
// appends the argument list and the environment of each run to python.argv and
// python.env next to the state file, then runs the real python3. The docker and
// curl functions answer as a healthy kept pair does: the recorded container
// ids, the bind address 127.0.0.1, a pong, HTTP 200 for the token check and an
// active downstream cluster. It returns the output, the argument lists and the
// environments.
func stateReadProgram(t *testing.T, state map[string]any, functions []string, call string) (out, argv, env string) {
	t.Helper()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(stateFile, raw, 0o600))
	program := "set -uo pipefail\n" +
		"STATE_FILE=" + stateFile + " CONTAINER=pair K3S_CONTAINER=pair-k3s ORACLE_IMAGE=oracle K3S_IMAGE=k3s BIND_ADDR=127.0.0.1 BOOT=helm\n" +
		"CHART_PIN=chart LOCAL_K3S_PIN=node-k3s CERT_MANAGER_MANIFEST=manifest HELM_COMPUTED='' HELM_ASSETS_IMAGE='' LOCAL_K3S_SOURCE=''\n" +
		"BASE_URL=https://127.0.0.1:8443 CA_FILE=" + filepath.Join(dir, "ca.pem") + " ADMIN_TOKEN='' DOWNSTREAM_ID=''\n" +
		`python3() {
  printf '%s\n' "$@" >> "` + filepath.Join(dir, "python.argv") + `"
  env >> "` + filepath.Join(dir, "python.env") + `"
  command python3 "$@"
}
docker() {
  case "$*" in
  *HostIp*) echo 127.0.0.1 ;;
  *pair-k3s) echo k3s-id ;;
  *) echo oracle-id ;;
  esac
}
curl() {
  case "$*" in
  */ping*) echo pong ;;
  */v3/clusters/*) printf '{"state":"active"}\n200' ;;
  *) printf 200 ;;
  esac
}
`
	for _, name := range functions {
		program += shellFunction(t, name)
	}
	program += call + "\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", program)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "output: %s", output)
	argvRaw, err := os.ReadFile(filepath.Join(dir, "python.argv"))
	require.NoError(t, err)
	envRaw, err := os.ReadFile(filepath.Join(dir, "python.env"))
	require.NoError(t, err)
	return string(output), string(argvRaw), string(envRaw)
}

// TestTheStateFileReadsKeepTheAdminTokenOffArgumentLists asserts that
// reuse_state and reuse_downstream, run to their end on a kept helm pair, put
// the admin token of the state file on no argument list and in no environment
// of python3, the program that reads the file: python3 opens the file by its
// path. Each case asserts that python3 ran with the path of the state file, so
// a case that reads nothing fails.
func TestTheStateFileReadsKeepTheAdminTokenOffArgumentLists(t *testing.T) {
	t.Parallel()
	const token = "token-secret-91d4"
	state := map[string]any{
		"containerId": "oracle-id", "adminToken": token, "oracleImage": "oracle", "boot": "helm",
		"k3sContainerId": "k3s-id", "k3sImage": "k3s", "downstream": "c-1", "installUuid": testPairInstallUUID,
		"chart": "chart", "localK3sImage": "node-k3s", "localK3sSource": "embedded", "assetsImage": "recorded@sha256:aa",
		"certManager": map[string]any{"manifest": "manifest", "images": []string{}},
	}
	cases := []struct {
		name      string
		functions []string
		call      string
		want      string
	}{
		{"reuse_state", []string{"read_state", "load_helm_state", "reuse_state"},
			`reuse_state; echo "status=$? failed=${REUSE_FAILED_CHECK:-} assets=${HELM_ASSETS_IMAGE}"`,
			"status=0 failed= assets=recorded@sha256:aa\n"},
		{"reuse_downstream", []string{"read_state", "reuse_downstream", "read_kept_downstream_state", "api"},
			`ADMIN_TOKEN=` + token + `; reuse_downstream; echo "status=$? failed=${REUSE_FAILED_CHECK:-} downstream=${DOWNSTREAM_ID}"`,
			"status=0 failed= downstream=c-1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, argv, env := stateReadProgram(t, state, c.functions, c.call)
			assert.Contains(t, out, c.want, "the function runs to its end")
			assert.Contains(t, argv, "state.json", "python3 reads the state file by its path")
			assert.NotContains(t, argv, token, "the admin token is on no argument list")
			assert.NotEmpty(t, env)
			assert.NotContains(t, env, token, "the admin token is in no environment")
		})
	}
}

// stateFileTextRead matches a line of a script that reads the text of the state
// file into the shell: an input redirect from STATE_FILE ($(<"${STATE_FILE}"),
// read ... < "${STATE_FILE}", done < "${STATE_FILE}", mapfile and readarray),
// and STATE_FILE as an argument of a command that prints or filters a file (cat,
// head, tail, dd, base64, od, xxd, strings, grep, awk, jq, sed, cut) before
// any output redirect. The text holds the admin token; a variable that holds it
// can reach an argument list. Not seen: a command that is not in the list, a
// path held under another name, and a read of the file by a program that the
// script starts (python3 reads it by path, which is the allowed form).
var stateFileTextRead = regexp.MustCompile(`(?:^|[^<])<\s*["']?\$\{?STATE_FILE\b|\b(?:cat|head|tail|dd|base64|od|xxd|strings|grep|awk|jq|sed|cut)\b[^|;&>]*\$\{?STATE_FILE\b`)

// TestStateFileTextReadPatternMatchesEveryForm asserts the lines that
// stateFileTextRead matches and the lines of the script that it must leave
// alone.
func TestStateFileTextReadPatternMatchesEveryForm(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`text="$(<"${STATE_FILE}")"`,
		`text="$(< "$STATE_FILE")"`,
		`text="$(cat "${STATE_FILE}")"`,
		`text=$(cat -- "${STATE_FILE}")`,
		`IFS= read -r -d '' text < "${STATE_FILE}" || true`,
		`done < "${STATE_FILE}"`,
		`mapfile -t lines <"${STATE_FILE}"`,
		`text="$(head -c 4096 "${STATE_FILE}")"`,
		`text="$(base64 < "${STATE_FILE}")"`,
		`token="$(grep -o '"adminToken":"[^"]*"' "${STATE_FILE}")"`,
		`token="$(awk -F'"' '{print $4}' "${STATE_FILE}")"`,
		`token="$(jq -r .adminToken "${STATE_FILE}")"`,
		`token="$(sed -n 2p "${STATE_FILE}")"`,
		`token="$(cut -d: -f2 "${STATE_FILE}")"`,
		`token="$(tr -d '"' < "${STATE_FILE}")"`,
	} {
		assert.Truef(t, stateFileTextRead.MatchString(line), "the pattern does not match a line that reads the state file: %s", line)
	}
	for _, line := range []string{
		`[ -f "${STATE_FILE}" ] || return 1`,
		`rm -f "${STATE_FILE}"`,
		`' "${STATE_FILE}" "$@")`,
		`REUSE_FAILED_DETAIL="the state file ${STATE_FILE} could not be read"`,
		`} < <(python3 -I -c 'print(1)' "${STATE_FILE}")`,
		`cat <<EOF >"${STATE_FILE}"`,
	} {
		assert.Falsef(t, stateFileTextRead.MatchString(line), "the pattern matches a line that does not read the state file: %s", line)
	}
}

// TestNoLineReadsTheStateFileTextIntoTheShell asserts that no line of
// scripts/e2e-fidelity reads the text of the state file into the shell (see
// stateFileTextRead). Continuation lines are joined and comments are skipped.
// The functions that read the state file give the path to python3, which reads
// the file itself.
func TestNoLineReadsTheStateFileTextIntoTheShell(t *testing.T) {
	t.Parallel()
	for _, line := range joinedScriptLines(t) {
		if isCommentLine(line) {
			continue
		}
		assert.Falsef(t, stateFileTextRead.MatchString(line), "a line reads the text of the state file into the shell: %s", strings.TrimSpace(line))
	}
}
