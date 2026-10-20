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
- [Revoking the CLI's tokens](#revoking-the-clis-tokens)
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
on` or `off`.

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

Every variable below can also be set by a flag or the config file;
precedence is flag > environment > file.

| Var | Effect |
|---|---|
| `RANCHER_CLI_URL` | server URL |
| `RANCHER_CLI_TOKEN` | token, used directly and never persisted |
| `RANCHER_CLI_CACERT` | CA bundle (PEM), a path or the PEM text; which commands apply it is in [Which CA a command trusts](#which-ca-a-command-trusts) |
| `RANCHER_CLI_CREDENTIAL_STORE` | `keychain` \| `file` \| `auto` \| `exec:<name>`; chooses the store for a directory that has not declared one, and must not disagree with one that has (a different value is a VALIDATION error, not an override); a keychain directory reads the keychain only, so set `file` where no keychain is reachable. `rancher kubeconfig` sets it in the exec entry: empty for a directory that declares its store, else the value set when it ran |
| `RANCHER_CLI_CONFIG_DIR` | config directory; `rancher kubeconfig` sets it, absolute, in every exec entry it writes |
| `RANCHER_CLI_OAUTH_AUTH_FLOW` | OAuth flow: `devicecode` \| `authcode` |
| `RANCHER_CLI_OAUTH_CALLBACK_PORT` | local callback port for the `authcode` flow |
| `RANCHER_CLI_CONFIG_NO_LOCK` | disables the config directory's cross-process write lock; escape hatch for filesystems with broken file locking |
| `RANCHER_CLI_COMPLETION_DEBUG` | when non-empty, traces each phase of a shell completion request to stderr; stdout is unchanged |

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
   `rancher` (unless `rancher kubeconfig --exec-command` named another),
   and a credential store helper is invoked as
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
That holds on a line with a `--` too, where every word is an argument:
completion offers the values the command accepts at that position, never a
flag, and never runs the command.

### Live completion (opt-in)

After installing the script, enable live resource-name completion:

    rancher config set-completion --network=true

This completes cluster names (including `inspect cluster` and `inspect project`
positional names), project names, namespaces, pods, deployments, and nodes from
the Rancher API. A credential-store capability note is printed to indicate which
completions are available given your current auth setup.

`--auth-provider` on `auth login`, `auth get-token` and `token` completes the
names of the auth configs this CLI can sign in with, from the server's provider
list, which needs no credential: the request carries none. The server and CA
are the ones the command would connect with for the words typed: for `auth
login`, `--url`, else the entry named, else the current entry; for `auth
get-token`, `--server`, else `RANCHER_CLI_URL`, else the current entry (`--name`
chooses only the CA, and an entry at another URL gives no suggestions); for
`token`, the `--server` it requires and `--cacerts`. Nothing is offered with
`--skip-verify`, when the command would refuse the line, or when the server
cannot be reached or trusted.

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

## Cluster scope (--cluster)

`--cluster <id-or-name>` is looked up once per command, as you: among the
clusters Rancher's steve API lists for you (`rancher access clusters` shows
them), by id or display name, so a cluster owner or project member who
cannot read the local cluster resolves it like an administrator. An id wins
over another cluster's display name, with a warning naming that cluster;
two clusters with the same display name are `AMBIGUOUS_SELECTION` (pass the
id). A cluster created moments ago that the list does not show yet is found
by one read of its id. `local` is always the local cluster, and an omitted
`--cluster` means it where the command allows it. kubectl's exec plugin
resolves its `--cluster` the same way on a cache miss, with the warning on
stderr; its `NOT_FOUND` lists the clusters you can read.

`get` and `patch` of the `cluster` kind and `get projects` keep kubectl's
semantics: they go through the local cluster's Kubernetes API, where
Rancher answers clusters only for users with access to the local cluster,
and projects by your own permissions. A refusal names the request and the
caller-scoped command to use instead (`rancher access clusters`, `rancher
inspect cluster <id>`, `rancher access projects --cluster <id>`).

## Project scope (--project)

`--project <id-or-name>` names a project on the cluster `--cluster` names.
Only the commands that take it accept it: the generated `get`/`delete`/`patch`
of Downstream namespaced kinds and of namespaces, `create resource`,
`create namespace`, `create secret`, `project add-namespace`, `grant`,
`revoke`, `usage`, and `access can`/`list`/`who-can`; `rancher schema` lists
`project` among each such command's flags. Every other command refuses a
typed `--project` with `VALIDATION` before it sends a request, whether the
flag is typed before or after the command name.

The project is looked up once per command, as you: among the projects
Rancher lists for you on that cluster, by id or display name. An id wins
over another project's display name, with a warning naming that project;
two projects with the same display name are `AMBIGUOUS_SELECTION` (pass the
id). A project created moments ago that the list does not show yet is
found by one read of its id. Without `--cluster` the command refuses and
names the clusters a project with that id or display name is on; it never
picks one.

What the project does: a list is filtered to the project's namespaces, and
a named object or a mutation must be in one of them. `get namespaces
--project <p>` lists the project's namespaces by their label, server-side.
`create resource` refuses `--project` for a Namespace manifest (use `create
namespace --project`). `create secret --project <p>` writes a project-scoped
secret into the project's backing namespace and cannot be combined with
`--namespace`. `usage` refuses `--project` with `--by node` (a node row
mixing project-scoped requests with whole-node metrics would be misleading).
`usage` reads a `--namespace` or `--node` that matches no pods, so a
name that does not exist is NOT_FOUND instead of an empty result. A
caller who can list pods there but cannot read the namespace or node
gets the result with a warning that it could not be checked; one who
cannot list pods there gets the RBAC error from the pod list, as before.

A flag typed with an empty or blank value (`--project ''`, `--namespace=`,
`-o ' '`) is refused with `VALIDATION` before any request instead of acting
as if it were omitted; in a script, pass `--flag "$VAR"` only when `VAR` is
set. A bool flag typed with `=` and an empty or blank value
(`--apply="$APPLY"` with `APPLY` unset) is refused the same way; write
`--apply` or `--apply=true|false`. Positional arguments beyond the ones a
command takes (`rancher schema <command>` lists them) are refused the same
way, and so is a separate word after a bool flag that the flag accepts as
its value: a bool flag takes its value joined with `=` (`--dry-run=false`),
and an argument that reads as a bool goes before the flags or after `--`.
A `-` on its own ends the command line, so a line with any word after it
(`rancher get pods - --cluster c1`) is refused the same way, before or
after the command name; a `-` as a flag's value (`-f -`) or after `--` is
not affected, and `--help` typed after a command's name, or `--version`
at the root, before the `-` still shows the help or the version.
A command name the CLI does not know is reported as `unknown command`
before anything typed after it is read, so a wrong name is never hidden
behind a flag error; an empty name (`rancher get ""`, an unset variable)
is also reported as `unknown command`.

## Kinds & verbs

Every command that takes `--namespace` also takes `-n`, as kubectl does.

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
- `user`, `project`, `roletemplate`, `globalrole` and `nodedriver` take the
  object's id as the argument, named `id` in help and the schema: the NAME
  column of `get`, not the username or display name. When a named `get`,
  `patch` or `delete` misses and the typed text is exactly the username or
  display name of an object, the error message says which id that is, the
  object is listed in `data.error.candidates`, and the hint is `rancher get
  <kind> '<id>'`; several objects with that text stay `NOT_FOUND` with every
  candidate listed. The lookup is one more read with the caller's own
  credential and a 10 s limit, only after a miss; any failure leaves the
  original error. Nothing acts on a match. A project is looked up among the
  projects the caller can read on the `--cluster` cluster, and also after a
  refusal (RBAC), because a project member's role allows a get by id only;
  the error keeps its code and message. `get cluster` and `patch cluster`
  take an id or a display name.
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
`/metadata` or of the whole object whose value carries them. A merge
patch (`--type merge`) that sets or removes either field is refused the
same way. The server sets both fields; the CLI always sends them back
exactly as fetched.

Every `diff[].path` a dry-run reports (in `-o json` and in the default text
render) is an RFC 6901 JSON Pointer - `~1` for a literal `/` and `~0` for a
literal `~` inside a key, so a domain-prefixed label or annotation key
renders as one escaped segment. The string is usable verbatim as a JSON
Patch op's `path`.

`patch <kind>` takes an RFC 6902 JSON Patch (an array of ops) by default,
and an RFC 7386 JSON merge patch (an object) with `--type merge`. A merge
patch replaces each list it names as a whole; kubectl's default strategic
merge patch merges some lists by key instead, and the CLI does not offer
it. When a merge patch drops entries of a list the object has, the plan
carries a warning naming the list.

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
address) the proxy-only file is printed and one stderr notice says why; when
Rancher refuses the credential (401) at any of those reads, the command fails
with `AUTH` (exit 3) and prints no file.
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

The exec entry records what the file depends on. Its `env` sets
`RANCHER_CLI_CONFIG_DIR` to the config directory `rancher kubeconfig` ran
against, absolute, the default included, and `RANCHER_CLI_CREDENTIAL_STORE`
to the empty value when that directory declares its store, else to the value
set when it ran (or empty); kubectl adds the entry's `env` after its own
environment, so the plugin opens that directory and store from any shell or
working directory. The file therefore works for this user on this machine
only, and every run says so on stderr. A directory that is moved, renamed or
deleted strands its kubeconfigs: the plugin opens the recorded path, which
is then empty (a sign-in there starts a new directory), so write the
kubeconfig again from the directory's new place. `--exec-command <command>`
names the command the exec entry runs, default `rancher`: a bare name kubectl
looks up on its PATH, or a path, made absolute from the working directory (a
leading `~` or `~/` is the home directory; `~user` is refused). `installHint`,
which kubectl prints when the command cannot be run, names the install URL
for the default command, and for another the steps that write the kubeconfig
again from the entry it was written from (`rancher --config <dir> config
use-server <entry>`, then `rancher --config <dir> kubeconfig --cluster
<id>`); a file written in an environment-only session, which has no entry,
names the variables to set instead (`RANCHER_CLI_URL`, `RANCHER_CLI_CACERT`
when the file carries a CA, and `RANCHER_CLI_TOKEN`) before `rancher --config
<dir> kubeconfig --cluster <id>`. When the directory's path holds characters
a terminal line cannot show, no command is offered, in `installHint` or in any
remedy: the text says the path cannot be printed. A second stderr notice comes from the exec plugin's own lookup over
the store it will open: which stored login kubectl will use when it is not
this session's (under `RANCHER_CLI_TOKEN`, the plugin's own login entry, or
an entry named `<current>-<id>`), how to get one when none answers, that the
plugin will refuse the file and why, or that the store could not be read.
The notices are a snapshot: a later login or logout changes the answer.

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
restricts the run to one cluster of that set, by id or display name (an id
wins over another cluster's display name, with a warning).
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
| `CONFLICT` | 5 | a write collided with existing state: a resource changed on the server since it was read, a name already exists, a server entry is registered with a different URL or CA, the config directory changed while the command ran, or the server answered 409 |
| `LOCK_TIMEOUT` | 8 | a config-directory lock was not released within the wait, usually because another rancher process is writing |
| `AUTH` | 3 | authentication failed: the server rejected the credential (HTTP 401), Rancher refused the sign-in, the exchange with the identity provider failed or timed out, an environment URL was set without RANCHER_CLI_TOKEN, or the credential belongs to another Rancher user than the call names |
| `CREDENTIAL_REPLACED` | 3 | the stored credential for the server entry was replaced by another config directory that shares this one's identity, or by a login in progress |
| `LOGIN_INCOMPLETE` | 3 | a login for the server entry is in progress, or ended before it recorded its credential identity |
| `CREDENTIAL_TRANSITION_INCOMPLETE` | 3 | a change of this directory's credential store has work left: the old copy of the credential it changed, or the store pin, is not settled yet; a second store change is refused until it is, and other commands proceed |
| `CREDENTIAL_STORE_UNAVAILABLE` | 14 | the credential store this directory uses cannot be read or written right now: a locked keychain, a keychain prompt that was cancelled, a keychain that cannot be reached, or a credential helper that is missing, timed out or failed; the message names the store, the entry, the cause as far as it is known, and the remedy |
| `NOT_FOUND` | 4 | no such resource, server entry, or name: the server answered 404 for an object or for an API through a downstream cluster, the config directory has no such entry, or the name matched nothing in already-fetched data |
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
- Flag: `--read-only` (global); refused on local commands (see Scoping).

> [!IMPORTANT]
> `RANCHER_CLI_READONLY` follows non-empty-means-set - the literal string
> `"false"` still turns read-only mode ON. Every `RANCHER_*` variable in
> this CLI behaves this way.

Refusal is `VALIDATION`, naming the command and the active source(s) (e.g.
"enabled by RANCHER_CLI_READONLY") - the one-way rule means you have to know
which knob to turn off. `rancher schema -o json` reports the resolved state
(`readOnly`, `readOnlySources`) so an agent can check before trying.

Scoping, stated honestly: read-only mode refuses `Mutating` commands - the
CLI's managed-resource surface - and the typed flag is also refused on local
commands (below). Under config or environment read-only mode, local commands
(`auth login`, `auth status`, `config *`, `kubeconfig`, `skill *`,
`completion`) stay usable; otherwise a read-only session couldn't even
establish itself. A typed `--read-only` on a local command is refused with
`VALIDATION` before the command does anything (`--read-only=false` included),
with no hint: run the command without `--read-only` and any value typed with
it. The environment variable and the config field are the only session-wide
forms.
An agent that types `--read-only` on every call loses `auth status`,
`config *` and `kubeconfig`; set `RANCHER_CLI_READONLY` for the process
instead.

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

Login deletes the transient session token with its own bearer through
Rancher's logout endpoint (`POST /v1/logout`, which deletes the token its
bearer names), and sends the delete through the Rancher proxy when logout
does not answer 200 or 204. A standard user cannot delete a
`management-token` through the proxy, so a session token neither request
deleted (a provider set to forced single logout fails logout) stays valid
until it expires. It is listed in `residualTokens` (on a failed login, in
`sessionToken`) with its `cleanup.error` naming both answers, for example
`logout 500; delete 403 Forbidden`. The text `an administrator can delete
<ref> with: <command>, or it expires` (with ` at <time>` when the server
reported the expiry) is in `warnings` on a successful login and in
`details.recovery` on a failed one (there it starts `An administrator can
delete`); it carries no command hint, because
only an administrator can run the command.

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
  certificate?, retry, recovery?}` for each token that may still be live.
  `entry` is the entry the token is recorded on, `error` the answer or
  transport error behind `failed`, `recovery` a command sequence; for a v3
  token it is the administrator's delete alone. `code` is `TLS_TRUST` when
  the delete failed because the server's certificate failed verification
  (it is absent for any other failure), `error` then says how to fix the
  certificate problem for what revokes the token, and `certificate` holds the
  certificate's facts: `kind`, `fingerprint`, `subject`, `issuer`,
  `notBefore`, `notAfter`.

The text output lists one line per token under `tokens on <server>:`, for
example `revoked ext/abc (cluster token, c-1)` or `not revoked ext/def
(unconfirmed; recorded on prod: the next login of prod as u-1 revokes it)`.
A token whose `code` is `TLS_TRUST` is followed by the message and the
certificate's lines, indented under its line.

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
   this CLI cannot sign in with, the call fails with `NOT_FOUND`, names the
   types it cannot use, and offers the auth configs it can sign in with; it
   does not go on to the next step.
2. Otherwise every auth config whose provider type (`openLdapProvider`) or
   provider name (`openldap`) is the value. One match is used. With several, a
   terminal gets a menu of only those; with no terminal the call fails with
   `AMBIGUOUS_SELECTION` and an error that lists their names, each valid as
   given to `--auth-provider`. Through the exec plugin, its remedies name
   `--auth-provider` with the auth configs that sign in without a terminal;
   when none does, or under `--no-input`, the refusal is `AUTH`.
3. No match is `NOT_FOUND`, with the auth configs this CLI can sign in with as
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
run `rancher delete management-token <name> --apply --yes --allow-deprecated`,
or it expires. When the sign-in's own token cannot be deleted (a provider set
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
write a `--name` that would start with either. A login that records no owner (a
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
`--auth-provider` the auth config after that) whose
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
A superseded record whose server the entry no longer names has the state
`otherServer`, and its sequence runs against that server.
Every text rendering of the sequence ends with what it assumes:
`RANCHER_CLI_URL` and `RANCHER_CLI_TOKEN` are unset, and either would
override the current server the sequence selects. For a v3 token the
recovery is the administrator's delete alone, `rancher delete
management-token <name> --apply --yes --allow-deprecated`, and the text is
one line: `an administrator logged in to <server> can delete it with:
<command>`, naming the record's server, else the entry's. `rancher config
delete-server` reports a token it could not revoke under `notRevoked`, with
the same recovery, in the invocation that removes the entry, so the last
reference to a live token is not lost with it.

Two `rancher` processes cannot log in to one server at once: the plugin
login holds a per-server lock across the whole interactive wait. A
concurrent kubectl blocked behind it waits up to 60 seconds and then fails
with `LOCK_TIMEOUT` (exit 8), which is retryable: the message names the
server being logged in to; retry once that login completes.

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
wait or collect across many requests (`--wait`, `support-bundle`, the
per-cluster notes of `access`, the degraded parts of `inspect`) retry it or
record it as they do any failed request. The error's `details` hold the
certificate's kind, subject, issuer and fingerprint (for `auth login` and the
sign-in of kubectl's credential plugin, inside the login failure's `primary`
error), and the message names the remedy for the command that failed: a
command that takes `--cacert` names it, `auth get-token` and `rancher token`
name their own flags, a command that reads a server entry names
`rancher config update-server`, and a server taken from `RANCHER_CLI_URL`
names `RANCHER_CLI_CACERT`. When `RANCHER_CLI_CACERT` supplied the CA, the
message says so. A certificate failure of an `https://` proxy is not a
`TLS_TRUST` error: it keeps the transport error's text.

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
