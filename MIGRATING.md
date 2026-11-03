# Migrating from the legacy Rancher CLI

Config and credentials are not imported - run `rancher auth login` once per
server. The old `cli2.json` is not read, not migrated, and not deleted by this
CLI - it may contain plaintext credentials; remove it yourself
(`rm ~/.rancher/cli2.json`, or the legacy `$RANCHER_CONFIG_DIR` location).

Output is enveloped JSON/YAML/table (`-o table|json|yaml`, plus `-o name` for
only the NAME column); table is the default for interactive use. Mutating
commands need `--apply` (and `--yes` for destructive ones) - there is no
implicit write. Run `rancher schema` to discover the full command surface,
including flags, contract, and stability, without reading docs.

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
| `token` | Existing Rancher-generated kubeconfigs that call `rancher token --server <url> --user <user-id>` keep working after a binary swap - the shim delegates to `auth get-token` machinery and also takes `--cacerts <path>`. Regenerate kubeconfigs (`rancher kubeconfig --cluster <id>`) to move them to `auth get-token` directly. A kubeconfig Rancher writes in the form `rancher auth get-token --server <url> [--cluster <id>] --user-id <id> [--auth-provider <auth config>]` runs as written: `--user-id` chooses the stored login that records that user and the plugin refuses a token of another user, and `--auth-provider` (the auth config's exact name) chooses the sign-in and the plugin refuses a token made through another auth config. Logins made before this release and pasted tokens record their auth config on the first kubectl call that names one; nothing needs to be redone. A `--no-verify` login records no owner and no auth config; a call with `--user-id` reaches it only through the `--name` that identifies it. Server-side token management: `rancher get tokens` / `create token` / `delete token`. Local exec-credential cache: `auth list-tokens` / `auth delete-token`. |
| `token delete` | → `rancher auth delete-token` |
| `up -f compose.yml` | DROPPED - compose-era, no replacement |
| `wait <type> <id>` | → `rancher get <kind> <name> --wait` |

## Changed in this CLI

| Before | Now |
|---|---|
| `rancher create secret <name> --project <p> --project-scoped --cluster <c>` | `rancher create secret <name> --project <p> --cluster <c>` - `--project-scoped` is gone; `--project` alone creates the project-scoped secret, and `--project` with `--namespace` is refused |
| `rancher project remove-namespace <ns> --project <p> --cluster <c>` | `rancher project remove-namespace <ns> --cluster <c>` - the namespace's own annotation names its project; a typed `--project` is refused, and when the namespace and `--cluster` are typed the refusal names this command |
| a namespace created by the old CLI's `namespaces create` carries the `field.cattle.io/creatorId` annotation | Rancher deletes such a namespace, with its workloads, when its project is deleted, and keeps every namespace without that annotation (the ones this CLI, kubectl and the dashboard create). `rancher delete project <id> --cluster <c> --dry-run` lists each namespace as "rancher deletes" or "rancher keeps"; `--delete-namespaces` deletes the kept ones too |
| `--project <p>` on any command: used by the commands that scope by project, refused by many others (a few only after sending a request), and ignored by the rest | `rancher schema` lists `project` among the flags of each command that takes it; every other command refuses `--project` (VALIDATION, exit 2) before sending a request, whether it is typed before or after the command name |
| a flag typed with an empty or blank value, such as `--namespace ""` from an unset variable, acted on most flags as if it were omitted (`get pods --namespace ""` listed every namespace, `revoke --role ""` revoked every role); a few flags already refused it | refused (VALIDATION, exit 2) before any request; pass the flag only with a value - `rancher get pods --cluster <c>` lists every namespace |
| `rancher create resource -f <namespace manifest> --cluster <c> --project <p>` created the namespace outside the project | refused (VALIDATION, exit 2); `rancher create namespace <name> --cluster <c> --project <p>` creates it in the project |
| positional arguments beyond the ones a command takes were ignored: `rancher create project p1 extra --cluster <c>` created `p1` | refused (VALIDATION, exit 2) before any request; the message names the extra words, and `rancher schema <command>` lists the arguments a command takes |
| a word after a bool flag, as in `--dry-run false`, was a positional argument and the flag was on: `rancher create user --password-stdin --dry-run false` created a user named `false`, and with the name already typed the word was ignored | a word typed after a bool flag that the flag accepts as its value (`true`, `false`, `1`, `0`, ...) is refused in any position (VALIDATION, exit 2) before any request. A bool flag takes its value only as `--flag=value` (`rancher create user <name> --password-stdin --dry-run=false`), and the hint is the command with each such value joined (the command's `--help` when that line would leave a declared argument empty or hold an extra one); an argument such as `true` or `false` goes before the flags or after `--` |
| a bool flag typed with `=` and an empty or blank value turned the flag on: `--apply="$APPLY"` with `APPLY` empty, blank or unset applied the change, and `--yes="$Y"` confirmed a delete | refused (VALIDATION, exit 2) before any request, for every bool flag except `--help` and `--version`, before or after the command name; the message names the flag. Type the flag alone to turn it on (`rancher delete namespace <name> --cluster <c> --apply --yes`) or with its value (`--apply=false`) |
| the words after a `-` on its own were ignored: `rancher get pods - --cluster <c> -o json` lost `--cluster` and `-o json`, and `rancher create namespace --cluster <c> --apply - --dry-run` went ahead without `--dry-run` | a line with any word after a `-` on its own is refused (VALIDATION, exit 2) before any request, before or after the command name; the message names the words after it, or gives their count when the `-` comes before the command name or the command takes a secret. `--help` typed after a command's name, or `--version` at the root, before the `-` still shows the help or the version. Put `-` last, or after `--` when it is meant as an argument; a `-` as a flag's value still reads stdin: `rancher create resource -f - --cluster <c> --dry-run` |
| `rancher auth delete-token <name> <name>...` deleted every name given | one name per call; a second name is refused (VALIDATION, exit 2). Run `rancher auth delete-token <name>` once per name, or `rancher auth delete-token --all` |
| a `--cluster` value that is one cluster's id and another cluster's display name was refused (AMBIGUOUS_SELECTION), in every command and in kubectl's exec plugin | the id wins, with a warning naming the other cluster; `rancher access clusters` lists the clusters you can read by id, and that cluster's id reaches it |
| a `--cloud-credential` (or `get cloud-credential`) value that is one credential's id and another credential's display name was refused (AMBIGUOUS_SELECTION) | the id wins, with a warning naming the other credential; `rancher get cloud-credential` lists the credentials by id, and that credential's id reaches it |
| `rancher schema` carried a `network` boolean per command, and the contract line a `network` segment | `rancher schema -o json` gives each command a `serverAccess`: `none` (never contacts the server), `read` (reads server state only) or `write` (may create or delete server objects); the contract line shows `server=read` or `server=write` |
| `rancher auth logout`, `rancher auth logout --all`, `rancher auth delete-token` and `rancher config delete-server` deleted local copies only; the tokens the CLI created stayed valid on the server | each also revokes on the server the tokens the CLI created and recorded, outside the config lock, under a 10 s budget, even under config or environment read-only mode. `rancher auth logout` prints `data.revoked` and `data.notRevoked` (`rancher auth delete-token`, `rancher config delete-server` and `rancher config update-server --url` too; the last sends nothing and lists only `notRevoked`); a token left behind carries a `retry` and, for `manual`, a `recovery` |
| `rancher auth logout <name>` deleted every cache record for the entry's server | deletes only the records the entry's login made; a record another entry still reads stays - `rancher auth logout <name>` |
| `rancher auth list-tokens` had no column for the token a record holds | `rancher auth list-tokens` shows a `TOKEN` column (kind and name, or `-`); JSON adds `tokenRef`, `tokenUserId` and `tokenAuthConfig` |
| `rancher config delete-server <name>` returned `data.recovery` | `data.recovery` is gone; `rancher config delete-server <name>` returns `data.revoked` and `data.notRevoked`, and a token left on the removed entry carries its own `recovery` |
| `rancher auth delete-token --all` returned `data.deleted` as the string `all` | `data.deleted` is the list of deleted cache keys under `rancher auth delete-token --all` too |
| `--read-only` typed on a local command (`auth login`, `auth logout`, `auth status`, `config *`, `kubeconfig`, `skill *`, `completion`, ...) was accepted and ignored: `rancher --read-only auth login` created server tokens and `rancher --read-only auth logout` revoked them | refused with `VALIDATION` (exit 2) before the command does anything, `--read-only=false` included; run the command without `--read-only`, for example `rancher auth login`. `RANCHER_CLI_READONLY` and config `readOnly: true` apply to every run and leave local commands usable |
| `rancher auth login` as a standard user left the transient session token it created valid for the session lifetime (`auth-user-session-ttl-minutes`, 16 hours by default) and listed it in `residualTokens`: the delete through the Rancher proxy is refused with 403 for a standard user | `rancher auth login` deletes the transient session token through Rancher's logout endpoint with the token's own bearer, and `residualTokens` stays empty. When logout does not answer 200 or 204, the delete through the proxy follows; a token that neither request deletes (a provider set to forced single logout) stays valid until it expires, and the login's `warnings` (on a failed login, `details.recovery`) say `<ref> expires; an administrator can delete it sooner with: <command>` (with ` at <time>` after `expires` when the server reported the expiry) |
| `rancher auth delete-token` deleted cache records without reading `config.yaml` | takes the config lock and refuses an unparseable `config.yaml` before deleting anything; fix the file, then run `rancher auth delete-token` again |
| `rancher create user <name> --password-stdin` created the User and its password Secret and no global role; the user could not create a token, so `rancher auth login` as that user failed with an RBAC error that said to log in again | also creates a `GlobalRoleBinding` for each global role the server marks `newUserDefault` (the built-in `user` role on a default install), or for each `--global-role <id>` typed (repeatable, replaces the defaults); the result lists them in `data.globalRoleBindings`. An operator who may create users but may not bind a default role (for example one with `users-manage` and `user-base`) is refused at the preview with RBAC (exit 6), and nothing is written; pass `--global-role <id>` with a role the operator may bind (such as `user-base`), or ask an administrator to grant a role after the user exists with `rancher grant <user> --global --role <id>`. A login refused for want of a role now says that the user has no role that allows creating tokens and gives the `rancher grant ... --global --role user --dry-run` command an administrator runs |
| creating a server entry (`config add-server`, `auth login --url` for a new name, the kubectl plugin's login) erased a credential left under that name only from the credential helper the running process uses; one left in another helper stayed, and a reader whose `RANCHER_CLI_CREDENTIAL_STORE` names that helper was served it | erases the credential left under that name from every credential helper the config directory's `credhelper-provenance.yaml` records for it, and refuses the creation (`CREDENTIAL_STORE_UNAVAILABLE`, retryable, for a helper that fails or is missing from `PATH`; `VALIDATION` only for a helper answer that cannot be read; nothing more is written) when a recorded helper cannot erase it, even when the creating process does not use that helper; the kubectl plugin's login refuses a recorded helper missing from `PATH` before its sign-in. Make the helper available or working and run the command again (`rancher config add-server <name> --url <url>`, `rancher auth login <name> --url <url>`), use a different `--config`, or, if the helper is gone for good, remove its record for that name from `credhelper-provenance.yaml` |
| `rancher auth login <name> --credential-store keychain` (and the kubectl plugin's login) was refused with CREDENTIAL_TRANSITION_INCOMPLETE before the sign-in while an earlier credential store change had any step left, even a step the login's own retry would finish, and a rerun was refused again | the sign-in is asked for, and the login retries the steps the earlier change left before its own work; it is refused after the sign-in when a step still cannot finish: unfinished work with CREDENTIAL_TRANSITION_INCOMPLETE, a step that ends terminal with its own VALIDATION error; a store rule can also refuse the directory as the retry left it. A change record named in `config.yaml` that cannot be read is still refused before the sign-in; `rancher config status` shows what is left |
| a refused request was `GENERIC` (exit 1) on `get`, `create`, `patch` and `delete` of `token`, `kubeconfig` and the cluster.x-k8s.io kinds, on `create password-change-request`, `auth get-token`, `token` and `auth login`'s token create, and on `access can`, `who-can` and `access explain` when API discovery was refused | `rancher get token <name>` and the other commands named exit with the code of the server's answer: a 401 is `AUTH` (exit 3) and a 403 `RBAC` (exit 6); a 404 on an object is `NOT_FOUND` (exit 4); a 404 on an API through the local cluster (API discovery, the provider list) is `GENERIC` (exit 1) and names the server URL; a script that retried on exit 1 signs in again with `rancher auth login <name>` on exit 3 |
| a 403 from Rancher's own API (`get kubernetes-versions`, `get clusterregistrationtoken`, the issue code in `access projects`) was `AUTH` (exit 3) | `rancher get clusterregistrationtoken <name>` and the others report `RBAC` (exit 6) |
| `rancher kubeconfig` printed the proxy-only file and exited 0 when Rancher refused the credential (401) at a direct-endpoint read | `rancher kubeconfig --cluster <id>` fails with `AUTH` (exit 3) and prints no file; a 403 there still prints the proxy-only file with a notice |
| a 429, and a 5xx whose body is not a Kubernetes Status, Rancher's own error answers included, were not retryable on Rancher's API, the sign-in and the token create | `retryable: true` in the error envelope, for example from `rancher get clusterregistrationtoken <name>` or `rancher auth login <name>`; a 502, 503 or 504 with a Kubernetes Status body is retryable too, and another 5xx with a Kubernetes Status body is not |
| `rancher logs` reported `NOT_FOUND` (exit 4) when every container's log was refused | `rancher logs <target>` reports `AUTH` (exit 3) or `RBAC` (exit 6) when every read was refused with that code, with one warning per container; a mix of refusals still reports `NOT_FOUND` |
| a 409 answering a request (API discovery, the provider list, Rancher's own API, the sign-in) was `GENERIC` (exit 1) | a 409 on a read is `CONFLICT` (exit 5) and retryable, for example from `rancher get clusterregistrationtoken <name>`; on a create, including the sign-in, `CONFLICT` and not retryable; Rancher's cluster, project and namespace lists keep their own wording for a 409 |
| a review the server refused with 405, 406, 415 or 429 was `VALIDATION` (exit 2) on `access can`, `who-can` and `access explain` | `rancher access can <verb> <resource> --cluster <id>` reports it as `GENERIC` (exit 1), retryable for a 429; a 409 is `CONFLICT` (exit 5) |
| `rancher auth status` and the verification of `auth login` reported any answer to `/version` as a retryable "cannot reach server" | `rancher auth status` reports "cannot reach server" only when no answer came; a 404 is `GENERIC` (exit 1), not retryable, and names the server URL; a 409 is `CONFLICT` (exit 5); a 429 or a 503 is retryable |
| some refusals carried Rancher's raw JSON error object as their message (`access can` under a 401 or 403, Rancher's own API) | the object's message; under a 401, `rancher access can <verb> <resource> --cluster <id>` reads "the server rejected the credential (401)" with the hint `rancher auth status` |
| a 401 whose Kubernetes Status message was only "Unauthorized" read `Unauthorized`, with no hint | `rancher get token <name>` and the commands that send a request to a resource read "the server rejected the credential (401)" with the hint `rancher auth status` |
| a 415 that gave no reason read with an empty message or only the bare status | the status's text, for example `creating derived token: Unsupported Media Type` from `rancher auth login <name>` |
| a 401 on the provider list or the SAML poll, which send no credential, was `GENERIC` (exit 1) | `rancher auth get-token --cluster <id>` reports `AUTH` (exit 3) with "the server answered 401; the request carried no credential" and no hint |
| a 5xx through the local cluster said "its agent may not be registered yet" or named an unreachable downstream cluster | through the local cluster it says "retrying may succeed"; the agent and downstream wording is kept for another cluster, for example `rancher get pod <name> --cluster <id>` |
| an identity provider's refusal of an OAuth sign-in was `GENERIC` (exit 1) and not retryable | `rancher auth login <name>` reports a 4xx from the identity provider as `AUTH` (exit 3), naming the status and the provider's error code; a 408, a 429 or a 5xx is `GENERIC` (exit 1) and retryable |
| `rancher auth get-token` with no terminal, several auth providers and no `--auth-provider` was `AMBIGUOUS_SELECTION` (exit 2) with every provider as a candidate and no login remedy | `rancher auth get-token --cluster <id>` names the piped-token login and lists only the auth configs that can sign in without a terminal (SAML, OAuth); when none can, or with `--no-input`, it is `AUTH` (exit 3) with the no-terminal remedies |
| `rancher auth logout`, `auth delete-token` and `config delete-server` never sent a delete for a v3 token: an unscoped v3 token cached by the legacy `rancher token` command, or a v3 login, stayed live and was reported `v3` | an unscoped v3 cache token and a v3 login are deleted through Rancher's logout endpoint with the token itself, as `rancher auth delete-token --all` shows; a cluster-scoped v3 token stays an administrator's delete, `rancher delete management-token <name> --apply --yes --allow-deprecated` |

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

## Waiting with a named `get`

`rancher get <kind> <name> [--wait] [--for state=<value>|condition=<Type>[=<Status>]] [--timeout <duration>]`,
with at least one of the three (any of them starts the wait),
replaces the legacy standalone `wait` command. Unlike `kubectl wait`, it
fails fast on a Rancher-reported provisioning error instead of burning the
full timeout. For anything `--wait` doesn't cover well (arbitrary JSONPath
conditions, non-Rancher resources), use `kubectl wait` via `rancher
kubeconfig`. `--for state=<value>` matches `status.state` (the Rancher
idiom); for kinds that carry no `status.state` (core kinds like Pod), it
falls back to `status.phase` instead, compared case-insensitively.
