# Command reference

Part of the [Rancher CLI docs](README.md) · [operational reference](REFERENCE.md)

Every fixed command of the `rancher` CLI, by group. For the live catalog -
which also lists the generated `get`/`patch`/`delete` commands for every
kind - run `rancher schema`; `rancher schema <command>` shows one command
in detail.

[Authentication](#authentication) ·
[Servers & configuration](#servers--configuration) ·
[Create](#create) ·
[Projects & access](#projects--access) ·
[OIDC client](#oidc-client) ·
[Cluster scaling & autoscaling](#cluster-scaling--autoscaling) ·
[Read & inspect](#read--inspect) ·
[Access](#access) ·
[Logs & support](#logs--support) ·
[kubectl integration](#kubectl-integration) ·
[Telemetry](#telemetry) ·
[Discovery & tooling](#discovery--tooling)

<!-- conformance:commands-table -->
## Authentication

| Command | Purpose |
|---|---|
| `auth login [<name>] [--url <url>]` | Log in with a token (interactive prompt or `--token-stdin`, secrets never on the command line); `--url` adds the entry first when `<name>` is new, and refuses when `<name>` is already registered at a different URL. Omitting `<name>` with `--url` derives it from the URL's hostname, with the same refuse-on-collision rule. Verifies a pasted/piped token against the server before storing it, with one `SelfSubjectReview` that also names the token's owner, which the login records (`--no-verify` skips this and records no owner; not available with the provider sign-in flow, which is already authenticated). Never switches the current server - only a fresh add can bootstrap it when unset. `--credential-store` changes where the directory keeps its credentials, after the new credential is stored: a change takes only this entry's credential, and refuses before the sign-in while another entry still has one in the store being left. |
| `auth logout [<name>]` | Clear the stored login token for the current server, `<name>`, or every server with `--all`; keeps server config; deletes the exec-credential cache entries its login made, then revokes on the server the tokens the CLI created and recorded (`revoked`, `notRevoked`); records the logout on the entry |
| `auth status` | Connectivity + credential preflight (calls the cluster's Kubernetes API); an unfinished credential store change and what it has left |
| `auth get-token` | exec-credential plugin (invoked by kubeconfigs, not by hand); with `--user-id` it hands kubectl only tokens Rancher reports as that user's |
| `auth list-tokens` | List cached exec-credential entries (name, server, cluster, token, expiry); the TOKEN column names the token the entry holds |
| `auth delete-token [<name>]` | Clear cached exec-credential token(s), then revoke on the server the cluster tokens the CLI created and recorded; idempotent (no match = success) |

## Servers & configuration

| Command | Purpose |
|---|---|
| `config list-servers` | List configured servers |
| `config use-server [name]` | Switch the current server |
| `config add-server <name> --url <url>` | Add a server entry (establishes trust); bootstraps the current server if unset - never an upsert (a different URL/CA under an existing name is a conflict); a credential still stored under a name no entry had is removed first, with a warning |
| `config update-server <name>` | Change a server's URL or CA without the credential purge `delete-server` forces; a URL change clears the stored token and exec-credential cache and sends nothing to the old address (the tokens left there are listed under `notRevoked`: on the moved entry `otherServer`, `manual`, never retired by its logins; on another entry still naming the old server, retired by that entry's next login), a CA-only change keeps them |
| `config delete-server <name>` | Remove a server and its stored credentials, after revoking on the server the tokens the CLI created and recorded; `revoked` and `notRevoked` report each token, with a `recovery` for one left on the removed entry |
| `config path` | Print the resolved config file path (whether or not the file exists yet) |
| `config status` | Print config diagnostics: file path, current server, effective credential store and its source, completion settings; token reuse and every entry whose CLI-created tokens are still live, with the commands that revoke them; every entry whose exec-credential cache writes are paused while a change of its credential finishes, with the reason; an unfinished credential store change and what it has left; first stop when tab completion shows no candidates |
| `config edit` | Edit the config in `$VISUAL`/`$EDITOR` on a validated temp copy - never in place; a change to `credentialStore` is refused (a login with `--credential-store` changes it) |
| `config set-completion` | Persist live-completion preferences: enable/disable network completion and set the per-TAB HTTP budget (max 2s) |
| `config set-token-reuse` | Let the kubectl exec plugin (`auth get-token`) reuse one login credential per server: on, the first cache miss runs one login and stores an unscoped credential under `exec-login-<host>`; off (default) restores the per-cluster login and lists the dormant plugin-created logins and how to remove them |

The server pointer is the config's only session state - cluster and project
scope are explicit flags per invocation. `use-server` is the migrating
target for the legacy `server switch` command.

## Create

| Command | Purpose |
|---|---|
| `create project <name>` | Create a project, optionally with a resource quota and default container resource limits - dry-run/apply |
| `create namespace <name>` | Create a namespace, optionally inside a project from birth - dry-run/apply |
| `create secret <name>` | Create a secret: bare namespaced, or project-scoped into the project's backing namespace - dry-run/apply |
| `create resource -f <file\|->` | Create/apply a resource from a YAML manifest (file or stdin); dry-run/apply |
| `create token` | Create a server-side derived token (`ext.Token`), shown once - sensitive; dry-run/apply --yes |
| `create kubeconfig` | Create a server-side, embedded, revocable kubeconfig (`ext.Kubeconfig`) to stdout, shown once - sensitive; dry-run/apply --yes |
| `create oidc-client <name>` | Create an OIDC client for Rancher's embedded OIDC provider; waits for provisioning, prints the initial client secret once with live discovery endpoints - sensitive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |
| `create user <username> --password-stdin [--global-role <id>]` | Create a local user (`v3 User`), its password secret (`cattle-local-user-passwords`) and a `v3 GlobalRoleBinding` for each global role Rancher gives new users by default (`newUserDefault`), or for each `--global-role`; password never on the command line - sensitive; dry-run/apply --yes. Requires a server that supports local user passwords. |
| `create password-change-request <user> --password-stdin` | Change a local user's password (`ext.cattle.io PasswordChangeRequest`); completes synchronously; password never on the command line - sensitive; dry-run/apply --yes. Requires a server that supports local user passwords. |
| `create cluster <name>` | Create a provisioning cluster: imported, custom, or node-driver with one machine pool - dry-run/apply |
| `create machine-config <kind> <name>` | Create a standalone machine config for `create cluster --machine-config` - dry-run/apply; flag details in [REFERENCE.md](REFERENCE.md#cluster-provisioning) |

## Projects & access

| Command | Purpose |
|---|---|
| `project add-namespace <ns>` | Add a namespace to a project - dry-run/apply |
| `project remove-namespace <ns>` | Remove a namespace from its project and remove its project label - dry-run/apply |
| `grant <subject>` | Grant a role at project, cluster, or global scope - dry-run/apply --yes |
| `revoke <subject>` | Revoke role binding(s) at a scope; omitting `--role` revokes every role there - destructive; dry-run/apply --yes |

## OIDC client

| Command | Purpose |
|---|---|
| `oidc-client add-secret <client>` | Add a new client secret; waits for the controller, shown once - sensitive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |
| `oidc-client rotate-secret <client> [secret-name]` | Regenerate a client secret (name required with more than one secret); shown once - sensitive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |
| `oidc-client remove-secret <client> <secret-name>` | Remove a client secret - destructive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |

See also `create oidc-client` in [Create](#create), which creates the client itself.

## Cluster scaling & autoscaling

| Command | Purpose |
|---|---|
| `cluster scale <cluster> --pool <name> --nodes <n>` | Change the number of nodes in a machine pool; refuses when the machine pool has autoscaling turned on and autoscaling is not paused, or when the cluster has no machine pools - destructive; dry-run/apply --yes |
| `cluster set-autoscaling <cluster> --pool <name> --min <n> --max <m>` | Turn on autoscaling for a machine pool - destructive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |
| `cluster clear-autoscaling <cluster> --pool <name>` | Turn off autoscaling for a machine pool; succeeds and changes nothing when the machine pool has no autoscaling - destructive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |
| `cluster pause-autoscaling <cluster>` | Pause the cluster autoscaler cluster-wide - destructive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |
| `cluster resume-autoscaling <cluster>` | Resume the cluster autoscaler cluster-wide - destructive; dry-run/apply --yes. A Rancher Prime feature; the connected server decides whether it is available. |

See also `create cluster` in [Create](#create) and `delete provisioning-cluster`
for lifecycle; `get provisioning-cluster` for reads.

## Read & inspect

| Command | Purpose |
|---|---|
| `get cloud-credential [name]` | List cloud credentials or show one - secret values never shown |
| `get kubernetes-versions` | List available Kubernetes versions (rke2/k3s), merged with the server's default and supported-range settings |
| `inspect cluster <name-or-id>` | Composite cluster read: identity, connectivity, provisioning, machine pools, members, machines, registration commands (node command and import command; gated behind `--registration`) |
| `inspect pod <name>` | Composite pod read: describe + recent logs |
| `inspect deployment <name>` | Composite deployment read |
| `inspect project <name-or-id> [--cluster]` | Composite project read: project, namespaces, members, resource quota, usage |
| `inspect user <id\|username\|displayName\|principal>` | Composite user read: id and names, groups, and role bindings at global, cluster and project level - bindings that exist only, never permissions |
| `images` | Active-pod container images, grouped by image or owning workload, fleet-wide |
| `usage` | Resource usage composite (grouped by `--by`, filtered by scope flags) |

## Access

| Command | Purpose |
|---|---|
| `access can <verb> [<resource>[/<name>]]` | Ask the cluster's authorizer whether you (or `--as` a subject) can perform a verb on a resource; exit 0 allowed, 12 denied, 13 unknown |
| `access list` | List the rules you (or `--as` a subject) hold in a namespace as the authorizer reports them; exit 13 when any namespace is incomplete |
| `access who-can <verb> [<resource>[/<name>]]` | List the subjects the cluster's bindings name that the authorizer allows to perform a verb on a resource, one SubjectAccessReview each; denied rows with `--show-denied`; exit 13 when a row has a request issue or is unknown because the authorizer returned an evaluation error |
| `access explain <verb> [<resource>[/<name>]]` | Explain the authorizer's decision for one subject, verb, and resource in one namespace: the binding and role the server's reason names, the rules covering the request, and the Rancher chain behind the binding (ClusterRoleTemplateBinding or ProjectRoleTemplateBinding, GlobalRoleBinding and GlobalRole, RoleTemplate inheritance); on deny, the bindings naming the subject or its groups; exit 0 allowed, 12 denied, 13 unknown, never changed by a partial chain |
| `access clusters` | List the clusters Rancher lets you (or `--as` a user) into, as its proxy answers per cluster, beside the management bindings that grant it; exit 13 when any cluster is unknown |
| `access projects [--cluster]` | List the projects you (or `--as` a user) can see into, grouped by cluster: the authorizer's answer to getting each namespace of the project, summarized per project (all, partial, none, unknown), beside the ProjectRoleTemplateBindings that make the subject a member and the cluster or global bindings that reach it without membership; exit 13 when any cluster, project or namespace is unknown |

## Logs & support

| Command | Purpose |
|---|---|
| `logs <target>` | Verified-location logs of Rancher components for `server`/`agent`/`fleet-agent`/`kube-api-auth`/`webhook`/`api-extension` |
| `support-bundle` | Pack what Rancher's Kubernetes API can reach (logs, cluster/node state, events, settings/features/schema) into one tar.gz for support |

## kubectl integration

| Command | Purpose |
|---|---|
| `token` | Legacy exec-credential shim for Rancher-generated kubeconfigs (deprecated; new kubeconfigs use `auth get-token`) |
| `kubeconfig` | Emit a LOCAL exec-credential kubeconfig for a cluster (no embedded secret), with the direct endpoint contexts when the cluster has the Authorized Cluster Endpoint; the exec entry records its config directory and, with `--user-id`, the owner its login records, and `--exec-command` names the binary kubectl runs - see `create kubeconfig` for the server-created, embedded alternative |

## Telemetry

| Command | Purpose |
|---|---|
| `telemetry status` | Show whether usage recording is on, what decided it (the `RANCHER_CLI_TELEMETRY` variable, the `telemetry` key of `config.yaml`, or the default), the counts of recorded and dropped events, the contract version and the config directory id; in a development build with `RANCHER_CLI_EXPERIMENT` set, also the active experiments and the upload endpoint; a development build sends the stored events of finished hours to that endpoint at most once an hour, after a recorded run, never from the kubectl exec plugin, and with an endpoint status shows the last attempt, the next attempt, the events waiting and the events of the current hour; creates nothing and works while recording is off |
| `telemetry on` | Set the `telemetry` key to `true`; creates the config directory when it is missing and deletes nothing |
| `telemetry off` | Set the `telemetry` key to `false`, then delete the recorded events, the server facts, the config directory id and the upload state (the upload lock and the state file in `telemetry/upload`); creates the config directory when it is missing |
| `telemetry reset` | Delete the recorded events, the server facts, the config directory id and the upload state (the upload lock and the state file in `telemetry/upload`); keeps the `telemetry` key |
| `telemetry show` | List the recorded events, oldest first; `-o json` carries every stored field of each event; in a development build with the `telemetryupload` experiment, a sent event is listed until the file that holds it is deleted |
| `telemetry upload` | Send the stored events of finished hours and of the current hour to the `telemetryupload` experiment endpoint now, without waiting for the hourly limit; development builds only, at most 30 seconds, never from the kubectl exec plugin; exits 0 whatever the endpoint answered; `sent` counts every event in a request the endpoint answered with a 2xx, including the ones it refused |

## Discovery & tooling

| Command | Purpose |
|---|---|
| `schema [command\|kind\|group]` | Schema manifest: bare = grouped table of contents; `<command>`/`<kind>`/`<group>` = one card; filter flags search all commands |
| `completion <bash\|zsh\|fish\|pwsh>` | Print a shell completion script; `-o` is ignored, output is always the raw script |
| `skill install` | Install the rancher-cli agent skill into coding-agent harnesses (a target that already holds the same files is left untouched and reported `unchanged`) |
| `skill uninstall` | Remove the agent skills that rancher-cli installed (a skill directory that rancher-cli did not install is never removed: the command stops with an error and changes no target) |
| `skill status` | Report, for each harness, the installed skill version and whether its files are unchanged; exits 7 when a selected target is not in the expected state (another version, changed files, content rancher-cli did not install, or missing under --require-installed) |
<!-- /conformance:commands-table -->

Nouns accept singular, plural, and short aliases (`get pod` = `get pods` =
`get po`). Addressing a namespaced kind by name needs `--cluster`
(management) or `--namespace`/`-n` (downstream); bare lists may omit them.
Every generated `get <kind>` list takes `--selector` (`-l`), a Kubernetes
label selector applied by the server (`get pods --selector app=web,tier!=cache`);
it cannot be combined with a name.

`get`, `patch` and `delete` of a `user`, `project`, `roletemplate`,
`globalrole` or `nodedriver` take the object's id, not its username or
display name. A miss names the id only when the typed text is exactly the
username or display name of one object.

Per-kind verb rules and kind notes:
[REFERENCE.md](REFERENCE.md#kinds--verbs). Composite commands:
[REFERENCE.md](REFERENCE.md#composite-commands). Project scoping:
[REFERENCE.md](REFERENCE.md#project-scope---project).

<!-- conformance:surface-counts -->
Generated per kind from the 51-kind table: `get <kind>` (51), `patch <kind>`
(51), `delete <kind>` (32 - 31 deletable kinds, including the RBAC binding
kinds, `token`, `kubeconfig`, `management-token`, `user`, and `oidcclient`, plus a `provisioning-cluster` override) -
134 generated commands, 203 total together with the fixed commands above. The authoritative, always-current list is
`rancher schema --all -o json`.
<!-- /conformance:surface-counts -->
