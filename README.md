# rancher-cli

An AI-native operations CLI for Rancher Manager (`github.com/rancher/rancher-cli`).

A greenfield rewrite of the Rancher CLI built so a terminal coding agent
(Claude Code, Codex, …) can drive Rancher as a first-class user: structured,
versioned output; a self-describing command surface; machine-readable errors
with next-step hints; branchable exit codes; and a plan/apply safety model.
Users get the same commands with readable table output.

## Status

**Phase 0 (foundation) — implemented.** Auth, configuration, context/server
management, the exec-credential token plugin, and the self-description surface
(`schema`). Kube-native: resource operations go through Rancher's Kubernetes
API, with one exception — interactive auth-provider login (`auth get-token`)
uses Rancher's legacy `/v1-public` endpoints. The command surface
is verb-first (kubectl-style): `auth …`, `config …`, `get …`, `create …`.

**Phase 1 Wave 1 — implemented.** The engine/provider layer plus generic,
table-driven `get` and `delete` over a kind table (management: cluster, node,
namespace, project; downstream core: pod, deployment, service, configmap,
secret) and the bespoke `create project` write, all running through the full
plan/apply pipeline (dry-run → apply, confirmation gating, tiered stability).
Generic `create`/`patch`, fleet/provisioning kinds, RBAC, kubeconfig, and
settings land in later Phase-1 waves.

## Install

    make build        # -> bin/rancher   (CGO disabled, static-ish)
    make test         # go test -race -cover ./...

Requires Go 1.26+. Build/release automation is make + `scripts/`, mirroring
`rancher/rancher`.

### Agent skill

[`skills/rancher/SKILL.md`](skills/rancher/SKILL.md) teaches an AI coding agent
to operate this CLI (auth, discovery, the dry-run/apply safety loop, structured
errors, waits). It follows the open [Agent Skills](https://agentskills.io)
standard: install it into any skill-supporting harness — Claude Code
(`~/.claude/skills/rancher/`) and GitHub Copilot CLI both load it natively —
or point any other agent's instructions file at it.

## Quickstart

    # Log in (token is the Rancher UI "id:secret" API key)
    rancher auth login --server https://rancher.example.com --name prod
    # (prompts for token, or pass via --token-stdin)

    # Scope the current context to a cluster/project
    rancher config set-context --current --cluster c-m-abc123 --project p-xyz789
    rancher config current-context -o json

    # Read clusters (crd tier, read-only)
    rancher get clusters -o json

    # Create a project: preview the effect, then apply
    rancher create project web --cluster c-m-abc123 --dry-run
    rancher create project web --cluster c-m-abc123 --apply

    # Discover the full command surface as JSON (for agents)
    rancher schema -o json

    # Verify connectivity + credentials
    rancher auth check -o json

## Commands

| Command | Purpose |
|---|---|
| `auth login --server <url> [--name --cacert --context]` | Store a server + token; set current context. Token via interactive prompt or `--token-stdin` (the `--token` flag is removed; secrets never on the command line). |
| `auth logout [<name>] [--all]` | Clear the stored login token for the current server (or `<name>`, or every server with `--all`); keeps server config; does not touch the exec-credential cache |
| `auth check` | Connectivity + credential preflight (calls the cluster's k8s API) |
| `auth get-token [--server --cluster --name --cacert --auth-provider --auth-flow --skip-verify]` | exec-credential plugin (invoked by kubeconfigs, not by hand) |
| `auth list-tokens` | List cached exec-credential entries (name, server, cluster, expiry) |
| `auth delete-token [<name>] [--all]` | Clear cached exec-credential token(s); a bare `<name>` deletes every matching record and is idempotent (no match = success) |
| `config current-context` | Show the active context |
| `config set-context --current --cluster <c> [--project <p>]` | Set the current context's scope |
| `config use-context <name>` | Activate a different named context |
| `config get-servers` | List configured servers |
| `config use-server [name]` | Switch the active server |
| `config delete-server <name>` | Remove a configured server, every context that targets it, and drain its stored credentials/exec-credential cache — does NOT revoke the server-side token (server-side revocation, `auth logout --revoke`, is tracked separately and not yet implemented) |
| `config path` | Print the resolved config file path — the agent-usable "where do I point tooling" primitive; prints the path whether or not the file exists yet |
| `config edit` | Edit the config file in `$VISUAL`/`$EDITOR` on a temp copy, validating (referential integrity) before writing back — never edits in place |
| `config purge-legacy` | Delete the migrated legacy `cli2.json` |
| `create project <name> --cluster <id> [--description --quota-limit --namespace-default-quota]` | Create a project, optionally with a resource quota — public tier; dry-run/apply |
| `create namespace <name> [--project --cluster]` | Create a namespace, optionally a project member from birth — public tier; dry-run/apply |
| `create secret <name> [--from-literal --type] [--namespace --cluster \| --project --project-scoped]` | Create a secret: bare namespaced, or project-scoped into the project's backing namespace — public tier; dry-run/apply |
| `create resource -f <file\|-> [--cluster --namespace]` | Create/apply a resource from a YAML manifest (file or stdin); dry-run/apply |
| `create token [--ttl --description --cluster]` | Mint a server-side derived token (`ext.Token`), shown once — public tier; sensitive; dry-run/apply --yes |
| `create kubeconfig [--clusters --current-context --ttl --description --include-default-entry]` | Mint a server-side, embedded, revocable kubeconfig (`ext.Kubeconfig`) to stdout, shown once — public tier; sensitive; dry-run/apply --yes |
| `project add-namespace <ns> --project <p> [--cluster]` | Add a namespace to a project — public tier; dry-run/apply |
| `project remove-namespace <ns> --project <p> [--cluster]` | Remove a namespace from a project — public tier; dry-run/apply |
| `project add-member <principal> --project <p> --role <r> [--cluster --group]` | Add a project member via a role template binding — public tier; dry-run/apply |
| `project remove-member <principal> --project <p> [--cluster --group]` | Remove a project member's role template binding(s) — public tier; destructive; dry-run/apply --yes |
| `inspect pod <name> --namespace <ns> [--cluster --tail]` | Composite pod read: describe + recent logs |
| `inspect deployment <name> --namespace <ns> [--cluster]` | Composite deployment read |
| `inspect project <name-or-id> [--cluster]` | Composite project read: namespaces, members, quota, usage |
| `top node [name] [--cluster]` | Node resource usage |
| `images [--cluster --all-clusters]` | Running container images, fleet-wide |
| `usage [--cluster --namespace] [--project]` | Resource usage composite (`--project` is the global flag) |
| `kubeconfig --cluster <id>` | Emit a LOCAL exec-credential kubeconfig for a cluster (no server call, no embedded secret) — see `create kubeconfig` for the server-minted, embedded alternative |
| `logs <target> [--cluster --tail --since --previous --container --timestamps]` | Curated, verified-location logs for `server`/`agent`/`fleet-agent`/`kube-api-auth`/`webhook` |
| `support-bundle [--targets --clusters --since --tail -f]` | Pack the API-reachable triage plane (logs, cluster/node state, events, settings/features/schema) into one tar.gz for support |
| `schema [command]` | Machine-readable capability manifest |

`config use-server` switches to a context that targets the named server
(creating one if needed); `config use-context` activates a named context
directly. `use-server` is the migrating target for the legacy `server
switch` command.

Nouns accept singular, plural, and short aliases (`get pod` = `get pods` =
`get po`). Scope routing is enforced: addressing a namespaced kind by name needs
`--cluster` (management) or `--namespace` (downstream); bare lists may omit them.

Generated per kind from the 40-kind table: `get <kind>` (40), `patch <kind>`
(40), `delete <kind>` (20 deletable, including `projectroletemplatebinding`,
`token`, and `kubeconfig`) — 100 generated commands, 135 total together with
the fixed commands above. The authoritative, always-current list is
`rancher schema -o json`.

`feature` (Rancher's `management.cattle.io/v3` Feature flags) is one of
those generated kinds — not deletable, sensitive (flipping a flag via
`patch feature` escalates to `--yes`). Its interesting fields
(`spec.value`, `status.default`, `status.lockedValue`) are booleans, which
render blank under the string-only table columns — `-o json` is the real
read for a feature's actual state.

`token` and `kubeconfig` are registered with an empty GVR version, resolved
via discovery at call time (the same mechanism the CAPI kinds use) — on a
Rancher older than 2.14 (the CLI's supported minimum), the generated
`get`/`patch`/`delete` ops on these two kinds fail with a friendly `NOT_FOUND`
(`requires Rancher >= 2.14, the supported minimum`) pointing at `auth
list-tokens` and the exec-credential `kubeconfig` instead.
Every operation on these two kinds is also plain Kubernetes API — `kubectl
get/create tokens.ext.cattle.io` / `kubeconfigs.ext.cattle.io` works too (with
`--validate=false` for a standard user, since kubectl's CRD-schema validation
fallback needs list rights most users lack). The CLI adds the auth bootstrap,
the token-vs-kubeconfig TTL unit conversion (`--ttl` takes a duration; the
underlying spec fields are milliseconds and seconds respectively — the CLI
never exposes the raw units), and shown-once handling for the minted secret.

`logs`/`support-bundle` cover only what Rancher's own k8s API can reach.
`rancher-system-agent` is a node systemd unit, not a pod — its logs are
`journalctl -u rancher-system-agent` on the node itself, and generic pod
logs outside the curated targets stay with `inspect pod` and kubectl.
`support-bundle` is deliberately the API-reachable half of a two-bundle
story: pair it with rancherlabs/support-tools' node collector for the
node-level half (journald, the system-agent unit, OS state) that no API
call can reach.

### Project scope (`--project`)

The global `--project <id-or-name>` flag scopes Downstream namespaced kinds
(`get`/`delete`/`patch`, plus `usage`) to one project's member namespaces:
a bare list is filtered to member namespaces; a named op or mutation
validates the target namespace's membership before touching the server.
`get namespaces --project <p>` is the one special case — it lists namespaces
by the membership label server-side rather than filtering client-side.
Every other command family (cluster-scoped kinds, management-plane kinds,
and the composites `inspect pod`/`inspect deployment`/`top node`/`images`)
rejects a non-empty `--project` with `VALIDATION` rather than silently
ignoring it. Precedence: `--project` flag > the current context's project
axis (`--context cluster:project` / `RANCHER_CONTEXT` / `config
set-context --project`) > unscoped.

## Output & the agent contract

Every command emits one JSON document per invocation under `-o json` — an
`apiVersion`-versioned envelope, so agents can pin to it:

```json
{
  "apiVersion": "cli.rancher.io/v1",
  "kind": "Context",
  "data": { "name": "prod", "cluster": "c-m-abc123", "project": "p-xyz789" },
  "warnings": []
}
```

- **Formats:** `-o`/`--output`: `table` (default, for users) · `json` · `yaml` ·
  `name` (`.metadata.name`/ids only) · any other value is a Go template over
  the JSON view of `data` (same shape as `-o json`). A kind without a table
  projection falls back to indented JSON under the default `table` format;
  use `-o json` for the versioned envelope. Some commands (e.g. `logs`) render
  a plain-text default under `table`/default format instead, with warnings on
  stderr as `# warning: <w>` lines; `-o json`/`yaml`/template/`name` still see
  the normal envelope over `data`.
- **Errors** are structured, on stderr, always the JSON envelope regardless of
  `-o`, with the error object under `data`. A plain-text notice (e.g. a
  legacy-config or credential-store warning) may precede it on stderr — the
  envelope is always the LAST JSON object on stderr:

  ```json
  { "apiVersion": "cli.rancher.io/v1", "kind": "Error",
    "data": { "error": { "code": "CONTEXT_NOT_SET", "message": "...",
              "hint": "run: rancher auth login --server <url>", "retryable": false } } }
  ```

- **Exit codes** (branch without parsing): `0` ok · `2` usage/validation ·
  `3` auth · `4` not-found · `5` conflict/precondition · `6` RBAC · `1` generic.
- **Discovery:** `rancher schema -o json` returns the CLI version, the output
  `apiVersion`, the error-code enum, the global flags, and every command with
  its args, flags, `commandType`, and (for server ops) `tier`/`stability`,
  as one document. `rancher schema <command>` (e.g. `rancher schema get pod`)
  filters the manifest to that command.

## Safety model (exercised by `create project`; more in later waves)

Server-mutating commands run through one path: `--dry-run` previews a
server-computed `Effect` (no change); without it, a mutation is refused unless
the exact confirmation flags are present. The refusal names them:

    --apply                 execute the mutation
    --yes                   confirm a destructive effect
    --allow-deprecated      permit a deprecated-tier write

`--dry-run` is valid only for mutating commands (rejected elsewhere with
`DRY_RUN_NOT_APPLICABLE`). Each server op carries a support **tier**
(`public` > `crd` > `norman`) and **stability** (`stable`/`beta`/`deprecated`)
surfaced in output so an agent knows the contract before acting.

`--dry-run` runs a real server-side dry-run: it needs the same RBAC verb as the
write, so read-only credentials get an `RBAC` error (that means the write would
be denied, not that dry-run is broken). See [ARCHITECTURE.md](ARCHITECTURE.md)
for the full pipeline, gates, and package-dependency model, and `rancher schema`
for the machine-readable surface.

Migrating from the legacy Rancher CLI: see [MIGRATING.md](MIGRATING.md).

## Read-only mode

Three sources, OR'd together, put a session into read-only mode, refusing
every mutating command before any network call — strictly one-way, no
per-invocation override exists:

- Config: `readOnly: true` (top-level config field).
- Env: `RANCHER_READONLY`, non-empty = on, including the literal string
  `"false"` — every `RANCHER_*` var in this CLI follows non-empty-means-set;
  there is no boolean-env precedent, so this is a sharp edge worth calling
  out explicitly.
- Flag: `--read-only` (global).

Refusal is `VALIDATION`, naming the command and the active source(s) (e.g.
"enabled by RANCHER_READONLY") — the one-way rule means you have to know
which knob to turn off. `rancher schema -o json` reports the resolved state
(`readOnly`, `readOnlySources`) so an agent can check before trying.

Scoping, stated honestly: read-only mode refuses `Mutating` commands only —
the CLI's managed-resource surface. `auth login`, `auth check`, and local
`config` commands stay usable; otherwise a read-only session couldn't even
establish itself.

The real enforcement is a Rancher token bound to read-only RBAC — read-only
mode is a CLI-side courtesy layer on top of that, not a replacement for it.
Two caveats:

- `--dry-run` of a mutating command still sends write-verb requests
  server-side (admission dry-run); a genuinely read-only RBAC token cannot
  dry-run at all. Read-only mode doesn't special-case this — it refuses
  mutating commands outright, `--dry-run` included.
- Pair `--read-only` (or the config/env form) with a read-only RBAC token
  for actual enforcement. The CLI-side gate alone stops an accidental
  mutation from a well-behaved client; it is not a security boundary.

## Supported server version

The CLI declares a minimum supported Rancher version, `>= 2.14`
(`rancher schema -o json` → `minRancherVersion`). Enforcement today is
warn-only: `auth login` and `auth check` read the server's `server-version`
setting and emit a warning — never a failure — when the connected server is
below the floor, or when the version can't be determined at all. RBAC
denial, a missing setting, or an unparseable value (e.g. a dev/head build)
all assume "supported" and warn instead of refusing.

Graduation intent, not implemented today: refuse-with-escape-hatch (e.g. a
`--allow-unsupported-server`-style flag) once there's real-use feedback on
the warn-only period. Ledgered, future work.

**`auth check` enrichment.** Beyond connectivity, `auth check -o json` also
reports `rancherVersion`/`supported` (the version-floor check above),
`identity` (the caller's `userID`, from a create-verb identity probe against
`ext.cattle.io`), and `token` (the effective token's
name/kind/provider/expiry/`current` marker — only for an `ext.cattle.io`
token; a legacy/non-ext token reports `token: null` with a warning). Any of
these fields degrading to `null` with a warning is expected, not a bug —
including `identity` under a read-only-RBAC token, since the identity probe
is itself a create-verb request. `auth check`'s connectivity result (`ok`)
never depends on them.

## Configuration

- **File:** `${XDG_CONFIG_HOME:-~/.config}/rancher/config.yaml` (mode `0600`),
  kubeconfig-shaped (`current-context`, `contexts`, `servers`). Secrets are NOT
  stored here.
- **Directory precedence:** `--config <dir>` > `RANCHER_CONFIG_DIR` >
  `$XDG_CONFIG_HOME/rancher` > `~/.config/rancher`.
- **Credential store:** OS keychain or a `0600` file. Precedence:
  `RANCHER_CREDENTIAL_STORE` > `credentialStore: keychain|file|auto` in config >
  a machine-local `credstore-pin` file > one-time probe. Under `auto` the backend
  is probed once and pinned; the pin is re-probed only at `auth login` and only to
  upgrade file→keychain, and a pinned-but-unusable backend is a hard error (never
  a silent downgrade to plaintext).
- **Environment overrides** (precedence flag > env > file):

  | Var | Effect |
  |---|---|
  | `RANCHER_URL` | server URL |
  | `RANCHER_TOKEN` | token, used directly and never persisted |
  | `RANCHER_CONTEXT` | `cluster:project` scope (colon-less = cluster only) |
  | `RANCHER_CACERT` | CA bundle (PEM) |
  | `RANCHER_CREDENTIAL_STORE` | `keychain` \| `file` \| `auto` |
  | `RANCHER_CONFIG_DIR` | config directory |

  The global `--context cluster[:project]` flag overrides `RANCHER_CONTEXT`
  for a single invocation. `--context c:` (trailing colon, no project) clears
  the project for this invocation. The context's project axis is shown by
  `config current-context`/`auth check`, and is consulted for routing only as
  a fallback: an unset `--project` flag falls back to it (`--project` always
  wins when both are set) — see "Project scope" above.

- **Migration:** on first run the CLI imports a legacy
  `~/.rancher/cli2.json` (also `$RANCHER_CONFIG_DIR/cli2.json`) into the new
  config + credential store, then warns you to remove the plaintext original
  with `rancher config purge-legacy`.

## kubectl integration

`rancher auth get-token` is a `client.authentication.k8s.io/v1beta1`
exec-credential plugin. Rancher-generated kubeconfigs invoke it to mint
short-lived Kubernetes credentials; you don't run it directly. Flags: `--server`,
`--cluster`, `--name` (cache slot, keyed per name+server+cluster; omitted = no
caching), `--cacert`,
`--auth-provider`, `--auth-flow`, `--skip-verify`. When flags are omitted,
server/CA/cluster fall back to the active config context. It supports SAML,
OAuth (device-code / auth-code), and basic auth flows. Long-poll flows are
interrupt-cancellable. List cached credentials with `rancher auth list-tokens`.
Clear them with `rancher auth delete-token [<name>]`.

## TLS & proxy

Server connections verify TLS against the configured CA (`--cacert` /
`RANCHER_CACERT`) or the system trust store by default — verification is never
disabled implicitly. HTTP(S) proxies are honored from the environment.

## Development

    make build        # bin/rancher
    make test         # go test -race -cover ./...
    make lint         # golangci-lint
    make validate     # gofmt + go vet + lint + race tests (the commit gate)
    make ci           # build + validate
    make e2e          # end-to-end suite against a fake, envtest-backed Rancher

A committed `.githooks/pre-commit` runs `make validate` on every commit
(`git config core.hooksPath .githooks`, set during setup).

Manual testing: `scripts/dev <args>` — never run the binary or `go run .`
directly; it forces an isolated config dir and the file credential store so
a manual run can never touch your real config or OS keychain.

`make e2e` runs the end-to-end suite: testscript scenarios in `e2e/scripts/`
drive the built binary against an envtest-backed proxy shim, plus the
provider package's envtest contract tests. First run downloads the
envtest kube-apiserver/etcd binaries (~80MB, cached after that). kubectl on
PATH enables the kubectl-interop scenario; otherwise it skips.

**Layout**

    main.go                 entrypoint (signal-cancellable context)
    internal/
      clierr/               structured error type + exit codes
      output/               versioned envelope + renderers
      config/               XDG config, credential store, migration, atomic writes
      client/kube/          rest.Config for the Rancher k8s proxy
      client/httpx/         HTTP client (TLS / proxy / timeouts)
      client/provider/      op tier/stability resolver + dynamic ClusterClient (adapter)
      runtime/              Command contract, middleware pipeline, schema
      engine/               ClusterClient port + plan/execute helpers
      engine/kinds/         kind table (MCP keys, GVRs, scope, bindings)
      engine/resources/     generic get/delete over the kind table
      engine/projects/      create project/namespace, project membership, name resolution
      engine/extres/        create token/kubeconfig (ext.cattle.io fixed mutating commands)
      commands/             auth login/check, config context/server ops, purge-legacy
      token/                exec-credential plugin (auth get-token / delete-token)
    surface/cli/            urfave/cli v3 adapter + root wiring + engine DI

**Conventions:** direct `t.Run` subtests where mocks/per-case setup are
involved, table-driven for pure functions; tests are hermetic (never touch the
real OS keychain).
