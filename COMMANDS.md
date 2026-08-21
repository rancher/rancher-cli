# Command reference

Part of the [Rancher CLI docs](README.md) · [operational reference](REFERENCE.md)

Every fixed command of the `rancher` CLI, by group. For the live catalog —
which also lists the generated `get`/`patch`/`delete` commands for every
kind — run `rancher schema`; `rancher schema <command>` shows one command
in detail.

[Authentication](#authentication) ·
[Servers & configuration](#servers--configuration) ·
[Create](#create) ·
[Projects & access](#projects--access) ·
[Read & inspect](#read--inspect) ·
[Logs & support](#logs--support) ·
[kubectl integration](#kubectl-integration) ·
[Discovery & tooling](#discovery--tooling)

<!-- conformance:commands-table -->
## Authentication

| Command | Purpose |
|---|---|
| `auth login` | Store a server + token; set the current server. Token via interactive prompt or `--token-stdin` (the `--token` flag is removed; secrets never on the command line). |
| `auth logout [<name>]` | Clear the stored login token for the current server, `<name>`, or every server with `--all`; keeps server config; does not touch the exec-credential cache |
| `auth status` | Connectivity + credential preflight (calls the cluster's Kubernetes API) |
| `auth get-token` | exec-credential plugin (invoked by kubeconfigs, not by hand) |
| `auth list-tokens` | List cached exec-credential entries (name, server, cluster, expiry) |
| `auth delete-token [<name>]` | Clear cached exec-credential token(s); idempotent (no match = success) |

## Servers & configuration

| Command | Purpose |
|---|---|
| `config get-servers` | List configured servers |
| `config use-server [name]` | Switch the current server |
| `config delete-server <name>` | Remove a server and its stored credentials — does NOT revoke the server-side token |
| `config path` | Print the resolved config file path (whether or not the file exists yet) |
| `config status` | Print config diagnostics: file path, current server, effective credential store and its source, completion settings — first stop when tab completion shows no candidates |
| `config edit` | Edit the config in `$VISUAL`/`$EDITOR` on a validated temp copy — never in place |
| `config set-completion` | Persist live-completion preferences: enable/disable network completion and set the per-TAB HTTP budget (max 2s) |

The server pointer is the config's only session state — cluster and project
scope are explicit flags per invocation. `use-server` is the migrating
target for the legacy `server switch` command.

## Create

| Command | Purpose |
|---|---|
| `create project <name>` | Create a project, optionally with a resource quota — dry-run/apply |
| `create namespace <name>` | Create a namespace, optionally inside a project from birth — dry-run/apply |
| `create secret <name>` | Create a secret: bare namespaced, or project-scoped into the project's backing namespace — dry-run/apply |
| `create resource -f <file\|->` | Create/apply a resource from a YAML manifest (file or stdin); dry-run/apply |
| `create token` | Create a server-side derived token (`ext.Token`), shown once — sensitive; dry-run/apply --yes |
| `create kubeconfig` | Create a server-side, embedded, revocable kubeconfig (`ext.Kubeconfig`) to stdout, shown once — sensitive; dry-run/apply --yes |
| `create cluster <name>` | Create a provisioning cluster: imported, custom, or node-driver with one machine pool — dry-run/apply |
| `create machine-config <kind> <name>` | Create a standalone machine config for `create cluster --machine-config` — dry-run/apply; flag details in [REFERENCE.md](REFERENCE.md#cluster-provisioning) |

## Projects & access

| Command | Purpose |
|---|---|
| `project add-namespace <ns>` | Add a namespace to a project — dry-run/apply |
| `project remove-namespace <ns>` | Remove a namespace from a project — dry-run/apply |
| `grant <subject>` | Grant a role at project, cluster, or global scope — dry-run/apply --yes |
| `revoke <subject>` | Revoke role binding(s) at a scope; omitting `--role` revokes every role there — destructive; dry-run/apply --yes |

## Read & inspect

| Command | Purpose |
|---|---|
| `get cloud-credential [name]` | List cloud credentials or show one — secret values never shown |
| `get kubernetes-versions` | List available Kubernetes versions (rke2/k3s), merged with the server's default and supported-range settings |
| `inspect cluster <name-or-id>` | Composite cluster read: identity, connectivity, provisioning, machine pools, members, machines, node-join registration (gated behind `--registration`) |
| `inspect pod <name>` | Composite pod read: describe + recent logs |
| `inspect deployment <name>` | Composite deployment read |
| `inspect project <name-or-id> [--cluster]` | Composite project read: project, namespaces, members, quota, usage |
| `inspect user <id\|username\|displayName\|principal>` | Composite user read: identity, groups, and roles across scopes — bindings that exist only, never permissions |
| `images` | Active-pod container images, grouped by image or owning workload, fleet-wide |
| `usage` | Resource usage composite (grouped by `--by`, filtered by scope flags) |

## Logs & support

| Command | Purpose |
|---|---|
| `logs <target>` | Curated, verified-location logs for `server`/`agent`/`fleet-agent`/`kube-api-auth`/`webhook` |
| `support-bundle` | Pack the API-reachable triage plane (logs, cluster/node state, events, settings/features/schema) into one tar.gz for support |

## kubectl integration

| Command | Purpose |
|---|---|
| `token` | Legacy exec-credential shim for Rancher-generated kubeconfigs (deprecated; new kubeconfigs use `auth get-token`) |
| `kubeconfig` | Emit a LOCAL exec-credential kubeconfig for a cluster (no server call, no embedded secret) — see `create kubeconfig` for the server-created, embedded alternative |

## Discovery & tooling

| Command | Purpose |
|---|---|
| `schema [command\|kind\|group]` | Capability manifest: bare = grouped table of contents; `<command>`/`<kind>`/`<group>` = one card; filter flags span the whole surface |
| `completion <bash\|zsh\|fish\|pwsh>` | Print a shell completion script; `-o` is ignored, output is always the raw script |
| `skill install` | Install the rancher agents skill into coding-agent harnesses |
| `skill uninstall` | Remove rancher-installed agents skills (only exact owner-marked installations) |
| `skill status` | Report installed skill versions and integrity; exits 7 when a selected target is out of state |
<!-- /conformance:commands-table -->

Nouns accept singular, plural, and short aliases (`get pod` = `get pods` =
`get po`). Addressing a namespaced kind by name needs `--cluster`
(management) or `--namespace` (downstream); bare lists may omit them.
Per-kind verb rules and kind notes:
[REFERENCE.md](REFERENCE.md#kinds--verbs). Composite commands:
[REFERENCE.md](REFERENCE.md#composite-commands). Project scoping:
[REFERENCE.md](REFERENCE.md#project-scope---project).

<!-- conformance:surface-counts -->
Generated per kind from the 48-kind table: `get <kind>` (48), `patch <kind>`
(48), `delete <kind>` (30 — 29 deletable kinds, including the RBAC binding
kinds, `token`, `kubeconfig`, `management-token`, and `user`, plus a `provisioning-cluster` override) —
126 generated commands, 169 total together with the fixed commands above. The authoritative, always-current list is
`rancher schema --all -o json`.
<!-- /conformance:surface-counts -->
