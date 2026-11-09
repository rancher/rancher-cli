package fidelity

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pairChangerStarts are the functions of scripts/e2e-fidelity that change the
// kept pair or the oracle: they boot, import, record, prime or create something.
// The functions that hold a line of the docker list or of the list of curl
// options, and every function that calls a member, join them.
var pairChangerStarts = []string{
	"boot_k3s", "rebuild_downstream", "import_downstream", "write_state", "prime_ip_san", "ensure_network",
	"boot_helm", "boot_helm_node", "install_cert_manager", "create_bootstrap_secret",
}

// pairChangingFunctions is the set of functions of the script that can change the
// pair: pairChangerStarts, the functions that hold a line of dockerLines or of
// the method option lines, and, to a fixed point, every function that calls a
// member of the set.
func pairChangingFunctions(t *testing.T) map[string]bool {
	t.Helper()
	funcStart := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\) \{$`)
	type bodyLine struct{ owner, text string }
	var body []bodyLine
	defined := map[string]bool{}
	owner := ""
	for _, line := range joinedScriptLines(t) {
		if m := funcStart.FindStringSubmatch(line); m != nil {
			owner = m[1]
			defined[owner] = true
			continue
		} else if line == "}" {
			owner = ""
			continue
		}
		if text := strings.TrimSpace(line); !strings.HasPrefix(text, "#") {
			body = append(body, bodyLine{owner, text})
		}
	}
	set := map[string]bool{}
	for _, name := range pairChangerStarts {
		require.Truef(t, defined[name], "the function %s of the list of pair changing functions is not in the script", name)
		set[name] = true
	}
	for _, list := range [][]reviewedLine{dockerLines, methodOptionLines(t)} {
		for _, entry := range list {
			if entry.owner != "" {
				set[entry.owner] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, line := range body {
			if line.owner == "" || set[line.owner] {
				continue
			}
			for name := range set {
				if callsFunction(name).MatchString(line.text) {
					set[line.owner] = true
					changed = true
					break
				}
			}
		}
	}
	return set
}

// callsFunction matches a line that names the function as a word of a command:
// after the start of the line or a separator, before the end or a separator.
func callsFunction(name string) *regexp.Regexp {
	return regexp.MustCompile("(?:^|[\\s;&|(!{`\"])" + regexp.QuoteMeta(name) + "(?:$|[\\s;&|)}`\"])")
}

// pairChangerCalls are the lines of scripts/e2e-fidelity that call a function
// that can change the pair, or name one in text, each with the function it sits
// in (empty for the top level) and why it is allowed. The ones in the fresh boot
// run after the refusal call; a kept run whose checks passed reaches only the
// reads, the refusal and the one rebuild after a k3s image change.
func pairChangerCalls(t *testing.T) []reviewedLine {
	t.Helper()
	return []reviewedLine{
		{"image_info_or_stop", `IMAGE_INFO="$(read_registry_image "$1" "$2" "${platform}" "$3")" || status=$?`, "reads image metadata from the registry before the lock; changes nothing"},
		{"helm_check_chart", `image_info_or_stop "${IMAGE_REPOSITORY}" "${IMAGE_DIGEST}" env "${ORACLE_IMAGE} is not published"`, "reads image metadata from the registry before the lock; changes nothing"},
		{"helm_check_chart", `image_info_or_stop "${assets_repository}" "${assets_tag}" digest "${assets_repository}:${assets_tag} is not published"`, "reads image metadata from the registry before the lock; changes nothing"},
		{"helm_prepare", `pull_or_stop "${LOCAL_K3S_PIN}"`, "pulls a pinned image that is missing, before the lock"},
		{"helm_prepare", `pull_or_stop "${K3S_IMAGE}"`, "pulls a pinned image that is missing, before the lock"},
		{"helm_prepare", `[ "${IMAGE_REPOSITORY##*/}" = "rancher-server" ] || pull_or_stop "${ORACLE_IMAGE}"`, "pulls a pinned image that is missing, before the lock"},
		{"helm_prepare", `helm_check_chart`, "reads the chart and image pins before the lock; changes nothing"},
		{"compare_head_images", `image_info_or_stop "${UPCOMING_REPO}" "${tag}" env "${UPCOMING_REPO}:${tag} is not published; the head images cannot be compared"`, "reads image metadata from the registry before the lock; changes nothing"},
		{"", `image_info_or_stop "${image_repo}" "${image_ref}" env "${not_published}"`, "reads image metadata from the registry before the lock; changes nothing"},
		{"", `image_info_or_stop "${UPCOMING_K3S_REPO}" "${node_k3s_tag}" digest "${UPCOMING_K3S_REPO}:${node_k3s_tag} (from CATTLE_K3S_VERSION ${node_k3s_version}) is not published; the node container of a helm boot runs that k3s"`, "reads image metadata from the registry before the lock; changes nothing"},
		{"", `image_info_or_stop "${UPCOMING_K3S_REPO}" "${k3s_tag}" digest "${UPCOMING_K3S_REPO}:${k3s_tag} (from CATTLE_K3S_VERSION ${K3S_VERSION}) is not published; set E2E_FIDELITY_K3S_IMAGE"`, "reads image metadata from the registry before the lock; changes nothing"},
		{"", `helm_check_chart`, "reads the chart and image pins before the lock; changes nothing"},
		{"", `compare_head_images "${image_ref}" "${UPCOMING_REVISION}"`, "compares the head images of an upcoming minor before the lock; changes nothing"},
		{"", `check_slot`, "the check that no other container holds the slot's port or subnet; reads only"},
		{"", `check_slot`, "the check that no other container holds the slot's port or subnet; reads only"},
		{"", `if [ "${E2E_FIDELITY_KEEP:-}" != "1" ] || ! read_kept_state; then`, "reads the kept state before anything is changed; reads only"},
		{"", `helm_prepare`, "prepares a helm boot before the lock: pulls images and reads versions; a kept run that passed its checks skips it"},
		{"", `trap cleanup EXIT`, "the EXIT trap: the diagnostics dump, and the removal of a run that is not under E2E_FIDELITY_KEEP=1"},
		{"refuse_reuse", `detail="reuse_state or reuse_downstream failed at a check that sets no name; this is a defect in scripts/e2e-fidelity"`, "the text that names the two functions; it calls neither"},
		{"rebuild_downstream", `prime_ip_san`, "the rebuild primes the TLS name for the new downstream"},
		{"rebuild_downstream", `boot_k3s`, "the rebuild boots the new downstream container"},
		{"rebuild_downstream", `import_downstream`, "the rebuild imports the new downstream into the oracle"},
		{"rebuild_downstream", `write_state`, "the rebuild records the new downstream in the state file"},
		{"boot_helm", `boot_helm_node`, "the fresh helm boot: starts the node container"},
		{"boot_helm", `install_cert_manager`, "the fresh helm boot: installs cert-manager"},
		{"boot_helm", `create_bootstrap_secret`, "the fresh helm boot: creates the bootstrap secret"},
		{"boot_helm", `check_rancher_pod`, "reads the Rancher pods in the fresh helm boot; reads only"},
		{"", `if [ "${E2E_FIDELITY_KEEP:-}" = "1" ] && reuse_state; then`, "the reuse test of the kept pair; reads only"},
		{"", `require_disk`, "the free disk check before a boot; reads only"},
		{"", `if ! reuse_downstream; then`, "the reuse test of the kept downstream; reads only"},
		{"", `[ -n "${downstream_pin_changed:-}" ] || refuse_reuse`, "the refusal: stops the run with exit 4 and removes nothing"},
		{"", `rebuild_downstream`, "the one rebuild: only in the block of a failed reuse_downstream, after the refusal call, after a k3s image change"},
		{"", `refuse_reuse`, "the refusal: stops the run with exit 4 and removes nothing"},
		{"", `[ "${BOOT}" != "helm" ] || [ -n "${HELM_COMPUTED}" ] || helm_prepare`, "prepares a helm boot before the lock: pulls images and reads versions; a kept run that passed its checks skips it"},
		{"", `refuse_pre_rename_leftovers`, "the check for a pair from before the rename; reads only"},
		{"", `ensure_network`, "the fresh boot creates the pair's network, after the refusal call"},
		{"", `require_disk`, "the free disk check before a boot; reads only"},
		{"", `boot_helm`, "the fresh boot of a helm pair, after the refusal call"},
		{"", `require_disk`, "the free disk check before a boot; reads only"},
		{"", `wait_for_ping`, "waits for the Rancher server of the new oracle to answer; reads only"},
		{"", `wait_for_webhook`, "waits for the Rancher webhook of the new oracle; reads only"},
		{"", `check_oracle_container`, "reads the status and restart count of the oracle container once more after the waits, so a restart in their last pass is printed; reads only"},
		{"wait_for_ping", `check_oracle_container || return 1`, "reads the status and restart count of the oracle container in each pass; reads only"},
		{"wait_for_webhook", `check_oracle_container || return 1`, "reads the status and restart count of the oracle container in each pass; reads only"},
		{"check_oracle_container", `fatal="$(oracle_log_tail | grep -F '[FATAL]' | tail -n 1)" || fatal=""`, "reads the last lines of the oracle container's output for the FATAL line of a restart; reads only"},
		{"", fmt.Sprintf(`LOGIN_TOKEN="$(%s)"`, loginOwner(t)), "logs in to the new oracle in the fresh boot"},
		{"", `prime_ip_san`, "the fresh boot primes the TLS name of the new oracle, after the refusal call"},
		{"", `boot_k3s`, "the fresh boot boots the downstream container, after the refusal call"},
		{"", `import_downstream`, "the fresh boot imports the downstream, after the refusal call"},
		{"", `[ "${E2E_FIDELITY_KEEP:-}" != "1" ] || write_state`, "the fresh boot records the new pair, after the refusal call; a kept run skips it"},
	}
}

// TestEveryCallOfAPairChangingFunctionIsReviewed asserts that every line of the
// script that calls a function that can change the pair is one of the reviewed
// lines, each matching exactly one line of its function: a call from a branch no
// test row takes, a call of a wrapper of such a function, and a second copy of a
// listed call fail until the line is checked and listed. The set of functions
// includes the starting names.
func TestEveryCallOfAPairChangingFunctionIsReviewed(t *testing.T) {
	changing := pairChangingFunctions(t)
	for _, name := range pairChangerStarts {
		assert.Truef(t, changing[name], "the set of pair changing functions lacks %s", name)
	}
	names := make([]string, 0, len(changing))
	for name := range changing {
		names = append(names, name)
	}
	slices.Sort(names)
	match := func(line string) bool {
		return slices.ContainsFunc(names, func(name string) bool { return callsFunction(name).MatchString(line) })
	}
	requireReviewedLines(t, match, pairChangerCalls(t))
}
