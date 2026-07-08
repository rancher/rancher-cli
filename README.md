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
uses Rancher's legacy `/v1-public` / `/v3-public` endpoints. The command surface
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
| `config purge-legacy` | Delete the migrated legacy `cli2.json` |
| `get <kind> [name] [--cluster --namespace]` | List a kind, or show one by name — read-only. Kinds: `cluster`, `node`, `namespace`, `project` (management); `pod`, `deployment`, `service`, `configmap`, `secret` (downstream). |
| `create project <name> --cluster <id> [--description]` | Create a project — public tier; dry-run/apply |
| `delete <kind> <name> [--cluster --namespace]` | Delete a kind by name — destructive; dry-run/apply. Deletable: `project`, `namespace` (management); `pod`, `deployment`, `service`, `configmap`, `secret` (downstream). |
| `schema [command]` | Machine-readable capability manifest |

Nouns accept singular, plural, and short aliases (`get pod` = `get pods` =
`get po`). Scope routing is enforced: addressing a namespaced kind by name needs
`--cluster` (management) or `--namespace` (downstream); bare lists may omit them.
Generic `create`/`patch`, fleet/provisioning kinds, more `config` subcommands,
and `version` are deferred to later waves.

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
  `name` (`.metadata.name`/ids only) · any other value is a Go template over `data`.
- **Errors** are structured, on stderr, always emitted (even under `-o name`):

  ```json
  { "apiVersion": "cli.rancher.io/v1", "kind": "Error",
    "error": { "code": "CONTEXT_NOT_SET", "message": "...",
               "hint": "run: rancher auth login --server <url>", "retryable": false } }
  ```

- **Exit codes** (branch without parsing): `0` ok · `2` usage/validation ·
  `3` auth · `4` not-found · `5` conflict/precondition · `6` RBAC · `1` generic.
- **Discovery:** `rancher schema -o json` returns the CLI version, the output
  `apiVersion`, the error-code enum, the global flags, and every command with
  its args, flags, `commandType`, and (for server ops) `tier`/`stability`.
  `rancher schema <command>` filters to one.

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

A committed `.githooks/pre-commit` runs `make validate` on every commit
(`git config core.hooksPath .githooks`, set during setup).

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
      engine/projects/      create project (bespoke write)
      commands/             auth login/check, config context/server ops, purge-legacy
      token/                exec-credential plugin (auth get-token / delete-token)
    surface/cli/            urfave/cli v3 adapter + root wiring + engine DI

**Conventions:** direct `t.Run` subtests where mocks/per-case setup are
involved, table-driven for pure functions; tests are hermetic (never touch the
real OS keychain).
