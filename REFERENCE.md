# Operational reference

Part of the [Rancher CLI docs](README.md) · [command reference](COMMANDS.md)

This reference expands [README.md](README.md) with the full operational detail.

- [Platforms & credential stores](#platforms--credential-stores)
- [Environment variables](#environment-variables)
- [Install internals](#install-internals)
- [Shell completion](#shell-completion)
- [Project scope (--project)](#project-scope---project)
- [Kinds & verbs](#kinds--verbs)
- [Cluster provisioning](#cluster-provisioning)
- [Composite commands](#composite-commands)
- [Access management](#access-management)
- [Diagnostics & support](#diagnostics--support)
- [Output formats](#output-formats)
- [Read-only mode](#read-only-mode)
- [Supported server version](#supported-server-version)
- [kubectl integration](#kubectl-integration)
- [TLS & proxy](#tls--proxy)

## Platforms & credential stores

macOS, Linux, and Windows (CGO-free static binary; the Linux build runs on
any distro including musl/distroless).

| | Config dir (default) | Credential store (default) | File fallback |
|---|---|---|---|
| macOS | `~/.config/rancher` | Keychain | `0600` file |
| Linux | `~/.config/rancher` | Secret Service (D-Bus; absent when headless → file) | `0600` file |
| Windows | `%APPDATA%\rancher` | Credential Manager | owner-only-ACL file |

Precedence above the default everywhere: `--config` flag (`-c`) >
`RANCHER_CLI_CONFIG_DIR` > `$XDG_CONFIG_HOME/rancher`. The credential
backend is `auto` (probe once, pin) unless `RANCHER_CLI_CREDENTIAL_STORE`
or the config `credentialStore` field says otherwise; headless machines
(no D-Bus, locked keychain) keep working read-only against an existing
file-stored token, while new logins there ask for an explicit
`RANCHER_CLI_CREDENTIAL_STORE=file` opt-in.

Credential-store precedence: `RANCHER_CLI_CREDENTIAL_STORE` >
`credentialStore: keychain|file|auto|exec:<name>` in config > a
machine-local `credstore-pin` file > one-time probe. Under `auto` the
backend is probed once and pinned; the pin is re-probed only at `auth
login` and only to upgrade file→keychain, and a pinned-but-unusable
backend is a hard error (never a silent downgrade to plaintext).

Custom backends plug in as credential helpers: `credentialStore: exec:<name>`
runs the `rancher-credential-<name>` binary from `PATH` (the shipped
`rancher-credential-vault` covers HashiCorp Vault and OpenBao). Protocol and
examples: [CREDENTIAL_HELPERS.md](CREDENTIAL_HELPERS.md).

## Environment variables

Every variable below can also be set by a flag or the config file;
precedence is flag > environment > file.

| Var | Effect |
|---|---|
| `RANCHER_CLI_URL` | server URL |
| `RANCHER_CLI_TOKEN` | token, used directly and never persisted |
| `RANCHER_CLI_CACERT` | CA bundle (PEM) |
| `RANCHER_CLI_CREDENTIAL_STORE` | `keychain` \| `file` \| `auto` \| `exec:<name>` |
| `RANCHER_CLI_CONFIG_DIR` | config directory |
| `RANCHER_CLI_OAUTH_AUTH_FLOW` | OAuth flow: `devicecode` \| `authcode` |
| `RANCHER_CLI_OAUTH_CALLBACK_PORT` | local callback port for the `authcode` flow |
| `RANCHER_CLI_CONFIG_NO_LOCK` | disables the config directory's cross-process write lock; escape hatch for filesystems with broken file locking |

Scope is per-invocation: `--cluster` routes to a downstream cluster; `--project`
filters namespaced kinds. No scope is persisted or ambient — see
[Project scope](#project-scope---project).

## Install internals

`install.sh` unpacks each release into its own versioned directory under
`~/.local/share/rancher-cli`, then repoints the `rancher` and
`rancher-credential-vault` symlinks in the bin dir as the final steps.
Each link is replaced atomically, so an interrupted install never leaves a
broken binary on `PATH`. A kill between the two link updates leaves the
pair split (fresh install: the helper link is missing; upgrade: the two
links point at different versions); rerunning the installer repairs
either state.
An install receipt at `~/.config/rancher-cli/install-receipt.json`
records what is installed; rerunning the installer silently replaces
managed links and prints a notice when taking over a previous manual
installation.

Checksums (`SHA256SUMS`) are always verified. Build provenance is
verified when an authenticated `gh` CLI is available, and
`--verify-attestation` makes that verification mandatory instead of
best-effort.

Token resolution order: `GH_TOKEN` > `GITHUB_TOKEN` > `gh auth token`
(when `gh` is on PATH and authenticated) > anonymous.

When a token is in play the installer downloads assets through the
GitHub API asset endpoint — required on internal repositories, where
the plain release download URLs serve nothing — and fetches any signed
storage redirect with a second, untokened request, so the token never
travels past the API host. A token that cannot read the release
metadata falls back to the direct download URLs with the failure
explained in any later error.

After linking, the installer warns about any other `rancher` or
`rancher-credential-vault` elsewhere on `PATH` (a shadowed install is
easy to miss behind a stale shell command hash — `hash -r` or `rehash`
clears it), staying silent for convenience symlinks that resolve back
to the installed binary. A version change is named at the switch:
upgrades as a note, downgrades as a warning — rollback stays a
one-command flow, it is just never silent.

Each release carries its own fixed `install.sh` as a release asset
(checksummed and attested like every other asset); the raw
`https://raw.githubusercontent.com/…/main/install.sh` URL always serves
the current copy from `main`.

### Defensive bootstrap

The README one-liner is safe against truncation — every statement in
`install.sh` lives inside a function and the final `main "$@"` line only
runs when the whole script arrived, so a cut download either stops on a
syntax error or executes nothing. The pipe form's one trade-off: when the
*fetch itself* fails, `sh` reads empty input and exits `0` — curl's
message on stderr is the only failure signal. For scripts that must
branch on the download's exit code, use the download-then-run form:

    (
      t="$(mktemp)" || exit 1
      trap 'rc=$?; rm -f "$t"; exit "$rc"' 0
      trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM
      curl --proto '=https' --tlsv1.2 -fsSL \
        -o "$t" \
        https://raw.githubusercontent.com/rancher/rancher-cli/main/install.sh \
        && sh "$t"
    )

It propagates curl's exit code, cleans up its temp file on any exit, and
never executes a partial stream.

While the repository is internal and `gh` is unavailable, use the
token-authenticated equivalent below — the token reaches curl via stdin
config and `sh` via the environment, never process arguments:

    (
      t="$(mktemp)" || exit 1
      trap 'rc=$?; rm -f "$t"; exit "$rc"' 0
      trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM
      printf 'header = "Authorization: Bearer %s"\n' "${GH_TOKEN:-$GITHUB_TOKEN}" | \
        curl --config - --proto '=https' --tlsv1.2 -fsSL \
          -o "$t" \
          https://raw.githubusercontent.com/rancher/rancher-cli/main/install.sh \
        && GH_TOKEN="${GH_TOKEN:-$GITHUB_TOKEN}" sh "$t"
    )

Flags append to the `sh "$t"` invocation (e.g. `sh "$t" --version <tag>`).

### Manual download & verification

Download the assets for your platform from the releases page, check them
against `SHA256SUMS`, and (optionally) verify provenance with
`gh attestation verify <file> --repo rancher/rancher-cli
--signer-workflow rancher/rancher-cli/.github/workflows/release.yaml`.

## Shell completion

PowerShell (Windows PowerShell 5.1 or PowerShell 7) — pipe into the session so
no script file is written and PowerShell's ExecutionPolicy does not apply:

    rancher completion pwsh | Out-String | Invoke-Expression

To persist it, add that line to your `$PROFILE`. Loading `$PROFILE` itself needs
an ExecutionPolicy that allows local scripts (`RemoteSigned` or looser); the
per-session command above works regardless.

Minimum shell versions: bash 3.2 (macOS stock) with the `bash-completion`
package installed — on bash 3 the double-TAB description column is absent,
completion itself works; zsh 5.0 with `compinit` loaded; fish 3.0;
Windows PowerShell 5.1 or pwsh 7. Each generated script states its own floor
in its header comment.

Completion is offline-only by default: command/group/flag names are free (the
whole tree); `get`/`patch`/`delete` additionally complete each kind's plural
and short aliases (`po<TAB>` → `po`/`pod`/`pods`); `config use-server` and
`config delete-server` complete from the local config file. It never touches the network, prompts, or writes anything —
a missing or unreadable config degrades to no candidates, never an error.

### Live completion (opt-in)

After installing the script, enable live resource-name completion:

    rancher config set-completion --network=true

This completes cluster names (including `inspect cluster` and `inspect project`
positional names), project names, namespaces, pods, deployments, and nodes from
the Rancher API. A credential-store capability note is printed to indicate which
completions are available given your current auth setup.

The per-TAB HTTP budget defaults to 500 ms; adjust with `--timeout`:

    rancher config set-completion --timeout=1s

## Project scope (--project)

The global `--project <id-or-name>` flag scopes Downstream namespaced kinds
(`get`/`delete`/`patch`, plus `usage`) to one project's project namespaces:
a bare list is filtered to project namespaces; a named op or mutation
validates the target namespace's membership before touching the server.
`get namespaces --project <p>` is the one special case — it lists namespaces
by the membership label server-side rather than filtering client-side.
Every other command family (cluster-scoped kinds, management-plane kinds,
and the composites `inspect pod`/`inspect deployment`/`inspect cluster`/`images`) rejects
a non-empty `--project` with `VALIDATION` rather than silently ignoring
it. `usage` rejects `--project` only in combination with `--by node`
(a node row mixing project-scoped requests with whole-node metrics would
be misleading); all other `usage` groupings accept `--project`.
Precedence: `--project` flag > unscoped.

## Kinds & verbs

Every kind in the schema vocabulary gets its generated verbs by these rules:

- `get` and `patch` exist for every kind in the schema kind vocabulary.
  One exception: `patch clusterregistrationtoken` is refused — the kind is
  read-only.
- `delete` exists only where removal is a sane operator action.
  Controller-owned objects (Fleet bundles, CAPI machines), server config
  (`setting`, `feature`), and role definitions (`roletemplate`, `globalrole`)
  are not deletable — delete the owning source object instead. The management
  `cluster` kind is not deletable either; cluster removal goes through
  `delete provisioning-cluster`. `delete user` is destructive (requires
  `--yes`): deleting a user removes the identity record and Rancher's
  controllers cascade-remove the user's role bindings (cluster memberships,
  project memberships, global role bindings) with it.
- `create` is bespoke per kind where a real creation UX exists (`project`,
  `namespace`, `secret`, `token`, `kubeconfig`, `cluster`); `create
  resource -f` covers any supported kind from a manifest.
- RBAC bindings (`projectroletemplatebinding`, `clusterroletemplatebinding`,
  `globalrolebinding`): prefer `grant`/`revoke` — subject resolution,
  duplicate detection, partial-failure reporting. The raw verbs exist for
  manifest-driven and declarative flows.

`rancher schema <kind>` shows one kind's effective verb table — available
(with contract and stability), refused (with the reason), or not available
(with the reason removal is routed elsewhere).

`feature` (Rancher's `management.cattle.io/v3` Feature flags) is one of
those generated kinds — not deletable, sensitive (flipping a flag via
`patch feature` escalates to `--yes`). Its interesting fields
(`spec.value`, `status.default`, `status.lockedValue`) are tri-state
booleans (an absent `spec.value` means "follow the default"), which a
table cell would misread — `-o json` is the real read for a feature's
actual state.

`nodedriver` (Rancher's `management.cattle.io/v3` NodeDriver registry — the
server side of `create cluster`'s node-driver mode) is a read-mostly
generated kind: not deletable, sensitive (`patch nodedriver` toggles a
driver's platform-wide availability and escalates to `--yes`). `get
nodedrivers` lists NAME, DISPLAYNAME, ACTIVE, and BUILTIN.

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
never exposes the raw units), and shown-once handling for the created secret.

## Cluster provisioning

Rancher's provisioning model (node drivers, machine pools, imported and
custom clusters) is documented in the
[Rancher manual](https://ranchermanager.docs.rancher.com/how-to-guides/new-user-guides/launch-kubernetes-with-rancher).

Node-driver clusters take a machine config plus a cloud credential. Two
prerequisites before the example below: a cloud credential must already
exist (`rancher get cloud-credentials` lists yours — `do-creds` below),
and the Kubernetes version must be one the server accepts
(`rancher get kubernetes-versions` lists them — substitute it for
`<supported-version>`):

    rancher create machine-config digitaloceanconfig web-pool \
      --region sfo3 --size s-2vcpu-4gb --image debian-13-x64 --apply
    rancher create cluster web \
      --machine-config digitaloceanconfig/web-pool --cloud-credential do-creds \
      --k8s-version <supported-version> --etcd --controlplane --worker --apply
    rancher get cluster web --wait --timeout 15m
    rancher kubeconfig --cluster web > web.yaml

`--wait` on `get <kind>` applies the kind's default readiness; for
clusters that is `condition=Ready`.

`create machine-config` curates driver flags for `digitaloceanconfig`
(`--region`, `--size`, `--image`, `--monitoring`, `--ipv6`,
`--private-networking`, `--tags`); all other kinds take
`--set field=value` for driver-specific fields; credential fields are
always rejected. Multi-pool node-driver clusters go through
`create resource -f`.

Imported and custom clusters need no machine config: `create cluster
<name> --import` or `--custom`. Custom-cluster nodes join with the
registration commands that `inspect cluster <name> --registration`
fetches.

A `creatorId not stamped` warning from `create cluster` means the
identity probe failed — dry-run may be denied, but `--apply` is still
expected to succeed.

## Composite commands

`inspect cluster` is a composite read: management identity + agent
connectivity (management Connected/Ready conditions; a disconnect warns) +
provisioning status/conditions (ready derived from the Ready condition),
machine pools, members (ClusterRoleTemplateBindings with display names
resolved), machines ([Cluster API](https://cluster-api.sigs.k8s.io/)
Machines with phase and first failing condition
message — surfacing provider errors invisible to other commands), and
node-join/import registration commands. Registration commands are withheld
by default — they contain join credentials; pass `--registration` to fetch
them, or use `get clusterregistrationtoken default-token --namespace
<cluster-id>`. The management read is mandatory (unknown/ambiguous cluster
or a failed read is a structured error, non-zero exit); provisioning,
members, machines, and registration sections degrade independently to
notes/warnings.

`inspect project` renders a sectioned text card (PROJECT, NAMESPACES,
MEMBERS, QUOTA, USAGE); members are ProjectRoleTemplateBindings with
display names resolved from Users/UserAttributes; the members section
degrades independently to a warning.

## Access management

`grant` and `revoke` manage
[Rancher role bindings](https://ranchermanager.docs.rancher.com/how-to-guides/new-user-guides/authentication-permissions-and-global-configuration/manage-role-based-access-control-rbac)
at project, cluster, or global scope. `grant` auto-provisions unknown principals when the binding
applies — this is Rancher behavior, not a CLI choice. For both commands,
partial failures leave already-created (or already-deleted) bindings in
place. Omitting `--role` on `revoke` revokes every role at the scope.

## Diagnostics & support

`logs`/`support-bundle` cover only what Rancher's own Kubernetes API can reach.
`rancher-system-agent` is a node systemd unit, not a pod — its logs are
`journalctl -u rancher-system-agent` on the node itself, and generic pod
logs outside the curated targets stay with `inspect pod` and kubectl.
`support-bundle` is deliberately the API-reachable half of a two-bundle
story: pair it with
[rancherlabs/support-tools](https://github.com/rancherlabs/support-tools)'
node collector for the
node-level half (journald, the system-agent unit, OS state) that no API
call can reach.

## Output formats

`-o`/`--output`: `table` (default, for users) · `wide` (table plus any
Wide-marked columns — presentation, never a scope change: a payload with no
wide-only columns renders identically to `table`) · `json` · `yaml` ·
`name` (`.metadata.name`/ids only) · `template=<go-template>` — a Go template
over the JSON view of `data` (same shape as `-o json`). Any other value is
rejected with a `VALIDATION` error before the command runs. A kind without a
table projection falls back to indented JSON under the default `table`
format; use `-o json` for the versioned envelope. Some commands (e.g. `logs`)
render a plain-text default under `table`/default format instead, with
warnings on stderr as `# warning: <w>` lines;
`-o json`/`yaml`/`template=`/`name` still see the normal envelope over `data`.
Structured output (json, yaml, template, and the table fallback) omits
`metadata.managedFields` (kubectl-parity; no opt-out flag — read the raw API
for
[server-side-apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
forensics).

## Read-only mode

Three sources, OR'd together, put a session into read-only mode, refusing
every mutating command before any network call — strictly one-way, no
per-invocation override exists:

- Config: `readOnly: true` (top-level config field).
- Env: `RANCHER_CLI_READONLY`.
- Flag: `--read-only` (global).

> [!IMPORTANT]
> `RANCHER_CLI_READONLY` follows non-empty-means-set — the literal string
> `"false"` still turns read-only mode ON. Every `RANCHER_*` variable in
> this CLI behaves this way.

Refusal is `VALIDATION`, naming the command and the active source(s) (e.g.
"enabled by RANCHER_CLI_READONLY") — the one-way rule means you have to know
which knob to turn off. `rancher schema -o json` reports the resolved state
(`readOnly`, `readOnlySources`) so an agent can check before trying.

Scoping, stated honestly: read-only mode refuses `Mutating` commands only —
the CLI's managed-resource surface. `auth login`, `auth status`, and local
`config` commands stay usable; otherwise a read-only session couldn't even
establish itself.

> [!WARNING]
> Read-only mode is a CLI-side courtesy layer, not a security boundary.
> Real enforcement is a Rancher token bound to read-only RBAC — pair them.

`--dry-run` of a mutating command still sends write-verb requests
server-side (admission dry-run); a genuinely read-only RBAC token cannot
dry-run at all. Read-only mode doesn't special-case this — it refuses
mutating commands outright, `--dry-run` included.

## Supported server version

The CLI declares a minimum supported Rancher version, `>= 2.14`
(`rancher schema -o json` → `minRancherVersion`). Enforcement today is
warn-only: `auth login` and `auth status` call the unauthenticated
`GET /rancherversion` endpoint and emit a warning — never a failure — when
the connected server is below the floor, or when the version can't be
determined at all. An unreachable endpoint, a non-200 response, or an
unparseable value (e.g. a dev/head build) all assume "supported" and warn
instead of refusing. No RBAC permissions are required.

Graduation intent, not implemented today: refuse-with-escape-hatch (e.g. a
`--allow-unsupported-server`-style flag) once there's real-use feedback on
the warn-only period. Tracked as future work.

**`auth status` enrichment.** Beyond connectivity, `auth status -o json` also
reports `rancherVersion`/`supported` (the version-floor check above),
`identity` (the caller's `userID`, from a create-verb identity probe against
`ext.cattle.io`), and `token` (the effective token's
name/kind/provider/expiry/`current` marker — for both `ext.cattle.io` tokens
and management.cattle.io/v3 tokens; `token: null` with a warning appears only
on a probe failure such as a revoked/rotated token). Any of these fields
degrading to `null` with a warning is expected, not a bug — including
`identity` under a read-only-RBAC token, since the identity probe is itself a
create-verb request. `auth status`'s connectivity result (`ok`) never depends
on them.

## kubectl integration

`rancher auth get-token` is a `client.authentication.k8s.io/v1beta1`
[exec-credential plugin](https://kubernetes.io/docs/reference/config-api/client-authentication.v1beta1/). Rancher-generated kubeconfigs invoke it to create
short-lived Kubernetes credentials; you don't run it directly. Older
kubeconfigs generated by the legacy Rancher CLI (which call `rancher token`
instead) also work after a binary swap — the compatibility shim delegates to
the same machinery. Flags: `--server`,
`--cluster` (required), `--name` (cache slot, keyed per name+server+cluster; omitted = no
caching), `--cacert`,
`--auth-provider`, `--auth-flow`, `--skip-verify`. When `--server` or `--cacert` are
omitted, they fall back to the current server in config. It supports SAML,
OAuth (device-code / auth-code), and basic auth flows; long-poll flows can
be canceled with Ctrl-C. List cached credentials with `rancher auth list-tokens`.
Clear them with `rancher auth delete-token [<name>]`. On a cache miss it first
tries to create a token from the stored `auth login` credential for the matching
server; when that credential lives in the OS keychain, this can trigger a
keychain-unlock prompt — CI and other non-interactive runs should use
`RANCHER_CLI_CREDENTIAL_STORE=file` or a warm `--name` cache.

## TLS & proxy

Server connections verify TLS against the configured CA (`--cacert` /
`RANCHER_CLI_CACERT`) or the system trust store by default — verification is never
disabled implicitly. HTTP(S) proxies are honored from the environment.
