package fidelity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const approvedAdminHeader = `-H @<(printf 'Authorization: Bearer %s\n' "${ADMIN_TOKEN}")`
const approvedLoginHeader = `-H @<(printf 'Authorization: Bearer %s\n' "${LOGIN_TOKEN}")`

// secretVariableLines are the lines of scripts/e2e-fidelity that expand
// ADMIN_TOKEN, LOGIN_TOKEN or BOOTSTRAP_PW, each with the function it sits in
// (empty for the main flow) and why it is allowed.
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
// scripts/e2e-fidelity that expands ADMIN_TOKEN, LOGIN_TOKEN or BOOTSTRAP_PW
// ($NAME, ${NAME} or ${NAME:-...}) equals one reviewed entry with the function
// it sits in, and that no script under scripts/ other than that one has such a
// line. Continuation lines are joined, comments are skipped and white space is
// squashed. A new line, a second copy of a listed line and an entry that no line
// matches each fail. The rule is that every line that puts one of the three on a
// command line is on the list; a scan cannot tell a builtin from an external
// command, so every line that expands one is listed with the reason it is
// allowed. Limits: an indirect expansion (${!name}) and a copy of the value
// under another name are not seen, the list does not prove a line allowed, it
// makes a new line need an entry, and E2E_FIDELITY_ADMIN_TOKEN is not in
// the set: the script only exports it, and scripts/e2e-fidelity-downstream reads
// it as ${!var:-} in a test.
func TestSecretVariableLinesOfTheScriptAreReviewed(t *testing.T) {
	secret := regexp.MustCompile(`\$\{?(?:ADMIN_TOKEN|LOGIN_TOKEN|BOOTSTRAP_PW)\b`)
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
