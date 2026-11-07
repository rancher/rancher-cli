# Operational reference

Part of the [Rancher CLI docs](README.md) · [command reference](COMMANDS.md)

This reference expands [README.md](README.md) with the full operational detail.

- [Platforms & credential stores](#platforms--credential-stores)
- [Environment variables](#environment-variables)
- [Install internals](#install-internals)
- [Shell completion](#shell-completion)
- [Cluster scope (--cluster)](#cluster-scope---cluster)
- [Project scope (--project)](#project-scope---project)
- [Kinds & verbs](#kinds--verbs)
- [Cluster provisioning](#cluster-provisioning)
- [Composite commands](#composite-commands)
- [Access management](#access-management)
- [Diagnostics & support](#diagnostics--support)
- [Output formats](#output-formats)
- [Color](#color)
- [Error codes](#error-codes)
- [Machine hints](#machine-hints)
- [Read-only mode](#read-only-mode)
- [Supported server version](#supported-server-version)
- [When login fails after a token was created](#when-login-fails-after-a-token-was-created)
- [Revoking the CLI's tokens](#revoking-the-clis-tokens)
- [Auth configs (`--auth-provider`)](#auth-configs---auth-provider)
- [kubectl integration](#kubectl-integration)
- [Token reuse for kubectl (opt-in)](#token-reuse-for-kubectl-opt-in)
- [Usage recording](#usage-recording)
- [Experiments](#experiments)
- [TLS & proxy](#tls--proxy)
- [What the CLI sends to the server](#what-the-cli-sends-to-the-server)

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
directory defaults to `auto`: its first login probes the keychain and
pins the answer. On macOS, a keychain that is locked - its prompt was
cancelled, or not answered within the probe's 5 seconds - refuses the
login with `CREDENTIAL_STORE_UNAVAILABLE`; the login stores no
credential and pins nothing: unlock the keychain with `security
unlock-keychain` or approve the prompt and run the command again, or run
`rancher auth login <name> --credential-store file` to choose the file
store on purpose (the kubectl plugin's own login names only the first,
since neither it nor `auth login` on its entry takes that flag). On other
systems, and for any other failure such as no keychain session on a
headless machine or in CI, the first login pins the file store. A
directory that already holds credentials is not decided by the probe:
one whose credentials are in the file store keeps it, and one whose
credentials are in the keychain or a helper is refused with
`CREDENTIAL_STORE_UNAVAILABLE` while the keychain cannot be used.

Changing a directory's store is `rancher auth login <name>
--credential-store keychain|file|auto|exec:<name>`. The flag names the
store outright, so it is not weighed against the declaration - changing
that is the point - but it must agree with
`RANCHER_CLI_CREDENTIAL_STORE` when that is set, or it is the same
VALIDATION error. The new store is recorded only after this entry's new
credential has been stored in it, so no process resolves the entry to a
store that does not hold its credential yet. A change takes only this
entry's credential, in every direction: while another entry still holds
one in the store being left - a credential, a login that never finished,
a keychain slot another directory now owns - the change is refused before
the sign-in, naming those entries and the `rancher auth logout` to run
first, so no entry's store changes unless it was logged in there itself.
A credential another entry has left in the store being moved to counts
too - a keychain copy kept by a logout that could not reach the
keychain, say - since every reader moves there with the change and would
be served it: log that entry out, with the keychain reachable for a
keychain copy. A change into a credential helper is refused while the
directory has any other entry, since the helper may answer for any of
them with a credential it created: keep the directory where it is and
use a different `--config` for the helper, or remove those entries with
`rancher config delete-server`.
A store that cannot be read at that moment refuses too - a keychain that
cannot be read fails as `CREDENTIAL_STORE_UNAVAILABLE` - and its remedy
is to make it reachable or use a different `--config`, since a logout
would have to reach the same store. A credential helper can answer for any
entry, so leaving a helper is refused while the directory has any other
entry: keep the directory on the helper and use a different `--config`
for the new store, or remove those entries with `rancher config
delete-server`. A variable naming a store other than the one the
credentials are in is a change asked for by name, like the flag.
`--credential-store auto` never moves a keychain or helper directory into
plaintext either: when the keychain cannot be used at that moment, the
login is refused, and naming `file` is the way to choose plaintext on
purpose. An `auto` login that could use the keychain on a directory whose
other entries are in the file store, or have a credential left in the
keychain, stays on the file store and says so: log those entries out,
then log in again, and the next login moves the directory to the
keychain. An unfinished change is `CREDENTIAL_TRANSITION_INCOMPLETE`:
its message names the step that could not finish and why; resolve that,
then run the command again. Every `auth login`, `auth logout`, `config
delete-server` and `config update-server --url` retries what the change
left before its own work, and only a second store change is refused while
some of it remains. With work left from an earlier change, a login that
changes the store is not refused before its sign-in on that work or on
the store rules; the retry under the lock decides first. `rancher config
status` shows the change as `credentialStoreChange`, with each piece of
work left and why it stopped.
A change record that cannot be read refuses all of those commands, logout
included, until it is restored or moved aside: it is the only record of
the old copies left to delete. While a change is unfinished, an error
whose remedy is a login or logout - "not logged in", credentials in both
stores, the kubectl plugin with no terminal - names the change in its
message; while the record config.yaml names cannot be read, its hint is
`rancher config status` instead, and the remedy stays in the message.
`config edit` refuses to set, change or remove
`credentialStore`: any of the three would send every reader to a store
the edit put nothing in. Change it with the login above; no command
removes a declaration, and a different `--config` starts a fresh
directory. It also refuses a change of `tokenReuse`, which a cached
kubectl credential is judged against: use `rancher config set-token-reuse
on` or `off`. An edit that changes whether usage recording is on is refused
too, because only `rancher telemetry off` deletes what is recorded: use
`rancher telemetry on` or `off`.

A directory whose backend is the keychain reads only the keychain: on a
headless machine (no D-Bus, locked keychain) every command fails with the
way out rather than falling back to a file-stored token, so a headless
machine wants `RANCHER_CLI_CREDENTIAL_STORE=file` from the start, before
the first login records it.

The exec-credential cache follows the same rule. Every record names the
directory that created it and the server entries its lookup read, each with
its cache binding id, so a copied config directory never serves the
credentials its source cached, and a login, a logout, a URL change or a
`config delete-server` makes every record of the previous credential a
miss at once (which records each change ends is under "kubectl
integration" below). While such a change runs, the entry's cache writes are
paused and each kubectl call for it makes a fresh token rather than
caching one. A pause that nothing still holds - a change that died or
failed before its last write - is cleared by the next cache miss that can
take the config lock. One held by a store change's pin step waits for the
next `auth login`, `auth logout`, `config delete-server` or `config
update-server --url`, each of which finishes the change; one made with
`RANCHER_CLI_CONFIG_NO_LOCK` waits for the entry's next login or logout;
and an unreadable change record holds every pause until it is restored or
moved aside. `rancher config status` lists every
paused entry, with the reason, as `cacheWritesPaused`. A
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
recorded by its first login; `RANCHER_CLI_CREDENTIAL_STORE` chooses the
store a command on a directory that has not made one reads and a login
writes, and disagreeing with one that has is an error rather than an
override. Under `auto` the value resolves through a machine-local
`credstore-pin` file, and only a login writes it: the directory's first
login probes the keychain and pins the answer - the file store when the
probe fails, except that on macOS a locked keychain (its prompt cancelled
or not answered within the probe's 5 seconds) refuses the login with
`CREDENTIAL_STORE_UNAVAILABLE`, which stores no credential and pins
nothing - and a later login
re-probes only to upgrade file to keychain - and only while no other
entry's credential is in the file store. No other command probes or pins.
With no pin, a command reads where the entries' stored credentials are:
the keychain when an entry has an identity record in `credstore-id/`, the
file store when only `credentials.yaml` holds them, and nothing when
neither does. Credentials in both with no pin are a VALIDATION error for
every credential command, a login included, because nothing says which
copy is current: log out the entries in the store you do not want to
keep - to keep the keychain, which holds no plaintext, log out the ones
the error lists in the file - or use a different `--config`; a keychain
entry's logout needs the keychain reachable. Only credentials of entries
in `config.yaml` count. A command that creates an entry - the kubectl
plugin's login, `auth login --url` for a new name, `config add-server` -
first removes any credential still stored under that name in the
keychain, the file store, or any credential helper this directory's
records (`credhelper-provenance.yaml`) name for that entry name, with a
warning naming each removal: from the moment the entry exists it would
count as the entry's credential, perhaps a bearer another server issued.
One in a keychain that cannot be reached, or in a recorded helper that is
not on `PATH`, refuses the command (a login before its sign-in), and one a
recorded helper cannot erase refuses it, with nothing more written; make
the helper available or working and run the command again, use a
different `--config`, or remove the helper's record for that name from
`credhelper-provenance.yaml` when the helper is gone for good. A keychain that `auto` chose
and the process cannot use is a hard error (never a silent downgrade to
plaintext) whose way out is making the keychain reachable or a different
`--config`. A login writes the pin as the last step of the change it
records, after its credential is stored, so a login that fails or is
cancelled leaves the pin and every other entry's credential exactly as
they were. The exceptions are written before the credential is stored,
and state what readers already use: a directory with no pin that a login
leaves gets the store readers find now, and a pin left behind under an
explicit declaration that names a store the directory does not use is
rewritten while no command reads it.

Custom backends plug in as credential helpers: `credentialStore: exec:<name>`
runs the `rancher-credential-<name>` binary from `PATH` (the shipped
`rancher-credential-vault` covers HashiCorp Vault and OpenBao). Protocol and
examples: [CREDENTIAL_HELPERS.md](CREDENTIAL_HELPERS.md).

## Environment variables

The Flag column names the flag for the same setting, on the commands that
take it; `-` means no flag. A variable and its flag relate in different ways:

- Config directory: `--config` > `RANCHER_CLI_CONFIG_DIR` >
  `$XDG_CONFIG_HOME/rancher` > the platform's default, see
  [Platforms & credential stores](#platforms--credential-stores).
- `--server`, `--cacert` and `--auth-flow` take precedence over their
  variables on the commands that read both. `config add-server` and
  `config update-server` take `--cacert` and never read `RANCHER_CLI_CACERT`;
  see [Which CA a command trusts](#which-ca-a-command-trusts).
- `--credential-store` (on `auth login`) must agree with
  `RANCHER_CLI_CREDENTIAL_STORE`; a difference is refused. The variable never
  overrides the store a directory has declared.
- Any one of `RANCHER_CLI_READONLY` (any non-empty value, `0` and `false`
  included), `--read-only` and the `readOnly` config key turns read-only mode
  on; none of them turns it off. Local commands refuse a typed `--read-only`.
- `--color=always` and `--color=never` take precedence over the color
  variables; under `--color=auto` the variables decide.
- `RANCHER_CLI_TELEMETRY` set to `true` or `false` takes precedence over the
  `telemetry` config key; any other value is ignored. It has no flag.
- `RANCHER_CLI_EXPERIMENT` lists experimental settings. Only a development
  build reads it; a release build ignores it. It has no flag. See
  [Experiments](#experiments).
- `--debug` has no variable. In a development build, a run that attempts a
  telemetry upload prints the lines about its requests on stderr; see
  [Experiments](#experiments).

| Var | Flag | Effect |
|---|---|---|
| `RANCHER_CLI_URL` | `--server` | server URL |
| `RANCHER_CLI_TOKEN` | - | token, used directly and never persisted |
| `RANCHER_CLI_CACERT` | `--cacert` | CA bundle (PEM), a path or the PEM text; which commands apply it is in [Which CA a command trusts](#which-ca-a-command-trusts) |
| `RANCHER_CLI_CREDENTIAL_STORE` | `--credential-store` | `keychain` \| `file` \| `auto` \| `exec:<name>`; chooses the store for a directory that has not declared one, and must not disagree with one that has (a different value is a VALIDATION error, not an override); a keychain directory reads the keychain only, so set `file` where no keychain is reachable. `rancher kubeconfig` sets it in the exec entry: empty for a directory that declares its store, else the value set when it ran |
| `RANCHER_CLI_CONFIG_DIR` | `-c/--config` | config directory; `rancher kubeconfig` sets it, absolute, in every exec entry it writes |
| `XDG_CONFIG_HOME` | - | base of the default config directory, `$XDG_CONFIG_HOME/rancher`, used when neither `--config` nor `RANCHER_CLI_CONFIG_DIR` is set |
| `RANCHER_CLI_OAUTH_AUTH_FLOW` | `--auth-flow` | OAuth flow: `devicecode` \| `authcode` |
| `RANCHER_CLI_OAUTH_CALLBACK_PORT` | - | local callback port for the `authcode` flow |
| `CATTLE_OAUTH_AUTH_FLOW` | `--auth-flow` | older name of `RANCHER_CLI_OAUTH_AUTH_FLOW`, read when that one is empty |
| `CATTLE_OAUTH_CALLBACK_PORT` | - | older name of `RANCHER_CLI_OAUTH_CALLBACK_PORT`, read when that one is empty |
| `RANCHER_CLI_TELEMETRY` | - | `true` or `false`; turns usage recording on or off and overrides the `telemetry` key of config.yaml, see [Usage recording](#usage-recording); empty counts as unset and any other value is ignored |
| `RANCHER_CLI_EXPERIMENT` | - | experimental, development builds only: a comma-separated list of `name` or `name=value` items, see [Experiments](#experiments); `telemetryupload=<url>` makes the build send the stored events of finished hours to that endpoint at most once an hour, after a recorded run, never from the kubectl exec plugin |
| `RANCHER_CLI_READONLY` | `--read-only` | when non-empty, every run refuses commands that make changes; local commands stay usable; see [Read-only mode](#read-only-mode) |
| `NO_COLOR` | `--color` | when non-empty, turns off styling under `--color=auto`; see [Color](#color) |
| `CLICOLOR_FORCE` | `--color` | when non-empty and not `0`, turns on styling under `--color=auto` unless `NO_COLOR` is set; see [Color](#color) |
| `FORCE_COLOR` | `--color` | same as `CLICOLOR_FORCE`; see [Color](#color) |
| `RANCHER_CLI_CONFIG_NO_LOCK` | - | does not take the locks that stop two CLI processes from writing the config directory at the same time; use it only on a filesystem with broken file locking |
| `RANCHER_CLI_COMPLETION_DEBUG` | - | when non-empty, traces each phase of a shell completion request to stderr; stdout is unchanged |
| `VISUAL` | - | editor that `config edit` runs, read before `EDITOR` |
| `EDITOR` | - | editor that `config edit` runs when `VISUAL` is empty |

Scope is per-invocation: `--cluster` routes to a downstream cluster; `--project`
names a project on it, for the commands that take it. No scope is persisted
or implied - see [Project scope](#project-scope---project).

### Which CA a command trusts

A flag beats `RANCHER_CLI_CACERT`, which beats the CA saved on a server entry;
with none of them the system roots verify the server.

| Command | Connects with | Saves |
|---|---|---|
| `auth get-token`, `rancher token` | `--cacert` (`--cacerts`), then `RANCHER_CLI_CACERT`, then the CA of the server entry the call identifies or whose URL matches, then system roots; `rancher token --skip-verify` uses none | nothing |
| ordinary commands | `RANCHER_CLI_CACERT`, then the current server entry's CA (dropped when `RANCHER_CLI_URL` names another server), then system roots | nothing |
| `auth login` (with or without `--url`) | `--cacert` (with `--url` only), then `RANCHER_CLI_CACERT`, then the entry's saved CA, then system roots | `--cacert`, or a certificate accepted at the trust prompt; never the variable (an entry left without a CA after a login that used it carries `caFromEnvironment: true`) |
| `auth logout`, `auth delete-token`, `config delete-server` (revokes) | the entry's saved CA; `RANCHER_CLI_CACERT` only for an entry `auth login` saved without a CA because that variable supplied it; otherwise system roots | nothing |
| `config add-server`, `config update-server` | `--cacert`, else (`update-server`) the saved CA it keeps; the variable is not read | `--cacert` |
| the exec plugin's own sign-in (token reuse on) | the same order as `auth get-token` | the CA it connected with, the variable's included (none under `--skip-verify`), in its `exec-login-*` entry |

`RANCHER_CLI_CACERT` is not applied to a command that names a server other
than the one `RANCHER_CLI_URL` names: a variable exported with
`RANCHER_CLI_URL` is for that server. A variable exported alone applies to
every command that connects to a server, so a call that names another
server with its own private CA fails TLS verification; pass `--cacert` there.

`auth login` never saves the variable's CA, so a later command run without
the variable connects with the entry's saved CA, or with none. It warns when
the entry it leaves has no saved CA, or one that differs from the variable,
and the warning gives the `config update-server <name> --cacert <file>`
that saves the CA. A login that connected with the variable never offers the
trust prompt, and its unknown-authority error names the variable.

`caFromEnvironment` is a field of a server entry in `config.yaml`. It is
`true` on an entry with no `ca` whose last `auth login` connected with
`RANCHER_CLI_CACERT`; the field records that fact, never the variable's
value. It is never set on an entry that has a `ca`. A login that connects
without the variable clears it, and so does every change that sets or clears
the entry's CA or URL (`config update-server --cacert`, `--clear-cacert`,
`--url`, an accepted trust prompt).

The exec plugin's own sign-in saves the CA it connected with because later
`kubectl` calls may run without the variable. Revokes use the variable only
for an entry with `caFromEnvironment: true`: a variable set for one server must
not stop revokes on others, and an entry with no saved CA and no
`caFromEnvironment` names a server the system roots verify.

## Install internals

`install.sh` unpacks each release into its own versioned directory under
`~/.local/share/rancher-cli` and activates it by atomically switching one
`current` symlink. The `rancher` and `rancher-credential-vault` links in the bin
directory point through `current`, so an upgrade updates both binaries in a
single switch - the two binaries can never end up on different versions. On a
fresh install the installer creates the two links in the bin directory one after
the other; if it is stopped between them, the `rancher-credential-vault` link
can be missing, and running the installer again creates it.
An install receipt at `~/.config/rancher-cli/install-receipt.json` records what
is installed; running the installer again replaces the links it created without
a message, and prints a notice when it replaces an existing file or link it did
not create.

`install.ps1` keeps the same layout under `%LOCALAPPDATA%\rancher-cli`: each
release is unpacked into `versions\<tag>\` as `rancher.exe` and
`rancher-credential-vault.exe`, and the `bin` directory (the one to put on
`PATH`) is a Windows directory link (NTFS junction) to the current version.
Unlike a symbolic link, this directory link needs no admin rights and no
developer mode. To switch versions, the installer creates a new directory link
next to the old one and renames it over the old one in one step: both binaries
change together, and if the installer is stopped at any point, either the old or
the new version of both binaries is in place. The receipt lives at
`%APPDATA%\rancher-cli\install-receipt.json`. `-BinDir` sets where the directory
link goes; the directory it names must be new, be empty, be a Windows directory
link (NTFS junction), or hold only a `rancher.exe` and
`rancher-credential-vault.exe` installed by hand; the installer replaces those
files, or a directory link it did not create, with a notice. Unlike
`install.sh`, which links each binary into a shared directory such as
`~/.local/bin`, `-BinDir` on Windows must name a dedicated directory: the whole
directory becomes the Windows directory link to the current version.

Checksums (`SHA256SUMS`) are always verified. Build provenance is
verified when a `gh` CLI that is logged in is available, and
`--verify-attestation` makes that verification mandatory instead of
best-effort.

GitHub token lookup order: `GH_TOKEN` > `GITHUB_TOKEN` > `gh auth token` (when
`gh` is on PATH and logged in) > anonymous.

When the installer has a GitHub token, it downloads assets through the GitHub
API asset endpoint (internal repositories require it, because their plain
release download URLs return nothing), and fetches any signed storage redirect
with a second request that carries no token: the token is sent only to the API
host. If the GitHub token cannot read the release metadata, the installer uses
the plain release download URLs instead; if the download of the `rancher` binary
then fails, the error also says the release metadata was unreadable and gives
its HTTP status.

After creating the links, the installer warns about any other `rancher` or
`rancher-credential-vault` elsewhere on `PATH` (your shell can keep running the
old one from its cached command locations until you run `hash -r` or `rehash`),
and gives no warning for symlinks that resolve to the installed binary. When the
installer changes the version it says so: a note for an upgrade, a warning for a
downgrade, and a note when the name of the previous version directory is not a
release tag. Going back to an earlier version is one command: rerun the
installer with `--version <tag>` (`-Version <tag>` on Windows).

Each release includes its own copy of `install.sh` and `install.ps1` as they
were at that release, as release assets with checksums and attestations like
every other asset; the raw `https://raw.githubusercontent.com/…/main/install.sh`
and `…/install.ps1` URLs always return the current copy from `main`.

### Downloading and running the installer safely

The one-line install command in the README is safe if the download is cut off:
every statement in `install.sh` is inside a function and only the last line,
`main "$@"`, runs it; a cut download either stops on a syntax error or runs
nothing. Piping the download into `sh` has one drawback: when the *download
itself* fails, `sh` reads empty input and exits `0`; curl's message on stderr is
the only sign of the failure. For scripts that must check the download's exit
code, download the installer to a file first and then run it:

    (
      t="$(mktemp)" || exit 1
      trap 'rc=$?; rm -f "$t"; exit "$rc"' 0
      trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM
      curl --proto '=https' --tlsv1.2 -fsSL \
        -o "$t" \
        https://raw.githubusercontent.com/rancher/rancher-cli/main/install.sh \
        && sh "$t"
    )

It passes on curl's exit code, removes its temp file on any exit, and never runs
a partial download.

While the repository is internal and `gh` is unavailable, use the command below,
which sends a GitHub token: curl reads the token from a configuration given on
standard input, and `sh` reads it from the environment; the token is never a
command-line argument:

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

Pass installer flags after `sh "$t"` (for example `sh "$t" --version <tag>`).

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
when a `gh` that is logged in is available (`-VerifyAttestation` makes
that mandatory), installs under `%LOCALAPPDATA%\rancher-cli`, and prints
the `PATH` line to add - it never edits `PATH` itself. Its atomic switch
to a new version relies on a directory rename that requires Windows 10
version 1607 or Windows Server 2016 or later. To pass parameters
(`-Version <tag>`, `-Pre`, `-BinDir <dir>`, `-VerifyAttestation`, `-DryRun`,
`-Quiet`), run the downloaded script as a script block:

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
2. Rename them to `rancher.exe` and `rancher-credential-vault.exe`. Both names
   matter: kubectl runs the kubeconfig's exec plugin as the command `rancher`
   (unless `rancher kubeconfig --exec-command` set another), and the CLI runs a
   credential helper as `rancher-credential-<name>`; on Windows, a program
   started by name is found only on `PATH` and only with an extension listed in
   `PATHEXT` (`.exe`).
3. Verify both binaries against `SHA256SUMS` (PowerShell);
   `rancher-credential-vault.exe` matters as much as `rancher.exe`: it is the
   process that stores your Rancher credentials and holds your Vault token;
   never install it unverified:

       Get-FileHash rancher.exe -Algorithm SHA256
       Get-FileHash rancher-credential-vault.exe -Algorithm SHA256

   Compare each output against its matching line in `SHA256SUMS`.
4. Optionally verify build provenance for both:
   `gh attestation verify rancher.exe --repo rancher/rancher-cli --signer-workflow rancher/rancher-cli/.github/workflows/release.yaml`,
   then the same command with `rancher-credential-vault.exe`.
5. Only after both binaries verify, place them in a directory on `PATH`.

##### Notepad on Windows 11

On Windows 11, the Store version of Notepad opens `config edit`'s temp file as a new tab in an already-running Notepad window rather than a new window; close the tab, not the window, to return control to the CLI.

## Shell completion

PowerShell (Windows PowerShell 5.1 or PowerShell 7) - pipe into the session so
no script file is written and PowerShell's ExecutionPolicy does not apply:

    rancher completion pwsh | Out-String | Invoke-Expression

To persist it, add that line to your `$PROFILE`. Loading `$PROFILE` itself needs
an ExecutionPolicy that allows local scripts (`RemoteSigned` or looser); the
per-session command above works regardless.

Minimum shell versions: bash 3.2 (the version macOS ships) with the
`bash-completion` package installed - on bash 3 a second TAB shows no
descriptions, but completion works; zsh 5.0 with `compinit` loaded; fish 3.0;
Windows PowerShell 5.1 or pwsh 7. Each generated script states the minimum shell
version it needs in its header comment.

Completion works offline by default: every command, group and flag name
completes; `get`/`patch`/`delete` also complete each kind's plural, singular and
short names (`po<TAB>` → `po`/`pod`/`pods`); `config use-server` and
`config delete-server` complete from the local config file. It never uses the
network, never prompts and never writes anything; a missing or unreadable config
gives no candidates and no error.
That holds on a line with a `--` too, where every word is an argument:
completion offers the values the command accepts at that position, never a
flag, and never runs the command.

### Live completion (opt-in)

After installing the script, enable live resource-name completion:

    rancher config set-completion --network=true

This completes cluster names (including `inspect cluster` and `inspect project`
positional names), project names, namespaces, pods, deployments, and nodes from
the Rancher API. `rancher config set-completion --network=true` prints a note on
which completions work with your credential store or `RANCHER_CLI_TOKEN`.

User, role template, global role and node driver ids complete as well. A user,
project, cluster, role template, global role or node driver id is offered with a
description when the object has a display name or, for a user, a username. Where
the shell shows descriptions, it shows one beside the id: a user's username and
display name (`u-abc12`, `alice (Alice Smith)`), the display name for the
others. The value inserted is always the id. A username or display name that
completion offers as a value carries no description. A description is cut to 56
characters; control and invisible characters in it are dropped or shown as a
space.

`grant --role` completion leaves out role templates whose `locked` field is true
and, at global scope, global roles that inherit a locked role template or one
that does not exist, since Rancher does not allow a new binding to such a role;
`revoke --role` still offers them. The candidates in a NOT_FOUND error of
`grant` follow the same rule, and `create user --global-role` completion and its
NOT_FOUND candidates leave out the same global roles.

- zsh and fish show the description in the completion list; PowerShell shows it
  as the tooltip of the candidate.
- bash lists descriptions on the second TAB (`token  -- description`) on bash
  4 and later. Bash 3 (the version macOS ships) lists the values only. The first
  TAB inserts the value on every bash.
- Windows PowerShell 5.1 decodes the CLI's output with the console encoding,
  so a description with non-ASCII characters may show garbled there.

`--auth-provider` on `auth login`, `auth get-token` and `token` completes the
names of the auth configs this CLI can sign in with, from the list of auth
configs the server publishes, which needs no credential: the request carries
none. The server and CA are the ones the command would connect with for what you
have typed so far: for `auth login`, `--url`, else the server entry you name,
else the current entry; for `auth get-token`, `--server`, else
`RANCHER_CLI_URL`, else the current entry (`--name` chooses only the CA, and an
entry at another URL gives no suggestions); for `token`, the `--server` it
requires and `--cacerts`. Nothing is offered with `--skip-verify`, when the
command would refuse the line, or when the server cannot be reached or trusted.

By default, each TAB waits at most 500 ms for the server's HTTP responses;
change the limit with `--timeout`:

    rancher config set-completion --timeout=1s

On any failure (an expired token, a slow server, a URL that no stored credential
matches), live completion offers no candidates and prints no message.
To see why, run the command the shell script runs, with tracing on: each step
prints one line on stderr with the time since the process started, and stdout
holds exactly what the shell would receive:

    RANCHER_CLI_COMPLETION_DEBUG=1 rancher --rancher-completion-proto=2 get cluster --rancher-completion-cur= --generate-shell-completion

PowerShell: `$env:RANCHER_CLI_COMPLETION_DEBUG = "1"` first, then the same
command.

## Cluster scope (--cluster)

`--cluster <id-or-name>` is looked up once per command with your permissions:
among the clusters Rancher lists for you (`rancher access clusters` shows them),
by id or display name; a cluster owner or project member who cannot read the
local cluster can use it the same way an administrator does. An id takes
precedence over another cluster's display name, with a warning naming that
cluster; two clusters with the same display name give `AMBIGUOUS_SELECTION`
(pass the id). If a cluster was created moments ago and the list does not show
it yet, the CLI gets it by its id. `local` is always the local cluster, and
where a command allows it, leaving out `--cluster` means the local cluster.
kubectl's exec plugin resolves its `--cluster` the same way when it has no
cached credential and creates one, with the warning on stderr; its `NOT_FOUND`
error lists the clusters you can read.

`get` and `patch` of the `cluster` kind and `get projects` work as in kubectl:
they go through the local cluster's Kubernetes API, where Rancher returns
clusters only to users with access to the local cluster, and returns projects
according to your permissions. When Rancher refuses one of these requests, the
error names the request and a command to use instead that works with your
permissions (`rancher access clusters`, `rancher inspect cluster <id>`,
`rancher access projects --cluster <id>`).

## Project scope (--project)

`--project <id-or-name>` selects a project on the cluster that `--cluster`
selects. Only these commands accept `--project`: `get`, `delete` and `patch` for
namespaced kinds on downstream clusters and for namespaces, `create resource`,
`create namespace`, `create secret`, `project add-namespace`, `grant`, `revoke`,
`usage`, and `access can`/`list`/`who-can`; `rancher schema` shows `project`
among the flags of each of them. Every other command refuses a typed `--project`
with `VALIDATION` before it sends a request, whether the flag is typed before or
after the command name.

`rancher project remove-namespace` also accepts a namespace that has the project
label and no project annotation, and removes its project label.

The project is looked up once per command with your permissions: among the
projects Rancher lists for you on that cluster, by id or display name. An id
takes precedence over another project's display name, with a warning naming that
project; two projects with the same display name give `AMBIGUOUS_SELECTION`
(pass the id). If a project was created moments ago and the list does not show
it yet, the CLI gets it by its id. Without `--cluster` the command refuses and
names the clusters a project with that id or display name is on; it never
picks one.

What `--project` does: a list shows only objects in the project's namespaces,
and an object you name or change must be in one of them. A namespace belongs to
the project its `field.cattle.io/projectId` annotation (`<cluster>:<project>`)
names, on the cluster the namespace is in; the label of the same name does not
decide which project a namespace belongs to. `get namespaces --project <p>` asks
the server for the namespaces with the project label and keeps the ones whose
annotation names the project. `create resource` refuses `--project` for a
Namespace manifest (use `create namespace --project`). `create secret --project
<p>` writes a project-scoped secret into the project's backing namespace and
cannot be combined with `--namespace`. `usage` refuses `--project` with `--by
node` (a node row mixing project-scoped requests with whole-node metrics would
be misleading). When a `--namespace` or `--node` matches no pods, `usage` gets
that namespace or node; a name that does not exist gives NOT_FOUND instead of an
empty result. If you can list pods there but cannot read the namespace or node,
you get the result with a warning that it could not be read; if you cannot
list pods there, you get the RBAC error from the pod list.

A flag typed with an empty or blank value (`--project ''`, `--namespace=`,
`-o ' '`) is refused with `VALIDATION` before any request instead of acting
as if it were omitted; in a script, pass `--flag "$VAR"` only when `VAR` is
set. A bool flag typed with `=` and an empty or blank value
(`--apply="$APPLY"` with `APPLY` unset) is refused the same way; write
`--apply` or `--apply=true|false`. Positional arguments beyond the ones a
command takes (`rancher schema <command>` lists them) are refused the same way,
and so is a separate argument after a bool flag that the flag could take as its
value: give a bool flag its value with `=` (`--dry-run=false`), and put a
positional argument such as `true` or `false` before the flags or after `--`.
The words after a `-` on its own are not used: a line with any argument after it
(`rancher get pods - --cluster c1`) is refused the same way, whether the `-`
comes before or after the command name. A `-` as a flag's value (`-f -`) or
after `--` is accepted, and `--help` after a command's name, or `--version` at
the root, still shows the help or the version when it comes before the `-`.
A command name the CLI does not know is reported as `unknown command` whatever
you typed after it; a wrong name never shows up as a flag error. An empty name
(`rancher get ""`, an unset variable) is also reported as `unknown command`.

## Kinds & verbs

Every command that takes `--namespace` also takes `-n`, as kubectl does.

Every kind that `rancher schema` lists gets its generated verbs by these rules:

- `get` and `patch` exist for every kind that `rancher schema` lists. One
  exception: `patch clusterregistrationtoken` is refused - the kind is
  read-only. A `get` that lists objects accepts `--selector` (`-l`), a
  Kubernetes label selector the server applies (the same grammar as kubectl's
  `-l`: `app=web`, `tier!=cache`, `env in (a,b)`, `!legacy`); it is checked
  before the request and refused on a `get` of one object. With `--project` and
  `--selector` together, `get namespaces` sends the project's label selector and
  your selector as one selector that requires both, and keeps the namespaces
  whose annotation names the project; for every other kind the server filters by
  `--selector` and the CLI keeps only the objects in the project's namespaces.
  `get clusterregistrationtoken` reads through the Steve API and applies the
  selector itself to the list it gets back.
- The catalog kinds `clusterrepo`, `app` and `operation` (catalog.cattle.io)
  are read-only. Each cluster has its own repositories, apps and operations,
  so they take `--cluster` (`local` for the local cluster); `app` and
  `operation` take `--namespace`; a `clusterrepo` is not namespaced and takes
  no `--namespace`. `patch` of each is refused with `VALIDATION` and the
  reason, before any request; `create resource` refuses a manifest of each;
  `delete` does not exist for them, and `rancher delete app` (or `apps`) fails
  with the reason. The
  CLI hides an app's `spec.values`, `spec.chart.values` and `spec.info.notes`
  and an operation's `status.token` from every output. The columns follow the
  table the Rancher server returns. For `operation`, COMMAND shows the
  command's arguments joined with commas, and TARGET-NAMESPACE is the namespace
  of the pod that ran the command, where the Rancher UI's "Tgt Namespace" is
  the release's namespace.
- The CLI hides the Helm `values` and `templateValues` of HelmOps, bundles
  and bundle deployments from every output. For a HelmOp or a bundle it also
  hides them in each item of `targets`. For a bundle deployment it hides them
  in `options` and `stagedOptions`. A HelmOp can take values from a Secret
  through `valuesFrom`. Those values are in the Secret, not in the HelmOp. The
  HelmOp output shows only the `valuesFrom` reference.
- `etcd-snapshot` (`rke.cattle.io`) is read-only: Rancher creates one etcd
  snapshot object for each snapshot file of a cluster. `--namespace` defaults to
  `fleet-default`, the namespace of provisioning clusters in the default Fleet
  workspace. You cannot list the snapshots of all namespaces at once. For a
  provisioning cluster in another Fleet workspace, pass that workspace's name.
  For an imported cluster, or a cluster imported through Rancher Turtles, pass
  the cluster's id: `--namespace <cluster id>`. Rancher v2.15 and later creates
  etcd snapshot objects for these clusters; Rancher v2.14 creates them only for
  provisioning clusters. `--selector rke.cattle.io/cluster-name=<name>` lists
  the snapshots of one cluster. For a provisioning cluster, `<name>` is the
  cluster's name. For an imported cluster, or a cluster imported through Rancher
  Turtles, `<name>` is the cluster's id (`c-...`). `STATUS` is the
  `snapshotFile.status` that Rancher sets: "successful" or "failed". A snapshot
  that is not ready yet also shows "failed". `snapshotFile.message` gives the
  reason for "failed"; `-o wide` shows it as MESSAGE. `patch` and `create
  resource` with an ETCDSnapshot manifest are refused with `VALIDATION` before
  any request. There is no `delete` for it: `rancher delete etcd-snapshot` fails
  with `VALIDATION` and an "unknown command" message that gives the reason. The
  columns follow the Rancher UI, since the server's Table has only the Name and
  Created At columns.
- `delete` exists only for kinds where deleting an object makes sense. Rancher
  settings and feature flags (`setting`, `feature`), role templates and global
  roles (`roletemplate`, `globalrole`), and objects a controller creates and
  manages (Fleet bundle deployments, CAPI machines) are not deletable. Deleting
  a GitRepo or a HelmOp deletes its bundles and their bundle deployments, which
  uninstalls what they deployed (the workloads of a GitRepo, the chart of a
  HelmOp) from every target cluster; lowering a machine
  pool's node count with `cluster scale` deletes machines; deleting a
  provisioning cluster deletes its Fleet cluster and CAPI cluster. Fleet bundles
  and cluster groups have no `delete` in this CLI either, and neither do Fleet
  workspaces (`fleet-workspace`): deleting one deletes its namespace and
  everything in it. The `cluster` kind
  (management.cattle.io) cannot be deleted either; to delete a cluster, use
  `delete provisioning-cluster`. `delete user` is destructive (requires
  `--yes`): it deletes the User object, and Rancher's controllers then delete
  the user's role bindings (cluster role template bindings, project role
  template bindings, global role bindings).
- `delete project` is destructive and lists the project's namespaces before it
  deletes anything. It reads them on the project's cluster: from the namespace
  list narrowed by the project label or, if Rancher refuses you that list (as it
  does a project owner), from the namespaces the Steve API lists for you; either
  way the project's namespaces are the ones whose annotation names the project.
  Rancher deletes the project's namespaces that have the
  `field.cattle.io/creatorId` annotation, with everything in them, and keeps the
  others, removing the project annotation from each; it also deletes the
  project's role bindings and project-scoped secrets. The dry-run preview and
  the result carry the list in `data.objects`: one item per namespace with
  `kind`, `name`, `outcome` and, for a failure, `code` and `message` (a `not
  deleted` item gives its reason in `message`). Text output shows at most five
  names per outcome and a count of the rest; JSON carries every item. Without
  `--delete-namespaces` the outcomes say what Rancher will do: `rancher
  deletes`, `rancher keeps`, `being deleted` (the namespace already has a
  deletion timestamp or the Terminating phase) and `unknown` (you cannot get the
  namespace, which hides its `field.cattle.io/creatorId` annotation). The CLI
  deletes namespaces only when you pass `--delete-namespaces`. If the namespaces
  cannot be read, the dry-run warns and the delete goes on. When a namespace is
  kept, the dry-run sends one `SelfSubjectAccessReview` to the project's cluster
  (verb `get`, resource `namespaces`, no name); if the cluster denies it, you
  can get namespaces only through your project, and a warning names the kept
  namespaces you will not be able to get after the delete. A failed review
  gives no warning. `--delete-namespaces` also deletes the project's namespaces:
  one at a time, in name order, each with the resourceVersion it was read with,
  then the project. Each namespace the command will delete gets a server-side
  dry-run delete first; the namespaces left alone, described next, get none. The
  command is refused before anything is deleted if the server refuses a dry-run
  delete, if you cannot get one of the namespaces it will delete, or if reading
  the project's namespaces fails. Namespaces already being deleted are listed
  and left alone. Kubernetes never deletes `default`, `kube-system` and
  `kube-public` on any cluster, and Rancher never deletes `local` and
  `fleet-local` on the local cluster: they are listed as `not deleted` with that
  reason and skipped. With `--delete-namespaces` the outcomes are `will be
  deleted` (dry-run), `deletion started`, `already gone` (the namespace was gone
  when the delete was sent), `not deleted`, `being deleted`, `failed` and `not
  attempted`; the project comes last in the list. A failed namespace delete
  stops the run and keeps the project: the error carries the failing call's code
  and `data.error.details.objects` holds every item with its outcome. When the
  project changes while its namespaces are being deleted, the project delete is
  retried up to three times with the new resourceVersion if only its used
  resource quota (`spec.resourceQuota.usedLimit`) and its status changed; any
  other change, or a fourth conflict, is `CONFLICT` with the project kept.
  Re-run the same command after any partial result: it reads the project's
  namespaces again and skips the ones already being deleted. The result's `kind`
  stays `ProjectDelete`; its `data` has the fields `deleted` and `objects`.
- `user`, `project`, `roletemplate`, `globalrole` and `nodedriver` take the
  object's id as the argument, named `id` in help and the schema: the NAME
  column of `get`, not the username or display name. When `get`, `patch` or
  `delete` finds no object with the id you typed, and the text you typed is
  exactly the username or display name of an object, the error message says
  which id that is, the object is listed in `data.error.candidates`, and the
  hint is `rancher get <kind> '<id>'`; several objects with that text stay
  `NOT_FOUND` with every candidate listed. The lookup is one more read, made
  with your permissions and a 10 s limit, only after the first read finds no
  object; any failure leaves the original error. The CLI only reports a match;
  it never runs the command again with that id. A project is looked up among the
  projects you can read on the cluster that `--cluster` selects, and also after
  Rancher refuses the read (RBAC), because a project member's role allows
  getting a project only by its id. The error keeps its code; its message,
  candidates and hint change only when the lookup finds a match. `get cluster`
  and `patch cluster` take an id or a display name.
- `create` has a dedicated command for kinds such as `project`, `namespace`,
  `secret`, `token`, `kubeconfig` and `cluster`; `create resource -f` covers any
  supported kind from a manifest, except a token or a kubeconfig: `create
  token` and `create kubeconfig` create those and show the value once.
- Rancher role bindings (`projectroletemplatebinding`,
  `clusterroletemplatebinding`, `globalrolebinding`): prefer `grant`/`revoke`,
  which resolve the subject, detect duplicates and report partial failures. The
  generated verbs are for working from manifests or with declarative tools.

`rancher schema <kind>` shows the verbs of one kind: available (with contract
and stability), refused (with the reason), or not available (with the reason).

`feature` (Rancher's `management.cattle.io/v3` Feature flags) is one of those
generated kinds - not deletable, sensitive (`patch feature` flips a flag and
requires `--yes`). Its main fields (`spec.value`, `status.default`,
`status.lockedValue`) can be true, false or unset (an unset `spec.value` means
"follow the default"), and a table cell can hide which of the three applies; use
`-o json` to read a feature's actual state.

`nodedriver` (Rancher's `management.cattle.io/v3` NodeDriver resource - the
server side of `create cluster`'s node-driver mode) is a generated kind that is
mostly read-only: not deletable, sensitive (`patch nodedriver` activates or
deactivates a driver for all of Rancher and requires `--yes`). `get nodedrivers`
lists NAME, DISPLAY NAME, ACTIVE and BUILTIN.

On a Rancher older than 2.14 (the CLI's supported minimum), the generated `get`,
`patch` and `delete` of `token` and `kubeconfig` fail with `NOT_FOUND` (the
message includes `requires Rancher >= 2.14, the supported minimum`) and point to
`auth list-tokens` for tokens and to the default exec-credential mode of
`rancher kubeconfig` for kubeconfigs. These two kinds are registered with an
empty API version, which the CLI resolves through discovery when the command
runs, as it does for the CAPI kinds. You can do the same operations with kubectl
on these two kinds: `kubectl get/create tokens.ext.cattle.io` /
`kubeconfigs.ext.cattle.io`. A standard user needs `--validate=false`, because
kubectl falls back to reading the CRD schema for validation, which requires list
permission that most users lack. The CLI adds authentication with your stored
login (or `RANCHER_CLI_TOKEN`), the conversion of `--ttl` (a duration) to the
unit of `spec.ttl` (milliseconds for a token, seconds for a kubeconfig), and
showing the created secret only once.

`oidcclient` (Rancher's `management.cattle.io/v3` OIDCClient - the server side
of the embedded OIDC provider) is the first generated kind that requires a
Rancher Prime capability (feature `oidc-provider`): `rancher schema oidcclient`
and `--help` say so. On a server where the feature is explicitly disabled, the
verbs refuse with `VALIDATION` and a message that says the feature is disabled,
on every edition, before the verb makes any read or server-side dry-run of its
own; `PRIME_REQUIRED` (exit 9) is used only for a Rancher Prime feature on a
server whose edition cannot enable it. Sensitive (`patch oidcclient` requires
`--yes`): `patch` can set the same client secret annotations that the dedicated
commands `create oidc-client` and `oidc-client
add-secret|rotate-secret|remove-secret` protect with confirmation, waiting and a
secret shown once; prefer the dedicated commands. Deleting a client also deletes
its Secret (Kubernetes garbage collection through an owner reference,
independent of the `oidc-provider` controller), which invalidates every client
secret at once.

### Field ownership

Every create, update and patch request the CLI sends to a Kubernetes API sets
the field manager to `cli.rancher.io`, including the ext Token create and the
access review requests. For an object the CLI creates or changes, the managed
fields (`kubectl get <kind> <name> -o yaml --show-managed-fields`) show which
fields the CLI wrote. Rancher's token API does not keep the request's field
manager: the managed fields of an ext Token show `rancher`, and the access
review requests are not stored. The field manager has no version in it; it stays
the same across releases and binary names.

A delete carries no field manager: Kubernetes has none for deletes. Requests
to Rancher's own endpoints (login, logout, the Norman token API) are not
Kubernetes writes and have none.

`patch` reads the object's managed fields before it changes anything. When a
field the patch changes is owned by a manager that applies it (server-side
apply, as Flux and Argo CD do), the dry-run preview carries a warning naming
that manager: it may put the old value back on its next reconcile. Fields owned
by managers that merely updated them (Rancher's controllers, `kubectl edit`) are
not flagged. The warning is informational; it never blocks the patch. A patch
operation whose `path` (or `from`) is `/metadata/managedFields` or
`/metadata/resourceVersion`, or anything under them, is refused with
`VALIDATION`; so is a replace of `/metadata` or of the whole object whose value
carries them. A merge patch (`--type merge`) that sets or removes either field
is refused the same way. The server sets both fields; the CLI always sends them
back exactly as fetched.

Every `diff[].path` a dry-run reports (in `-o json` and in the default text
output) is an RFC 6901 JSON Pointer - `~1` for a literal `/` and `~0` for a
literal `~` inside a key; a domain-prefixed label or annotation key therefore
renders as one escaped segment. The string is usable verbatim as the `path` of a
JSON Patch operation.

A create's dry-run lists the fields the command sends, one entry per field,
with `from` null; it does not list the server's defaults. The fields that
name the object (`apiVersion`, `kind`, `metadata.name`,
`metadata.generateName`) are in the headline, not the list, and a create that
sets nothing past its name has no `diff`. In the default text render a map or
list value prints as compact JSON. A value that is hidden shows `<redacted>`
in `from` or `to`, and a side that has no value stays null.
The `--apply` result of a mutating command in json, yaml and template leaves
out the same values. The exceptions are the commands that show a secret once on
purpose: `create token`, `create oidc-client`, `oidc-client add-secret` and
`oidc-client rotate-secret`. `create kubeconfig` prints its kubeconfig and
accepts only table output. `get` in json, yaml and template leaves out the same
values, except that `get secret` shows `data` and `stringData`. kubectl with a
kubeconfig from `rancher create kubeconfig` shows a value the server returns
on a read.

`patch <kind>` takes an RFC 6902 JSON Patch (an array of operations) by default,
and an RFC 7386 JSON merge patch (an object) with `--type merge`. In a merge
patch, every list you include replaces the whole list in the object; kubectl's
default strategic merge patch merges some lists by key, and the CLI does not
support strategic merge patch. When a merge patch removes entries of a list
the object has, the dry-run preview carries a warning naming the list. A path
(and a from of move or copy) in a JSON Patch must be empty or start with /, as
RFC 6901 requires; the patch is refused before the object is read otherwise.
The path of a move or copy cannot be empty; to replace the whole object, use
replace with an empty path.

An operation of a JSON Patch that does not apply to the object (no value at the
path, a failed `test`, an index past the end of a list) is refused with
`VALIDATION` after the object is read and before anything is sent. The message
names the operation when it can, by its number, counted from 0, and its name,
then the path and what to check. The refusal of a patch that replaces the whole
object with a value that is not an object, and the fixed sentences for a merge
patch and for a failure with no known operation, name no operation.

## Cluster provisioning

Rancher's provisioning model (node drivers, machine pools, imported and custom
clusters) is documented in the
[Rancher Manager documentation](https://ranchermanager.docs.rancher.com/how-to-guides/new-user-guides/launch-kubernetes-with-rancher).

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

`--wait` on `get <kind>` applies the kind's default wait condition; for
clusters that is `condition=Ready`. `--for <condition>` on its own also
starts the wait; `--timeout` on its own waits for the kind's default wait
condition, and a kind with none refuses it and asks for `--for`. An
authentication (`AUTH`) or certificate (`TLS_TRUST`) failure of a request ends
the wait at once with that error, and so does `NOT_FOUND` (exit 4); any other
error of a request is retried, and a timeout names the last one (`last error:
<CODE>: <message>`). The commands that wait on an OIDC client (`create
oidc-client` and `oidc-client add-secret|rotate-secret|remove-secret`) follow
the same rule.

The wait of `get <kind>` watches the object and sees a change within a
fraction of a second. The wait lists the object by name, watches it from the
list's resourceVersion for about a minute, then lists again. The wait needs the
`list` and `watch` permissions to be fast; with `get` alone, when the server
refuses a List or a watch, or when a List has no resourceVersion or holds more
than one item, it polls with a GET every 5 seconds. A refused List or watch
counts as one of the requests: when the List is refused, the second GET comes 10
seconds after the first; when the List works and the watch is refused, the
first GET comes 10 seconds after the List. A user who has only `get` on the
object always polls, with one refused List first; Rancher's cluster member role
on the management Cluster is such a user. A change of permission or a revoked
token is noticed at the next cycle, within about 70 seconds. An `AUTH` error
from the List or the watch may name `list` or `watch` where an error from a Get
names `get`. The commands that wait on an OIDC client watch the object the same
way, with a poll interval of 2 seconds instead of 5 and a default timeout of 60
seconds; with `get` alone they poll every 2 seconds, and the second GET comes 4
seconds after the first request.

`rancher kubeconfig` prints one context through the Rancher proxy and, when the
cluster has the Authorized Cluster Endpoint enabled and the
multi-cluster-management feature is on, the Authorized Cluster Endpoint contexts
Rancher's own kubeconfig carries: `<entry>-fqdn` when the endpoint names an fqdn
(its `caCerts` as the CA), else one `<entry>-<node>` per control-plane node at
`https://<address>:6443` with the cluster's CA; every context uses the one user
whose `exec` block runs the plugin. The fqdn context, else the first Ready node,
is current; with no Ready node the proxy stays current. When the Authorized
Cluster Endpoint contexts cannot be derived (the feature off or unreadable, the
nodes not readable, no control-plane node with an address, a malformed CA or
address) the proxy-only file is printed and one stderr notice says why; when
Rancher refuses the credential (401) at any of those reads, the command fails
with `AUTH` (exit 3) and prints no file. The `exec` block then sets
`provideClusterInfo`, which tells the plugin which endpoint kubectl is about to
use. A cluster token the plugin has just created is accepted by the Authorized
Cluster Endpoint only after Rancher has synced it there. The plugin therefore
waits up to ten seconds until that endpoint accepts the token (it sends a
SelfSubjectReview; a Forbidden answer counts as accepted, a 401 does not), and
records in its cache that this endpoint accepts it. If the wait times out, the
plugin returns the credential with a warning that names the proxy context as the
fallback. If you cancel, the plugin revokes the token it created. There are two
limits. First, the plugin needs Rancher to create or refresh the token; an
Authorized Cluster Endpoint context therefore keeps working while Rancher is
unreachable only until the cached credential expires. Second, the identity the
Authorized Cluster Endpoint sees has only the user and `system:authenticated`,
without the `system:cattle:authenticated` group and the `principalid` and
`username` extra attributes the proxy adds; a binding on the groups the proxy
adds therefore gives different access on the two paths.

The `exec` block records the config directory and the credential store the
kubeconfig depends on. Its `env` sets `RANCHER_CLI_CONFIG_DIR` to the config
directory `rancher kubeconfig` ran against, absolute, the default included, and
`RANCHER_CLI_CREDENTIAL_STORE` to the empty value when that directory's config
file sets `credentialStore`, else to the value set when it ran (or empty);
kubectl adds the `exec` block's `env` after its own environment; the plugin
therefore opens that directory and credential store from any shell or working
directory. The file therefore works for this user on this machine only, and
every run says so on stderr. If a config directory is moved, renamed or deleted,
its kubeconfigs stop working: the plugin opens the recorded path, which is then
empty (signing in there creates a new directory); write the kubeconfig again
from the directory's new location. `--exec-command <command>` names the command
the `exec` block runs, default `rancher`: a bare name kubectl looks up on its
PATH, or a path, made absolute from the working directory (a leading `~` or `~/`
is the home directory; `~user` is refused). `installHint`, which kubectl prints
when it cannot run the command, gives the install URL for the default command.
For another command it gives the steps that write the kubeconfig again from the
entry it was written from (`rancher --config <dir> config use-server <entry>`,
then `rancher --config <dir> kubeconfig --cluster <id>`). A file written in a
session that used only environment variables, which has no entry, tells you
which variables to set (`RANCHER_CLI_URL`, `RANCHER_CLI_CACERT` when the file
carries a CA, and `RANCHER_CLI_TOKEN`) before `rancher --config <dir> kubeconfig
--cluster <id>`. When the directory's path has characters a terminal
cannot show, the commands that would name it are left out, in `installHint` or in any
remedy: the text says why. A second stderr notice comes from the exec
plugin's own lookup in the credential store it will open: which stored login
kubectl will use when it is not this session's (under `RANCHER_CLI_TOKEN`, the
plugin's own login, or an entry named `<current>-<id>`), how to get one when
none answers, that the plugin will refuse the file and why, or that the
credential store could not be read. The notices are a snapshot: a later login or
logout changes the answer.

`create machine-config` has its own flags for `digitaloceanconfig` (`--region`,
`--size`, `--image`, `--monitoring`, `--ipv6`, `--private-networking`,
`--tags`); every other kind takes `--set field=value` for driver-specific
fields; credential fields are always rejected. Multi-pool node-driver clusters
go through `create resource -f`.

Imported and custom clusters need no machine config: `create cluster
<name> --import` or `--custom`. Custom-cluster nodes join with the
registration commands that `inspect cluster <name> --registration`
fetches.

A `creatorId annotation not set` warning from `create cluster` means the
CLI could not set the `field.cattle.io/creatorId` annotation: the
SelfUser request for your user ID failed or returned no valid user ID, or
the CLI could not add the annotation to the cluster object it builds.
rancher-webhook may then reject the server-side dry-run, and a rejected
dry-run also stops `--apply`, which runs the same dry-run first. Log in
again, or check that you may create `selfusers.ext.cattle.io`, then
retry.

## Composite commands

`inspect cluster` is a composite read: the management cluster's id, name,
state and Kubernetes version + agent connectivity (management Connected/Ready
conditions; an agent that is not connected adds a warning) + provisioning
status/conditions (ready derived from the Ready condition), machine pools,
members (ClusterRoleTemplateBindings with display names resolved), machines
([Cluster API](https://cluster-api.sigs.k8s.io/) Machines with phase and the
message of the first failing condition, preferring a condition whose reason
contains `Error`), and the registration commands (node command and import
command). Registration commands are not fetched by default - they contain the
cluster registration token; pass `--registration` to fetch them, or use `get
clusterregistrationtoken default-token --namespace <cluster-id>`. The
management read is mandatory (unknown/ambiguous cluster or a failed read is a
structured error, non-zero exit); if reading the provisioning, members,
machines or registration section fails, the command shows a note or warning
for that section and the other sections still show.

`inspect project` prints text in sections (PROJECT, NAMESPACES, MEMBERS,
RESOURCE QUOTA, USAGE); members are ProjectRoleTemplateBindings with display
names resolved from Users/UserAttributes; if the members section cannot be
read, it shows a warning and the other sections still show.

`inspect user <id|username|displayName|principal>` answers who a user is and
what role bindings they have at global, cluster and project level - never what
they can actually do (only the server can answer that, and no command in this
CLI asks it). The output puts the user's id and names, group memberships (as
Rancher last refreshed them, which also happens at login) and role bindings at
global, cluster and project level into one document. If reading group
memberships fails, the role binding sections are left out too, because without
the groups the CLI cannot confirm the role bindings are complete.

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
CLI reads those principals (the user's UserAttribute) through Rancher's
`/v1` API, which applies your own roles; it needs `get` on
`userattributes.management.cattle.io`. When Rancher refuses the read, the
review carries only `system:authenticated` and `system:cattle:authenticated`,
`groupAliases.status` is `unavailable` (the text line reads `groups from
rancherUserAttribute (unavailable)`), a warning says why, and a denied
answer is reported `unknown` (exit 13): an allowed answer still stands.
`access explain` then marks the chain `partial` or `unknown` with a
`managementRead` issue for the UserAttribute. An impersonated review does
not send the groups, so its answer does not change. The management cluster never impersonates: `access can --cluster local --as`
uses SubjectAccessReview and `access list --as` refuses there. Exit
codes: `can` 0 allowed, 12 denied, 13 unknown; `list` 13 when any
namespace answered incompletely or not at all. Every answer states its
source (`authorizer`, `rancherProxy`, `scopeDiscovery`, `request`,
`mixed`) and identity (`caller`, `impersonated`,
`subjectAccessReview`).

Every `access` command runs under one time limit for the whole command:
discovery, identity reads and every review share it, at most five
reviews in flight at once. The default is 60 s; `--timeout <duration>`
(for example `--timeout 5m`) sets it for that command. Running out of time
is a terminal error naming the limit and the stage it ended in, and the
clusters or namespaces that had already answered are not reported. A
fleet-wide `access clusters` on a large installation needs a longer
`--timeout`; `access projects` can also be narrowed to one cluster with
`--cluster`, and `access can|list|who-can` to one namespace (`-n`) or
project (`--project`).

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
cached for it, read through Rancher's `/v1` API (when Rancher refuses the
read, that user's rows are `unknown`, each with the refusal as its issue);
a group is asked as itself and lists, under
`knownMembers`, the users Rancher has cached in it (users who have logged
in - groups are not expanded further). The members come from listing every
UserAttribute through `/v1`, which needs `list` on
`userattributes.management.cattle.io`; without it a warning says so and no
`knownMembers` are claimed; a username with no Rancher User is
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
read, through Rancher's `/v1` API with your own roles (`get` on
`userattributes.management.cattle.io`); a match through a group is limited
to what Rancher has cached.
`reachable[]` is the compact answer for a program: every reachable
cluster with its grants (RoleTemplates and GlobalRoles, `null` unless the
binding reads were complete). Exit 0 when every cluster answered, 13 when
any is `unknown`. `--as` takes a user (name, id, or principal); a group or a
service account is refused.

`access projects [--cluster <c>] [--as <user>]` answers which projects a
subject can see into and which of them it is a Rancher member of, and keeps
the two apart. The projects are what steve lists for you, grouped by
cluster, each cluster carrying its `access clusters` row; `--cluster`
restricts the run to one cluster of that set, by id or display name (an id
wins over another cluster's display name, with a warning).
For every reachable cluster the command reads the namespaces you see
through steve (a project member cannot LIST namespaces through the
Kubernetes API, and steve answers every caller with exactly the namespaces
their access grants), groups them by the project their
`field.cattle.io/projectId` annotation names on that cluster,
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
cached group principals, read through Rancher's `/v1` API) and, when you ask about yourself, your own User
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
logs outside the Rancher components that `logs` covers stay with `inspect pod`
and kubectl.
A log the server refuses is missing evidence, not an absent artifact: the
target's `manifest.json` item becomes an `error:` status naming the pod and
container, the result's `errors` count includes it, and a warning names
it, so `errors: 0` means every requested log is in the archive. A
previous-container log that does not exist stays silent (the container
never restarted); a refused one is a warning.
An entry is left out of the archive when its file name is not a plain
file name: it has a character other than letters, digits, `.`, `_` and
`-` (a cluster id, namespace, pod or container name is part of it), it is
over 255 bytes, or its part before the first `.` is a Windows device name
such as `con` or `aux`. The `manifest.json` item is an `error:` status (a
cluster whose id is left out is listed as the item `cluster`), the
result's `errors` count includes it, and a warning names the value.
Each pod has its own directory under `logs/<cluster>/<target>/`: the
current log of a container is `<pod>/<container>.log` and the log of its
previous run is `<pod>/previous/<container>.log`. An entry whose name was
already written (ignoring letter case) is left out and shows like any other
left-out entry. A cluster or target named twice in `--clusters` or
`--targets` is used once. A long pod name can make a log path longer
than 260 bytes, which some Windows extractors refuse unless long paths are
turned on.
`support-bundle` is deliberately the half of a two-bundle story that covers
only what Rancher's Kubernetes API can reach: pair it with
[rancherlabs/support-tools](https://github.com/rancherlabs/support-tools)'
node collector for the
node-level half (journald, the system-agent unit, OS state) that no API
call can reach.

## Output formats

`-o`/`--output`: `table` (default, for users) · `wide` (table plus the
wide-only columns: a kind's Wide-marked columns, or the priority columns the
cluster's API server sends for a built-in kind - presentation, never a scope
change: a payload with no wide-only columns renders identically to `table`) ·
`json` · `yaml` ·
`name` (only the NAME column, one per line) · `template=<go-template>` - a
Go template over the `data` of the JSON output (same shape as `-o json`).
Any other value is rejected with a `VALIDATION` error before the command
runs. Every kind has its own table, its columns or the cluster's own (next
paragraph); there is no pretty-JSON fallback under `table`/`wide` - a kind
that did not implement it would fail with a rendering error instead of
silently dumping JSON, so use `-o json` for the versioned envelope.

The table of a built-in Kubernetes kind (node, namespace, pod, deployment,
service, configmap, secret, statefulset, daemonset, replicaset, job, cronjob,
ingress, persistentvolume, persistentvolumeclaim, serviceaccount, event,
horizontalpodautoscaler, limitrange, networkpolicy, poddisruptionbudget,
resourcequota, storageclass) comes from the cluster's API server, as
kubectl's does: the server's column names and cells, the same as
`kubectl get` shows, and with `-o wide` the columns the
server marks as wide. The columns follow the cluster's Kubernetes version.
`json`, `yaml`, `name` and `template=` ask for the object as before. A
namespaced kind listed without `--namespace` has a NAMESPACE column first, and
an empty list prints its header line. If the server answers without a table,
the CLI shows NAME and AGE with a warning. The kinds of Rancher's own APIs
keep the CLI's columns.

Some commands (e.g. `logs`) render a plain-text default
under `table`/default format instead, with warnings on stderr as
`# warning: <w>` lines; `-o json`/`yaml`/`template=`/`name` still see the
normal envelope over `data`.
Every text format - `table`, `wide`, `name`, the text of composites and
`access`, the dry-run preview, and the `# warning:` lines and `error (...)`
lines on stderr - removes control characters (including the escape character
that starts an ANSI sequence, carriage return and tab) and Unicode format
characters from server-sourced text before it reaches the terminal, so server
text cannot drive the terminal. Progress messages on stderr get the same
treatment under every `-o` format. Table cells, `-o name`, the `# warning:`
and `error (...)` lines, the summary line and the dry-run preview also remove
newlines, so a value cannot add a row or a line there. In the text of
`inspect cluster`, `inspect deployment`, `inspect project`, `inspect user`,
`config status`, `config set-token-reuse`, `create token`,
`project remove-namespace`, the OIDC client commands and `auth status`, in the
revoke lines of `auth logout`, `auth delete-token`, `config delete-server` and
`config update-server`, and in the messages `auth login` and the kubectl plugin
print, free text such as a display name, description, condition message or
error text, and timestamps, token names, user ids and auth config names,
whether read from the server or from config.yaml, lose their newlines too.
Verbatim by design, and untrusted content when echoed: `json`, `yaml` and
`template=` (the bytes are
the data), `logs` (container output whose newlines are its structure), and the
file and script output of `kubeconfig`, `create kubeconfig` and `completion`.
`-o yaml` and `kubeconfig` write DEL, the characters U+0080 to U+009F, U+FFFE
and U+FFFF as YAML escapes (`\x7F`, `\N`, `\uFFFE`) inside a double-quoted
string, which YAML readers decode back to the original character, and `-o json`
writes them as they are.
A value a command shows only once (a bearer token, a client secret) that holds
such a character prints in Go's quoted form, and its line says so; `-o json`
carries the value as it is.
Structured output (json, yaml, and template)
omits `metadata.managedFields` (kubectl-parity; no opt-out flag - read the
raw API for
[server-side-apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
forensics). The structured output of `get` also leaves out the values a
preview hides, except that `get secret` shows `data` and `stringData`.

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

Each command in `rancher schema -o json` also carries `serverAccess`, the
most it may do on the Rancher server: `none` (never contacts it), `read`
(reads server state only) or `write` (may create or delete server objects;
the command's `desc` names which). A mutating command is always `write`;
`auth logout`, `auth delete-token` and `config delete-server` are `write`
because they delete the tokens the CLI created. A command card's contract
line shows it as `server=read` or `server=write`.

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
being written to (stdout or stderr, resolved independently) is a
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
| `AMBIGUOUS_SELECTION` | 2 | more than one value matched and none was selected; the error lists the candidates the command holds |
| `CONFLICT` | 5 | a write collided with existing state: a resource changed on the server since it was read, a name already exists, a server entry is registered with a different URL or CA, the config directory changed while the command ran, or the server answered 409 |
| `LOCK_TIMEOUT` | 8 | a config-directory lock was not released within the wait, usually because another rancher process is writing |
| `AUTH` | 3 | authentication failed: the server rejected the credential (HTTP 401), Rancher refused the sign-in, the exchange with the identity provider failed or timed out, an environment URL was set without RANCHER_CLI_TOKEN, or the credential belongs to another Rancher user than the call names |
| `CREDENTIAL_REPLACED` | 3 | the stored credential for the server entry was replaced by another config directory that shares this one's identity, or by a login in progress |
| `LOGIN_INCOMPLETE` | 3 | a login for the server entry is in progress, or ended before it recorded its credential identity |
| `CREDENTIAL_TRANSITION_INCOMPLETE` | 3 | a change of this directory's credential store has work left: the old copy of the credential it changed, or the store pin, is not settled yet; a second store change is refused until it is, and other commands proceed |
| `CREDENTIAL_STORE_UNAVAILABLE` | 14 | the credential store this directory uses cannot be read or written right now: a locked keychain, a keychain prompt that was cancelled, a keychain that cannot be reached, or a credential helper that is missing, timed out or failed; the message names the store, the entry, the cause as far as it is known, and the remedy |
| `NOT_FOUND` | 4 | no such resource, server entry, or name: the server answered 404 for an object or for an API through a downstream cluster, the config directory has no such entry, or the name matched nothing in already-fetched data; when the command holds the list of names it could have meant, the error lists them as candidates |
| `RBAC` | 6 | the server denied the request for lack of permission (HTTP 403), including Rancher's proxy rejecting an invalid or insufficient token |
| `VALIDATION` | 2 | the request was refused before or by the server: a missing or invalid flag or argument, a mutation under read-only mode, a disabled feature, or an object the server refused (HTTP 400 or 422) |
| `PRIME_REQUIRED` | 9 | the command requires Rancher Prime and the connected server does not have it |
| `TLS_TRUST` | 10 | the server's TLS certificate is not trusted and was not accepted |
| `CAPABILITY_UNSUPPORTED` | 11 | the workflow the command needs is not supported by the connected server or by this CLI |
| `INTERRUPTED` | 130 | the command was ended by Ctrl-C or a termination signal |
| `GENERIC` | 1 | an error with no more specific code, including a server or transport failure |
<!-- /conformance:error-codes -->

When a candidate list is cut to its first entries,
`data.error.candidatesTotal` gives the size of the full list.

When the error cannot be encoded in the chosen format, or its write to
stderr fails once, stderr gets the one line `error (CODE): message`, the same
line under `-o json` and `-o yaml`. When stderr cannot be written at all,
nothing is written and the exit code is the error's own.

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

A hint or remedy that changes something carries `--dry-run`, and the
message says to add the gates of the command (`--apply`, and `--yes` for a
destructive change) after reading the preview. A command that deletes a
token or kubeconfig the CLI created or recorded keeps `--apply --yes`: the
header of a created kubeconfig file and the text that says how to create a
token or kubeconfig again name the delete that way. A remedy command ends
its sentence: no comma, period, parenthesis or word follows its last
argument. A name that starts with `-` follows the command's flags and `--`,
and is read as a name. These rules hold for every text. The exceptions keep
`--apply --yes`: a command that deletes a token or kubeconfig the CLI created
or recorded, and the exec plugin's `create kubeconfig` remedy when no terminal
can sign in.

A remedy command in an error's message, hint, warnings or details, in the
warnings of a successful run and in the prompter's notices names `--config`
and the config directory (absolute, shell-quoted) after `rancher` unless a
command pasted without `--config` in the same environment would open the same
directory. So a run against a directory other than the platform default
names it (chosen with `--config`, `RANCHER_CLI_CONFIG_DIR` or a custom
`XDG_CONFIG_HOME`), and so does a run against the platform default when
`RANCHER_CLI_CONFIG_DIR` or `XDG_CONFIG_HOME` points elsewhere; a run whose
directory is the default and nothing else says otherwise prints no
`--config`. Result text, the summary line, the dry-run preview and the json and
yaml output, result data included, name the directory like the other channels,
and so do `notRevoked[].error` and `notRevoked[].recovery` of `config
delete-server`, `config update-server`, `auth logout` and `auth delete-token`;
the text of `rancher logs`, table, wide, name and template output print remedy
commands without it. Errors rendered before the directory is
resolved (an unknown command or flag, a flag value that is another flag)
print them without it too, and so do commands that read no directory
(`--help`, `--version`, `rancher schema`, `rancher completion`,
`rancher skill`). `kubeconfig` names the directory in every remedy, the
default included; `auth get-token` does too, except in a call with
`--user-id` and no `--name`, and so does the legacy `token` command.

## Read-only mode

Any one of three sources puts a run into read-only mode, which refuses
every command that makes changes before any network call; once on,
nothing in that run turns it off:

- Config: `readOnly: true` (top-level config field).
- Env: `RANCHER_CLI_READONLY`.
- Flag: `--read-only` (global); refused on local commands (see Scoping).

> [!IMPORTANT]
> `RANCHER_CLI_READONLY` follows non-empty-means-set - the literal string
> `"false"` still turns read-only mode ON. Every `RANCHER_*` variable in
> this CLI behaves this way.

Refusal is `VALIDATION`, naming the command and the active source(s) (e.g.
"enabled by RANCHER_CLI_READONLY"). Nothing in a run turns read-only mode
off; to run the command, remove every source the message names.
`rancher schema -o json` reports the resolved state (`readOnly`,
`readOnlySources`) so an agent can check before trying.

Scoping, stated honestly: read-only mode refuses `Mutating` commands - the
CLI's managed-resource surface - and the typed flag is also refused on local
commands (below). Under config or environment read-only mode, local commands
(`auth login`, `auth status`, `config *`, `kubeconfig`, `skill *`,
`completion`) stay usable; otherwise read-only mode would also stop you from
logging in and editing the config. A typed `--read-only` on a local command
is refused with `VALIDATION` before the command does anything
(`--read-only=false` included), with no hint: run the command without
`--read-only` and without any value typed with it. The environment variable and the
config field are the only forms that apply to every run. An agent that types
`--read-only` on every call loses `auth status`, `config *` and
`kubeconfig`; set `RANCHER_CLI_READONLY` for the process instead.

Under config or environment read-only mode the CLI still revokes its own
tokens: `auth logout`, `auth delete-token` and `config delete-server` delete
the tokens the CLI created and recorded (see
[Revoking the CLI's tokens](#revoking-the-clis-tokens)).

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

One command refuses on a server below a named version. On Rancher before
2.15, `create kubeconfig` with a token the CLI created for a login (or an ext
token in `RANCHER_CLI_TOKEN`) ends in `CAPABILITY_UNSUPPORTED` (exit `11`),
names Rancher 2.15 and the remedy: log in with an API token
(`rancher auth login <name> --token-stdin`) or set `RANCHER_CLI_TOKEN` to one.
The error has no hint. Its message ends with the login command only when the
stored login's server entry accepts `rancher auth login` (a plugin login entry
does not); otherwise the message names `RANCHER_CLI_TOKEN`. A server whose version
cannot be read, or that reports no version number, keeps the server's own
error.

Graduation intent, not implemented today: refuse-with-escape-hatch (e.g. a
`--allow-unsupported-server`-style flag) once there's real-use feedback on
the warn-only period.

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
  when the login created the entry). For the exec plugin in a config
  directory whose path holds characters a terminal line cannot show, both
  recovery `command` fields are empty and the message says why: no command
  can name that directory.
  Each superseded token in `prior.supersededTokens` carries `server`, the
  canonical URL of the server it lives on.
- `operationMarker`: the tokens a password or device-code sign-in creates
  carry `[op:<marker>]` in their description, so a leftover can be found
  by listing the collection its `kind` names and revoked with the
  `recovery` command.
- `primary.details`: the certificate's facts (`kind`, `fingerprint`,
  `subject`, `issuer`, `notBefore`, `notAfter`) when `primary` is a
  `TLS_TRUST` error, including the check of the signed-in user against
  `--user-id` in the kubectl plugin's sign-in. Absent for any other failure.

Login deletes the transient session token (a kubeconfig token after a
browser (SAML) sign-in) with its own bearer through
Rancher's logout endpoint (`POST /v1/logout`, which deletes the token its
bearer names), and sends the delete through the Rancher proxy when logout
does not answer 200 or 204. A standard user cannot delete a
`management-token` through the proxy, so a token neither request
deleted (a provider set to forced single logout fails logout) stays valid
until it expires. It is listed in `residualTokens` (on a failed login, in
`sessionToken`) with its `cleanup.error` naming both answers, for example
`logout 500; delete 403 Forbidden`. The text `<ref> expires at <time>; an
administrator can delete it sooner with: <command>` (`<ref> expires; an
administrator can delete it sooner with: <command>` when the server
reported no expiry) is in
`warnings` on a successful login and in `details.recovery` on a failed one;
it carries no command hint, because
only an administrator can run the command.

A leftover derived token is always a `token`. A leftover session token is
whatever kind Rancher created it as: `management-token` (a deprecated
kind: deleting it needs `--allow-deprecated`) or, on a Rancher that has
moved session tokens to the new API, a `token`; the failure details say
which (`sessionToken.ref.kind`, or `sessionToken.kind` when the name is
unknown). When the sign-in response of a password or OAuth login was
lost, the kind is unknown too and `recovery` names both collections, so
the marker search cannot miss the token. The management-token list and
delete commands are an administrator's to run. A pasted or piped token is
yours and is never revoked.

A browser (SAML) sign-in is different: Rancher creates a kubeconfig
token from the browser, with its own description, so it carries no marker.
A kubeconfig token is always a `management-token`, and deleting it needs
`--allow-deprecated`. When the CLI learned its name from the sign-in,
`recovery` gives the exact `management-token` command, which is an
administrator's to run. When it did not, `sessionToken.search` carries
what the CLI knows: the auth provider, when the browser was opened
(`handoffAt`), when the CLI stopped waiting (`pollEndedAt`, which is not
a bound: the browser sign-in can complete later), and, when the server
declared one, the request expiry (`requestExpiresAt`). Those facts narrow
a manual check; they cannot identify the token. An administrator can
inspect the candidates with `rancher get management-tokens -o json`; the
user who signed in can find them in the UI under Account & API Keys.

## Revoking the CLI's tokens

`rancher auth logout`, `rancher auth logout --all`, `rancher auth
delete-token` and `rancher config delete-server` delete the CLI's local
copies - the login token in every backend and the exec-credential cache
records - then delete on the server ("revoke") the tokens among them the CLI
created and recorded:

- a cached cluster token whose cache record carries `tokenRef`, revoked with
  the token itself as bearer;
- a superseded token of the same server and the same owner, revoked with the
  stored login;
- the login token itself, last, with itself;
- an unscoped v3 token - a cache token with no cluster, cached by the legacy
  `rancher token` command - and a v3 login token, through Rancher's logout
  endpoint with the token itself as bearer; when logout does not answer 200
  or 204 a `DELETE` of its `management.cattle.io` object is sent, again with
  its own bearer (a standard user's answers 403);
- a v3 superseded record that is the stored login's own token (the record a
  logout leaves when its login delete failed), revoked the same way with that
  login once the run deletes the login, and `notDeleted` until then.

The CLI cannot delete a cluster-scoped v3 token (Rancher refuses it on every
request that names no cluster) or a superseded v3 record whose bearer it does
not store (a standard user cannot delete a v3 token with another token's
bearer). Both are reported with reason `v3`.

Every revoke runs after the local delete, outside the config lock, one at a
time, under one 10 s budget. A request goes only to a server a current config
entry names, with that entry's CA (or `RANCHER_CLI_CACERT` for an entry
`auth login` saved without a CA under that variable): a bearer is never sent
anywhere else.
Read-only mode set by config or the environment does not stop these revokes
(a typed `--read-only` is refused on these commands). The command exits 0
when every local delete succeeds, whatever the server answered; a Ctrl-C or
termination signal that stops the local delete, or leaves a token
unreached, ends it with `INTERRUPTED` (exit 130), both lists in the error's
details (`config delete-server` then leaves the entry for a rerun to
remove, and when its login token was deleted the tokens recorded on it are
`manual`). A signal during the wait for the first config lock stops the
command before anything changes. A signal after the local delete that
leaves no token unsent does not stop the command: it ends normally, and
`config delete-server` still removes the entry - though if that last step
then fails (a `CONFLICT`, say), the error is reported as `INTERRUPTED`, its
message and details kept. A process killed after
the local delete loses the report of the tokens it had neither revoked nor
recorded; none of them were revoked.

A login token delete that fails is the command's failure (a non-zero exit;
`INTERRUPTED` for a signal), with both lists in the error's details, and
the deletes of the other targets' login tokens stop there. When the store
the CLI reads no longer holds the token after the failure (the keychain
item was deleted, then removing its identity record or deleting another
backend's copy failed), the login still counts as deleted: it is revoked
and reported by its outcome, the entry stays logged out, and `deletedFrom`
names the backends it was deleted from. `auth logout` does not list the
target in `loggedOut`. A copy the failed step left (a plaintext token in an
unreadable `credentials.yaml`, say) stays; the error names the step, and a
rerun deletes the copy once that step can succeed.

`rancher config update-server --url` deletes the entry's credentials and its
own cache records but sends nothing to the old address. The tokens the CLI
created there stay recorded with `server` set to the old server. On the moved
entry they are `otherServer`, `manual`: no login of the new address retires
them, so run their `recovery`. On another entry that still names the old
server they are `notAttempted`, and that entry's next login retires them. Its
data adds only `notRevoked`.

The data of each command gains two lists, `[]` when empty, never null:

- `revoked`: `{ref, source, server}` for each token the server confirmed
  deleted (200 or 204, or 404 to another token of the same owner).
  `source` is `cache`, `superseded` or `login`.
- `notRevoked`: `{ref?, source, server, entry?, reason, code?, error?,
  certificate?, retry, recovery?, loginAs?}` for each token that may still be
  live. `entry` is the entry the token is recorded on, `error` the answer or
  transport error behind `failed`, `recovery` a command sequence; for a v3
  token it is the administrator's delete alone. `loginAs` is the user the
  first command of `recovery` logs in as, the token's owner; it is omitted
  when the recovery needs no login or the token's owner is unknown. The owner's
  id has control characters, invisible format characters (bidi overrides and
  isolates among them) and the line and paragraph separators removed; an id
  that is empty after that is treated as an unknown owner, so `loginAs` is
  omitted. `code` is `TLS_TRUST` when the delete failed because the server's
  certificate failed verification (it is absent for any other failure),
  `error` then says how to fix the certificate problem for what revokes the
  token, and `certificate`
  holds the certificate's facts: `kind`, `fingerprint`, `subject`, `issuer`,
  `notBefore`, `notAfter`. When the config directory's path has characters a
  terminal cannot show, no command names it: each `recovery` item of `config
  delete-server` and `config update-server` holds the sentence that says so,
  and each `recovery` item of the details of a failed `auth logout` or `auth
  delete-token` is an empty string, so the list keeps its length.

The text output lists one line per token under `tokens on <server>:`, for
example `revoked ext/abc (cluster token, c-1)` or `not revoked ext/def
(unconfirmed; recorded on prod: the next login of prod as u-1 revokes it)`.
A token whose `code` is `TLS_TRUST` is followed by the message and the
certificate's lines, indented under its line. A user id in these lines and in
the warnings that name the next login has control characters, invisible
format characters (bidi overrides and isolates among them) and the line and
paragraph separators removed; when nothing of the id remains, the text leaves
out `as <id>`.

| `reason` | Meaning |
|---|---|
| `failed` | any other answer, or a transport error (`error` holds it) |
| `unconfirmed` | 401 or 404 to the token's own bearer; for a v3 token, 401 from logout (the token is deleted, expired or disabled) or 404 to the delete |
| `forbidden` | 403 to an ext token's own bearer; the record is removed |
| `budget` | the 10 s budget ran out before this token |
| `cancelled` | a Ctrl-C or termination signal |
| `noBearer` | no readable bearer matches the token's `ref` |
| `v3` | a cluster-scoped v3 token, or a v3 record without its bearer: the CLI cannot delete it; an administrator can run `rancher delete management-token <name> --apply --yes --allow-deprecated`, or it expires |
| `unproven` | no `tokenRef`: nothing proves the CLI created it; find it with `rancher get tokens` |
| `noEntry` | its parent entry no longer exists, or no current entry names its server; nothing is sent |
| `otherServer` | a token of a server the entry no longer names |
| `notAttempted` | `update-server`: recorded on another entry that still names its server |
| `notDeleted` | its local copy was not deleted and it stays in use; run the command again |
| `otherOwner`, `unknownOwner` | the token's owner is not the entry's user, or is not known |
| `noCliBearer` | a superseded record under a pasted login |
| `userSupplied` | the pasted login token itself; not the CLI's to revoke |

`retry` says what revokes the token later:

| `retry` | Meaning |
|---|---|
| `nextLogin` | the next login the CLI creates for `entry` revokes it: `rancher auth login <entry>` without a pasted token, or the kubectl sign-in through a plugin login entry. Given only when the token's owner equals the entry's `tokenUserId` (both known) and its server is the entry's current one; never for a v3 token |
| `manual` | run `recovery`, or the reason's remedy; for a v3 token, an administrator's delete |
| `none` | a pasted token |

A command warns once per `manual` token, and once per entry for its
`nextLogin` tokens.

`rancher auth logout <entry>` deletes only the cache records its login made:
records naming that entry, and records of removed entries. A record another
entry still reads stays. It clears the entry's `tokenRef` and stamps
`loggedOutAt` on every target. When a logout fails, the error's details carry
both lists.

`rancher auth delete-token` takes the config lock and refuses an unparseable
`config.yaml` before it deletes anything. `data.deleted` lists the deleted
cache keys, with `--all` too. `data.kept` lists records the exec plugin
rewrote meanwhile; they stay, with a warning. `--all` prints `deleted N
cached credentials`. Under `--all` a cache file that cannot be read is
removed, and its tokens can be neither revoked nor recorded.

`rancher config delete-server` removes the entry after the revokes only while
it still carries the `loggedOutAt` stamp. A login or a URL change in between
makes it refuse with `CONFLICT` and keep the entry. `revoked` and
`notRevoked` report each token; one left on the removed entry is `manual`,
with its `recovery`.

`rancher auth list-tokens` has a `TOKEN` column: the kind and name from the
record's `tokenRef`, or `-`. In JSON a record adds `tokenRef` (`{kind, name}`),
`tokenUserId` and `tokenAuthConfig` when present. The exec plugin records them
only for a token it created in the same call, never for the stored login it
hands out as is on a Rancher without `ext.cattle.io` tokens.

A superseded record carries `server`, the canonical URL of the server the
token lives on. `rancher config status` shows it, gives a record whose server
the entry no longer names the state `otherServer`, and prints its recovery
against that server. The retirement pass, at the next `rancher auth
login` or plugin login, skips records of another server, leaves v3 records
unattempted (the CLI cannot delete them without their bearer), drops records
whose `expiresAt` passed more than an hour ago, and attempts nothing under a
pasted login.

## Auth configs (`--auth-provider`)

A Rancher server can have several auth configs of one provider type (Rancher
v2.16 and later: two OpenLDAP configs, or two Azure AD configs). `--auth-provider`
on `auth login`, `auth get-token` and the legacy `token` names one auth config.
The name is the auth config's Kubernetes name, the `id` the server's provider
list (`/v1-public/authproviders`) shows, not the name the Rancher UI shows. The
menu, the candidates of an error and shell completion all list these names.

The value is matched in this order:

1. An auth config whose name is the value. If that auth config's type is one
   this CLI cannot sign in with, the call fails with `CAPABILITY_UNSUPPORTED`
   (exit 11), names the auth config and its type and the types it cannot use,
   and offers the auth configs it can sign in with; it does not go on to the
   next step. The remedy is the login that takes an API token piped to it
   (`auth login <name> --url <url> --token-stdin`), for a token created in the
   Rancher UI. The hint is that login when it runs as written. Otherwise the
   hint is empty: the call has no entry name, an inline CA, or (for `auth
   login`) an entry that changed since it was registered; the message then
   holds the command, or says to check the entry.
2. Otherwise every auth config whose provider type (`openLdapProvider`) or
   provider name (`openldap`) is the value. One match is used. With several, a
   terminal gets a menu of only those; with no terminal the call fails with
   `AMBIGUOUS_SELECTION` and an error that lists their names, each valid as
   given to `--auth-provider`. Through the exec plugin, its remedies name
   `--auth-provider` with the auth configs that sign in without a terminal;
   when none does, or under `--no-input`, the refusal is `AUTH`.
3. When no auth config this CLI can sign in with matched, an auth config of a
   type this CLI cannot sign in with whose provider type or provider name is
   the value fails the way an auth config named by the value does (the first in
   the server's order when several match), also when the server has no auth
   config this CLI can sign in with. A call with `--user-id` matches by name
   only (the first and last items).
4. No match is `NOT_FOUND`, with the auth configs this CLI can sign in with as
   candidates.

Every password and OAuth sign-in sends the chosen auth config's name as
`configName` in the login request, which Rancher v2.16 needs for LDAP, Active
Directory and Azure AD. When the chosen type has several auth configs, the
sign-in first says which one it signs in through, before the first prompt. An
entry of the provider list with no `id` is named by its provider name and sends
no `configName`.

A SAML sign-in goes through the Rancher sign-in page in the browser, which
makes its own choice among the SAML auth configs. The CLI cannot pass the
chosen one to it, and says so when several SAML auth configs are enabled.

Without `--user-id` the flag chooses the auth config of a new sign-in only. A
cached exec credential, a stored login and the plugin-created login (token
reuse on) are served whatever `--auth-provider` says.

With `--user-id` the flag also decides which tokens the call may use (see [The
user a kubeconfig names](#the-user-a-kubeconfig-names---user-id)). The value
must be an auth config's exact name, a Kubernetes object name such as
`corp-ldap`: a provider type such as `openLdapProvider` is `VALIDATION`, and a
provider name such as `openldap` that no auth config is named by fails at the
sign-in with `NOT_FOUND`, before any prompt, with the auth configs this CLI can
sign in with as candidates. The
token every call hands kubectl was made through that auth config, as the
principal Rancher reported when it created the token says (its name begins with
the auth config, `<auth config>_user://<id>`, or is `local://<id>`).

A stored login records the auth config of the token its login created
(`tokenAuthConfig` in `config info` and `config set-token-reuse off`,
`authConfig` in `auth status`). A pasted token, a v3 API key included, records
none; the first token the plugin creates from such a login for a call that
names an auth config records the auth config Rancher reports, whichever it is.
A login whose token was made through another auth config is refused (see
below).

`auth login` warns when a login replaces the user of the server entry's stored
login: `server entry prod is now logged in as user u-abc12; it was logged in as
user u-xyz34`. It warns the same way when a login keeps the user and changes
the auth config: `server entry prod is now logged in through auth config
openldap; it was logged in through auth config corp-ldap`; one that changes both
gets both lines. It says nothing for a first login, for the same user or auth
config, after `auth logout`, or when either value is unknown (a pasted token
records no auth config, and one stored with `--no-verify` no user).

## kubectl integration

`rancher auth get-token` is a `client.authentication.k8s.io/v1beta1`
[exec-credential plugin](https://kubernetes.io/docs/reference/config-api/client-authentication.v1beta1/). Rancher-generated kubeconfigs invoke it to create
short-lived Kubernetes credentials; you don't run it directly. Older
kubeconfigs generated by the legacy Rancher CLI (which call `rancher token`
instead) also work after a binary swap - the compatibility shim delegates to
the same machinery. Flags: `--server`,
`--cluster` (required unless `--user-id` is given), `--name` (cache slot, keyed per name+server+cluster; omitted = no
caching, unless `--user-id` is given), `--user-id` (the Rancher user every token must belong to; see below), `--cacert`,
`--auth-provider` (an auth config name, see [Auth configs](#auth-configs---auth-provider); with `--user-id` the exact name, and only tokens made through it are used), `--auth-flow`. When `--server` or `--cacert` are
omitted, they fall back to the current server in config, and the CA is the
first of `--cacert`, `RANCHER_CLI_CACERT` and the saved CA of the server
entry the call identifies or whose URL matches. It supports SAML,
OAuth (device-code / auth-code), and basic auth flows; long-poll flows can
be canceled with Ctrl-C. List cached credentials with `rancher auth list-tokens`.
Clear them with `rancher auth delete-token [<name>]`. That deletes the
cache records, then revokes the cluster tokens the plugin created and
recorded in them (see [Revoking the CLI's tokens](#revoking-the-clis-tokens));
a token it could not revoke stays valid on the server until it expires, and
`rancher get tokens` lists it. The plugin's interactive sign-in (with
`--cluster` or `--user-id`, token reuse off) creates the cluster token itself:
Rancher creates a token for the sign-in, the plugin creates an ext cluster token
with it (with no cluster for a call with `--user-id` and none), and deletes the
sign-in's token. The cluster token belongs to the user who signed
in, who can delete it. On a server without `ext.cattle.io` tokens the cluster
token is a cluster-scoped v3 token the CLI cannot delete; an administrator can
run `rancher delete management-token <name> --apply --yes --allow-deprecated`
to delete it before it expires. When the sign-in's own token cannot be deleted (a provider set
to forced single logout), the call still succeeds and a warning says who can
delete it; the plugin-created login (token reuse on) warns the same way for
the token its sign-in could not delete. On a cache miss the plugin first
tries to create a token from a stored `auth login` credential; when that
credential lives in the OS keychain, this can trigger a keychain-unlock
prompt - CI and other non-interactive runs should run `rancher kubeconfig`
from a directory that declares the file store (log in with
`--credential-store file`), whose exec entry then reads that store whatever
kubectl's environment sets, or keep a warm `--name` cache.

The plugin opens the directory the exec entry records (`RANCHER_CLI_CONFIG_DIR`
in its `env`), and every command that reads a config directory it prints as a
remedy - in an error, a warning or a notice, its own and the shared ones it
relays - names that directory with `--config`, the default included, so it
runs from a shell that names another (`rancher schema`, which reads none,
stays as it is). `rancher kubeconfig` does the same. The legacy `rancher token`
shim's remedies carry no `--config`: its kubeconfigs record no directory.

`--name` also picks the server entry whose credential is used: the entry with
exactly that name, else the entry `<entry>` when `--name` is
`<entry>-<cluster>` with the call's own `--cluster` (the form `rancher
kubeconfig` writes). Only that entry's credential is used - another entry for
the same URL never stands in - and, with token reuse on, the plugin's own login
entry for the server is the one fallback. An entry registered at another server
is an error. When `--name` picks no entry, the plugin considers the entries for
the URL you logged in to that have a credential, the current server's entry
first. It uses the entry that comes first; if several tie for first, it fails
and names them, and if the store cannot read an entry that would come first or
tie for it, it fails with that error. With token reuse on and no such entry, it
uses the plugin's own login entry for the server, and fails when there is more
than one (see below). To
choose, add `--name <entry>-<cluster>` to the exec entry, or run `rancher config
use-server <entry>` (this switches the directory's current server for every
process using it; switch back with `rancher config use-server <previous>`) and
regenerate the kubeconfig with `rancher kubeconfig --cluster <id>`. The legacy
`rancher token` shim picks no entry by name and ignores the current server, so
it fails when several user entries have a credential, and, with token reuse
on, when no user entry has one and the server has more than one plugin login
entry; regenerate its kubeconfig, or clear the unwanted entry's login with
`rancher auth logout <entry>`.

A cached kubectl credential is served only while that choice would still be
made the same way. None is served once `--name` picks another entry, or an
entry at another server, nor while the call would be refused. One made from
the picked entry's own login is a miss after that entry's next login,
logout, removal or URL change. One made any other way - through the
plugin's own login entry, by an interactive sign-in, or for a `--name` that
picks no entry - is also a miss after a login, logout, removal or URL
change of any entry the choice reads, or a new entry it would read. With no
entry picked it is a miss after `rancher config use-server` too, when the
current server moves to or from one of the entries for the URL (not for the
legacy `rancher token` shim, which ignores the current server). Each one
whose choice depends on token reuse is a miss after `rancher config
set-token-reuse` changes it. A miss makes one fresh token for the cluster,
which is then cached. A sign-in for a `--name` that picks a plugin login
entry with no login is stored in that entry, so its record is that
entry's own: a miss after the entry's next login, logout, removal or URL
change, or a change of token reuse.

### The user a kubeconfig names (`--user-id`)

Rancher writes `--user-id <id>` into the exec entries of the kubeconfigs it
generates, with `--server`, `--cluster` for a cluster, and no `--name`; `rancher
kubeconfig` writes it after `--name` when the login the plugin will use
records its owner (the Rancher user id a token create, or `auth login`'s check
of a pasted token, reported). With it, the plugin hands kubectl only tokens
Rancher reports as that user's. Rancher's exec entries also carry
`--auth-provider <auth config>` beside it, and `rancher kubeconfig` writes that
after `--user-id` when the login records an auth config (and no notice when it
records none); with it, the plugin hands kubectl only tokens Rancher reports as
made through that auth config. The rules below apply to a call with an auth
config as noted:

- which stored login answers: with a `--name` that identifies a server entry,
  that entry; with none, or a name that identifies no entry, the entries for
  the server that record that user, user entries before the plugin's own login
  entries (read only with token reuse on), the current server first, then by
  name. The first with a login answers and the rest are not read; a failed
  read of an entry before it is the error. An entry that records no owner or
  another user is never chosen. With an auth config, the entries that record it
  come first, then the ones that record none, and an entry that records another
  is never chosen. When the entry the `--name` identifies has no login, the
  fallback is the plugin's own login entry that records that user (with an auth
  config: the ones that record it, else the ones that record none), and
  several of them are refused (`AMBIGUOUS_SELECTION`);
- the entry the `--name` identifies, when its login records another owner, is
  refused before any request (`AUTH`), naming a login of that entry with
  `--token-stdin`, or, for the plugin's own login entry, its removal, after
  which the next call signs in as the call's user in a terminal, or uses a
  login piped to `auth login <name> --url <url> --token-stdin`. With a cluster
  the error also names regenerating the kubeconfig from an entry that holds
  the user's login;
- the entry the `--name` identifies, when it holds a login that records another
  auth config, is refused before any request (`AUTH`), naming a sign-in through
  the call's auth config with `auth login <entry> --auth-provider <auth
  config>` and a token piped to `auth login <entry> --token-stdin` that was
  made through it; for the plugin's own login entry, which holds a login or
  last held one through another auth config, the removal as above. The owner is
  checked first, and an entry that records none or the call's goes on to the
  create;
- a token created from a stored login whose create response names another
  owner, or none, is revoked and nothing is cached (`AUTH`, or `GENERIC`
  for no owner); one whose response names another auth config, or none, is
  revoked the same way (`AUTH`). A response that names another auth config is
  recorded on an entry that recorded none, so a later call that names no entry
  goes on to the next one and a `--name` that identifies the entry is refused
  as above. The write that records an auth config is skipped, with the token
  still served, when the config lock is busy or `RANCHER_CLI_CONFIG_NO_LOCK`
  is set, when the entry's cache binding id changed or its cache writes were
  paused when the call loaded, and when the entry records an auth config
  already or records no owner; a later call tries again, and a failed write
  is no error;
- on a Rancher without `ext.cattle.io` tokens the call is refused
  (`CAPABILITY_UNSUPPORTED`): no response names whose login it would hand
  over;
- an interactive sign-in creates the cluster token itself, with the sign-in's
  login token as bearer, and deletes the login token. The create response names
  the owner, and with an auth config the auth config: another user revokes the
  token (`AUTH`), and so does another auth config (a browser sign-in chooses its
  own on Rancher's sign-in page) or none (`AUTH`). Only a response that names no
  owner makes one `SelfSubjectReview` through `/k8s/clusters/<cluster>`
  (`/k8s/clusters/local` without a cluster), whose `status.userInfo.username`
  names it; a failed request revokes the token with the request's error. With
  an auth config the sign-in accepts only an enabled auth config of exactly
  that name;
- the plugin-created login (token reuse on) makes the plugin's own login entry
  named for the server, the user and the auth config, `exec-login-<host>-<user
  id>-<auth config>` (with the port appended to the host when the URL names
  one; a call without an auth config has `-<user id>` alone), records both, and
  stores nothing unless one of its creates names the user and the auth config
  and neither names another. An entry under that name that records another
  user or auth config is not written over: the call signs in instead. A name
  longer than the credential store accepts, 467 characters, is refused
  (`VALIDATION`) with a sign-in through the auth config as a login of an entry
  of a shorter name, or a piped token, as the remedy.

Without `--cluster`, `--user-id` asks for a token for Rancher itself, with no
cluster: the sign-in creates it as it creates a cluster token, from a create
with no cluster, and it is checked as any other and cached in a slot of its own.
A sign-in in a terminal is kept as for a call with a cluster: the plugin's own
login with token reuse on, else the token until it expires. `--cluster` is
required without `--user-id`.

Without a terminal, the plugin's error names the ways to a login that need
none: a token of that user, made through the call's auth config when it names
one, piped to `auth login` - when the exec entry's
`--name` stands for the plugin's own login entry, which `auth login` refuses,
the login of a new entry, that entry made current and the kubeconfig written
again - a sign-in in a terminal as that user, which lasts until the cached
token expires when token reuse is off, and `create kubeconfig` only with a
cluster and when the directory's current server answers for the call's server
and holds a login that records that user and, for a call with an auth config, that
auth config (one that recorded none does not count), since it uses that login. A call with
`--user-id` and no `--name` names no `--config` in any remedy: the file is
Rancher's, and the environment that ran kubectl chose the directory; with a
`--name` every remedy does.

With token reuse on, a call without `--user-id` that reaches the server's
plugin login entries is refused (`AMBIGUOUS_SELECTION`) when no user entry
answers and there is more
than one, before any is read and whichever holds a login: one per user can
exist. The error names the login of an entry first, and last how to keep one.
The legacy `rancher token` is refused the same way.

A cached token is kept under the user id and the name, so a call with
`--user-id` and one without it never share a slot, and a call with `--user-id`
and no `--name` is cached. Its record is valid while the entries for the
server that record the user are as they were - none appears, goes, is
logged in again, or begins or stops recording that user - while the current
server among them and token reuse are as they were. Another user's entries
changing leaves it valid. `--user-id` must be a Rancher user id (a lowercase
object name such as `u-abc12`); typed empty, it is refused, as is a typed
empty `--cluster`. `--name` values and server entry names starting with
`user-id:` are reserved, as `legacy:` ones are; `rancher kubeconfig` refuses to
write a `--name` that would start with either. A new server entry name cannot
hold a control character, an invisible or direction-control character (Unicode
category Cf) or a line or paragraph separator, because a terminal removes them
when it prints the name and a command that names the entry would name another;
a name that is not valid UTF-8 is refused too. An entry already stored with such
a name keeps working. A login that records no owner (a
pasted token stored with `--no-verify`, or one made before owners were
recorded) gets no `--user-id`, and answers a call with `--user-id` only through
the `--name` that identifies it; `rancher kubeconfig` says how to log in so it
records one.

A cached token of a call with an auth config is kept under the user id, the auth
config and the name, so it never serves a call with another auth config or none.
Its record lists each of the user's entries with the auth config it recorded,
the ones that recorded none included, and is valid while they are as they were:
an entry that has since recorded an auth config ends it. A plugin-created
login that signs in through another auth config than the call's is refused
(`AUTH`) with nothing stored, as is one whose tokens name none.

## Token reuse for kubectl (opt-in)

By default every kubeconfig cluster logs in on its own: a cache miss for a
cluster the CLI has no stored `auth login` credential for runs an
interactive login, and the token that login creates is scoped to that
cluster. `rancher config set-token-reuse on` (config key `tokenReuse`,
default off) changes the miss path: the first miss for a server runs one
interactive login, stores the resulting unscoped login credential under a
server entry named `exec-login-<host>` (with the port appended when the URL
names one, for a call with `--user-id` the user id after it, and with
`--auth-provider` the auth config after that; the kubectl plugin refuses to
create it for a host that holds a character of the kind above) whose
`provenance` is `execLogin`, and creates a cluster-scoped
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
A line before the commands says who logs in: the token's owner when the CLI
knows it, else the token's owner or an administrator.
A superseded record whose server the entry no longer names has the state
`otherServer`, and its sequence runs against that server.
Every text rendering of the sequence ends with what it assumes:
`RANCHER_CLI_URL` and `RANCHER_CLI_TOKEN` are unset, and either would
override the current server the sequence selects. For a v3 token the
recovery is the administrator's delete alone, `rancher delete
management-token <name> --apply --yes --allow-deprecated`, and the text is
one line: `an administrator logged in to <server> can delete it; run:
<command>`, naming the record's server, else the entry's. `rancher config
delete-server` reports a token it could not revoke under `notRevoked`, with
the same recovery, in the invocation that removes the entry, so the last
reference to a live token is not lost with it.

Two `rancher` processes cannot log in to one server at once: the plugin
login holds a per-server lock across the whole interactive wait. A
concurrent kubectl blocked behind it waits up to 60 seconds and then fails
with `LOCK_TIMEOUT` (exit 8), which is retryable: the message names the
server being logged in to; retry once that login completes.

## Usage recording

`rancher telemetry` shows, turns on, turns off and deletes the usage data
stored in `<config dir>/telemetry`. Release builds send no recorded event. A
development build with the `telemetryupload` experiment sends them; see
[Experiments](#experiments).

| Command | What it does |
|---|---|
| `rancher telemetry status` | Shows whether recording is on and what decided it, the counts of recorded and dropped events, the contract version and the config directory id. In a development build with `RANCHER_CLI_EXPERIMENT` set, it also lists the active experiments and the upload endpoint; with an endpoint it shows the last upload attempt, the next attempt, the events waiting and the events of the current hour, and warns about an upload lock that cannot be read. Creates nothing and works while recording is off. |
| `rancher telemetry on` | Writes `telemetry: "true"` to config.yaml. Creates the config directory when it is missing and deletes nothing. |
| `rancher telemetry off` | Writes `telemetry: "false"`, then deletes the recorded events, the server facts, the config directory id and the upload state (the upload lock and the state file in `telemetry/upload`). Creates the config directory when it is missing. |
| `rancher telemetry reset` | Deletes the recorded events, the server facts, the config directory id and the upload state (the upload lock and the state file in `telemetry/upload`), and keeps the `telemetry` key. |
| `rancher telemetry show` | Lists the recorded events, oldest first; `-o json` carries every stored field of each event. In a development build with the `telemetryupload` experiment, a sent event is listed until the file that holds it is deleted. |
| `rancher telemetry upload` | Sends the stored events of finished hours and of the current hour to the `telemetryupload` experiment endpoint now, without waiting for the hourly limit. Development builds only, at most 30 seconds, never from the kubectl exec plugin; see [Experiments](#experiments). |

Only `rancher telemetry upload` makes a request, to the experiment endpoint,
through a function the CLI passes it; none of them reads a credential.

The config directory id is a random id stored for this config directory,
shown by `rancher telemetry status`. The contract version is the version of
the event format. The server facts are stored details about Rancher servers:
version, edition and install id. They come from the requests described below.

Each command run records one event, when recording is on and the config
directory already exists: a run never creates the directory to record into.
The event holds:

- the command, by its registered name (`get cluster`, `auth login`), and the
  names of the flags typed on the command line, never their values;
- the outcome (`ok` or `error`), the error code when there is one, and the
  exit code;
- how long the run took, as a bucket (`lt1s`, `1to5s` or `gt5s`);
- the operating system, the architecture and the CLI version;
- the client type (`person`, `agent` or `automation`), whether it ran in CI
  and whether kubectl ran it as the exec plugin;
- the server's version, edition and install id, when they are known;
- a random id for the config directory, a random id for the process and one
  for the event, and the hour the run happened (never a finer time).

Never recorded: resource, namespace, cluster, project, user or server names,
server URLs, positional arguments, flag values, file paths, tokens and error
messages.

A run records nothing when no command was chosen: an unknown command, a
group or `rancher` alone, `--help`, `--version`, a flag the CLI cannot parse
(an unknown flag, a flag with no value or a value of the wrong type, or a
true/false flag with a value that is neither), a line the CLI refuses before
running it (words after a lone `-`, a true/false flag typed with `=` and no
value, or a true/false word after a true/false flag), and a shell completion
request. The `completion`
command and the `telemetry` commands record nothing either. A refusal after
the command is chosen, such as a missing argument or an invalid `-o` value, is
recorded as an error with the code `VALIDATION`. Recording never changes a
command's output or exit code.

While recording is on and the config directory existed, a command that got an
answer from a Rancher server may ask that server for its version and edition
(`GET /rancherversion`, with no credential). When the command also called the
root API, it may ask for the install id too, with the command's own token.
Commands that call the root API include `access`, `inspect cluster`,
`get kubernetes-versions`, `support-bundle` and `kubeconfig`, and any command
that names a cluster other than `local` with `--cluster` or a project by name
with `--project`. A command that names only the `local` cluster, such as
`get node --cluster local`, does not ask for the install id. `auth login` asks
for it with the token it stored, after it checked a pasted token or signed in
through an auth provider; never for a pasted token stored with `--no-verify`.
`auth status` asks with the token it checked, after the server answered its
connectivity check. `auth get-token` and `token` ask for the install id only
when they do not serve a cached credential, with the token they signed in with
or the stored login. These requests go out after the output is written, add up
to 2 seconds, and never change the output or the exit code. A fact is asked
for again 24 hours after an answer the CLI could use or one it could not, 6
hours after a 403 or 404, and an hour after any other failure. After
`auth login`, and after an `auth get-token` or `token` call that signed in or
created a login, the install id is asked for again whatever failure is stored.
A call of `auth get-token` or `token` that signs in asks during the sign-in
instead, before it deletes the token it signed in with, and for the version
and edition when those are due too; kubectl can wait up to 2 seconds longer
for the credential. The first `auth login --url`, `auth get-token` or `token`
run that creates the config directory writes only the server's facts file when
it succeeds and recording is on, and records no event. A call that serves a
cached credential sends nothing. The answers reach the next recorded event,
never the one just written.

Recording is on by default. The first of these that applies decides,
in this order, and `rancher telemetry status` names it as `decidedBy`:

1. `RANCHER_CLI_TELEMETRY`: `true` turns recording on and `false` turns it
   off, whatever the key says. An empty value counts as unset. Any other
   value is ignored: the key or the default decides, and the telemetry
   commands warn.
2. The `telemetry` key of config.yaml: `telemetry: true` turns recording on
   and `telemetry: false` turns it off. The commands above write them, and
   YAML's own boolean spellings (`on`, `off`, `yes`, `no`) load as `true` and
   `false`. A missing key means the default. Any other value is ignored, the
   default applies, and `telemetry status`, `show` and `reset` warn.
3. The default: on.

`rancher telemetry off` and `reset` delete what is recorded and keep
config.yaml and the `pruned` marker in the telemetry directory.
`RANCHER_CLI_TELEMETRY=false` turns recording off and deletes nothing.
`RANCHER_CLI_TELEMETRY=true` turns it on whatever the key says, and
`rancher telemetry off` still deletes what is recorded and warns that the
variable keeps recording on.

A config.yaml the CLI cannot load stops these commands like every config
command: repair the file first, or choose another config directory.

## Experiments

`RANCHER_CLI_EXPERIMENT` turns on experimental settings. An experiment can
change or go away in any release. Only a development build reads the variable:
a build whose version is `dev`, `dev-<hex>` or `dev-unknown`. A release build
never reads it and prints no warning for it.

The value is a comma-separated list of `name` or `name=value` items:

- An item splits at its first `=`; the value runs to the next comma. An empty
  item is skipped, and nothing is trimmed. An empty value is the same as the
  bare name.
- A name is lower-case letters and digits, starting with a letter. Any other
  item is ignored with the warning `ignored an unrecognized item (item <n>)`,
  where `<n>` counts the items from 1, empty ones included. The warning never
  holds the item, because it can hold a password.
- A name that is not in the table is ignored with a warning that names it.
- The last item of a name decides. An invalid value leaves the experiment off,
  with a warning that names the experiment and the reason and never shows the
  value, and an earlier item of the same name does not apply.

A warning goes to stderr, or to `warnings` in `-o json` and `-o yaml`. There is
none for completion, `--help`, `--version`, the kubectl exec plugin (the
commands `auth get-token` and `token`, and any run with `KUBERNETES_EXEC_INFO`
set) or a command that fails before it runs. `rancher telemetry status` lists
the warnings in every other run, and none in an exec plugin run.

| Experiment | Value | Effect |
|---|---|---|
| `telemetryupload` | `scheme://host[:port]` with a path that is empty or `/`: `https`, or `http` for `localhost`, `::1` and `127.0.0.0/8`; a port from 1 to 65535, read as a number (leading zeros and the default port are dropped); a host that is an IP address or a DNS name in ASCII (write an international name in its `xn--` form); no user name, password, query or fragment | the upload endpoint, shown by `rancher telemetry status` as its canonical origin; a development build sends the stored events of finished hours to it, see below |

`telemetryupload` with no value warns that it has no endpoint, and
`rancher telemetry status` shows `upload endpoint: not set`. There is no
default endpoint.

With an endpoint, a development build sends the stored events after a recorded
run. Release builds never send. The kubectl exec plugin (the commands
`auth get-token` and `token`, and any run with `KUBERNETES_EXEC_INFO` set)
never sends, and nothing is sent while recording is off, including when
`rancher telemetry off` runs during an upload.

- When: at most once an hour for one config directory, after the run's event
  and the server facts refresh, inside the same 2-second deadline (counted from
  the end of the hourly pass over the stored files, which follows the event
  write, when the refresh sent no request). Nothing is sent
  when less than 1 second of the deadline is left. One process at a time sends
  for a directory.
- What: every stored event of an hour that has ended and is at most 30 days
  old, including events stored before the experiment was set and events a
  release build stored in the same config directory. The events of the current
  hour wait until the hour ends; `rancher telemetry upload` sends them now. A
  request holds one contract version, at most 1000 events and at most 1 MiB
  compressed, goes to the endpoint's origin as OTLP/HTTP JSON with
  `Content-Encoding: gzip`, and is never redirected.
- The request carries the CLI's User-Agent and no credential, and honors the
  proxy variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`).
- Decisions: the HTTP status and `Retry-After` only, never the answer's body. A
  2xx, a 400, a 413 or a 415 answer deletes the files whose events the request
  held (events in 400, 413 and 415 answers are counted as deleted, not sent).
  Every other answer, a redirect, a timeout and a network error keep the files
  and end the attempt. The next attempt is about an hour later, with up to 10
  more minutes after an attempt that kept nothing; after a kept answer it is
  the larger of 1 hour and `Retry-After`, or 1 hour doubled for each earlier
  attempt in a row that kept files, at most 24 hours.
- A file whose events were all sent is deleted. A file that is open in another
  program on Windows, and the files of an attempt that ran out of time, are
  deleted by a later attempt without sending their events again, as long as
  `state.json` still holds their record ids; it keeps the 1280 most recent, and
  events with older ids are sent again.
- `rancher telemetry status` shows the last attempt, the next attempt, the
  events waiting and the events of the current hour.
- `rancher telemetry upload` sends the stored events now, on request. It
  refuses with a `VALIDATION` error (exit 2) in a release build, in an exec
  plugin run, with no valid `telemetryupload` endpoint and while recording is
  off. It sends the events of finished hours and of the current hour, ignores
  the limit of one attempt an hour and any `Retry-After` wait, uses the same
  upload lock, request limits and decisions, records its attempt in
  `state.json` (the next automatic attempt comes about an hour later) and
  stops after 30 seconds. It exits 0 whatever the endpoint answered, when
  another upload holds the lock, when nothing waits and when the 30 seconds
  passed before the first request. It exits 1 with a `GENERIC` error that
  names the failed step and the requests already sent when `telemetry` or
  `telemetry/upload` is a symlink or not a directory, the upload lock cannot
  be read or created, the stored events cannot be listed, a request cannot be
  built or the sender fails; it exits 2 with a `VALIDATION` error when
  recording is turned off while it runs, and nothing is deleted; Ctrl-C gives
  `INTERRUPTED` (exit 130). Text output is one line. `-o json` and `-o yaml`
  carry `sent`, `refused`, `dropped`, `httpStatus`, `noAnswer`, `nextAttempt`,
  `timeLimitReached` and `skipped` (`lockHeld`, `nothingWaiting`, `timeUp` or
  empty). `sent` counts every event in a request the endpoint answered with a
  2xx, including the ones it refused; `refused` says how many. The upload
  command takes `--debug` like an automatic attempt, described below.

With `--debug` on a run that attempts an upload, stderr gets one line before
each request, `debug: telemetry upload: sending <n> events (contract
<version>) to <origin>`, and one after it, `debug: telemetry upload: answer
HTTP <status>, <k> refused` (or `refused count unknown`) or `debug: telemetry
upload: no answer: <timeout | network error | refused redirect>`. The lines
never hold event contents, ids or the configured text of the endpoint. A run
that attempts no upload prints nothing more.

Set the variable for one shell, not in a shell profile, and check
`rancher telemetry status`.

## TLS & proxy

Server connections verify TLS against a CA from `--cacert`, `RANCHER_CLI_CACERT` or
the server entry, which one depending on the command (see
[Which CA a command trusts](#which-ca-a-command-trusts)), or the system trust store
by default - verification is never disabled implicitly. HTTP(S) proxies are honored from the environment.

Trust is scoped to the Rancher server's origin. The configured CA (and the
legacy shim's `--skip-verify`) applies only to requests aimed at that
scheme, host, and port. An identity provider reached during an OAuth login
is verified against the operating system trust store, and its endpoints
must be `https`; `--cacert` cannot make a self-signed provider trusted. A
redirect to a different origin is never followed: the request fails with a
`GENERIC` error naming both origins, because a redirected request would
carry credentials to a server the configured trust was never meant to
cover.

A certificate failure from the Rancher server is a `TLS_TRUST` error (exit
10, not retryable) on every command that stops on the failed request. Two
groups keep their own handling: the revoke commands (`auth logout`, `auth
delete-token`, `config delete-server`) report it in their `notRevoked` list
with `code` `TLS_TRUST`, the message in `error` and the certificate's facts in
`certificate`, and keep exit 0 when the local deletes succeed; and steps that
collect across many requests (`support-bundle`, the per-cluster notes of
`access`, the degraded parts of `inspect`) record it as they do any failed
request. A wait (`--wait`, and the OIDC client commands) stops on it at once.
The error's `details` hold the certificate's kind, subject, issuer and
fingerprint (for `auth login` and the sign-in of kubectl's credential plugin,
inside the login failure's `primary` error). Where a command already wrote to
the server (the four OIDC client commands and `create user`), the error's
`details` are what the command already changed, and its message names the
certificate's subject, issuer and fingerprint. The message names the remedy
for the command that failed: a command that takes `--cacert` names it, `auth
get-token` and `rancher token` name their own flags, a command that reads a
server entry names `rancher config update-server`, and a server taken from
`RANCHER_CLI_URL` names `RANCHER_CLI_CACERT`. When `RANCHER_CLI_CACERT`
supplied the CA, the message says so. A certificate failure of an `https://`
proxy is not a `TLS_TRUST` error: it keeps the transport error's text.

There is no skip-verify flag anywhere in the native surface, ever: a
self-signed or private-CA server is used by pinning its CA (`--cacert`,
`RANCHER_CLI_CACERT` or the stored server entry's CA), never by turning verification off. The one
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

## What the CLI sends to the server

Every request the CLI sends to Rancher or to a downstream cluster's direct
endpoint carries a User-Agent that says which CLI version sent it, on which
platform, and how the process was started:

```
cli.rancher.io/<version> (<os>/<arch>[; exec-plugin][; agent=<id>][; ci])
```

| Example | Meaning |
|---|---|
| `cli.rancher.io/v0.1.0-alpha.8 (darwin/arm64)` | a terminal or a script |
| `cli.rancher.io/v0.1.0-alpha.8 (darwin/arm64; agent=claude-code)` | inside a Claude Code session |
| `cli.rancher.io/v0.1.0-alpha.8 (linux/amd64; exec-plugin)` | kubectl's exec plugin |
| `cli.rancher.io/v0.1.0-alpha.8 (linux/amd64; exec-plugin; agent=codex)` | kubectl run inside a Codex session |
| `cli.rancher.io/v0.1.0-alpha.8 (linux/amd64; ci)` | CI |
| `cli.rancher.io/dev-83a23a71 (darwin/arm64)` | a local build |

The product, `cli.rancher.io`, is the name the CLI also uses as its Kubernetes
field manager and in the `apiVersion` of its output envelope, so an
administrator can match audit entries and managed fields by one name.

The version is `unknown` when it is empty, longer than 64 bytes, has a
character outside the HTTP token characters, or contains "mozilla" in any
case (Rancher treats a User-Agent with it as a browser). The value is built
once per process, from the environment only; no file, keychain, terminal or
network is read.

### What each part means

- `exec-plugin`: `KUBERNETES_EXEC_INFO` is non-empty, which client-go sets on
  every call of an exec credential plugin. It marks only the plugin's own
  requests: creating the token at Rancher, and the check that the direct
  endpoint accepts it, which goes to that endpoint and not to Rancher. The API
  requests kubectl then sends carry kubectl's User-Agent. A hand-run
  `rancher auth get-token` is not the plugin.
- `ci`: `CI` is non-empty and not `false` or `0` (in any case), or
  `JENKINS_URL`, `TEAMCITY_VERSION`, `TF_BUILD` or `CODEBUILD_BUILD_ARN` is
  non-empty. `CODEX_CI` belongs to Codex and does not mean CI.
- `agent=<id>`: the process ran inside that agent harness's session. It says
  where the process ran; it does not show that the model chose the command.
  The first row that matches decides:

  | Order | Variables | Id |
  |---|---|---|
  | a | `CLAUDE_CODE_CHILD_SESSION=1`, or `CLAUDECODE=1` with `AI_AGENT` starting `claude-code` | `claude-code` |
  | b | `CODEX_THREAD_ID` non-empty, or `CODEX_CI=1` | `codex` |
  | c | `GEMINI_CLI=1` | `gemini-cli` |
  | d | `COPILOT_CLI=1`, also when a Copilot `AI_AGENT` is set | `copilot-cli` |
  | e | `OPENCODE=1` | `opencode` |
  | f | `CLINE_ACTIVE=true` | `cline` |
  | g | `ROO_ACTIVE=true` | `roo-code` |
  | h | `AI_AGENT` non-empty | its normalized value when it is on the list, else `agent` |

  The list: `claude-code`, `codex`, `gemini-cli`, `copilot-cli`, `opencode`,
  `cline`, `roo-code`, `copilot-vscode`, `copilot-cloud`, `crush`, `pi`. No
  environment value is ever copied into the User-Agent: an `AI_AGENT` that is
  not on the list is reported as `agent`.

  `AI_AGENT` is normalized first: lower case; cut at the first `@`;
  `github_copilot_<x>_agent` becomes `copilot-<x>`; otherwise cut at the first
  `_`. So `claude-code_2-1-308_agent` is `claude-code` and
  `github_copilot_vscode_agent` is `copilot-vscode`. `CLAUDECODE=1` alone is
  not detected: IDE extension terminals set it with no session.

  The variables of rows a to g are set inside an agent session, also for a
  command the user types there. When harnesses are nested, the first match
  can name the outer one. A harness the CLI does not detect can set
  `AI_AGENT` to its name.

### The variables the CLI reads for this

`CLAUDE_CODE_CHILD_SESSION`, `CLAUDECODE`, `AI_AGENT`, `CODEX_THREAD_ID`,
`CODEX_CI`, `GEMINI_CLI`, `COPILOT_CLI`, `OPENCODE`, `CLINE_ACTIVE`,
`ROO_ACTIVE`, `KUBERNETES_EXEC_INFO`, `CI`, `JENKINS_URL`, `TEAMCITY_VERSION`,
`TF_BUILD` and `CODEBUILD_BUILD_ARN`. The CLI defines none of them and reads no
other variable for this. A test checks that this section names every variable
the CLI reads for this.

### Limits

- The value is self-reported by the caller's environment. It never selects a
  credential, a server entry or a code path, and a server must not authorize
  on it.
- There is no way to turn it off in the CLI, and `DO_NOT_TRACK` does not
  change it. An administrator who must not record the header redacts it with
  a Rancher `AuditPolicy` (`additionalRedactions` with `headers:
  ["User-Agent"]`).
- Requests to an identity provider during a login carry Go's default
  User-Agent, not this one.
- The CONNECT request that sets up a tunnel to an `https://` server through an
  HTTP proxy carries Go's default User-Agent; the requests inside the tunnel
  carry this one.

### Where it shows up on the server

- Rancher's audit log, as `requestHeader["User-Agent"]`, when the audit log is
  enabled (it is off by default) and either its level is 1 or higher or an
  `AuditPolicy` keeps request headers. Level 0 records no headers, and an
  `AuditPolicy` applies only when the audit log is enabled. This covers every
  path through Rancher, including `/v3`, `/v1` and `/ext`.
- The audit log of a downstream cluster's API server (and of Rancher's own
  cluster), as `userAgent`, for `/k8s/clusters/<id>/api` and `/apis` requests,
  and for requests to the direct endpoint. Rancher forwards the header
  unchanged on those paths.
- Not in a cluster's audit log for `/v1` requests (`/k8s/clusters/<id>/v1/...`,
  and `/v1/...` for Rancher's own cluster): the cluster agent, or Rancher for
  its own cluster, makes the request to the API server with its own
  User-Agent, and a list may be answered from the agent's cache with no audit
  entry there. Rancher's own audit log still records it.
- Rancher stores it on no token or other object.
