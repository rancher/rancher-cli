# Logins, credential stores and tokens

Reports server, server version, `rancherVersion`/`supported`
(the 2.14 minimum-version floor - warn-only, never fails login/check),
`identity`/`token` metadata, and credential-store state. A `null` `identity`
or `token` field plus a `warnings` entry is a normal degradation (e.g.
`identity` is a create-verb probe - expect it null under a read-only-RBAC
token), not an error condition; don't treat it as one. If it fails with
`AUTH` or `NOT_LOGGED_IN`, establish a session. When the server refuses
the token of a stored login with a 401 and gives no reason, the `AUTH`
error's hint is the `rancher auth login` command for that login:
`--auth-provider '<auth config>'` for a login made through an auth
config whose name the CLI saved, and `--token-stdin` for every other
stored login. With `--token-stdin` you pipe an API token to the command;
this works for every entry `rancher auth login` accepts and needs no
terminal. In a terminal, `rancher auth login <name>` with no flags asks
whether to paste an API token or sign in via an identity provider. The
error has no such hint when the token comes from `RANCHER_CLI_TOKEN` or
belongs to a login entry of the exec plugin. Two more codes mean the
stored credential itself cannot be used: `LOGIN_INCOMPLETE` (exit `3`,
retryable) means a login for that entry is running or ended early - run
the command again, and log in again if it persists;
`CREDENTIAL_REPLACED` (exit `3`, not retryable) means another config
directory sharing this one's identity stored a different credential for
the entry - log in again for that entry.
`CREDENTIAL_TRANSITION_INCOMPLETE` (exit `3`, retryable) means a change
of the directory's credential store has work left; the message names the
step, the entry and the backend error that stopped it - fix that, then
run the command again. A step no retry can finish is reported as
`VALIDATION` instead, and the message carries the cause. `rancher config
status` shows the change as `credentialStoreChange`. A change record
that cannot be read refuses logins, logouts and server changes with
`VALIDATION` naming the file: restore it or move it aside. None of the
three is a reason to try a different server entry. A `VALIDATION` error
saying the directory has credentials in both the keychain and the
plaintext file means nothing records which is current: log out the
entries it lists in the store to drop (the file ones keep the keychain),
with the keychain reachable, then run the command again.

- Interactive: `rancher auth login <name> --url <url>` - adds the server
  entry and logs in, no UI trip needed (or add and log in as two steps:
  `rancher config add-server <name> --url <url>` then `rancher auth login
  <name>`). With no piped/pasted token, it offers a choice: paste an
  existing API token, or sign in via an identity provider
  (`--auth-provider`, `--auth-flow devicecode|authcode` skip the prompt;
  `--auth-provider` takes an auth config name from the candidates of an
  error or from completion, such as `local` or `corp-ldap`; a provider
  name or type such as `openldap` also works, and when it matches several
  auth configs the call fails with their names). A provider the server
  advertises but this CLI cannot log in with (GitHub and the other OAuth
  providers whose client secret the server
  holds) is named as such in the prompt, and `--auth-provider` naming one
  (by auth config name, provider type or provider name) fails with
  `CAPABILITY_UNSUPPORTED` (exit 11) rather than being dropped: the fix is
  server-side (rancher/rancher#57034); create an API token in the UI and pipe
  it to the login in the error's hint meanwhile (when that command cannot run
  as written - no entry name, an inline CA, or an entry that changed since the
  registration - the hint is empty and the message holds the command or says
  to check the entry). The provider path creates its own
  purpose-made, individually revocable token behind the scenes (see the
  token taxonomy below). Dropping `<name>` works too - `rancher auth login
  --url <url>` derives the entry name from the URL's host.

- The directory owns its credential store. The first login records it in
  `config.yaml`, and from then on `RANCHER_CLI_CREDENTIAL_STORE` may only
  repeat that value: a different one is a VALIDATION error naming both,
  not an override. Set the variable before the first login on a directory
  that needs a particular store (CI and headless machines want `file`),
  and leave it unset afterwards. `rancher config status` shows the
  declaration, its source, and the conflict when there is one. To change
  a directory's store afterwards, pass `--credential-store` to
  `rancher auth login`: it records the new store only after the new
  credential is stored in it. A change takes only that entry's credential
  and refuses while another entry still holds one in the store being left;
  the refusal comes before the sign-in and names the entries and the
  `rancher auth logout` to run first. `auto` never moves a keychain
  directory into plaintext: if the keychain is unusable it refuses, and
  `--credential-store file` is the explicit choice. `config edit` refuses
  any change to `credentialStore`. An `auto` login whose
  directory has other entries in the file store stays there and warns: log
  them out, then log in again to move to the keychain. A credential another
  entry has left in the destination (a keychain copy a logout could not
  reach) refuses a change by name and keeps `auto` on the file store the
  same way; its logout needs the keychain reachable. A change into a
  credential helper is refused while the directory has any other entry,
  logged in or not, since the helper may answer for any of them: use a
  different `--config` for the helper, or remove those entries with
  `rancher config delete-server`.
  `CREDENTIAL_TRANSITION_INCOMPLETE` (exit 3, retryable) means an earlier
  change has work left: the message names the step and why it could not
  finish; resolve that, then run the command again.

- Scripted/CI: pipe the token - `rancher auth login <name> --url <url> --token-stdin < token.txt`,
  or set `RANCHER_CLI_URL` + `RANCHER_CLI_TOKEN` env vars (no stored config needed).
  A piped token is VERIFIED against the server before it is stored: a bad
  token or unreachable server fails the login and stores nothing. The check
  also records the token's owner (`tokenUserId` in the `-o json` result;
  `tokenAuthConfig` is empty for a pasted token until a kubectl call that names
  an auth config records it). To stage a
  credential without the round-trip (e.g. the server is down), add
  `--no-verify` - the result then reports `"verified": false` under `-o json`
  and says "not verified" in text. Behind a corporate proxy, set
  `HTTPS_PROXY`/`NO_PROXY` - see REFERENCE.md's TLS & proxy section.

- An `AUTH` error naming an "identity provider" certificate is about the
  provider, not Rancher: `--cacert` covers the Rancher server only. The
  provider must be trusted by the operating system store (or, on Linux,
  through `SSL_CERT_FILE`).

- SAML/OAuth provider flows also live on `rancher auth get-token`
  (`--auth-provider` takes an auth config name, `--auth-flow
  devicecode|authcode`) - the kubeconfig exec
  plugin drives them automatically; that path creates cached exec credentials,
  not a stored login session.

- Kubeconfigs that use the exec plugin log in once per cluster by default.
  `rancher config set-token-reuse on` lets the plugin keep one login
  credential per server (entry `exec-login-<host>`, with `-<user id>` after it
  for a call with `--user-id` and `-<auth config>` after that when it names one;
  provenance `execLogin`)
  and create cluster tokens from it silently; `off` restores the default
  without deleting it. `rancher config status` lists every entry whose
  CLI-created tokens are still live, each with a four-command recovery
  sequence; run the sequence, never assume a replaced token is gone.

- If a login fails with `details.stage` and token outcomes, follow
  `details.recovery` before retrying; an outcome of `unknown` or
  `unidentified` means the server may hold a token this login created -
  never assume it is clean. The fates a login reports are `sessionToken`,
  `derivedToken`, and `clusterToken` (the plugin-created login's cluster
  token).

- Added ≠ logged in: a server can be added without a stored credential
  (`NOT_LOGGED_IN` until you log in). `auth login` never switches the current
  server - `rancher config use-server <name>` is the only switch verb.

**Three kinds of token** - three distinct things called "token", each created by a
different actor for a different purpose:

1. `auth login` - takes a token you already have (pasted, piped, or created by
   its own provider path above) and stores it in config. It never creates a
   token itself; Rancher does.
2. `auth get-token` - drives the identity-provider flow via `/v1-public`;
   RANCHER creates a token for the sign-in, `auth get-token` creates an ext
   cluster token with it (owned by the user who signed in) and deletes the
   sign-in's token. The kubeconfig exec plugin drives this automatically and
   caches the cluster token.
3. `create token` - an existing authenticated session asks the ext Token
   store to create a DERIVED token (`spec.kind` empty), individually revocable
   and visible in `get tokens`.

- **`config path` / `config status` / `config edit`:** `config path` prints the
  resolved config file path - point other tooling at it. `config status` adds
  the effective credential store and its source (env > config > auto default,
  plus the pin a login recorded, or with none where the stored credentials
  are, and the error when they are in both stores), and a nested
  `completion` section (network: on/off;
  timeout: effective per-TAB budget; timeoutRaw: only when clamped or
  unparseable) - the first place to check when TAB shows no candidates;
  and `cacheWritesPaused`, every entry whose exec-credential cache writes
  are paused while a change of its credential finishes, with the reason
  (`pinStep`: the next login, logout, `config delete-server` or `config
  update-server --url` finishes the store change; `noLock`: a login or
  logout of the entry clears it; `changeRecordUnreadable`: restore or move
  the change record aside; `none`: the next cache miss resumes it) -
  where kubectl's "not caching ... (see `rancher config status`)" notice
  points;
  config.yaml alone cannot reveal an env-supplied store or diagnose a
  misconfigured completion timeout. `config edit` opens the config in
  `$VISUAL`/`$EDITOR` on a temp copy and validates it before writing back;
  it refuses any change to server entries, `currentServer` and
  `credentialStore` - use `config add-server`, `update-server`,
  `delete-server`, `use-server` and `auth login --credential-store` for
  those - so it is for settings such as `readOnly` and `completion`; needs
  a TTY and `$VISUAL`/`$EDITOR` set.

- **Token commands - local vs. server:**

  | Command | Scope | Manages |
  |---|---|---|
  | `auth list-tokens` / `auth delete-token` | local | the exec-credential cache (`token-cache.yaml`); `delete-token` also revokes the tokens the CLI created in it |
  | `get tokens` / `get token <name>` | server | `ext.Token` objects |
  | `create token` | server | creates a derived `ext.Token` |
  | `delete token <name>` | server | revokes an `ext.Token` |

  They are not interchangeable - `auth delete-token`, `auth logout` and
  `config delete-server` revoke only the tokens the CLI created and recorded,
  never one you made with `create token`, and `delete token` does not touch
  the cache.
