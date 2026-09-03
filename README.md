# Rancher CLI

An AI-native operations CLI for [Rancher Manager](https://github.com/rancher/rancher).

A greenfield rewrite of the Rancher CLI, built so a terminal coding agent
(Claude Code, Codex, …) can drive Rancher as a first-class user — and
operators get the same commands with readable table output.

- **Structured, versioned output** — nearly every command emits one JSON
  envelope under `-o json` (YAML under `-o yaml`), versioned as
  `cli.rancher.io/v1`; the default is readable text for an operator at a
  terminal. Five terminal-integration commands are exceptions — details:
  [REFERENCE.md](REFERENCE.md#output-formats).
- **Self-describing surface** — `rancher schema` lists every command, flag,
  contract, and gate; agents orient themselves without docs.
- **Machine-readable errors** — the same envelope as a success, under
  `-o json`/`-o yaml`, with next-step hints; a short text block otherwise.
- **Branchable exit codes** — scripts and agents branch on the code without
  parsing anything.
- **Plan/apply safety** — a server-computed preview before every mutation,
  with explicit gates for destructive effects.

[Install](#install) · [Quickstart](#quickstart) ·
[Commands](COMMANDS.md) · [Reference](REFERENCE.md) ·
[Migration](MIGRATING.md) · [Contributing](AGENTS.md)

<!-- demo: asciinema/GIF slot — fill from rehearsal recordings -->

## Install

    curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/rancher/rancher-cli/main/install.sh | sh

On Windows (PowerShell 7 or Windows PowerShell 5.1):

    irm https://raw.githubusercontent.com/rancher/rancher-cli/main/install.ps1 | iex

> [!NOTE]
> While the repository is internal the anonymous forms above fail. Use
> `gh` instead (authenticated once via `gh auth login`) — the installer
> picks up gh's token automatically, and it never appears in process
> arguments:

    gh api "repos/rancher/rancher-cli/contents/install.sh?ref=main" -H "Accept: application/vnd.github.raw" | sh -s -- --pre

PowerShell:

    iex "& { $(gh api 'repos/rancher/rancher-cli/contents/install.ps1?ref=main' -H 'Accept: application/vnd.github.raw' | Out-String) } -Pre"

`--pre` (`-Pre` in PowerShell) selects the newest prerelease — early
access ships prereleases only; drop it once a stable release exists.
Prerelease resolution on Linux and macOS needs `jq` or `python3` on PATH.

Without `gh`, the token-authenticated download-then-run form is in
[REFERENCE.md](REFERENCE.md#defensive-bootstrap).

On Linux and macOS both forms install the `rancher` CLI and its credential
helper to `~/.local/bin`; on Windows to `%LOCALAPPDATA%\rancher-cli\bin`
(rootless, no admin rights, no profile or `PATH` edits; the script prints
the `PATH` line if needed). Checksums are always verified; build
provenance is verified when an authenticated `gh` CLI is available. The
by-hand Windows steps stay in [REFERENCE.md](REFERENCE.md#windows) as a
fallback. The CLI supports Rancher `>= 2.14`
([details](REFERENCE.md#supported-server-version)); install internals and
manual download: [REFERENCE.md](REFERENCE.md#install-internals).

<details>
<summary>Options</summary>

Flags (either pipe form: `… | sh -s -- --version <tag>`):
`--version <tag>` installs a specific release
(`RANCHER_CLI_VERSION` env twin; flag wins), `--pre` selects the newest
prerelease, `--bin-dir <dir>` overrides the destination
(`RANCHER_CLI_INSTALL_BIN_DIR` env twin), `--verify-attestation` makes
provenance verification mandatory, `--dry-run` prints the plan without
writing anything, `-q` suppresses progress notes. Set `GH_TOKEN` or
`GITHUB_TOKEN` for internal or rate-limited use.

PowerShell takes the same options as parameters — `-Version <tag>`,
`-Pre`, `-BinDir <dir>`, `-VerifyAttestation`, `-DryRun`, `-Quiet` — with
the same env twins. The pipe form cannot pass parameters; wrap the script
in a scriptblock instead:

    iex "& { $(irm https://raw.githubusercontent.com/rancher/rancher-cli/main/install.ps1) } -Pre"

</details>

<details>
<summary>Update, rollback &amp; uninstall</summary>

To update, rerun the install command — resolution finds the newer release
and the switch is atomic. To downgrade or roll back, rerun with
`--version <tag>` (`-Version <tag>`).

Uninstall on Linux and macOS: remove the `rancher` and
`rancher-credential-vault` symlinks from your bin dir,
`~/.local/share/rancher-cli`, and
`~/.config/rancher-cli/install-receipt.json`. On Windows: remove
`%LOCALAPPDATA%\rancher-cli` (the `bin` junction and the version
directories), `%APPDATA%\rancher-cli\install-receipt.json`, and the `PATH`
entry you added.

</details>

## Quickstart

The login token is a Rancher
[API key](https://ranchermanager.docs.rancher.com/reference-guides/user-settings/api-keys)
in its `access-key:secret-key` form.

    # Log in (token is the Rancher UI "id:secret" API key)
    rancher auth login prod --url https://rancher.example.com
    # (prompts for token, or pass via --token-stdin)

    # Verify connectivity + credentials
    rancher auth status

    # Read clusters (shows the cluster id for the next step)
    rancher get clusters

    # Create a project: preview the effect, then apply
    rancher create project web --cluster c-m-abc123 --dry-run
    rancher create project web --cluster c-m-abc123 --apply

    # Discover the command surface: TOC, per-command card, per-kind verbs
    rancher schema
    rancher schema delete project        # one command: contract + gates
    rancher schema pod                   # one kind: aliases, routing, verbs
    rancher schema --search kubeconfig   # find commands by substring
    rancher schema --all -o json         # full manifest (for agents)

Provisioning a cluster (machine configs, cloud credentials, readiness
waits): [REFERENCE.md](REFERENCE.md#cluster-provisioning).

## Agent skill

[`skills/rancher/SKILL.md`](skills/rancher/SKILL.md) teaches an AI coding
agent to operate this CLI: auth, discovery, the dry-run/apply safety loop,
structured errors, and waits. It follows the open
[Agent Skills](https://agentskills.io) standard — install it into Claude
Code, Codex, or GitHub Copilot CLI:

    rancher skill install

or point any other agent's instructions file at it.

## Commands

| Group | Commands |
|---|---|
| [Authentication](COMMANDS.md#authentication) | login, logout, check, and the exec-credential token plugin |
| [Servers & configuration](COMMANDS.md#servers--configuration) | servers, config file, completion settings |
| [Create](COMMANDS.md#create) | project, namespace, secret, resource -f, token, kubeconfig, cluster, machine-config |
| [Projects & access](COMMANDS.md#projects--access) | project namespace membership, grant, revoke |
| [Cluster scaling & autoscaling](COMMANDS.md#cluster-scaling--autoscaling) | machine-pool scale, cluster-autoscaler set/clear/pause/resume |
| [Read & inspect](COMMANDS.md#read--inspect) | cloud credentials, Kubernetes versions, composite cluster/pod/deployment/project/user reads, images, usage |
| [Logs & support](COMMANDS.md#logs--support) | curated logs, support bundle |
| [kubectl integration](COMMANDS.md#kubectl-integration) | exec-credential kubeconfig, legacy token shim |
| [Discovery & tooling](COMMANDS.md#discovery--tooling) | schema, shell completion, agent-skill install |

Composite reads (`inspect *`, `images`, `usage`) answer with what the caller
can see: a collector they cannot read is named in the payload's `degraded`
field with a paired warning, and the command still exits 0.

Generated `get`/`patch`/`delete` commands cover every kind in the 49-kind
vocabulary. The full catalog is [COMMANDS.md](COMMANDS.md), and
`rancher schema` is its live version. Per-kind verb rules:
[REFERENCE.md](REFERENCE.md#kinds--verbs); project scoping:
[REFERENCE.md](REFERENCE.md#project-scope---project).

## Safety model

Every server-mutating command runs one pipeline, in order:

1. `--dry-run` previews a server-computed effect (action, diff, warnings)
   and changes nothing.
2. `--apply` executes. A bare mutating invocation — no `--dry-run`, no
   `--apply` — is refused with `CONFIRMATION_REQUIRED`, and the refusal
   names the exact flags required.
3. `--yes` stacks on `--apply` (never alone) when the effect is
   destructive: every `delete`, `revoke`, writes to sensitive kinds, and
   creating shown-once secrets (`create token`, `create kubeconfig`).
4. `--allow-deprecated` is additionally required for a
   deprecated-stability write.
5. Updates and deletes carry a
   [resourceVersion](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)
   precondition from their own
   plan — if the resource changes between the plan's preview and execution,
   the mutation fails (exit `5`) instead of clobbering. A separate
   `--dry-run` invocation is a preview only; the later `--apply` run
   re-plans and binds its precondition to the state it sees then. Creates have no
   precondition (there is no existing object to pin).

Rule of thumb: `--apply` says "yes, mutate"; `--yes` says "yes, I saw it's
destructive".

Why this shape: the CLI is agent-first. Intent is stated in
machine-checkable flags, never inferred, and there is no interactive "are
you sure?" prompt — a prompt cannot be scripted, retried, or audited.
Every refusal is a structured error naming exactly what is missing, so an
agent (or a script) escalates deliberately or stops.

> [!NOTE]
> `--dry-run` runs a real
> [server-side dry-run](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run)
> and needs the same RBAC verb
> as the write — read-only credentials get an `RBAC` error. That means the
> write would be denied, not that dry-run is broken.

Each server operation carries a contract (`public` | `internal`) and
stability (`stable`/`beta`/`deprecated`), surfaced in the schema manifest.
Full pipeline and gate model: [ARCHITECTURE.md](ARCHITECTURE.md).
Read-only mode: [REFERENCE.md](REFERENCE.md#read-only-mode).

## Output & exit codes

Nearly every command emits one JSON document per invocation under `-o json`
— an `apiVersion`-versioned envelope agents can depend on (exceptions:
[REFERENCE.md](REFERENCE.md#output-formats)):

```json
{
  "apiVersion": "cli.rancher.io/v1",
  "kind": "LoginResult",
  "data": { "server": "prod", "url": "https://rancher.example.com" },
  "warnings": []
}
```

Formats: `-o table` (default) · `wide` · `json` · `yaml` · `name` ·
`template=<go-template>`. Full format semantics:
[REFERENCE.md](REFERENCE.md#output-formats).

- **Errors** follow the same contract as a success: under `-o json`/
  `-o yaml` they are the envelope, error object under `data`, any warnings
  folded into the envelope's own `warnings` array. Use `-o json` for the
  envelope — including errors:

  ```json
  { "apiVersion": "cli.rancher.io/v1", "kind": "Error",
    "data": { "error": { "code": "NOT_LOGGED_IN", "message": "...",
              "hint": "rancher auth login 'prod'", "retryable": false } } }
  ```

  Every other format, including the default, renders a short text block
  on stderr instead:

  ```
  error (NOT_LOGGED_IN): ...
    hint: rancher auth login 'prod'
  ```

- **Exit codes** (branch without parsing): `0` ok · `2` usage/validation ·
  `3` auth · `4` not-found · `5` conflict/precondition · `6` RBAC ·
  `8` lock-timeout · `9` Prime required · `10` TLS trust ·
  `11` capability unsupported · `1` generic.
  Rancher's proxy paths reject an invalid or insufficient token as a 403
  (`RBAC`, exit `6`), not a 401 (`AUTH`, exit `3`).
- **Discovery:** `rancher schema -o json` is the orientation payload —
  commands, kinds, and exit codes as a compact table of contents;
  `rancher schema --all -o json` is the complete manifest, and the single
  source for contract and stability. `rancher schema <command|kind|group>`
  narrows to one card.

## Configuration

- **File:** `${XDG_CONFIG_HOME:-~/.config}/rancher/config.yaml` (mode `0600`):
  `servers`, `currentServer` (pointer to the active server), `credentialStore`,
  `completion`, `readOnly`. Secrets are NOT stored here.
- **Directory precedence:** `--config <dir>` > `RANCHER_CLI_CONFIG_DIR` >
  `$XDG_CONFIG_HOME/rancher` > `~/.config/rancher`.
- **Credential store:** OS keychain or a `0600` file; custom backends plug
  in as credential helpers ([CREDENTIAL_HELPERS.md](CREDENTIAL_HELPERS.md)).
  Precedence and probe/pin semantics:
  [REFERENCE.md](REFERENCE.md#platforms--credential-stores).
- **Environment overrides** (precedence flag > env > file): the full table
  of available environment overrides is in
  [REFERENCE.md](REFERENCE.md#environment-variables).
- **Migration:** config and credentials are not imported — run `rancher auth
  login` once per server.

## Shell completion

    source <(rancher completion bash)   # ~/.bashrc
    source <(rancher completion zsh)    # ~/.zshrc
    rancher completion fish > ~/.config/fish/completions/rancher.fish
    rancher completion pwsh | Out-String | Invoke-Expression   # add to $PROFILE

Completion is offline-only by default — it never touches the network and
degrades to no candidates, never an error. Live resource-name completion
is an opt-in: `rancher config set-completion --network=true`. Shell
version floors, offline guarantees, and live-completion detail:
[REFERENCE.md](REFERENCE.md#shell-completion).

## Documentation

- [COMMANDS.md](COMMANDS.md) — the command catalog, by group (operators).
- [REFERENCE.md](REFERENCE.md) — platforms and credential stores, install
  internals, completion, scoping, kinds and verbs, cluster provisioning,
  composites, access management, diagnostics, output formats, read-only
  mode, server-version support, kubectl integration, TLS (operators,
  automation authors).
- [ARCHITECTURE.md](ARCHITECTURE.md) — layer map and design decisions
  (contributors).
- [CREDENTIAL_HELPERS.md](CREDENTIAL_HELPERS.md) — credential-helper
  protocol and examples (integrators).
- [MIGRATING.md](MIGRATING.md) — moving from the legacy Rancher CLI
  (operators).
- [skills/rancher/SKILL.md](skills/rancher/SKILL.md) — the agent skill
  (agents and their authors).

## Development

Requires Go 1.26+ to build from source:

    make build        # bin/rancher
    make test         # go test -race -cover ./...
    make validate     # gofmt + vet + lint + race tests (the commit gate)
    make e2e          # end-to-end suite against an envtest-backed fake Rancher
    make e2e-real     # verify recorded fidelity goldens against a real Rancher in Docker
    make              # the full ci pipeline (build + validate)

`make e2e-real` boots the pinned `rancher/rancher` image (~5–7 min) and
checks that the fake Rancher the e2e suite runs against still behaves
like the real one; `make e2e-real-update` re-records those goldens.

One-time setup: `git config core.hooksPath .githooks` enables the
pre-commit `make validate` gate.

Manual testing: `scripts/dev <args>` — never run the binary directly; it
forces an isolated config dir and a file credential store so a manual run
cannot touch your real config or OS keychain. Contributor guide:
[AGENTS.md](AGENTS.md).

## Support & community

Questions and discussion: the [Rancher forums](http://forums.rancher.com/)
and [Rancher Slack](https://slack.rancher.io/). Bugs and feature requests:
[rancher/rancher issues](https://github.com/rancher/rancher/issues) — the
central hub for Rancher product issues.

For security issues, check the
[security policy](https://github.com/rancher/rancher/security) and email
security-rancher@suse.com instead of opening a public issue.

## License

Copyright (c) 2026 [SUSE](http://rancher.com)

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
