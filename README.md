# rancher-cli

An AI-native operations CLI for Rancher Manager (`github.com/rancher/rancher-cli`).

A greenfield rewrite of the Rancher CLI built so a terminal coding agent
(Claude Code, Codex, …) can drive Rancher as a first-class user: structured,
versioned output; a self-describing command surface; machine-readable errors
with next-step hints; branchable exit codes; and a plan/apply safety model.
Users get the same commands with readable table output.

## Status

**Phase 0 (foundation) — implemented.** Auth, configuration, context/server
management, the `kubectl` token plugin, and the self-description surface
(`schema`). Kube-native: talks to Rancher's Kubernetes API, no Norman
dependency.

**Phase 1 (not yet) —** cluster/node/namespace reads and project/RBAC/token
writes. The safety pipeline (dry-run → apply, confirmation gating, tiered
stability) is wired and the global flags exist, but no server-mutating command
ships yet, so those flags currently have nothing to act on.

## Install

    make build        # -> bin/rancher   (CGO disabled, static-ish)
    make test         # go test -race -cover ./...

Requires Go 1.26+. Build/release automation is make + `scripts/`, mirroring
`rancher/rancher`.

## Quickstart

    # Log in (token is the Rancher UI "id:secret" API key)
    rancher login https://rancher.example.com --token <id:secret> --name prod

    # Scope the current context to a cluster[:project]
    rancher context switch c-m-abc123:p-xyz789
    rancher context current -o json

    # Discover the full command surface as JSON (for agents)
    rancher schema -o json

    # Verify connectivity + credentials
    rancher auth check -o json

## Commands (Phase 0)

| Command | Purpose |
|---|---|
| `login <SERVERURL> --token <id:secret> [--name --cacert --context]` | Store a server + token; set current context |
| `context current` | Show the active context |
| `context switch <cluster[:project]>` | Set the current context's scope (colon-less keeps the project) |
| `context use <name>` | Activate a different named context |
| `server ls` | List configured servers |
| `server switch [name]` | Switch the active server |
| `auth check` | Connectivity + credential preflight (calls the cluster's k8s API) |
| `config purge-legacy` | Delete the migrated legacy `cli2.json` |
| `token …` | kubectl exec-credential plugin (invoked by kubeconfigs, not by hand) |
| `schema [command]` | Machine-readable capability manifest |

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

- **Formats:** `-o table` (default, for users) · `json` · `yaml` · `-q/--quiet`
  (ids only) · any other `-o` value is a Go template over `data`.
- **Errors** are structured, on stderr, always emitted (even under `-q`):

  ```json
  { "apiVersion": "cli.rancher.io/v1", "kind": "Error",
    "error": { "code": "CONTEXT_NOT_SET", "message": "...",
               "hint": "run: rancher login <SERVERURL>", "retryable": false } }
  ```

- **Exit codes** (branch without parsing): `0` ok · `2` usage/validation ·
  `3` auth · `4` not-found · `5` conflict/precondition · `6` RBAC · `1` generic.
- **Discovery:** `rancher schema -o json` returns the CLI version, the output
  `apiVersion`, the error-code enum, the global flags, and every command with
  its args, flags, `commandType`, and (for server ops) `tier`/`stability`.
  `rancher schema <command>` filters to one.

## Safety model (pipeline wired; exercised in Phase 1)

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

## Configuration

- **File:** `${XDG_CONFIG_HOME:-~/.config}/rancher/config.yaml` (mode `0600`),
  kubeconfig-shaped (`current-context`, `contexts`, `servers`). Secrets are NOT
  stored here.
- **Directory precedence:** `--config <dir>` > `RANCHER_CONFIG_DIR` >
  `$XDG_CONFIG_HOME/rancher` > `~/.config/rancher`.
- **Credential store:** OS keychain or a `0600` file, chosen by
  `credentialStore: keychain|file|auto` (default `auto` — keychain if usable,
  else file) or `RANCHER_CREDENTIAL_STORE`.
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

`rancher token` is a `client.authentication.k8s.io/v1beta1` exec-credential
plugin. Rancher-generated kubeconfigs invoke it (`rancher token --server …
--user … --cluster …`) to mint short-lived Kubernetes credentials; you don't
run it directly. It caches credentials in the configured store and supports the
SAML, OAuth (device-code / auth-code), and basic auth flows. Long-poll flows
are interrupt-cancellable.

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
      client/provider/      operation -> tier/stability resolver
      runtime/              Command contract, middleware pipeline, schema
      commands/             login, context, server, auth, config
      token/                kubectl exec-credential plugin
    surface/cli/            urfave/cli v3 adapter + root wiring

**Conventions:** direct `t.Run` subtests where mocks/per-case setup are
involved, table-driven for pure functions; tests are hermetic (never touch the
real OS keychain).
