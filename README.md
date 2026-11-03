# rancher-cli

An AI-native command-line tool for operating [Rancher Manager](https://github.com/rancher/rancher).

A greenfield rewrite of the legacy Rancher CLI, built so a coding agent in a
terminal harness (Claude Code, Codex, …) can operate Rancher like any other
user - and operators get the same commands with readable table output.

- **Structured, versioned output** - nearly every command prints one JSON
  document under `-o json` (YAML under `-o yaml`), versioned as
  `cli.rancher.io/v1`; the default is readable text for an operator at a
  terminal. Five commands are exceptions, because a shell or kubectl reads
  their output in its own format - details:
  [REFERENCE.md](REFERENCE.md#output-formats).
- **Self-describing** - `rancher schema` lists every command, flag,
  contract, and gate; an agent learns the CLI from it without reading the
  docs.
- **Machine-readable errors** - the same document as a success, under
  `-o json`/`-o yaml`, with hints for the next step; a short text message
  otherwise.
- **Distinct exit codes** - scripts and agents act on the exit code without
  parsing any output.
- **Dry-run/apply safety** - a preview computed by the server before every
  change, with explicit gates for destructive changes.

[Install](#install) · [Quickstart](#quickstart) ·
[Commands](COMMANDS.md) · [Reference](REFERENCE.md) ·
[Migration](MIGRATING.md) · [Contributing](AGENTS.md)

<!-- demo: asciinema/GIF slot -->

## Install

    curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/rancher/rancher-cli/main/install.sh | sh

On Windows (PowerShell 7 or Windows PowerShell 5.1):

    irm https://raw.githubusercontent.com/rancher/rancher-cli/main/install.ps1 | iex

> [!NOTE]
> While the repository is internal, the install commands above fail because
> they send no GitHub token. Use `gh` instead (logged in once with
> `gh auth login`) - the installer picks up the GitHub token that gh holds
> automatically, and the token never appears in process arguments:

    gh api "repos/rancher/rancher-cli/contents/install.sh?ref=main" -H "Accept: application/vnd.github.raw" | sh -s -- --pre

PowerShell:

    iex "& { $(gh api 'repos/rancher/rancher-cli/contents/install.ps1?ref=main' -H 'Accept: application/vnd.github.raw' | Out-String) } -Pre"

`--pre` (`-Pre` in PowerShell) selects the newest prerelease - early
access ships prereleases only; drop it once a stable release exists.
Looking up a prerelease on Linux and macOS needs `jq` or `python3` on PATH.

Without `gh`, how to download the installer with a GitHub token and then run
it is in
[REFERENCE.md](REFERENCE.md#downloading-and-running-the-installer-safely).

On Linux and macOS both commands install `rancher` and its credential
helper to `~/.local/bin`; on Windows to `%LOCALAPPDATA%\rancher-cli\bin`
(no root or admin rights needed, no profile or `PATH` edits; the installer
prints the `PATH` line if needed). Checksums are always verified; build
provenance is verified when a `gh` CLI that is logged in is available. The
manual Windows steps are in [REFERENCE.md](REFERENCE.md#windows) as a
fallback. The CLI supports Rancher `>= 2.14`
([details](REFERENCE.md#supported-server-version)); install internals and
manual download: [REFERENCE.md](REFERENCE.md#install-internals).

<details>
<summary>Options</summary>

Flags (with either piped command: `… | sh -s -- --version <tag>`):
`--version <tag>` installs a specific release
(or set the environment variable `RANCHER_CLI_VERSION`; the flag takes
precedence), `--pre` selects the newest prerelease, `--bin-dir <dir>` sets
the bin directory (or set the environment variable
`RANCHER_CLI_INSTALL_BIN_DIR`), `--verify-attestation` makes provenance
verification mandatory, `--dry-run` prints the release, the files it would
download and the directories it would use, without writing anything, `-q`
suppresses progress notes. Set `GH_TOKEN` or `GITHUB_TOKEN` to give the
installer a GitHub token, which is needed while the repository is internal
and helps with GitHub rate limits.

PowerShell takes the same options as parameters - `-Version <tag>`,
`-Pre`, `-BinDir <dir>`, `-VerifyAttestation`, `-DryRun`, `-Quiet` - and
reads the same environment variables. A piped command cannot pass
parameters; wrap the installer in a script block instead:

    iex "& { $(irm https://raw.githubusercontent.com/rancher/rancher-cli/main/install.ps1) } -Pre"

</details>

<details>
<summary>Update, rollback &amp; uninstall</summary>

To update, rerun the install command - it looks up the newer release
and switches to it atomically. To downgrade or roll back, rerun with
`--version <tag>` (`-Version <tag>`).

Uninstall on Linux and macOS: remove the `rancher` and
`rancher-credential-vault` symlinks from your bin directory, and remove
`~/.local/share/rancher-cli` and
`~/.config/rancher-cli/install-receipt.json`. On Windows: remove
`%LOCALAPPDATA%\rancher-cli`, which holds the `bin` Windows directory link
(NTFS junction) and the version directories; remove
`%APPDATA%\rancher-cli\install-receipt.json`; and remove the `PATH` entry
you added.

</details>

## Quickstart

The login token is a Rancher
[API key](https://ranchermanager.docs.rancher.com/reference-guides/user-settings/api-keys)
in its `access-key:secret-key` form.

    # Log in (the token is a Rancher API key in access-key:secret-key form)
    rancher auth login prod --url https://rancher.example.com
    # (prompts for token, or pass via --token-stdin)

    # Verify connectivity + credentials
    rancher auth status

    # Read clusters (shows the cluster id for the next step)
    rancher get clusters

    # Create a project: preview the change, then apply
    rancher create project web --cluster c-m-abc123 --dry-run
    rancher create project web --cluster c-m-abc123 --apply

    # See what the CLI offers: table of contents, one command, one kind
    rancher schema
    rancher schema delete project        # one command: contract + gates
    rancher schema pod                   # one kind: aliases, routing, verbs
    rancher schema --search kubeconfig   # find commands by substring
    rancher schema --all -o json         # full manifest (for agents)

Provisioning a cluster (machine configs, cloud credentials, waiting for the
cluster to be ready): [REFERENCE.md](REFERENCE.md#cluster-provisioning).

## Agent skill

[`skills/rancher/SKILL.md`](skills/rancher/SKILL.md) teaches a coding agent
to operate this CLI: logging in, finding commands, the dry-run and apply
steps, structured errors, and waiting for resources. It follows the open
[Agent Skills](https://agentskills.io) standard - install it into a
coding-agent harness such as Claude Code, Codex, or GitHub Copilot CLI:

    rancher skill install

or point the instructions file of any other coding-agent harness at it.

## Commands

| Group | Commands |
|---|---|
| [Authentication](COMMANDS.md#authentication) | login, logout, status, and the exec plugin (`auth get-token`) |
| [Servers & configuration](COMMANDS.md#servers--configuration) | servers, config file, completion settings |
| [Create](COMMANDS.md#create) | project, namespace, secret, resource -f, token, kubeconfig, cluster, machine-config |
| [Projects & access](COMMANDS.md#projects--access) | project namespace membership, grant, revoke |
| [Cluster scaling & autoscaling](COMMANDS.md#cluster-scaling--autoscaling) | machine-pool scale, cluster-autoscaler set/clear/pause/resume |
| [Read & inspect](COMMANDS.md#read--inspect) | cloud credentials, Kubernetes versions, composite cluster/pod/deployment/project/user reads, images, usage |
| [Access](COMMANDS.md#access) | who can do what, as the cluster's authorizer answers it (`access can`, `access list`, `access who-can`, `access explain`), which clusters a subject can reach through Rancher's proxy (`access clusters`), and which projects a subject can see and is a member of (`access projects`) |
| [Logs & support](COMMANDS.md#logs--support) | logs of Rancher components, support bundle |
| [kubectl integration](COMMANDS.md#kubectl-integration) | kubeconfig that uses the exec plugin, legacy `rancher token` command |
| [Telemetry](COMMANDS.md#telemetry) | status, on, off, reset, show and upload for local usage recording; recording is on by default, and release builds send no event |
| [Discovery & tooling](COMMANDS.md#discovery--tooling) | schema, shell completion, agent-skill install |

With a kubeconfig that authenticates through the exec plugin and no stored `rancher auth login` credential, the plugin asks you to log in once per cluster by default; `rancher config set-token-reuse on` lets the plugin keep one login credential per server instead (see REFERENCE.md, Token reuse for kubectl).

A kubeconfig from `rancher kubeconfig` records the config directory in use when it was created. kubectl then runs the exec plugin with that config directory from any shell or working directory. The kubeconfig works for this user on this machine only, and if the directory moves, the kubeconfigs that name it must be created again. It also names, with `--user-id`, the Rancher user its stored login belongs to. The exec plugin then refuses to give kubectl another user's token. The kubeconfigs Rancher writes name that user too, with `--user-id` and no `--name`, and the auth config used for its login, with `--auth-provider`: the plugin then uses the stored login that records that user (and auth config), gives kubectl only tokens created through that auth config, and, with no `--cluster`, creates a token that is not tied to one cluster. A stored login that has no auth config recorded takes it from the first token the plugin creates from that stored login for a call that names one, and `rancher kubeconfig` writes `--auth-provider` once the stored login has one. `--exec-command` names the binary kubectl runs when `rancher` is not on its PATH.

Composite reads (`inspect *`, `images`, `usage`) return what you can
see: each section you cannot read in full is named in the `data.degraded`
field, and each entry has one warning. The command still exits 0.

Generated `get`/`patch`/`delete` commands cover the 49 kinds that
`rancher schema` lists (`delete` only for the kinds that can be deleted).
The full catalog is [COMMANDS.md](COMMANDS.md), and `rancher schema` lists
the same for the version you have installed. Per-kind verb rules:
[REFERENCE.md](REFERENCE.md#kinds--verbs); project scoping:
[REFERENCE.md](REFERENCE.md#project-scope---project).

Every request to Rancher carries a User-Agent that names the CLI version and
platform, and whether the process ran inside an agent session, as kubectl's
exec plugin or in CI. An administrator can see it in Rancher's audit log when
that log is on at level 1 or higher (it is off by default); nothing in it
affects which credential is used or grants access. What it holds and where it
shows up: [REFERENCE.md](REFERENCE.md#what-the-cli-sends-to-the-server).

## Safety model

Every command that changes the server runs these steps, in order:

1. `--dry-run` previews the change (the action, the fields the command sets
   or changes, and warnings) and changes nothing. The preview succeeds only
   if the server-side dry-run succeeds.
2. `--apply` executes. A command that changes the server, run with neither
   `--dry-run` nor `--apply`, is refused with `CONFIRMATION_REQUIRED`, and
   the refusal names the exact flags required.
3. `--yes` is required together with `--apply` (never alone) when the change
   is destructive: every `delete` and `revoke`, writes to sensitive kinds,
   and creating secrets that are shown only once (`create token`,
   `create kubeconfig`).
4. `--allow-deprecated` is also required for a write whose stability is
   `deprecated`.
5. Updates and deletes carry a
   [resourceVersion](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)
   precondition taken from their own dry-run preview: if the resource
   changes between that preview and the apply, the change fails (exit `5`)
   instead of overwriting it. A `--dry-run` run on its own is a preview
   only; the later `--apply` run takes a new dry-run preview and sets its
   precondition from the resource as it is at that time. Creating an object
   has no precondition (there is no existing object to check).

Rule of thumb: `--apply` means "yes, make the change"; `--yes` means "yes, I
saw it is destructive".

Why it works this way: the CLI is designed for coding agents first. Intent
is stated in machine-checkable flags, never inferred, and there is no
interactive "are you sure?" prompt - a prompt cannot be scripted, retried,
or audited. Every refusal is a structured error that names exactly what is
missing. An agent (or a script) can then add the missing flags on purpose or
stop.

> [!NOTE]
> `--dry-run` runs the
> [server-side dry-run](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run)
> and needs the same RBAC verb
> as the write - read-only credentials get an `RBAC` error. That means the
> write would be denied, not that dry-run is broken.

Most commands carry a contract (`public` | `internal`) and a stability
(`stable`/`beta`/`deprecated`), shown in the schema manifest.
All the steps and gates in full: [ARCHITECTURE.md](ARCHITECTURE.md).
Read-only mode: [REFERENCE.md](REFERENCE.md#read-only-mode).

## Output & exit codes

Nearly every command prints one JSON document per run under `-o json`,
versioned by `apiVersion`, that agents can depend on (exceptions:
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

- **Errors** have the same shape as a success: under `-o json`/`-o yaml`
  they are the same document, with the error object under `data`, and any
  warnings go in the document's own `warnings` array. Use `-o json` to get
  the document, errors included:

  ```json
  { "apiVersion": "cli.rancher.io/v1", "kind": "Error",
    "data": { "error": { "code": "NOT_LOGGED_IN", "message": "...",
              "hint": "rancher auth login 'prod'", "retryable": false } } }
  ```

  Every other format, including the default, prints a short text message
  on stderr instead:

  ```
  error (NOT_LOGGED_IN): ...
    hint: rancher auth login 'prod'
  ```

- **Exit codes** (act on the code without parsing output): `0` ok · `2` usage/validation ·
  `3` auth · `4` not-found · `5` conflict/precondition · `6` RBAC ·
  `8` lock-timeout · `9` Prime required · `10` TLS trust ·
  `11` capability unsupported · `14` credential store unavailable ·
  `130` interrupted · `1` generic; every code
  with its meaning: [REFERENCE.md](REFERENCE.md#error-codes). Ctrl-C or a
  termination signal ends the command with `INTERRUPTED`, exit `130` for
  either; a second signal forces a command that did not stop on the first
  to exit with 128 plus that signal's own number (`130` Ctrl-C, `143`
  SIGTERM).
  Rancher's proxy paths reject an invalid or insufficient token as a 403
  (`RBAC`, exit `6`), not a 401 (`AUTH`, exit `3`).
- **Discovery:** `rancher schema -o json` is the overview -
  commands, kinds, and exit codes as a compact table of contents;
  `rancher schema --all -o json` is the complete manifest, and the single
  source for contract and stability. `rancher schema <command|kind|group>`
  shows only that command, kind or group.

## Configuration

- **File:** `${XDG_CONFIG_HOME:-~/.config}/rancher/config.yaml` (mode `0600`):
  `servers`, `currentServer` (the name of the active server), `credentialStore`,
  `completion`, `readOnly`, `telemetry`. Credentials are not stored here.
- **Directory precedence:** `--config <dir>` > `RANCHER_CLI_CONFIG_DIR` >
  `$XDG_CONFIG_HOME/rancher` > `~/.config/rancher`.
- **Credential store:** OS keychain or a `0600` file; custom backends plug
  in as credential helpers ([CREDENTIAL_HELPERS.md](CREDENTIAL_HELPERS.md)).
  The config directory records the credential store its first login used;
  change it with
  `rancher auth login <name> --credential-store keychain|file|auto|exec:<name>`.
  Changing the credential store applies only to that entry's credential, and
  requires the other entries to be logged out of the credential store you
  are leaving first. How the credential store is chosen, how the CLI checks
  the keychain, and the credential store pin:
  [REFERENCE.md](REFERENCE.md#platforms--credential-stores).
- **Environment variables:** the full table, with each variable's flag and
  how the variable and the flag interact, is in
  [REFERENCE.md](REFERENCE.md#environment-variables).
- **Migration:** config and credentials are not imported - run `rancher auth
  login` once per server.

## Shell completion

    source <(rancher completion bash)   # ~/.bashrc
    source <(rancher completion zsh)    # ~/.zshrc
    rancher completion fish > ~/.config/fish/completions/rancher.fish
    rancher completion pwsh | Out-String | Invoke-Expression   # add to $PROFILE

Completion is offline-only by default - it never touches the network and
offers no candidates instead of an error. Live resource-name completion
is an opt-in: `rancher config set-completion --network=true`. Minimum shell
versions, what completion does offline, and details of live completion:
[REFERENCE.md](REFERENCE.md#shell-completion).

## Documentation

- [COMMANDS.md](COMMANDS.md) - the command catalog, by group (operators).
- [REFERENCE.md](REFERENCE.md) - platforms and credential stores, install
  internals, completion, scoping, kinds and verbs, cluster provisioning,
  composites, access management, diagnostics, output formats, read-only
  mode, server-version support, kubectl integration, TLS (operators,
  automation authors).
- [ARCHITECTURE.md](ARCHITECTURE.md) - layer map and design decisions
  (contributors).
- [CREDENTIAL_HELPERS.md](CREDENTIAL_HELPERS.md) - credential-helper
  protocol and examples (integrators).
- [MIGRATING.md](MIGRATING.md) - moving from the legacy Rancher CLI
  (operators).
- [skills/rancher/SKILL.md](skills/rancher/SKILL.md) - the agent skill
  (agents and their authors).

## Development

Requires the Go version set in go.mod (1.27); with the default
`GOTOOLCHAIN=auto`, `go` downloads it:

    make build        # bin/rancher
    make test         # go test -cover ./... (-race everywhere but the source scans and the cache model)
    make validate     # gofmt + vet + lint + tests (race detector except the source scans and the cache model); the commit gate
    make e2e          # end-to-end suite against an envtest-backed fake Rancher
    make e2e-fidelity     # verify recorded fidelity goldens against a real Rancher in Docker
    make e2e-fidelity-downstream  # verify downstream-cluster fidelity goldens against your own Rancher
    make              # the full ci pipeline (build + validate)

`make e2e-fidelity` boots the pinned `rancher/rancher` image, imports a pinned
`rancher/k3s` container as a downstream cluster (~7-9 min in all), and
checks that the fake Rancher the e2e suite runs against still behaves
like the real one, downstream proxy behavior included; `make
e2e-fidelity-update` re-records those goldens. `make e2e-fidelity-downstream`
compares a Rancher you already run, with a downstream cluster already
registered, against the same downstream goldens: point it at the server
via `E2E_FIDELITY_URL`, `E2E_FIDELITY_ADMIN_TOKEN`, `E2E_FIDELITY_CACERT` (a PEM, or
the literal `system` to trust your OS root store), and
`E2E_FIDELITY_DOWNSTREAM` (the cluster id); `make e2e-fidelity-downstream-update`
re-records the downstream goldens from that server instead.

One-time setup: `git config core.hooksPath .githooks` enables the
pre-commit `make validate` gate.

Manual testing: `scripts/dev <args>` - never run the binary directly; it
forces an isolated config directory and a file credential store, which keeps
a manual run from touching your real config or OS keychain. Contributor guide:
[AGENTS.md](AGENTS.md).

## Support & community

Questions and discussion: the [Rancher forums](http://forums.rancher.com/)
and [Rancher Slack](https://slack.rancher.io/). Bugs and feature requests
for the CLI, the agent skill, the installer, the credential helper or
this documentation:
[rancher/rancher-cli issues](https://github.com/rancher/rancher-cli/issues/new/choose).
A problem in the Rancher server itself goes to
[rancher/rancher issues](https://github.com/rancher/rancher/issues).

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
