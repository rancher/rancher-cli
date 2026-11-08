package fidelity

import (
	"regexp"
	"testing"
)

// dockerLines are the lines of scripts/e2e-fidelity that name docker: every
// command it runs, and the text that names one. Each has the function it sits in
// (empty for the top level) and why it is allowed. A line is a command that reads,
// or one of the few that change a pair: the cleanup of a run without
// E2E_FIDELITY_KEEP=1, the fresh boot after the refusal call, the downstream
// boot, and the one rebuild after a k3s image change. A kept run that passed its
// own checks and whose downstream failed one runs none of those.
var dockerLines = []reviewedLine{
	{"", `REGISTRY_URL="${E2E_FIDELITY_REGISTRY_URL:-https://registry-1.docker.io}"`, "the default address of the image registry the registry reader queries; no docker command"},
	{"", `for tool in docker curl openssl python3 go; do`, "the check that the tools the run needs are installed"},
	{"", `docker info >/dev/null 2>&1 || { echo "error: docker daemon not reachable" >&2; exit 1; }`, "the check that the Docker daemon answers"},
	{"read_registry_image", `"application/vnd.docker.distribution.manifest.list.v2+json, "`, "the media type the registry reader asks the registry for; no docker command"},
	{"read_registry_image", `"application/vnd.docker.distribution.manifest.v2+json"`, "the media type the registry reader asks the registry for; no docker command"},
	{"read_registry_image", `"application/vnd.docker.distribution.manifest.v2+json"`, "the media type the registry reader asks the registry for; no docker command"},
	{"check_slot", `container_ports="$(docker ps -aq | xargs -r docker inspect -f '{{.Name}} {{range $p, $b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}} {{end}}{{end}}')" ||`, "the check that no other container holds the slot's port or subnet; reads only"},
	{"check_slot", `{ echo "error: could not read the containers' published ports from docker" >&2; exit 1; }`, "the error text of the port check; no docker command"},
	{"check_slot", `networks="$(docker network ls -q | xargs -r docker network inspect -f '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}')" ||`, "the check that no other container holds the slot's port or subnet; reads only"},
	{"check_slot", `{ echo "error: could not read the networks' subnets from docker" >&2; exit 1; }`, "the error text of the subnet check; no docker command"},
	{"read_kept_state", `running="$(docker inspect --format '{{.Id}}' "${CONTAINER}" 2>/dev/null)" || return 1`, "reads the id of the kept oracle container; reads only"},
	{"pull_or_stop", `docker image inspect "$1" >/dev/null 2>&1 ||`, "checks that a pinned image is present and pulls it when it is missing, before the lock"},
	{"pull_or_stop", `docker pull -q "$1" >/dev/null ||`, "checks that a pinned image is present and pulls it when it is missing, before the lock"},
	{"helm_prepare", `platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')" ||`, "reads what a helm pair needs from the images before the lock; reads only (the container it runs removes itself)"},
	{"helm_prepare", `version_line="$(docker run --rm --entrypoint k3s "${ORACLE_IMAGE}" --version 2>/dev/null)" ||`, "reads what a helm pair needs from the images before the lock; reads only (the container it runs removes itself)"},
	{"", `platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')" ||`, "reads the Docker platform before the images are pulled"},
	{"", `docker image inspect "${image_ref}" >/dev/null 2>&1 ||`, "checks that a pinned image is present before the lock"},
	{"", `docker pull -q "${image_ref}" >/dev/null ||`, "pulls a pinned image that is missing, before the lock and before anything of a kept pair is removed"},
	{"cleanup", `oracle_state="$(docker inspect -f 'status={{.State.Status}} exitCode={{.State.ExitCode}} oomKilled={{.State.OOMKilled}} finishedAt={{.State.FinishedAt}}' "${CONTAINER}" 2>/dev/null)" || oracle_state=""`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `k3s_state="$(docker inspect -f 'status={{.State.Status}} exitCode={{.State.ExitCode}} oomKilled={{.State.OOMKilled}} finishedAt={{.State.FinishedAt}}' "${K3S_CONTAINER}" 2>/dev/null)" || k3s_state=""`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker stats --no-stream 2>&1 | sed 's/^/[host] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl get nodes -o wide 2>&1 | sed 's/^/[rancher] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl describe nodes 2>&1 | sed -n '/^Conditions:/,/^Addresses:/p' | sed 's/^/[rancher] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl get pods -A -o wide 2>&1 | sed 's/^/[rancher] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl get events -A --sort-by=.lastTimestamp 2>&1 | tail -40 | sed 's/^/[rancher] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${K3S_CONTAINER}" kubectl get nodes -o wide 2>&1 | sed 's/^/[k3s] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${K3S_CONTAINER}" kubectl describe nodes 2>&1 | sed -n '/^Conditions:/,/^Addresses:/p' | sed 's/^/[k3s] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${K3S_CONTAINER}" kubectl get pods -A -o wide 2>&1 | sed 's/^/[k3s] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker logs --tail 200 "${CONTAINER}" 2>&1 | redact_log "[rancher] " || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker logs --tail 50 "${K3S_CONTAINER}" 2>&1 | redact_log "[k3s] " || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${K3S_CONTAINER}" kubectl -n cattle-system logs deploy/cattle-cluster-agent --tail 50 2>&1 | redact_log "[agent] " || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl -n kube-system logs job/helm-install-rancher --tail 200 2>&1 | redact_log "[helm] " || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl -n cattle-system logs deploy/rancher --tail 200 2>&1 | redact_log "[rancher] " || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl -n cattle-system get pods -l app=rancher -o jsonpath='{range .items[*]}{.metadata.name} restarts={.status.containerStatuses[*].restartCount} last={.status.containerStatuses[*].lastState}{"\n"}{end}' 2>&1 | redact_log "[rancher] " || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker exec "${CONTAINER}" kubectl -n cert-manager get pods -o wide 2>&1 | sed 's/^/[cert-manager] /' || true`, "the diagnostics dump after a failure: reads the state, load, logs and pods of the containers; changes nothing"},
	{"cleanup", `docker rm -f -v "${K3S_CONTAINER}" "${CONTAINER}" >/dev/null 2>&1 || true`, "removes the pair when the run is not under E2E_FIDELITY_KEEP=1; a kept pair skips it"},
	{"cleanup", `docker network rm "${NETWORK}" >/dev/null 2>&1 || true`, "removes the network when the run is not under E2E_FIDELITY_KEEP=1; a kept pair skips it"},
	{"require_disk", `root="$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || true)"`, "reads the Docker root directory for the free disk check"},
	{"wait_for_webhook", `if docker exec "${CONTAINER}" kubectl -n cattle-system get deploy rancher-webhook -o jsonpath='{.status.readyReplicas}' 2>/dev/null | grep -q '^1$'; then`, "reads the Rancher webhook deployment of the oracle; reads only"},
	{"ensure_network", `docker network inspect "${NETWORK}" >/dev/null 2>&1 && return 0`, "creates or finds the pair's network, in the fresh boot"},
	{"ensure_network", `docker network create --subnet "${SUBNET}" "${NETWORK}" >/dev/null`, "creates or finds the pair's network, in the fresh boot"},
	{"refuse_pre_rename_leftovers", `docker container inspect "${c}" >/dev/null 2>&1 && found="${found} container ${c}"`, "reads whether a pair from before the rename is present; reads only"},
	{"refuse_pre_rename_leftovers", `if [ "$(docker network inspect "${old}-net" -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null)" = "${SUBNET}" ]; then`, "reads whether a pair from before the rename is present; reads only"},
	{"refuse_pre_rename_leftovers", `echo "error: the suite was renamed from e2e-real to e2e-fidelity; remove them (docker rm -f -v, docker network rm) together with the old state root ${STATE_ROOT/e2e-fidelity/e2e-real}, or rename them, then rerun" >&2`, "the error text for a pair from before the rename; names the commands in text"},
	{"boot_k3s", `docker rm -f -v "${K3S_CONTAINER}" >/dev/null 2>&1 || true`, "removes the downstream container before a new one; called from the fresh boot and from the rebuild"},
	{"boot_k3s", `docker run -d --name "${K3S_CONTAINER}" --privileged ${restart_args} --network "${NETWORK}" --ip "${K3S_IP}" --tmpfs /run --tmpfs /var/run "${K3S_IMAGE}" server --disable traefik --disable servicelb --disable metrics-server >/dev/null`, "starts the downstream container; called from the fresh boot and from the rebuild"},
	{"boot_k3s", `if docker exec "${K3S_CONTAINER}" kubectl get nodes 2>/dev/null | grep -q ' Ready '; then`, "boots the downstream container; called from the fresh boot and from the rebuild"},
	{"import_downstream", `curl -s --cacert "${CA_FILE}" --max-time 30 "${BASE_URL}/v3/import/${reg}_${DOWNSTREAM_ID}.yaml" | docker exec -i "${K3S_CONTAINER}" kubectl apply -f - >/dev/null`, "applies the import manifest to the downstream; called from the fresh boot and from the rebuild"},
	{"reuse_state", `running="$(docker inspect --format '{{.Id}}' "${CONTAINER}" 2>/dev/null)" || {`, "reads the id and published address of the kept oracle container; reads only"},
	{"reuse_state", `bound_ip="$(docker inspect --format '{{(index (index .NetworkSettings.Ports "443/tcp") 0).HostIp}}' "${CONTAINER}" 2>/dev/null)" || {`, "reads the id and published address of the kept oracle container; reads only"},
	{"refuse_reuse", `echo "error: with E2E_FIDELITY_KEEP=1 a kept pair whose container, container id, bind address, /ping, admin token, state file, k3s container, k3s container id or downstream cluster check fails is never removed. Find out why (docker ps -a --filter name=${named_container}; docker logs --tail 50 ${named_container}) and run again."`, "the refusal's text that names the checks and the commands that find out why; no removal"},
	{"refuse_reuse", `echo "error: the last read of the downstream cluster got no HTTP answer; also find out why the oracle did not answer (docker ps -a --filter name=${CONTAINER}; docker logs --tail 50 ${CONTAINER})"`, "the refusal's text for a read of the downstream that got no answer; no removal"},
	{"refuse_reuse", `printf 'error: docker rm -f -v %q %q\n' "${CONTAINER}" "${K3S_CONTAINER}"`, "the refusal prints the command that removes the pair; it runs nothing"},
	{"refuse_reuse", `printf 'error: docker network rm %q\n' "${NETWORK}"`, "the refusal prints the command that removes the network; it runs nothing"},
	{"reuse_downstream", `k3s_running="$(docker inspect --format '{{.Id}}' "${K3S_CONTAINER}" 2>/dev/null)" || {`, "reads the id of the kept downstream container; reads only"},
	{"rebuild_downstream", `docker rm -f -v "${K3S_CONTAINER}" >/dev/null 2>&1 || true`, "removes the downstream container, only after a k3s image change (the one rebuild)"},
	{"write_state", `cid="$(docker inspect --format '{{.Id}}' "${CONTAINER}")"`, "reads the ids of the pair's containers to record them"},
	{"write_state", `k3s_cid="$(docker inspect --format '{{.Id}}' "${K3S_CONTAINER}")"`, "reads the ids of the pair's containers to record them"},
	{"boot_helm_node", `docker run -d --name "${CONTAINER}" --privileged ${restart_args} --network "${NETWORK}" --ip "${ORACLE_IP}" -p "${BIND_ADDR}:${HOST_PORT}:443" --tmpfs /run --tmpfs /var/run "${LOCAL_K3S_PIN}" server --cluster-init --disable=traefik,servicelb,metrics-server,local-storage --node-name=local-node >/dev/null`, "starts the helm node container in the fresh boot"},
	{"boot_helm_node", `nodes="$(docker exec "${CONTAINER}" kubectl get nodes 2>/dev/null)" || nodes=""`, "boots the helm node container in the fresh boot, and reads its state and log"},
	{"boot_helm_node", `if [ "$(docker inspect --format '{{.State.Status}}' "${CONTAINER}" 2>/dev/null)" != "running" ]; then`, "boots the helm node container in the fresh boot, and reads its state and log"},
	{"boot_helm_node", `echo "error: the node container stopped (exit code $(docker inspect --format '{{.State.ExitCode}}' "${CONTAINER}" 2>/dev/null)); its last log lines:" >&2`, "boots the helm node container in the fresh boot, and reads its state and log"},
	{"boot_helm_node", `docker logs --tail 20 "${CONTAINER}" 2>&1 | redact_log "" >&2 || true`, "boots the helm node container in the fresh boot, and reads its state and log"},
	{"install_cert_manager", `docker exec -i "${CONTAINER}" kubectl apply -f - <"${DL_DIR}/cert-manager.yaml" >/dev/null`, "applies the cert-manager manifest in the fresh helm boot"},
	{"install_cert_manager", `docker exec "${CONTAINER}" kubectl -n cert-manager rollout status "deploy/${name}" --timeout=300s >/dev/null ||`, "installs cert-manager in the fresh helm boot"},
	{"install_cert_manager", `ca_bundle="$(docker exec "${CONTAINER}" kubectl get validatingwebhookconfiguration cert-manager-webhook -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null)" || ca_bundle=""`, "installs cert-manager in the fresh helm boot"},
	{"install_cert_manager", `docker exec "${CONTAINER}" kubectl wait --for=condition=Established crd/issuers.cert-manager.io --timeout=60s >/dev/null ||`, "installs cert-manager in the fresh helm boot"},
	{"install_cert_manager", `CERT_MANAGER_IMAGES="$(docker exec "${CONTAINER}" kubectl -n cert-manager get pods -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}' |`, "installs cert-manager in the fresh helm boot"},
	{"create_bootstrap_secret", `docker exec -i "${CONTAINER}" kubectl create -f - >/dev/null <<MANIFEST`, "creates the bootstrap secret in the fresh helm boot"},
	{"check_rancher_pod", `pods="$(docker exec "${CONTAINER}" kubectl -n cattle-system get pods -l app=rancher -o jsonpath='{range .items[*]}image={.spec.containers[?(@.name=="rancher")].image} id={.status.containerStatuses[?(@.name=="rancher")].imageID}{"\n"}{end}')" ||`, "reads the Rancher pods in the fresh helm boot; reads only"},
	{"boot_helm", `helm_chart_manifest | docker exec -i "${CONTAINER}" kubectl apply -f - >/dev/null`, "applies the Rancher chart in the fresh helm boot"},
	{"boot_helm", `docker exec "${CONTAINER}" kubectl -n cattle-system get deploy rancher -o name >/dev/null 2>&1 && break`, "installs the Rancher chart in the fresh helm boot"},
	{"boot_helm", `docker exec "${CONTAINER}" kubectl -n cattle-system get deploy rancher -o name >/dev/null 2>&1 ||`, "installs the Rancher chart in the fresh helm boot"},
	{"boot_helm", `docker exec "${CONTAINER}" kubectl -n cattle-system rollout status deploy/rancher --timeout=600s >/dev/null ||`, "installs the Rancher chart in the fresh helm boot"},
	{"", `docker rm -f -v "${K3S_CONTAINER}" "${CONTAINER}" >/dev/null 2>&1 || true`, "the fresh boot removes both containers of the pair, after the refusal call; a kept pair that passed its checks never reaches it"},
	{"", `docker network rm "${NETWORK}" >/dev/null 2>&1 || true`, "the fresh boot removes the network, after the refusal call"},
	{"", `docker run -d --name "${CONTAINER}" --privileged ${RESTART_ARGS} --network "${NETWORK}" --ip "${ORACLE_IP}" -p "${BIND_ADDR}:${HOST_PORT}:443" --env-file "${ENV_FILE}" "${ORACLE_IMAGE}" >/dev/null`, "the fresh boot starts the oracle container, after the refusal call"},
}

// TestEveryDockerLineOfTheScriptIsReviewed asserts that every line of the script
// that names docker is one of the reviewed lines, each matching exactly one line
// of its function. A docker command added to any function, a docker call with
// another verb or a global option, and a second copy of a listed line fail until
// the line is checked and listed, so no end of a refused run can remove or stop
// a container unseen.
func TestEveryDockerLineOfTheScriptIsReviewed(t *testing.T) {
	requireReviewedLines(t, wordLines(regexp.MustCompile(`\bdocker\b`)), dockerLines)
}
