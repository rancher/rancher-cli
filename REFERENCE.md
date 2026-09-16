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
- [Error codes](#error-codes)
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
`RANCHER_CLI_CONFIG_DIR` > `$XDG_CONFIG_HOME/rancher`.

The credential backend belongs to the directory, not to the process. The
first login records the store it used in `config.yaml`'s `credentialStore`
field, and from then on that is the only store any process on the
directory reads. `RANCHER_CLI_CREDENTIAL_STORE` still chooses the store
for a directory that has not declared one - a fresh directory, a CI run -
and setting it to the same value the directory declares is fine, but
setting it to a different one is a VALIDATION error naming both, not a
silent override: two processes on one directory reading different
backends is how a command ends up serving a credential nobody asked for.
Unset the variable, or use a different `--config`. An undeclared
directory defaults to `auto` (probe once, pin).

A directory whose backend is the keychain reads only the keychain: on a
headless machine (no D-Bus, locked keychain) every command fails with the
way out rather than falling back to a file-stored token, so a headless
machine wants `RANCHER_CLI_CREDENTIAL_STORE=file` from the start, before
the first login records it.

The exec-credential cache follows the same rule. Every record names the
directory that created it, the server entry it came from and that entry's
cache binding id, so a copied config directory never serves the
credentials its source cached, and a login, a logout or a credential
clearing makes every record of the previous login a miss at once. A
`config.yaml` the CLI cannot read makes every cached credential a miss as
well - the record's owner is named in that file - so a broken config
shows its own error rather than a stale token. A directory with no
`config.yaml` at all is not that case: it is the state a freshly
installed exec plugin runs in, and it caches and serves normally.

A keychain credential belongs to the config directory that stored it. Each
directory keeps a per-entry identity in `credstore-id/`, and the keychain
account carries it, so two directories never share a stored credential for
a server name they both use. A copied directory inherits its original's
identity and reads the same credential until one side stores a new one:
the other side's next read then fails with `CREDENTIAL_REPLACED`, and its
next `rancher auth login` for that entry separates it - one entry, not the
whole directory. A login that did not finish leaves `LOGIN_INCOMPLETE`,
which means run the command again. Deleting a file under `credstore-id/`
is indistinguishable from never having logged in: the entry reads as not
logged in and needs a fresh login.

Credential-store resolution: `credentialStore:
keychain|file|auto|exec:<name>` in config is the directory's own choice,
recorded by its first login; `RANCHER_CLI_CREDENTIAL_STORE` supplies that
choice for a directory that has not made one, and disagreeing with one
that has is an error rather than an override. Under `auto` the value
resolves through a machine-local `credstore-pin` file, and failing that a
one-time probe: the backend is probed once and pinned, the pin is
re-probed only at `auth login` and only to upgrade file to keychain, and a
pinned-but-unusable backend is a hard error (never a silent downgrade to
plaintext).

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
| `RANCHER_CLI_CREDENTIAL_STORE` | `keychain` \| `file` \| `auto` \| `exec:<name>`; chooses the store for a directory that has not declared one, and must not disagree with one that has (a different value is a VALIDATION error, not an override); a keychain directory reads the keychain only, so set `file` where no keychain is reachable |
| `RANCHER_CLI_CONFIG_DIR` | config directory |
| `RANCHER_CLI_OAUTH_AUTH_FLOW` | OAuth flow: `devicecode` \| `authcode` |
| `RANCHER_CLI_OAUTH_CALLBACK_PORT` | local callback port for the `authcode` flow |
| `RANCHER_CLI_CONFIG_NO_LOCK` | disables the config directory's cross-process write lock; escape hatch for filesystems with broken file locking |
| `RANCHER_CLI_COMPLETION_DEBUG` | when non-empty, traces each phase of a shell completion request to stderr; stdout is unchanged |

Scope is per-invocation: `--cluster` routes to a downstream cluster; `--project`
filters namespaced kinds. No scope is persisted or implied - see
[Project scope](#project-scope---project).

## Install internals

`install.sh` unpacks each release into its own versioned directory under
`~/.local/share/rancher-cli` and activates it by atomically switching one
`current` symlink. The `rancher` and `rancher-credential-vault` links in
the bin dir point through `current`, so an upgrade updates both binaries
in a single switch - the pair can never end up on different versions. On
a fresh install the two bin-dir links are created separately; a kill
between them can leave the helper link missing, and rerunning the
installer repairs that state.
An install receipt at `~/.config/rancher-cli/install-receipt.json`
records what is installed; rerunning the installer silently replaces
managed links and prints a notice when taking over a previous manual
installation.

`install.ps1` keeps the same layout under `%LOCALAPPDATA%\rancher-cli`:
each release lands in `versions\<tag>\` as `rancher.exe` and
`rancher-credential-vault.exe`, and the `bin` directory (the one to put on
`PATH`) is an NTFS junction to the current version. A junction needs no
admin rights and no developer mode, and the installer switches it by
creating a new junction beside it and renaming that over the old one in a
single operation - both binaries change together, and a kill at any point
leaves either the old pair or the new pair in place, never neither. The
receipt lives at `%APPDATA%\rancher-cli\install-receipt.json`. `-BinDir`
moves the junction, so the directory it names must be new, empty, or hold
only a prior by-hand `rancher.exe`/`rancher-credential-vault.exe` pair,
which the installer takes over with a notice. Unlike `install.sh`, which
links each binary into a shared directory such as `~/.local/bin`,
`-BinDir` on Windows must name a dedicated directory: the whole directory
becomes the version junction.

Checksums (`SHA256SUMS`) are always verified. Build provenance is
verified when an authenticated `gh` CLI is available, and
`--verify-attestation` makes that verification mandatory instead of
best-effort.

Token resolution order: `GH_TOKEN` > `GITHUB_TOKEN` > `gh auth token`
(when `gh` is on PATH and authenticated) > anonymous.

When a token is in play the installer downloads assets through the
GitHub API asset endpoint - required on internal repositories, where
the plain release download URLs serve nothing - and fetches any signed
storage redirect with a second, untokened request, so the token never
travels past the API host. A token that cannot read the release
metadata falls back to the direct download URLs with the failure
explained in any later error.

After linking, the installer warns about any other `rancher` or
`rancher-credential-vault` elsewhere on `PATH` (a shadowed install is
easy to miss behind a stale shell command hash - `hash -r` or `rehash`
clears it), staying silent for convenience symlinks that resolve back
to the installed binary. A version change is named at the switch:
upgrades as a note, downgrades as a warning - rollback stays a
one-command flow, it is just never silent.

Each release carries its own fixed `install.sh` and `install.ps1` as
release assets, checksummed and attested like every other asset; the raw
`https://raw.githubusercontent.com/…/main/install.sh` and `…/install.ps1`
URLs always serve the current copy from `main`.

### Defensive bootstrap

The README one-liner is safe against truncation - every statement in
`install.sh` lives inside a function and the final `main "$@"` line only
runs when the whole script arrived, so a cut download either stops on a
syntax error or executes nothing. The pipe form's one trade-off: when the
*fetch itself* fails, `sh` reads empty input and exits `0` - curl's
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
token-authenticated equivalent below - the token reaches curl via stdin
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

#### Windows

`install.sh` covers Linux and macOS only; on Windows, run the PowerShell
installer (PowerShell 7 or Windows PowerShell 5.1):

    irm https://raw.githubusercontent.com/rancher/rancher-cli/main/install.ps1 | iex

It verifies both binaries against `SHA256SUMS`, verifies build provenance
when an authenticated `gh` is available (`-VerifyAttestation` makes that
mandatory), installs under `%LOCALAPPDATA%\rancher-cli`, and prints the
`PATH` line to add - it never edits `PATH` itself. Its atomic switch
to a new version relies on a directory rename that requires Windows 10
version 1607 / Windows Server 2016 or later. Parameters
(`-Version <tag>`, `-Pre`, `-BinDir <dir>`, `-VerifyAttestation`,
`-DryRun`, `-Quiet`) go through the scriptblock form:

    iex "& { $(irm https://raw.githubusercontent.com/rancher/rancher-cli/main/install.ps1) } -Version v1.2.3"

While the repository is internal, fetch the script through `gh` instead
(the installer then reuses gh's token for the release assets):

    iex "& { $(gh api 'repos/rancher/rancher-cli/contents/install.ps1?ref=main' -H 'Accept: application/vnd.github.raw' | Out-String) } -Pre"

Like `install.sh`, the whole script lives inside a function invoked on its
last line, so a truncated download either fails to parse or runs nothing.

##### By hand

If the installer cannot be used, install by hand:

1. Download the `windows` release assets: the CLI binary and
   `rancher-credential-vault`.
2. Rename them to `rancher.exe` and `rancher-credential-vault.exe`. Both
   names matter: the kubeconfig exec plugin invokes the bare command
   `rancher`, and a credential store helper is invoked as
   `rancher-credential-<name>` - on Windows, Go's process lookup only
   resolves a bare name against a `PATHEXT` extension (`.exe`) on `PATH`.
3. Verify both executables against `SHA256SUMS` (PowerShell) - the
   helper matters as much as the CLI: it is the process that receives
   your vault credentials, so never install it unverified:

       Get-FileHash rancher.exe -Algorithm SHA256
       Get-FileHash rancher-credential-vault.exe -Algorithm SHA256

   Compare each output against its matching line in `SHA256SUMS`.
4. Optionally verify build provenance for both:
   `gh attestation verify rancher.exe --repo rancher/rancher-cli --signer-workflow rancher/rancher-cli/.github/workflows/release.yaml`,
   then the same command with `rancher-credential-vault.exe`.
5. Only after both files verify, place them in a directory on `PATH`.

##### Editor quirk

On Windows 11, the Store version of Notepad opens `config edit`'s temp file as a new tab in an already-running Notepad window rather than a new window; close the tab, not the window, to return control to the CLI.

## Shell completion

PowerShell (Windows PowerShell 5.1 or PowerShell 7) - pipe into the session so
no script file is written and PowerShell's ExecutionPolicy does not apply:

    rancher completion pwsh | Out-String | Invoke-Expression

To persist it, add that line to your `$PROFILE`. Loading `$PROFILE` itself needs
an ExecutionPolicy that allows local scripts (`RemoteSigned` or looser); the
per-session command above works regardless.

Minimum shell versions: bash 3.2 (macOS stock) with the `bash-completion`
package installed - on bash 3 the double-TAB description column is absent,
completion itself works; zsh 5.0 with `compinit` loaded; fish 3.0;
Windows PowerShell 5.1 or pwsh 7. Each generated script states its own floor
in its header comment.

Completion is offline-only by default: command/group/flag names are free (the
whole tree); `get`/`patch`/`delete` additionally complete each kind's plural
and short aliases (`po<TAB>` → `po`/`pod`/`pods`); `config use-server` and
`config delete-server` complete from the local config file. It never touches the network, prompts, or writes anything -
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

Live completion degrades to no candidates on any failure - an expired token,
a slow server, a URL the credential store cannot match - without a message.
To see why, run the request the shell script issues with tracing on; every
phase prints one timed line on stderr and stdout stays exactly what the
shell would receive:

    RANCHER_CLI_COMPLETION_DEBUG=1 rancher --rancher-completion-proto=2 get cluster --rancher-completion-cur= --generate-shell-completion

PowerShell: `$env:RANCHER_CLI_COMPLETION_DEBUG = "1"` first, then the same
command.

## Project scope (--project)

The global `--project <id-or-name>` flag scopes Downstream namespaced kinds
(`get`/`delete`/`patch`, plus `usage`) to one project's project namespaces:
a bare list is filtered to project namespaces; a named op or mutation
validates the target namespace's membership before touching the server.
`get namespaces --project <p>` is the one special case - it lists namespaces
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
  One exception: `patch clusterregistrationtoken` is refused - the kind is
  read-only. A `get` list accepts `--selector` (`-l`), a Kubernetes label
  selector the server applies (the same grammar as kubectl's `-l`:
  `app=web`, `tier!=cache`, `env in (a,b)`, `!legacy`); it is validated
  before the request and refused on a named get. Under `--project` both
  apply: `get namespaces` sends the two selectors as one conjunction; for
  every other kind the server filters by `--selector` and the CLI keeps
  only the project's namespaces. `get clusterregistrationtoken` reads
  through Steve and applies the selector to the decoded list itself.
- `delete` exists only where removal is a sane operator action.
  Controller-owned objects (Fleet bundles, CAPI machines), server config
  (`setting`, `feature`), and role definitions (`roletemplate`, `globalrole`)
  are not deletable - delete the owning source object instead. The management
  `cluster` kind is not deletable either; cluster removal goes through
  `delete provisioning-cluster`. `delete user` is destructive (requires
  `--yes`): deleting a user removes the identity record and Rancher's
  controllers cascade-remove the user's role bindings (cluster memberships,
  project memberships, global role bindings) with it.
- `create` is bespoke per kind where a real creation UX exists (`project`,
  `namespace`, `secret`, `token`, `kubeconfig`, `cluster`); `create
  resource -f` covers any supported kind from a manifest.
- RBAC bindings (`projectroletemplatebinding`, `clusterroletemplatebinding`,
  `globalrolebinding`): prefer `grant`/`revoke` - subject resolution,
  duplicate detection, partial-failure reporting. The raw verbs exist for
  manifest-driven and declarative flows.

`rancher schema <kind>` shows one kind's effective verb table - available
(with contract and stability), refused (with the reason), or not available
(with the reason removal is routed elsewhere).

`feature` (Rancher's `management.cattle.io/v3` Feature flags) is one of
those generated kinds - not deletable, sensitive (flipping a flag via
`patch feature` escalates to `--yes`). Its interesting fields
(`spec.value`, `status.default`, `status.lockedValue`) are tri-state
booleans (an absent `spec.value` means "follow the default"), which a
table cell would misread - `-o json` is the real read for a feature's
actual state.

`nodedriver` (Rancher's `management.cattle.io/v3` NodeDriver registry - the
server side of `create cluster`'s node-driver mode) is a read-mostly
generated kind: not deletable, sensitive (`patch nodedriver` toggles a
driver's platform-wide availability and escalates to `--yes`). `get
nodedrivers` lists NAME, DISPLAYNAME, ACTIVE, and BUILTIN.

`token` and `kubeconfig` are registered with an empty GVR version, resolved
via discovery at call time (the same mechanism the CAPI kinds use) - on a
Rancher older than 2.14 (the CLI's supported minimum), the generated
`get`/`patch`/`delete` ops on these two kinds fail with a friendly `NOT_FOUND`
(`requires Rancher >= 2.14, the supported minimum`) pointing at `auth
list-tokens` and the exec-credential `kubeconfig` instead.
Every operation on these two kinds is also plain Kubernetes API - `kubectl
get/create tokens.ext.cattle.io` / `kubeconfigs.ext.cattle.io` works too (with
`--validate=false` for a standard user, since kubectl's CRD-schema validation
fallback needs list rights most users lack). The CLI adds the auth bootstrap,
the token-vs-kubeconfig TTL unit conversion (`--ttl` takes a duration; the
underlying spec fields are milliseconds and seconds respectively - the CLI
never exposes the raw units), and shown-once handling for the created secret.

`oidcclient` (Rancher's `management.cattle.io/v3` OIDCClient - the server
side of the embedded OIDC provider) is the first generated kind gated behind
a Rancher Prime capability requirement (feature `oidc-provider`): `rancher
schema oidcclient` and `--help` mark it accordingly. On a server where the
feature is explicitly disabled, the verbs refuse with a `VALIDATION`
"feature is disabled on this server" message on every edition, before
reaching the server's data; `PRIME_REQUIRED` (exit 9) is reserved for a
Prime-tiered feature on a server whose edition cannot enable it. Sensitive (`patch oidcclient`
escalates to `--yes`) - the generic patch path can set the same client-secret
lifecycle annotations the curated `create oidc-client` / `oidc-client
add-secret|rotate-secret|remove-secret` commands gate behind confirmation,
waiting, and one-time delivery; prefer the curated commands. Deleting a
client cascades: its backing secret is removed (Kubernetes garbage
collection via an owner reference, independent of the `oidc-provider`
controller), instantly invalidating every live client secret.

### Field ownership

Every `create` and `patch` the CLI performs names itself to the server as
the field manager `cli.rancher.io`, so an object's managed fields
(`kubectl get <kind> <name> -o yaml --show-managed-fields`) show which
fields the CLI wrote. The name is the CLI's domain, unversioned, so it
stays the same across releases and binary names.

`patch` reads the same record before it changes anything. When a field the
patch changes is owned by a manager that applies it (server-side apply, as
Flux and Argo CD do), the plan carries a warning naming that manager: it
may put the old value back on its next reconcile. Fields owned by managers
that merely updated them (Rancher's controllers, `kubectl edit`) are not
flagged. The warning informs; it never gates. A patch op whose `path` (or
`from`) is `/metadata/managedFields` or `/metadata/resourceVersion`, or
anything under them, is refused with `VALIDATION`; so is a replace of
`/metadata` or of the whole object whose value carries them. The server
sets both fields; the CLI always sends them back exactly as fetched.

Every `diff[].path` a dry-run reports (in `-o json` and in the default text
render) is an RFC 6901 JSON Pointer - `~1` for a literal `/` and `~0` for a
literal `~` inside a key, so a domain-prefixed label or annotation key
renders as one escaped segment. The string is usable verbatim as a JSON
Patch op's `path`.

## Cluster provisioning

Rancher's provisioning model (node drivers, machine pools, imported and
custom clusters) is documented in the
[Rancher manual](https://ranchermanager.docs.rancher.com/how-to-guides/new-user-guides/launch-kubernetes-with-rancher).

Node-driver clusters take a machine config plus a cloud credential. Two
prerequisites before the example below: a cloud credential must already
exist (`rancher get cloud-credentials` lists yours - `do-creds` below),
and the Kubernetes version must be one the server accepts
(`rancher get kubernetes-versions` lists them - substitute it for
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

`rancher kubeconfig` prints one context through the Rancher proxy and,
when the cluster has the Authorized Cluster Endpoint enabled and the
multi-cluster-management feature is on, the direct contexts Rancher's own
kubeconfig carries: `<entry>-fqdn` when the endpoint names an fqdn (its
`caCerts` as the CA), else one `<entry>-<node>` per control-plane node at
`https://<address>:6443` with the cluster's CA; every context shares the
one exec user entry. The fqdn context, else the first Ready node, is
current; with no Ready node the proxy stays current. When the direct
contexts cannot be derived (the feature off or unreadable, the nodes not
readable, no control-plane node with an address, a malformed CA or
address) the proxy-only file is printed and one stderr notice says why.
The exec block then sets `provideClusterInfo`, so the plugin learns which
endpoint kubectl is about to use: a cluster token it has just created is
accepted by the direct endpoint only after Rancher has synced it there,
so the plugin waits, up to ten seconds, until that endpoint authenticates
the token (a SelfSubjectReview; a Forbidden answer counts, a 401 does
not) and remembers the endpoint in the cache; on timeout the credential
is returned with a warning naming the proxy context as the fallback, and
a cancellation revokes the token it created. Two limits: the plugin needs
Rancher to create or refresh the token, so a direct context works while
Rancher is unreachable only as long as the cached credential lasts; and
the identity the direct endpoint sees carries the user and
`system:authenticated` only, without the `system:cattle:authenticated`
group and the principal extras the proxy adds, so a binding on those
groups decides differently on the two paths.

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
identity probe failed - dry-run may be denied, but `--apply` is still
expected to succeed.

## Composite commands

`inspect cluster` is a composite read: management identity + agent
connectivity (management Connected/Ready conditions; a disconnect warns) +
provisioning status/conditions (ready derived from the Ready condition),
machine pools, members (ClusterRoleTemplateBindings with display names
resolved), machines ([Cluster API](https://cluster-api.sigs.k8s.io/)
Machines with phase and first failing condition
message - surfacing provider errors invisible to other commands), and
node-join/import registration commands. Registration commands are withheld
by default - they contain join credentials; pass `--registration` to fetch
them, or use `get clusterregistrationtoken default-token --namespace
<cluster-id>`. The management read is mandatory (unknown/ambiguous cluster
or a failed read is a structured error, non-zero exit); provisioning,
members, machines, and registration sections degrade independently to
notes/warnings.

`inspect project` renders a sectioned text card (PROJECT, NAMESPACES,
MEMBERS, QUOTA, USAGE); members are ProjectRoleTemplateBindings with
display names resolved from Users/UserAttributes; the members section
degrades independently to a warning.

`inspect user <id|username|displayName|principal>` answers who a user is
and what roles they hold everywhere - never what they can actually do
(that is a permissions question, answered only by the server; no command
in this CLI computes it). Identity, group memberships (as last synced by
Rancher, refreshed at login), and roles at global/cluster/project scope
are joined into one document; a failed group-memberships read blocks the
role sections too, since their completeness cannot be verified without
knowing the user's groups.

## Access management

`grant` and `revoke` manage
[Rancher role bindings](https://ranchermanager.docs.rancher.com/how-to-guides/new-user-guides/authentication-permissions-and-global-configuration/manage-role-based-access-control-rbac)
at project, cluster, or global scope. `grant` auto-provisions unknown principals when the binding
applies - this is Rancher behavior, not a CLI choice. For both commands,
partial failures leave already-created (or already-deleted) bindings in
place. Omitting `--role` on `revoke` revokes every role at the scope.

`access can` and `access list` ask the cluster's own authorizer through
the Rancher proxy (SelfSubjectAccessReview, SubjectAccessReview,
SelfSubjectRulesReview); nothing is inferred from bindings. For
`access can`, `--as` names a user, a group (`--type group` or a group
principal), or `system:serviceaccount:<ns>:<name>`; for `access list`,
`--as` names a user or a service account only (no server operation
lists a group's rules; a group is refused with a validation error that
points at `access can`). A named user is impersonated when
your GlobalRoles allow it; otherwise the CLI asks a SubjectAccessReview
about the user with the group principals Rancher has cached for it. The
management cluster never impersonates: `access can --cluster local --as`
uses SubjectAccessReview and `access list --as` refuses there. Exit
codes: `can` 0 allowed, 12 denied, 13 unknown; `list` 13 when any
namespace answered incompletely or not at all. Every answer states its
source (`authorizer`, `rancherProxy`, `scopeDiscovery`, `request`,
`mixed`) and identity (`caller`, `impersonated`,
`subjectAccessReview`).

Every `access` command runs under one time budget for the whole call:
discovery, identity reads and every review share it, at most five
reviews in flight at once. The default is 60 s; `--timeout <duration>`
(for example `--timeout 5m`) sets it for that call. Running out of budget
is a terminal error naming the budget and the stage it ended in, and the
clusters or namespaces that had already answered are not reported; a
fleet-wide `access clusters` or `access projects` on a large installation
either gets a longer `--timeout` or a narrower question (`--cluster`,
`--project`).

Resource names resolve against the cluster's discovery, asked for in
aggregated form (two requests) and read group by group where a server
does not serve it. When the server could not describe every API group (an
aggregated API whose backend is down), the answer says so instead of
pretending: a bare name that one healthy group serves resolves and a
warning names the groups that were not consulted; a name no healthy
group serves, or one qualified with a failed group, is a retryable error
naming those groups, never "unknown resource"; a qualified name a healthy
group serves is exact and carries no warning.

`access who-can` answers the reverse question. It reads the subjects the
cluster's RoleBindings and ClusterRoleBindings name (plus
`system:masters`), posts one SubjectAccessReview per subject as you (per
namespace with `--project` or `--all-namespaces`), and lists who is
allowed; denied subjects are listed with `--show-denied`. A binding user
that is a Rancher user is asked with the group principals Rancher has
cached for it; a group is asked as itself and lists, under
`knownMembers`, the users Rancher has cached in it (users who have logged
in - groups are not expanded further); a username with no Rancher User is
asked with `system:authenticated` only, so an allowed answer is definite
and a denied one is reported as `unknown` ("groups unknown"), folded into
one text line and counted in `rollup.groupsUnknown`. Every Kubernetes
cluster binds such users (`system:kube-scheduler`, ...), which is why
they never affect the exit code. With several namespaces there is one row
per subject per namespace; rows the server attributes to the same
ClusterRoleBinding in every namespace collapse to `namespace: "*"`. Exit
13 when a row has a request issue or is unknown because the authorizer
returned an evaluation error; an allowed answer that also carries an
evaluation error stays allowed and raises a warning. Needs
`list` on rolebindings and clusterrolebindings on the cluster and `create
subjectaccessreviews` there; `rollup.outsideCandidateSet` names what no
row can show (`system:masters` members, non-RBAC authorizers).

`access explain` answers why. For one subject (you, or `--as`), one verb
and resource, and one namespace (or the cluster scope), it asks the same
review `access can` asks and renders the answer as a tree. On allow, the
root is the one binding the server's reason names, its role with the
rules covering the request (a display filter; the server chose the
binding), and the Rancher chain behind the binding read from Rancher's
own labels and annotations: the ClusterRoleTemplateBinding or
ProjectRoleTemplateBinding, its RoleTemplate and what it inherits, or the
GlobalRoleBinding and GlobalRole with its inherited RoleTemplates. Both of
Rancher's label grammars are followed (the default `rtb-owner-updated`
form and the `crtb-owner-<name>`/`prtb-owner-<name>` form of the
`aggregated-roletemplates` feature). On deny, the root lists the bindings
on the cluster naming the subject or its groups. The decision is the
server's and alone decides the exit (0, 12, 13); what the chain could not
establish is typed in `chainIssues` (`bindingRead`, `roleRead`,
`labelTraversal`, `managementRead`, `reasonParse`) with `chain:
partial|unknown`, printed after the tree, never as a warning. A caller
the downstream refuses its RBAC objects still sees the binding and role
from the reason and, under `Bindings held by the subject`, the
ClusterRoleTemplateBindings and ProjectRoleTemplateBindings it may read on
the management plane. A binding with no Rancher owner label or annotation
says so and nothing more; Rancher itself creates such bindings. A binding
whose label names a parent the caller could not match says that no
Rancher parent could be verified, and `chainIssues` names the label.

`access clusters` answers where Rancher lets a subject in. It lists the
management clusters you can see (read from Rancher's steve API, which
answers every caller with exactly the clusters their access grants; the
Kubernetes-style LIST through the proxy is refused to anyone who is not an
administrator) and, for each, whether Rancher's proxy admits you, or an
impersonated `--as` user, to the cluster's API: `reachable`, `refused`, or
`unknown` when the probe itself failed. Reachability is the proxy's answer
before any Kubernetes rule is consulted; what the identity may do inside a
cluster is `access list`'s question. The management cluster is always a
row: in caller mode your own identity review answers for it; for another
user it is `notApplicable`, because the management cluster does not
impersonate, and that row never makes the run incomplete. Beside each
row, GRANTED BY shows the management bindings that name the subject, its
principals, or a group Rancher has cached for it: ClusterRoleTemplateBindings
in the cluster's namespace, ProjectRoleTemplateBindings on its projects,
and GlobalRoleBindings whose GlobalRole reaches clusters (`admin`,
`restricted-admin`, or one with `inheritedClusterRoles`). Those reads have
their own completeness (`bindingsDiscovery`: `complete`, `partial`,
`unavailable`, with one issue per failed read) and never change the exit
code; a caller who cannot read the management plane, such as a member
asking about themselves, gets the reachability rows and `unavailable`
bindings. `groupAliases` says whether Rancher's cached groups could be
read; a match through a group is limited to what Rancher has cached.
`reachable[]` is the compact answer for a program: every reachable
cluster with its grants (RoleTemplates and GlobalRoles, `null` unless the
binding reads were complete). Exit 0 when every cluster answered, 13 when
any is `unknown`. `--as` takes a user (name, id, or principal); a group or a
service account is refused.

`access projects [--cluster <c>] [--as <user>]` answers which projects a
subject can see into and which of them it is a Rancher member of, and keeps
the two apart. The projects are what steve lists for you, grouped by
cluster, each cluster carrying its `access clusters` row; `--cluster`
restricts the run to one cluster of that set, by id or unique display name.
For every reachable cluster the command reads the namespaces you see
through steve (a project member cannot LIST namespaces through the
Kubernetes API, and steve answers every caller with exactly the namespaces
their access grants), groups them by the `field.cattle.io/projectId` label,
and asks the cluster's authorizer, as you or as the impersonated user,
whether it may get each namespace a project owns: that is the grant Rancher
gives every member of the project, and false for other projects'
namespaces. A project's `visibility` is the rollup of those answers:
`all`, `partial`, `none`, or `unknown` when a review failed, the project
owns no namespace, or the cluster's namespace read failed
(`namespaceDiscovery`); each namespace row keeps the server's reason
verbatim. Membership is a Rancher fact read from the management plane: a
visible project with a ProjectRoleTemplateBinding naming the subject is in
`memberOf.projects[]` with the RoleTemplates its bindings name. The set's
own `complete` is true only when every read the match depends on
succeeded: the ProjectRoleTemplateBinding LIST, the UserAttribute (the
cached group principals) and, when you ask about yourself, your own User
(its principal ids; a `--as` user arrives resolved with them). When one of
those failed, proven memberships stay, `complete` is false and their
`grants` are `null`. `memberOf` itself is `null` only when the
ProjectRoleTemplateBinding read failed ("membership not determined"). A
visible project reached only through ClusterRoleTemplateBindings or
GlobalRoleBindings is `inheritedAccess[]`, never a membership; that list is
`null` ("inherited access not determined") whenever the evidence for an
exclusive classification is incomplete: the membership reads above, or the
ClusterRoleTemplateBinding or GlobalRoleBinding LIST, failed. Its entries'
grants are `null` when a GlobalRoleBinding is among the bindings and the
GlobalRole read failed. A visible project with no Rancher binding at all
is noted as such, with the Kubernetes bindings the authorizer's reasons
credit when every reason parses, and a project whose binding the
authorizer denies everywhere is a disagreement row, not hidden. Namespaces
whose label names a project outside your set are listed per cluster and
never asked about.
A member asking about themselves gets visibility and no membership: the
binding reads are refused to them. Exit 0 when every cluster, project and
namespace answered, 13 when any is `unknown`.

## Diagnostics & support

`logs`/`support-bundle` cover only what Rancher's own Kubernetes API can reach.
`rancher-system-agent` is a node systemd unit, not a pod - its logs are
`journalctl -u rancher-system-agent` on the node itself, and generic pod
logs outside the curated targets stay with `inspect pod` and kubectl.
A log the server refuses is missing evidence, not an absent artifact: the
target's `manifest.json` item becomes an `error:` status naming the pod and
container, the result's `errors` count includes it, and a warning names
it, so `errors: 0` means every requested log is in the archive. A
previous-container log that does not exist stays silent (the container
never restarted); a refused one is a warning.
`support-bundle` is deliberately the API-reachable half of a two-bundle
story: pair it with
[rancherlabs/support-tools](https://github.com/rancherlabs/support-tools)'
node collector for the
node-level half (journald, the system-agent unit, OS state) that no API
call can reach.

## Output formats

`-o`/`--output`: `table` (default, for users) · `wide` (table plus any
Wide-marked columns - presentation, never a scope change: a payload with no
wide-only columns renders identically to `table`) · `json` · `yaml` ·
`name` (`.metadata.name`/ids only) · `template=<go-template>` - a Go template
over the JSON view of `data` (same shape as `-o json`). Any other value is
rejected with a `VALIDATION` error before the command runs. Every kind
declares its own table projection; there is no pretty-JSON fallback under
`table`/`wide` - a kind that did not implement it would fail with a
rendering error instead of silently dumping JSON, so use `-o json` for the
versioned envelope. Some commands (e.g. `logs`) render a plain-text default
under `table`/default format instead, with warnings on stderr as
`# warning: <w>` lines; `-o json`/`yaml`/`template=`/`name` still see the
normal envelope over `data`. Every text format - `table`, `wide`, `name`,
the text of composites and `access`, the `# warning:` and `error (...)`
lines on stderr, and the dry-run preview - strips control characters
(including ANSI escape introducers) and Unicode format characters from
server-sourced text before it reaches the terminal, so a description or
condition message cannot forge a row or drive the terminal. Three outputs
are verbatim by design and are untrusted content when echoed: `json`,
`yaml`, and `template=` (the bytes are the data), and `logs` (container
output whose newlines are its structure). Structured output (json, yaml, and template)
omits `metadata.managedFields` (kubectl-parity; no opt-out flag - read the
raw API for
[server-side-apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
forensics).

Five commands are exceptions to the versioned-envelope rule, all for the
same reason: their consumer parses a specific format, not the envelope.
`completion`, `kubeconfig`, and `create kubeconfig` reject `-o json`/
`-o yaml` outright (a table-only `OutputPolicy`) - a shell completion
script and a kubeconfig YAML file have their own formats to satisfy.
`auth get-token` and `token` accept `-o json` but emit a raw Kubernetes
`ExecCredential` object with no envelope wrapper, verbatim to what
kubectl's exec-credential plugin protocol requires. `rancher schema
-o json`'s per-command `outputPolicy` field is the authoritative
machine-readable source for which formats any given command accepts.

## Color

`--color`: `auto` (default) · `always` · `never` - controls whether
`table`/default/`wide` text (the header row, the `error (CODE):`/`hint:`
prefixes, a once-only credential's shown-once line) is styled with ANSI
escapes; `json`, `yaml`, `name`, and `template` output never carries styling
regardless of this flag. Precedence, checked in order: `--color=never` or
`--color=always` always wins, overriding every environment variable. Under
`auto`, `NO_COLOR` set to any non-empty value turns styling off; otherwise
`CLICOLOR_FORCE` or `FORCE_COLOR` set to a non-empty value other than `0`
turns it on. If none of those apply, styling follows whether the stream
being written to (stdout or stderr, resolved independently) is a real
terminal.

## Error codes

Every error the CLI reports carries one of these codes in
`data.error.code` (and in the `error (CODE):` text prefix) and exits with
the status shown. The set is stable: `rancher schema --all -o json` lists
it under `errorCodes` with the same exit and meaning, and this table is
tested against that manifest. Whether a particular error is worth
retrying is a property of the error, not of its code, and travels in the
envelope's `retryable` field; the `hint`, when present, is the remedy.

<!-- conformance:error-codes -->
| Code | Exit | Meaning |
|---|---|---|
| `NOT_LOGGED_IN` | 2 | no server is selected, the selected server entry does not exist, or it has no stored credential and RANCHER_CLI_TOKEN is not set |
| `CONFIRMATION_REQUIRED` | 2 | the mutation was not applied because a required gate flag (--apply, --yes, --allow-deprecated) was missing; the message names the flags |
| `DRY_RUN_NOT_APPLICABLE` | 2 | --dry-run was given to a command that does not mutate |
| `AMBIGUOUS_SELECTION` | 2 | more than one value matched and none was selected; the error lists the candidates when it can |
| `CONFLICT` | 5 | a write collided with existing state: a resource changed on the server since it was read, a name already exists, a server entry is registered with a different URL or CA, or the config directory changed while the command ran |
| `LOCK_TIMEOUT` | 8 | a config-directory lock was not released within the wait, usually because another rancher process is writing |
| `AUTH` | 3 | authentication failed: the server rejected the credential (HTTP 401), the exchange with the identity provider failed or timed out, or an environment URL was set without RANCHER_CLI_TOKEN |
| `CREDENTIAL_REPLACED` | 3 | the stored credential for the server entry was replaced by another config directory that shares this one's identity, or by a login in progress |
| `LOGIN_INCOMPLETE` | 3 | a login for the server entry is in progress, or ended before it recorded its credential identity |
| `CREDENTIAL_TRANSITION_INCOMPLETE` | 3 | a change of this directory's credential store has work left: some credentials are still in the backend it moved away from, so a second store change is refused and so is a login for an entry whose credential the change is still carrying; a logout or a delete proceeds and converts the unfinished work to cleanup |
| `NOT_FOUND` | 4 | no such resource, server entry, or name: the server returned HTTP 404, the config directory has no such entry, or the name matched nothing in already-fetched data |
| `RBAC` | 6 | the server denied the request for lack of permission (HTTP 403), including Rancher's proxy rejecting an invalid or insufficient token |
| `VALIDATION` | 2 | the request was refused before or by the server: a missing or invalid flag or argument, a mutation under read-only mode, a disabled feature, or an object the server refused (HTTP 400 or 422) |
| `PRIME_REQUIRED` | 9 | the command requires Rancher Prime and the connected server does not have it |
| `TLS_TRUST` | 10 | the server's TLS certificate is not trusted and was not accepted |
| `CAPABILITY_UNSUPPORTED` | 11 | the connected server does not support the workflow the command needs |
| `INTERRUPTED` | 130 | the command was ended by Ctrl-C or a termination signal |
| `GENERIC` | 1 | an error with no more specific code, including a server or transport failure |
<!-- /conformance:error-codes -->

## Machine hints

An error's `data.error.hint` field, when present, is exactly one
paste-valid command targeting POSIX sh, carrying only concrete,
already-known values - never prose, a `<placeholder>`, an `[optional]`
flag, or an "X or Y" alternative. Guidance that needs a value the CLI
does not have (an unknown URL, a name that does not exist) goes in the
message body instead, with no `hint` field at all: the field's absence is
itself informative - nothing here is safe to run as-is. Agents may
execute a present hint after the same inspection any other emitted text
gets before being treated as argv. Windows agents adapt a runnable hint
themselves - the same posture as the rest of the machine contract.

## Read-only mode

Three sources, OR'd together, put a session into read-only mode, refusing
every mutating command before any network call - strictly one-way, no
per-invocation override exists:

- Config: `readOnly: true` (top-level config field).
- Env: `RANCHER_CLI_READONLY`.
- Flag: `--read-only` (global).

> [!IMPORTANT]
> `RANCHER_CLI_READONLY` follows non-empty-means-set - the literal string
> `"false"` still turns read-only mode ON. Every `RANCHER_*` variable in
> this CLI behaves this way.

Refusal is `VALIDATION`, naming the command and the active source(s) (e.g.
"enabled by RANCHER_CLI_READONLY") - the one-way rule means you have to know
which knob to turn off. `rancher schema -o json` reports the resolved state
(`readOnly`, `readOnlySources`) so an agent can check before trying.

Scoping, stated honestly: read-only mode refuses `Mutating` commands only -
the CLI's managed-resource surface. `auth login`, `auth status`, and local
`config` commands stay usable; otherwise a read-only session couldn't even
establish itself.

> [!WARNING]
> Read-only mode is a CLI-side courtesy layer, not a security boundary.
> Real enforcement is a Rancher token bound to read-only RBAC - pair them.

`--dry-run` of a mutating command still sends write-verb requests
server-side (admission dry-run); a genuinely read-only RBAC token cannot
dry-run at all. Read-only mode doesn't special-case this - it refuses
mutating commands outright, `--dry-run` included.

## Supported server version

The CLI declares a minimum supported Rancher version, `>= 2.14`
(`rancher schema -o json` → `minRancherVersion`). Enforcement today is
warn-only: `auth login` and `auth status` call the unauthenticated
`GET /rancherversion` endpoint and emit a warning - never a failure - when
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
name/kind/provider/expiry/`current` marker - for both `ext.cattle.io` tokens
and management.cattle.io/v3 tokens; `token: null` with a warning appears only
on a probe failure such as a revoked/rotated token). Any of these fields
degrading to `null` with a warning is expected, not a bug - including
`identity` under a read-only-RBAC token, since the identity probe is itself a
create-verb request. `auth status`'s connectivity result (`ok`) never depends
on them.

## When login fails after a token was created

A provider sign-in creates tokens on the server before anything is stored
locally. If the login then fails, the CLI revokes what it created and
reports the outcome in the error's `details`:

- `stage`: `authentication` (before the credential was ready), `commit`, or
  `clusterToken` (the kubectl plugin's own step, creating the first
  cluster-scoped token from a login credential it just created).
- `sessionToken` / `derivedToken` / `clusterToken`: each token's `outcome`
  (`known`, `unknown` when the server may hold a token the CLI never saw,
  `unidentified`, `notCreated`), its resource name, and its `cleanup`
  result. `clusterToken` is the plugin-created login's cluster token, revoked
  with the login credential as bearer.
- `credentialRecovery`: what the local credential store may still hold and
  the command that clears it (`rancher auth logout <name>`, which removes
  the credential from every backend).
- `configRecovery`: an entry this login wrote could not be restored;
  `command` removes it and `prior` is its complete earlier state (absent
  when the login created the entry).
- `operationMarker`: the tokens a password or device-code sign-in creates
  carry `[op:<marker>]` in their description, so a leftover can be found
  by listing the collection its `kind` names and revoked with the
  `recovery` command.

A leftover derived token is always a `token`. A leftover session token is
whatever kind Rancher created it as: `management-token` (a deprecated
kind: deleting it needs `--allow-deprecated`) or, on a Rancher that has
moved session tokens to the new API, a `token`; the failure details say
which (`sessionToken.ref.kind`, or `sessionToken.kind` when the name is
unknown). When the sign-in response was lost, the kind is unknown too and
`recovery` names both collections, so the marker search cannot miss the
token. A pasted or piped token is yours and is never revoked.

A browser (SAML) sign-in is different: Rancher creates the session token
from the browser, so it carries no marker. When the CLI learned its name
from the sign-in, `recovery` gives the exact `management-token` command.
When it did not, `sessionToken.search` carries what the CLI knows: the
auth provider, when the browser was opened (`handoffAt`), when the CLI
stopped waiting (`pollEndedAt`, which is not a bound: the browser sign-in
can complete later), and, when the server declared one, the request expiry
(`requestExpiresAt`). Those facts narrow a manual check of
`rancher get management-tokens -o json` or the UI's Account & API Keys
page; they cannot identify the token.

## kubectl integration

`rancher auth get-token` is a `client.authentication.k8s.io/v1beta1`
[exec-credential plugin](https://kubernetes.io/docs/reference/config-api/client-authentication.v1beta1/). Rancher-generated kubeconfigs invoke it to create
short-lived Kubernetes credentials; you don't run it directly. Older
kubeconfigs generated by the legacy Rancher CLI (which call `rancher token`
instead) also work after a binary swap - the compatibility shim delegates to
the same machinery. Flags: `--server`,
`--cluster` (required), `--name` (cache slot, keyed per name+server+cluster; omitted = no
caching), `--cacert`,
`--auth-provider`, `--auth-flow`. When `--server` or `--cacert` are
omitted, they fall back to the current server in config. It supports SAML,
OAuth (device-code / auth-code), and basic auth flows; long-poll flows can
be canceled with Ctrl-C. List cached credentials with `rancher auth list-tokens`.
Clear them with `rancher auth delete-token [<name>]`. That clears the local
cache only: the cluster token the plugin created stays valid on the server
until it expires; `rancher get tokens` lists it and `rancher delete token
<name>` revokes it (on a server without `ext.cattle.io` tokens the plugin
creates a v3 token instead: `rancher get management-tokens` and `rancher
delete management-token <name>`). On a cache miss the plugin first
tries to create a token from the stored `auth login` credential for the matching
server; when that credential lives in the OS keychain, this can trigger a
keychain-unlock prompt - CI and other non-interactive runs should use
`RANCHER_CLI_CREDENTIAL_STORE=file` or a warm `--name` cache.

## Token reuse for kubectl (opt-in)

By default every kubeconfig cluster logs in on its own: a cache miss for a
cluster the CLI has no stored `auth login` credential for runs an
interactive login, and the token that login creates is scoped to that
cluster. `rancher config set-token-reuse on` (config key `tokenReuse`,
default off) changes the miss path: the first miss for a server runs one
interactive login, stores the resulting unscoped login credential under a
server entry named `exec-login-<host>` (with the port appended when the URL
names one) whose `provenance` is `execLogin`, and creates a cluster-scoped
token from that credential for every other cluster of the same server
without prompting. Credentials you created with `rancher auth login` are
never touched, read differently, or removed by the setting.

The credential this stores is unscoped: it reaches the whole management
plane, not one cluster, and it lives as long as the server allows: the
login sends no TTL, so the server applies `auth-token-default-ttl-minutes`
clamped by `auth-token-max-ttl-minutes`. When the credential store is the
plaintext file, the CLI says so before the login runs, naming the file the
credential will be written to. This setting is a client-side convenience,
not an enforcement boundary: it decides what this CLI does on a cache miss,
and nothing about what the stored token can do on the server.

`rancher config set-token-reuse off` restores the per-cluster login and
deletes nothing. Its output lists every plugin-created login left dormant
and the two commands that remove each: `rancher auth logout <entry>`, then
`rancher config delete-server <entry>`.

`rancher config status` lists, under `servers`, every entry whose CLI-created
tokens are still live on a server: its provenance, the token's owner and
expiry, and, for the entry's current token and for each superseded record
it still names, a four-command recovery sequence that logs in as a
throwaway entry, selects it, revokes the token by name, and logs out again.
Every text rendering of the sequence ends with what it assumes:
`RANCHER_CLI_URL` and `RANCHER_CLI_TOKEN` are unset, and either would
override the current server the sequence selects. `rancher config delete-server`
returns the same account under `recovery` in the invocation that removes the
entry, so the last reference to a live token is not lost with it.

Two `rancher` processes cannot log in to one server at once: the plugin
login holds a per-server lock across the whole interactive wait. A
concurrent kubectl blocked behind it waits up to 60 seconds and then fails
with `LOCK_TIMEOUT` (exit 8), which is retryable: the message names the
server being logged in to; retry once that login completes.

## TLS & proxy

Server connections verify TLS against the configured CA (`--cacert` /
`RANCHER_CLI_CACERT`) or the system trust store by default - verification is never
disabled implicitly. HTTP(S) proxies are honored from the environment.

Trust is scoped to the Rancher server's origin. The configured CA (and the
legacy shim's `--skip-verify`) applies only to requests aimed at that
scheme, host, and port. An identity provider reached during an OAuth login
is verified against the operating system trust store, and its endpoints
must be `https`; `--cacert` cannot make a self-signed provider trusted. A
redirect to a different origin is never followed: the request fails with a
`GENERIC` error naming both origins, because a redirected request would
carry credentials to a server the configured trust was never meant to
cover.

There is no skip-verify flag anywhere in the native surface, ever: a
self-signed or private-CA server is used by pinning its CA (`--cacert` or the
stored server entry's CA), never by turning verification off. The one
exception is the legacy `rancher token` shim, whose `--skip-verify` exists
only because Rancher-generated kubeconfigs pass it verbatim - legacy
compatibility, not an endorsement; kubeconfigs produced by this CLI never
emit it.

Proxying is environment-only - there is no `--proxy` flag, matching
kubectl's posture. Three variables control it, each also honored in
lowercase (`https_proxy`, `http_proxy`, `no_proxy`); when a variable is
set in both cases with different values, the uppercase one wins and the
lowercase one is silently ignored - worth checking first when a proxy
setting seems to have no effect, since shell profiles usually set the
lowercase form:

- `HTTPS_PROXY` - the proxy used for `https://` server connections (every
  Rancher server this CLI talks to).
- `HTTP_PROXY` - the proxy used for `http://` server connections.
- `NO_PROXY` - a comma-separated list of hosts to reach directly: exact
  hostnames, domain suffixes (`foo.com` also matches `bar.foo.com`; a
  leading dot restricts it to subdomains only), IP addresses, CIDR blocks,
  optionally with a literal port, or a bare `*` to disable proxying
  entirely.

An `https://` connection routed through a proxy is tunneled with `CONNECT`:
the proxy never terminates TLS, so the pinned CA (or system trust store)
still verifies the real server's certificate end to end. A loopback target
(`127.0.0.1`, `::1`, or `localhost`) is never proxied, proxy settings
notwithstanding - the same rule Go's standard library applies everywhere.
If the proxy itself is unreachable, the error names the proxy's address so
it reads distinctly from a failure to reach the Rancher server itself.
