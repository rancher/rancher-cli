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
rancher auth check -o json
```

Reports server, cluster context, server version, and credential-store state.
If it fails with `AUTH` or `CONTEXT_NOT_SET`, establish a session:

- Interactive: `rancher auth login --server <url> --name <alias>` — prompts for
  a Rancher API token (`id:secret`, created in the Rancher UI under Account &
  API Keys). It does NOT run provider login flows.
- Scripted/CI: pipe the token — `rancher auth login --server <url> --token-stdin < token.txt`,
  or set `RANCHER_URL` + `RANCHER_TOKEN` env vars (no stored config needed).
- SAML/OAuth provider flows live on `rancher auth get-token` (`--auth-provider`,
  `--auth-flow devicecode|authcode`) — the kubeconfig exec plugin drives them
  automatically; they mint cached exec credentials, not stored login sessions.

### 2. Discover, don't guess

```
rancher schema -o json              # every command: args, flags, aliases, destructive markers, tier/stability, error-code enum
rancher schema get pod -o json      # one command's contract
```

One JSON document for the whole surface, or extract client-side for bulk work.
The schema is the source of truth for the installed version. When unsure of a
flag, a kind name, or whether a command is destructive, read the schema before
constructing a call. Kind names have kubectl-style aliases (e.g. `get pods`,
`get cm`).

### 3. Always parse, never scrape

Use `-o json` for anything you will read programmatically. Every success is an
envelope:

```json
{ "apiVersion": "cli.rancher.io/v1", "kind": "...", "data": { ... }, "warnings": [] }
```

Read `data`; surface `warnings` to the user. Other formats: `table` (default,
human), `yaml`, `name` (ids only), or any other value as a Go template over
the JSON view of `data` (same shape as `-o json`).

### 4. Mutation safety loop — never skip

Every server mutation goes through the same gate:

```
rancher create project web --cluster prod --dry-run -o json   # 1. preview the server-computed Effect (no change)
rancher create project web --cluster prod --apply             # 2. execute
rancher delete project web --cluster prod --apply --yes       # 3. destructive effects additionally need --yes
```

- A mutation without `--apply` is refused with `CONFIRMATION_REQUIRED`; the
  error's `hint` names the exact flags to add. Relay the dry-run Effect (action,
  diff, tier) to the user before applying anything non-trivial.
- Patch previews include a field-level diff (`data.diff`) — show it.
- `secret` diffs are redacted by design: `data`/`stringData` entries show the
  changed path but `from`/`to` are `<redacted>`, never the raw value.
- `--allow-deprecated` is required to write through a deprecated-tier API.
- `--dry-run` is a real server dry-run: it needs the same RBAC verb as the
  write. An `RBAC` error on dry-run means the write would be denied too.
- Never chain `--apply --yes` onto a command you have not dry-run or whose
  effect the user has not asked for.

### 5. Branch on structured errors, not text

Errors land on stderr as an envelope with the error object under `data`:

```json
{ "apiVersion": "cli.rancher.io/v1", "kind": "Error",
  "data": { "error": { "code": "NOT_FOUND", "message": "...", "hint": "...", "retryable": false } } }
```

Read `data.error.code` / `data.error.hint`. The JSON envelope appears on
stderr regardless of the `-o` format. A plain-text notice (e.g. a legacy-config
or credential-store warning) may precede it on stderr — the error envelope is
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
burning the timeout.

### 7. Triage composites

One call each, with graceful degradation (missing metrics/logs become
`warnings`, never failures):

- `rancher inspect pod <name>` — status, owners, events, logs, metrics
- `rancher inspect deployment <name>` — rollout + its pods (selector-scoped)
- `rancher inspect project <name-or-id>` — member namespaces, workload counts, members, quota, usage
- `rancher top node` / `rancher usage` — capacity and consumption
- `rancher images` — running images inventory

### 8. kubectl interop

For anything the CLI does not cover, mint a kubeconfig and use kubectl:

```
rancher kubeconfig --cluster <name-or-id> > kc.yaml
kubectl --kubeconfig kc.yaml get nodes
```

The kubeconfig uses an exec-credential plugin that calls `rancher auth
get-token` — the `rancher` binary must stay on PATH for kubectl to
authenticate. Cached credentials: `rancher auth list-tokens` /
`rancher auth delete-token`.

## Conventions

- **Routing:** `--cluster` accepts a display name or id; `--namespace` scopes
  namespaced kinds. Contexts carry cluster+project defaults: switch persistently
  with `rancher config use-context` / `set-context`, or override for a single
  invocation with the `RANCHER_CONTEXT` env var (`cluster[:project]`) or the
  global `--context cluster[:project]` flag (flag > `RANCHER_CONTEXT` env >
  current context). Fleet kinds default to the `fleet-default` workspace.
- **Project scope (`--project`):** the global `--project <id-or-name>` flag
  scopes Downstream namespaced kinds (`get`/`delete`/`patch`, plus `usage`)
  to a project's member namespaces — a list filters to members, a named op
  or mutation validates membership first. `get namespaces --project <p>` is
  the membership listing (server-side label selector). Every other kind
  family and the non-project composites (`inspect pod`/`deployment`, `top
  node`, `images`) reject a non-empty `--project` with `VALIDATION` — it is
  never a silent no-op. Precedence: `--project` flag > the context's project
  axis (set via `--context`/`RANCHER_CONTEXT`/`set-context`) > unscoped.
- **Isolation for scripted sessions:** point `RANCHER_CONFIG_DIR` at a
  temporary directory and set `RANCHER_CREDENTIAL_STORE=file` so a scripted run
  never touches the operator's real config or OS keychain.
- **Secrets:** never echo tokens into commands or logs; use `--token-stdin` or
  env vars. Sensitive kinds (users, settings, role bindings, …) escalate
  mutations to the destructive gate — expect `--yes` to be demanded even for a
  patch.
- **Tier/stability:** server ops surface `tier` (`public` > `crd` > `norman`)
  and `stability` (`stable`/`beta`/`deprecated`) — mention them when the user
  is deciding whether to depend on an operation.
- Migrating from the legacy `rancher` CLI? `MIGRATING.md` in the release maps
  every old command to its replacement.
