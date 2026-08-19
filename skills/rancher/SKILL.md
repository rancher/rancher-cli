---
name: rancher
description: Operate Rancher-managed Kubernetes with the rancher CLI. Use when asked to inspect, create, change, delete, or wait on Rancher resources (clusters, projects, fleet, workloads), to triage cluster state, or to produce a kubeconfig for kubectl. Covers auth, discovery, the dry-run/apply safety loop, structured errors, and waits.
---

# Operating Rancher with the rancher CLI

The `rancher` CLI is agent-first: every command emits a structured envelope,
every mutation is gated, and the CLI describes itself at runtime. Prefer
querying the CLI over guessing syntax.

## Protocol (follow in order)

### 1. Verify auth before anything else

```
rancher auth status -o json
```

Reports server, server version, `rancherVersion`/`supported`
(the 2.14 minimum-version floor — warn-only, never fails login/check),
`identity`/`token` metadata, and credential-store state. A `null` `identity`
or `token` field plus a `warnings` entry is a normal degradation (e.g.
`identity` is a create-verb probe — expect it null under a read-only-RBAC
token), not an error condition; don't treat it as one. If it fails with
`AUTH` or `NOT_LOGGED_IN`, establish a session:

- Interactive: `rancher auth login --server <url> --name <alias>` — no UI trip
  needed. With no piped/pasted token, it offers a choice: paste an existing
  API token, or sign in via an identity provider (`--auth-provider`,
  `--auth-flow devicecode|authcode` skip the prompt). The provider path creates
  its own purpose-made, individually revocable token behind the scenes (see
  the token taxonomy below).
- Scripted/CI: pipe the token — `rancher auth login --server <url> --token-stdin < token.txt`,
  or set `RANCHER_CLI_URL` + `RANCHER_CLI_TOKEN` env vars (no stored config needed).
- SAML/OAuth provider flows also live on `rancher auth get-token`
  (`--auth-provider`, `--auth-flow devicecode|authcode`) — the kubeconfig exec
  plugin drives them automatically; that path creates cached exec credentials,
  not a stored login session.

**Three kinds of token** — three distinct things called "token", each created by a
different actor for a different purpose:

1. `auth login` — takes a token you already have (pasted, piped, or created by
   its own provider path above) and stores it in config. It never creates a
   token itself; Rancher does.
2. `auth get-token` — drives the identity-provider flow via `/v1-public`;
   RANCHER creates the session token (`spec.kind: "session"`). The kubeconfig
   exec plugin drives this automatically and caches the result.
3. `create token` — an existing authenticated session asks the ext Token
   store to create a DERIVED token (`spec.kind` empty), individually revocable
   and visible in `get tokens`.

### 2. Discover, don't guess

```
rancher schema -o json              # orientation: command names + usage per group, kind names, exit codes (small)
rancher schema get pod -o json      # one command's full contract: args, flags, gates
rancher schema --all -o json        # the complete manifest in one document (bulk extraction only)
```

The orientation payload is compact; the full manifest is for bulk extraction.
The schema is the source of truth for the installed version. When unsure of a
flag, a kind name, or whether a command is destructive, read the schema before
constructing a call. Kind names have kubectl-style aliases (e.g. `get pods`,
`get cm`). `get kubernetes-versions` always marks the newest release per
distro (`newest`); when the server declares no default version, that marker
is the only pick-this signal and a warning says so.

Kind lookups are two steps: `rancher schema <kind> -o json` returns the
kind's row — routing, aliases, per-verb availability — and a verb's full
flag set is one more call: `rancher schema get <kind> -o json`.

### 3. Always parse, never scrape

Use `-o json` for anything you will read programmatically. Every success is an
envelope:

```json
{ "apiVersion": "cli.rancher.io/v1", "kind": "...", "data": { ... }, "warnings": [] }
```

Read `data`; surface `warnings` to the user. Other formats: `table` (default,
for users), `yaml`, `name` (ids only), or `template=<go-template>` — a Go template
over the JSON view of `data` (same shape as `-o json`). Anything else is a
`VALIDATION` error. `metadata.managedFields` never appears in CLI output across
all formats.

### 4. Mutation safety loop — never skip

Every server mutation goes through the same gate:

```
rancher create project web --cluster prod --dry-run -o json   # 1. preview the server-computed Effect (no change)
rancher create project web --cluster prod --apply             # 2. execute
rancher delete project web --cluster prod --apply --yes       # 3. destructive effects additionally need --yes
```

- A mutation without `--apply` is refused with `CONFIRMATION_REQUIRED`; the
  error's `hint` names the exact flags to add. Relay the dry-run Effect (action,
  target, diff) to the user before applying anything non-trivial.
- Patch previews include a field-level diff (`data.diff`) — show it.
- `secret` diffs are redacted by design: `data`/`stringData` entries show the
  changed path but `from`/`to` are `<redacted>`, never the raw value.
- `--allow-deprecated` is required to write through a deprecated-stability API.
- `--dry-run` is a real server dry-run: it needs the same RBAC verb as the
  write. An `RBAC` error on dry-run means the write would be denied too.
- The dry-run's `resourceVersion`/`conflictDetection` fields describe THAT
  invocation only. `--apply` re-plans from scratch — fresh read, fresh
  resourceVersion — and its conflict check spans its own plan-to-execute
  window, not the gap since your dry-run. If the object may have changed
  since the preview, re-run `--dry-run` and re-read the diff before applying.
- Never chain `--apply --yes` onto a command you have not dry-run or whose
  effect the user has not asked for.

Node-driver provisioning is two steps — author the machine config, then
reference it from the cluster:

```
rancher create machine-config digitaloceanconfig web-pool --region nyc3 --size s-2vcpu-4gb --dry-run -o json
rancher create machine-config digitaloceanconfig web-pool --region nyc3 --size s-2vcpu-4gb --apply
rancher create cluster do-1 --machine-config digitaloceanconfig/web-pool --cloud-credential do-prod --k8s-version v1.30.5+rke2r1 --etcd --controlplane --worker --dry-run -o json
rancher create cluster do-1 --machine-config digitaloceanconfig/web-pool --cloud-credential do-prod --k8s-version v1.30.5+rke2r1 --etcd --controlplane --worker --apply
```

The uncurated kinds (amazonec2config, azureconfig, harvesterconfig,
vmwarevsphereconfig) take driver fields via repeatable `--set field=value`.
Credential fields are rejected on a machine config — they belong on a
cloud credential (`cc-*` secret), referenced via `--cloud-credential`.
Completion on `--set` returns only statically-known driver fields for curated
kinds; uncurated kinds return no completion candidates by design, not by
failure.

Unset curated machine-config fields inherit the node-driver's declared defaults,
which may be stale (a retired image slug will loop provisioning forever). The
`create machine-config` dry-run preview lists the unset value fields in its
`warnings`; set them explicitly rather than trusting the driver's defaults.

A `creatorId not stamped` warning on `create cluster` means the identity
probe (an ext.cattle.io SelfUser create) failed before the server-side
dry-run. rancher-webhook's validator requires the `creatorId` annotation on
dry-run even though its mutator only stamps it on real creates, so the
dry-run may be denied with an admission error. This is a diagnostic warning,
not a definitive block — surface it to the user, but do not treat the
dry-run denial as final: a real `--apply` create is still expected to
succeed because the mutator stamps identity server-side on real creates.

Follow through to readiness and access — and read the registration
(join) commands from `inspect cluster --registration`, never from the UI:

```
rancher create cluster web --custom --k8s-version v1.32.4+rke2r1 --apply
rancher inspect cluster web --registration -o json      # data.registration.nodeCommand: run on each node to join
rancher get cluster web --wait --timeout 15m   # kind default: condition=Ready
rancher kubeconfig --cluster web > kc.yaml
```

`inspect cluster <name-or-id>` is the one-call cluster triage: management
identity and version, the provisioning CR's ready flag and conditions,
machine pools, and both registration pairs (node join and import) plus
`manifestUrl`. The management read is mandatory: an unknown or ambiguous
cluster, or a failed management read, is a structured error with a
non-zero exit. Only the provisioning and registration sections degrade —
to `warnings`, or a `provisioningNote` for a confirmed missing
provisioning CR. Check `data.status` presence before reading conditions.
`data.agent` carries the management Cluster's agent connectivity
(`connected`/`ready` + failing message) — a disconnected or not-ready agent also lands
in `warnings`, so check it before trusting `data.status.ready`. The
registration section is withheld by default (it contains live join
credentials); pass `--registration` to fetch and show it. The credential
is also available via `rancher get clusterregistrationtoken default-token
--namespace <cluster-id>`.
Without the
flag `data.registration` carries `{"withheld":true,"hint":"..."}` — check
`data.registration.withheld` before reading `nodeCommand`. On an
imported cluster `data.registration.notApplicable` names the node-join
fields that do not apply (they stay present — pick the import
`command`/`manifestUrl` instead), and member entries with `system: true`
are Rancher-internal accounts, not operator grants.

### 5. Branch on structured errors, not text

Errors land on stderr as an envelope with the error object under `data`:

```json
{ "apiVersion": "cli.rancher.io/v1", "kind": "Error",
  "data": { "error": { "code": "NOT_FOUND", "message": "...", "hint": "...", "retryable": false } } }
```

Read `data.error.code` / `data.error.hint`. The JSON envelope appears on
stderr regardless of the `-o` format. A plain-text notice (the credential-store
pin warning) may precede it on stderr — the error envelope is
always the LAST JSON object on stderr; parse from there, not the first line.

Exit codes: `0` ok · `2` usage/validation/confirmation · `3` auth ·
`4` not-found · `5` conflict/precondition · `6` RBAC · `1` generic.

- Follow the `hint` — it is machine-actionable (e.g. the exact missing flag).
- `retryable: true` (timeouts, conflicts) → retry is reasonable; otherwise do
  not repeat the identical call.
- `AMBIGUOUS_SELECTION` returns a `candidates` list — pick with the user, don't
  guess.

### 6. Waits

```
rancher get gitrepo main --wait --timeout 5m                       # kind's default readiness
rancher get cluster c-abc --wait --for condition=Ready --timeout 10m
rancher get pod x --wait --for state=running
```

Grammar: `state=<value>` or `condition=<Type>[=<Status>]` (default Status
`True`). JSONPath is deliberately unsupported — for that, use `kubectl wait`
via `rancher kubeconfig`. Waits fail fast when Rancher marks the object
`transitioning=error` (non-retryable, with the server's message) instead of
using up the timeout. On success the `-o json`/`-o yaml` envelope carries a
one-line `summary` (`wait: condition=Ready satisfied after 53s`) so pass/fail
needs no object parsing; default/table format prints the same summary as its
own stdout line above the table header. When only the verdict matters, the
exit code is authoritative; `-o name` keeps a successful wait's output to
the bare resource name — no need to receive or parse the full object.

### 7. Triage composites

One call each, with graceful degradation (missing metrics/logs become
`warnings`, never failures).

A composite answers with what you can read. A collector you lack access to
is named in `data.degraded` — `{section, state: unavailable|partial, code,
target?}` — with one matching `warnings` entry, and the command still exits
0. Exit 0 therefore does not mean "complete": read `data.degraded` (and
`warnings`) before concluding a section is empty, absent, or zero. Only a
command's required collectors — its identity, scope, and substance reads,
listed in each command's `rancher schema <command>` card — fail the command
outright.

- `rancher inspect pod <name>` — status, owners, events, logs, metrics
- `rancher inspect deployment <name>` — rollout + its pods (selector-scoped)
- `rancher inspect project <name-or-id>` — member namespaces, workload counts, members, quota, usage
- `rancher usage` — capacity and consumption, grouped by `--by namespace|project|node|pod` and narrowed by `--namespace`/`--project`/`--node`
- `rancher images [--by image|workload] [--match substr]` — active-pod image inventory; repeatable `--match` substring-filters (OR), `--by workload` maps each image to its owning workload (CVE blast radius: `--match <affected-repo>: --by workload --all-clusters`)

### 8. kubectl interop

For anything the CLI does not cover, generate a kubeconfig and use kubectl:

```
rancher kubeconfig --cluster <name-or-id> > kc.yaml
kubectl --kubeconfig kc.yaml get nodes
```

The kubeconfig uses an exec-credential plugin that calls `rancher auth
get-token` — the `rancher` binary must stay on PATH for kubectl to
authenticate. Cached credentials: `rancher auth list-tokens` /
`rancher auth delete-token`.

### 9. Observability & support

- `rancher logs <target> [--cluster <c>] [--since 1h] [--tail 500]` — curated,
  verified-location logs for `server`, `agent`, `fleet-agent`,
  `kube-api-auth`, or `webhook`; multi-pod targets merge with
  `[pod/container]` prefixes. A missing target degrades to an honest
  `NOT_FOUND` (a single-node Docker install's server logs live in `docker
  logs <container>`; a non-ACE cluster has no `kube-api-auth`).
- `rancher support-bundle -f bundle.tar.gz [--targets ...] [--clusters ...]` —
  packs every curated target's logs plus cluster/node state, events, and an
  allowlisted settings/features/schema snapshot into one tar.gz;
  `manifest.json` inside records each item as collected, skipped (not
  applicable), or errored, and partial bundles are normal. Complements
  rancherlabs/support-tools' node-level collector, not a replacement for it.
- `rancher patch feature <name> --patch '[{"op":"replace","path":"/spec/value","value":true}]' --apply --yes` —
  flip a Rancher feature flag (RFC 6902 patch, like every `patch <kind>`);
  gated like any sensitive-kind patch (`--yes` required, since flipping a
  flag is destructive by policy).

### 10. Role grants and secrets

- `rancher grant <subject> (--project <p> [--cluster <c>] | --cluster <c> | --global) --role <r>` —
  grant a role at project, cluster, or global scope. Subject is a user/group
  name, id, or fully qualified principal. Groups must be passed as a fully
  qualified group principal id (e.g. `okta_group://<id>`); `--type` only
  disambiguates principals whose provider prefix cannot be inferred.
  Principals with no matching user trigger a provisioning warning;
  Rancher auto-creates the user when the binding applies. Dry-run/apply --yes.
- `rancher revoke <subject> (--project <p> [--cluster <c>] | --cluster <c> | --global) [--role <r>]` —
  revoke role binding(s). Omitting --role revokes every role at the scope
  (destructive, needs `--yes`). Partial failures leave already-removed
  bindings gone. Revoke removes the binding only; an auto-provisioned user
  is removed with `rancher delete user <id> --apply --yes` (destructive,
  cascades role bindings).
- `rancher get roletemplates -o json` — filter `context=="cluster"` or
  `context=="project"` to find role names for `--role`; `-o table` shows the
  same as a CONTEXT column. Once a scope flag (`--cluster`, `--project`, or
  `--global`) is on the line, `--role <TAB>` completes live (requires the
  network-completion opt-in). Positional completion may return the full
  candidate list and rely on the shell's client-side prefix filtering.
- `rancher create secret <name> --namespace <ns> --cluster <c> --from-literal k=v[,k=v...]` —
  an ordinary namespaced secret without hand-rolling base64.
- `rancher create secret <name> --project <p> --project-scoped --from-literal k=v` —
  writes into the project's backing namespace for Rancher to propagate into
  every member namespace; a `warnings` entry means propagation isn't
  guaranteed on this server (requires Rancher >= 2.14, the supported
  minimum) and it won't propagate.

## Conventions

- **Routing:** `--cluster` (display name or id) routes to a downstream cluster;
  `--namespace` scopes namespaced kinds. Scope is per-invocation only — no
  cluster or project is persisted or ambient. Fleet kinds default to the
  `fleet-default` workspace.
- **Project scope (`--project`):** `--project <id-or-name>` scopes Downstream
  namespaced kinds (`get`/`delete`/`patch`, plus `usage`) to a project's member
  namespaces — a list filters to members, a named op or mutation validates
  membership first. `get namespaces --project <p>` is the membership listing
  (server-side label selector). Every other kind family and the non-project
  composites (`inspect pod`/`deployment`, `top node`, `images`) reject a
  non-empty `--project` with `VALIDATION` — it is never a silent no-op.
  Precedence: `--project` flag > unscoped.
- **Isolation for scripted sessions:** point `RANCHER_CLI_CONFIG_DIR` at a
  temporary directory and set `RANCHER_CLI_CREDENTIAL_STORE=file` so a scripted run
  never touches the operator's real config or OS keychain.
- **Read-only sessions:** `--read-only` (or config `readOnly: true` / a
  non-empty `RANCHER_CLI_READONLY` — including the literal `"false"`) refuses
  every mutating command before any network call, one-way with no
  per-invocation override. `rancher schema -o json` reports
  `readOnly`/`readOnlySources`. For real enforcement pair it with a Rancher
  token bound to read-only RBAC — the CLI-side gate is a courtesy, not a
  security boundary, and a read-only RBAC token can't `--dry-run` a mutation
  either (dry-run still needs write verbs server-side), so read-only mode
  refuses those outright too.
- **`config path` / `config status` / `config edit`:** `config path` prints the
  resolved config file path — point other tooling at it. `config status` adds
  the effective credential store and its source (env > config > auto default,
  plus the probed pin), and a nested `completion` section (network: on/off;
  timeout: effective per-TAB budget; timeoutRaw: only when clamped or
  unparseable) — the first place to check when TAB shows no candidates;
  config.yaml alone cannot reveal an env-supplied store or diagnose a
  misconfigured completion timeout. `config edit` opens the config in
  `$VISUAL`/`$EDITOR` on a temp copy, validating referential integrity
  before writing back — reach for it over several individual `use-server`
  calls when reshaping multiple servers at once; needs a TTY and
  `$VISUAL`/`$EDITOR` set.
- **Secrets:** never echo tokens into commands or logs; use `--token-stdin` or
  env vars. Sensitive kinds (users, settings, role bindings, …) escalate
  mutations to the destructive gate — expect `--yes` to be demanded even for a
  patch.
- **Contract/stability:** server ops surface `contract` (`public` | `internal`)
  and `stability` (`stable`/`beta`/`deprecated`) in the schema manifest — mention them when the user
  is deciding whether to depend on an operation.
- **Token commands — local vs. server:**

  | Command | Scope | Manages |
  |---|---|---|
  | `auth list-tokens` / `auth delete-token` | local | the exec-credential cache (`token-cache.yaml`) |
  | `get tokens` / `get token <name>` | server | `ext.Token` objects |
  | `create token` | server | creates a derived `ext.Token` |
  | `delete token <name>` | server | revokes an `ext.Token` |

  They are not interchangeable — deleting a cache entry does not revoke the
  server-side token, and vice versa.
- **`kubeconfig` vs. `create kubeconfig`:** `kubeconfig` renders a LOCAL
  exec-credential kubeconfig from config alone (no server call, no embedded
  secret) — the interactive default. `create kubeconfig` generates a
  SERVER-SIDE, embedded, revocable kubeconfig via the ext Kubeconfig store —
  for CI/handoff where an exec plugin isn't usable. Every ext.cattle.io
  operation here is also plain Kubernetes API — `kubectl get/create
  tokens.ext.cattle.io` and `kubeconfigs.ext.cattle.io` work too — the CLI
  adds the auth bootstrap, the ms-vs-seconds TTL conversion, and shown-once
  handling on top.
- Migrating from the legacy `rancher` CLI? `MIGRATING.md` in the release maps
  every old command to its replacement.
