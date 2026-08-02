# Migrating from the legacy Rancher CLI

Config and credentials are not imported — run `rancher auth login` once per
server. The old `cli2.json` is not read, not migrated, and not deleted by this
CLI — it may contain plaintext credentials; remove it yourself
(`rm ~/.rancher/cli2.json`, or the legacy `$RANCHER_CONFIG_DIR` location).

Output is enveloped JSON/YAML/table (`-o table|json|yaml`, plus `-o name` for
IDs only); table is the default for interactive use. Mutating commands need
`--apply` (and `--yes` for destructive ones) — there is no implicit write.
Run `rancher schema` to discover the full command surface, including flags,
contract, and stability, without reading docs.

## Command disposition

Every legacy command and subcommand below maps to exactly one of: a new
spelling, a replacement recipe, or a documented drop. This table is kept
exhaustive against the full legacy command inventory by a tracked
conformance test (`internal/commands/migrating_test.go` +
`internal/commands/testdata/legacy-commands.txt`).

| Legacy | Disposition |
|---|---|
| `login` (alias `l`) | → `rancher auth login` |
| `context switch` | → `rancher config use-context` |
| `context current` | → `rancher config current-context` |
| `clusters ls` | → `rancher get clusters` |
| `clusters create` | DROPPED — the `cluster` kind is read/patch only (`Deletable: false`, no fixed create-cluster command); use the Rancher UI/API to create a cluster |
| `clusters import` | DROPPED — same reason as `clusters create` |
| `clusters add-node` | DROPPED — RKE1 node-driver docker-command workflow; no CAPI/RKE2 equivalent surfaced |
| `clusters delete` (alias `rm`) | DROPPED — the `cluster` kind is `Deletable: false`; no fixed delete-cluster command |
| `clusters export` | DROPPED — no export/backup command surfaced |
| `clusters kubeconfig` (alias `kf`) | → `rancher kubeconfig --cluster <id>` |
| `clusters add-member-role` | → `rancher grant <subject> --role <r> --cluster <c> --apply --yes` |
| `clusters delete-member-role` | → `rancher revoke <subject> [--role <r>] --cluster <c> --apply --yes` |
| `clusters list-roles` | → `rancher get roletemplates` — filter the `context` field (`cluster`/`project`, and empty is admitted by the webhook): `-o json` + `data.#(context=="cluster")#`, or read the CONTEXT table column |
| `clusters list-members` | → `rancher get clusterroletemplatebindings --cluster <id>` (MEMBER/ROLE/CLUSTER table; add `-o json` for full objects) |
| `machines ls` | → `rancher get machines` |
| `namespaces ls` | → `rancher get namespaces` (`--project <p>` lists a project's member namespaces) |
| `namespaces create` | → `rancher create namespace <name> [--project <p>]` |
| `namespaces delete` (alias `rm`) | → `rancher delete namespace <name>` |
| `namespaces move` | → `rancher project remove-namespace <ns> --project <old>` then `rancher project add-namespace <ns> --project <new>` — an explicit two-step, never an implicit move (spec PC-4) |
| `nodes ls` | → `rancher get nodes` |
| `nodes delete` (alias `rm`) | DROPPED — the `node` kind is `Deletable: false`; use `kubectl delete node` via `rancher kubeconfig --cluster <id>` + kubectl, or the Rancher UI |
| `projects ls` | → `rancher get projects` |
| `projects create` | → `rancher create project <name> --cluster <id> [--description] [--quota-limit ...] [--namespace-default-quota ...]` |
| `projects delete` (alias `rm`) | → `rancher delete project <id>` |
| `projects add-member-role` | → `rancher grant <subject> --role <r> --project <p> --cluster <c> --apply --yes` |
| `projects delete-member-role` | → `rancher revoke <subject> [--role <r>] --project <p> --cluster <c> --apply --yes` |
| `projects list-roles` | → `rancher get roletemplates` — filter the `context` field: `-o json` + `data.#(context=="project")#`, or read the CONTEXT table column |
| `projects list-members` | → `rancher inspect project <name-or-id>` (MEMBERS section, display names resolved); raw objects: `rancher get projectroletemplatebindings --namespace <backing-namespace> -o json` |
| `ps` | → `rancher get deployments` / `rancher get pods` (+ `rancher inspect deployment`) |
| `inspect <type> <id>` | → `rancher get <kind> <name> -o json` (+ `rancher inspect pod/deployment/project` composites) |
| `kubectl …` | → one-time `rancher kubeconfig --cluster <id>` + native `kubectl` (merge recipe below); `auth get-token` handles credentials transparently |
| `server current` | → `rancher config current-context` (server field) |
| `server delete` | DROPPED — no config command removes a stored server; edit the config file directly |
| `server ls` | → `rancher config get-servers` |
| `server switch` | → `rancher config use-server` |
| `settings ls` | → `rancher get settings` |
| `settings get` | → `rancher get setting <name>` |
| `settings set` | → `rancher patch setting <name> --patch '<RFC 6902 JSON patch>' --apply` |
| `ssh <node>` | DROPPED — RKE1/node-driver only, Norman-only; keep the legacy binary for this |
| `token` | Existing Rancher-generated kubeconfigs that call `rancher token` keep working after a binary swap — the shim delegates to `auth get-token` machinery. Regenerate kubeconfigs (`rancher kubeconfig --cluster <id>`) to move them to `auth get-token` directly. Server-side token management: `rancher get tokens` / `create token` / `delete token`. Local exec-credential cache: `auth list-tokens` / `auth delete-token`. |
| `token delete` | → `rancher auth delete-token` |
| `up -f compose.yml` | DROPPED — compose-era, no replacement |
| `wait <type> <id>` | → `rancher get <kind> <name> --wait` |

## `rancher kubeconfig`

`rancher kubeconfig --cluster <id-or-name>` emits a bare kubeconfig YAML
document to stdout. Its user entry runs `rancher auth get-token` as a
kubectl exec-credential plugin, so no long-lived token is ever written to
disk — only the CLI's own short-lived cache. It replaces the legacy
`kubectl` passthrough's purpose; the CLI itself does not wrap kubectl.

**PATH note:** the exec stanza runs `rancher` by name, so the `rancher`
binary must be on `PATH` for kubectl to authenticate.

**Merging into your default kubeconfig** (the command only writes to
stdout — it never touches `~/.kube/config`):

```
KUBECONFIG=~/.kube/config:<(rancher kubeconfig --cluster X) kubectl config view --flatten > merged && mv merged ~/.kube/config
```

**No `rancher` binary on the target machine?** (CI, teammates without the
CLI) `rancher kubeconfig`'s exec mode is not the only option: `rancher create
kubeconfig [--clusters ...] --apply` mints a server-side, embedded, revocable
kubeconfig (same as the Dashboard's "Download KubeConfig") that needs no
local `rancher` binary to authenticate — the output goes straight to stdout,
with a shown-once notice on where its server-assigned name is.

## `--wait` on named `get`

`rancher get <kind> <name> --wait [--for state=<value>|condition=<Type>[=<Status>]] [--timeout <duration>]`
replaces the legacy standalone `wait` command. Unlike `kubectl wait`, it
fails fast on a Rancher-reported provisioning error instead of burning the
full timeout. For anything `--wait` doesn't cover well (arbitrary JSONPath
conditions, non-Rancher resources), use `kubectl wait` via `rancher
kubeconfig`. `--for state=<value>` matches `status.state` (the Rancher
idiom); for kinds that carry no `status.state` (core kinds like Pod), it
falls back to `status.phase` instead, compared case-insensitively.
