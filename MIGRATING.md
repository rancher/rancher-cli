# Migrating from the legacy Rancher CLI

Config and credentials are not imported - run `rancher auth login` once per
server. The old `cli2.json` is not read, not migrated, and not deleted by this
CLI - it may contain plaintext credentials; remove it yourself
(`rm ~/.rancher/cli2.json`, or the legacy `$RANCHER_CONFIG_DIR` location).

Output is enveloped JSON/YAML/table (`-o table|json|yaml`, plus `-o name` for
IDs only); table is the default for interactive use. Mutating commands need
`--apply` (and `--yes` for destructive ones) - there is no implicit write.
Run `rancher schema` to discover the full command surface, including flags,
contract, and stability, without reading docs.

## Command disposition

Every legacy command and subcommand below maps to exactly one of: a new
spelling, a replacement recipe, or a documented drop. This table is kept
exhaustive against the full legacy command inventory by a tracked
conformance test (`internal/cli/migrating_test.go` +
`internal/cli/testdata/legacy-commands.txt`).

| Legacy | Disposition |
|---|---|
| `login` (alias `l`) | → `rancher auth login` |
| `context switch` | REMOVED - scope is per-invocation; set the active server pointer with `rancher config use-server` |
| `context current` | REMOVED - scope is per-invocation; `rancher config status` shows the current server; `rancher config list-servers` lists all (active marked with *) |
| `help` (alias `h`) | REMOVED - there is no `help` command; add `--help` or `-h` to any command (`rancher get pod --help`), or run `rancher schema` for every command and flag |
| `clusters ls` | → `rancher get clusters` |
| `clusters create` | → `rancher create cluster <name> --import` (imported), `--custom` (custom cluster), or `--machine-config <kind/name> --cloud-credential <id>` (node-driver) |
| `clusters import` | DROPPED - same reason as `clusters create` |
| `clusters add-node` | DROPPED - RKE1 node-driver docker-command workflow; no CAPI/RKE2 equivalent surfaced |
| `clusters delete` (alias `rm`) | → `rancher delete provisioning-cluster <name> --apply --yes` - the `cluster` kind itself is `Deletable: false`; deletion goes through the backing `provisioningcluster` kind |
| `clusters export` | DROPPED - no export/backup command surfaced |
| `clusters kubeconfig` (alias `kf`) | → `rancher kubeconfig --cluster <id>` |
| `clusters add-member-role` | → `rancher grant <subject> --role <r> --cluster <c> --apply --yes` |
| `clusters delete-member-role` | → `rancher revoke <subject> [--role <r>] --cluster <c> --apply --yes` |
| `clusters list-roles` | → `rancher get roletemplates` - filter the `context` field (`cluster`/`project`, and empty is admitted by the webhook): `-o json` + `data.#(context=="cluster")#`, or read the CONTEXT table column |
| `clusters list-members` | → `rancher get clusterroletemplatebindings --cluster <id>` (MEMBER/ROLE/CLUSTER table; add `-o json` for full objects) |
| `machines ls` | → `rancher get machines` |
| `namespaces ls` | → `rancher get namespaces --cluster <c>` (`--project <p>` lists a project's project namespaces) |
| `namespaces create` | → `rancher create namespace <name> --cluster <c> [--project <p>]` |
| `namespaces delete` (alias `rm`) | → `rancher delete namespace <name> --cluster <c>` |
| `namespaces move` | → `rancher project remove-namespace <ns> --cluster <c>` then `rancher project add-namespace <ns> --project <new> --cluster <c>` - an explicit two-step, never an implicit move |
| `nodes ls` | → `rancher get nodes --cluster <c>` |
| `nodes delete` (alias `rm`) | DROPPED - the `node` kind is `Deletable: false`; use `kubectl delete node` via `rancher kubeconfig --cluster <id>` + kubectl, or the Rancher UI |
| `projects ls` | → `rancher get projects` |
| `projects create` | → `rancher create project <name> --cluster <id> [--description] [--quota-limit ...] [--namespace-default-quota ...] [--container-default-resource-limit ...]` |
| `projects delete` (alias `rm`) | → `rancher delete project <id> --cluster <c>` |
| `projects add-member-role` | → `rancher grant <subject> --role <r> --project <p> --cluster <c> --apply --yes` |
| `projects delete-member-role` | → `rancher revoke <subject> [--role <r>] --project <p> --cluster <c> --apply --yes` |
| `projects list-roles` | → `rancher get roletemplates` - filter the `context` field: `-o json` + `data.#(context=="project")#`, or read the CONTEXT table column |
| `projects list-members` | → `rancher inspect project <name-or-id> --cluster <c>` (MEMBERS section, display names resolved); raw objects: `rancher get projectroletemplatebindings --namespace <backing-namespace> -o json` |
| `ps` | → `rancher get deployments --cluster <c>` / `rancher get pods --cluster <c>` (+ `rancher inspect deployment <name> --cluster <c> --namespace <ns>`) |
| `inspect <type> <id>` | → `rancher get <kind> <name> -o json` (+ `rancher inspect pod/deployment/project` composites) |
| `kubectl …` | → one-time `rancher kubeconfig --cluster <id>` + native `kubectl` (merge recipe below); `auth get-token` handles credentials transparently |
| `server current` | → `rancher config status` / `rancher config list-servers` (active marked with *) |
| `server delete` | → `rancher config delete-server <name>` |
| `server ls` | → `rancher config list-servers` |
| `server switch` | → `rancher config use-server` |
| `settings ls` | → `rancher get settings` |
| `settings get` | → `rancher get setting <name>` |
| `settings set` | → `rancher patch setting <name> --patch '<RFC 6902 JSON patch>' --apply` |
| `ssh <node>` | DROPPED - RKE1/node-driver only, Norman-only; keep the legacy binary for this |
| `token` | Existing Rancher-generated kubeconfigs that call `rancher token --server <url> --user <user-id>` keep working after a binary swap - the shim delegates to `auth get-token` machinery and also takes `--cacerts <path>`. Regenerate kubeconfigs (`rancher kubeconfig --cluster <id>`) to move them to `auth get-token` directly. Server-side token management: `rancher get tokens` / `create token` / `delete token`. Local exec-credential cache: `auth list-tokens` / `auth delete-token`. |
| `token delete` | → `rancher auth delete-token` |
| `up -f compose.yml` | DROPPED - compose-era, no replacement |
| `wait <type> <id>` | → `rancher get <kind> <name> --wait` |

## Changed in this CLI

| Before | Now |
|---|---|
| `rancher create secret <name> --project <p> --project-scoped --cluster <c>` | `rancher create secret <name> --project <p> --cluster <c>` - `--project-scoped` is gone; `--project` alone creates the project-scoped secret, and `--project` with `--namespace` is refused |
| `rancher project remove-namespace <ns> --project <p> --cluster <c>` | `rancher project remove-namespace <ns> --cluster <c>` - the namespace's own label names its project; a typed `--project` is refused, and when the namespace and `--cluster` are typed the refusal names this command |
| `--project <p>` on any command: used by the commands that scope by project, refused by many others (a few only after sending a request), and ignored by the rest | `rancher schema` lists `project` among the flags of each command that takes it; every other command refuses `--project` (VALIDATION, exit 2) before sending a request, whether it is typed before or after the command name |
| a flag typed with an empty or blank value, such as `--namespace ""` from an unset variable, acted on most flags as if it were omitted (`get pods --namespace ""` listed every namespace, `revoke --role ""` revoked every role); a few flags already refused it | refused (VALIDATION, exit 2) before any request; pass the flag only with a value - `rancher get pods --cluster <c>` lists every namespace |
| `rancher create resource -f <namespace manifest> --cluster <c> --project <p>` created the namespace outside the project | refused (VALIDATION, exit 2); `rancher create namespace <name> --cluster <c> --project <p>` creates it in the project |
| positional arguments beyond the ones a command takes were ignored: `rancher create project p1 extra --cluster <c>` created `p1` | refused (VALIDATION, exit 2) before any request; the message names the extra words, and `rancher schema <command>` lists the arguments a command takes |
| a word after a bool flag, as in `--dry-run false`, was a positional argument and the flag was on: `rancher create user --password-stdin --dry-run false` created a user named `false`, and with the name already typed the word was ignored | a word typed after a bool flag that the flag accepts as its value (`true`, `false`, `1`, `0`, ...) is refused in any position (VALIDATION, exit 2) before any request. A bool flag takes its value joined with `=` (`rancher create user <name> --password-stdin --dry-run=false`), and the hint is the command with each such value joined (the command's `--help` when that line would leave a declared argument empty or hold an extra one); an argument that reads as a bool goes before the flags or after `--` |
| a bool flag typed with `=` and an empty or blank value turned the flag on: `--apply="$APPLY"` with `APPLY` empty, blank or unset applied the change, and `--yes="$Y"` confirmed a delete | refused (VALIDATION, exit 2) before any request, for every bool flag except `--help` and `--version`, before or after the command name; the message names the flag. Type the flag alone to turn it on (`rancher delete namespace <name> --cluster <c> --apply --yes`) or with its value (`--apply=false`) |
| a `-` on its own ended the command line and the words after it were ignored: `rancher get pods - --cluster <c> -o json` lost `--cluster` and `-o json`, and `rancher create namespace --cluster <c> --apply - --dry-run` went ahead without `--dry-run` | a line with any word after a `-` on its own is refused (VALIDATION, exit 2) before any request, before or after the command name; the message names the words after it. `--help` typed after a command's name, or `--version` at the root, before the `-` still shows the help or the version. Put `-` last, or after `--` when it is meant as an argument; a `-` as a flag's value still reads stdin: `rancher create resource -f - --cluster <c> --dry-run` |
| `rancher auth delete-token <name> <name>...` deleted every name given | one name per call; a second name is refused (VALIDATION, exit 2). Run `rancher auth delete-token <name>` once per name, or `rancher auth delete-token --all` |

## `rancher kubeconfig`

`rancher kubeconfig --cluster <id-or-name>` emits a bare kubeconfig YAML
document to stdout. Its user entry runs `rancher auth get-token` as a
kubectl exec-credential plugin, so no long-lived token is ever written to
disk - only the CLI's own short-lived cache. It replaces the legacy
`kubectl` passthrough's purpose; the CLI itself does not wrap kubectl.

**PATH note:** the exec stanza runs `rancher` by name, so the `rancher`
binary must be on `PATH` for kubectl to authenticate.

**Merging into your default kubeconfig** (the command only writes to
stdout - it never touches `~/.kube/config`):

```
KUBECONFIG=~/.kube/config:<(rancher kubeconfig --cluster X) kubectl config view --flatten > merged && mv merged ~/.kube/config
```

**No `rancher` binary on the target machine?** (CI, teammates without the
CLI) `rancher kubeconfig`'s exec mode is not the only option: `rancher create
kubeconfig [--clusters ...] --apply` creates a server-side, embedded, revocable
kubeconfig (same as the Dashboard's "Download KubeConfig") that needs no
local `rancher` binary to authenticate - the output goes straight to stdout,
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
