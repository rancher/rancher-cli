# Users, auth configs, settings, secrets and OIDC clients

- `rancher patch feature <name> --patch '[{"op":"replace","path":"/spec/value","value":true}]' --apply --yes` -
  flip a Rancher feature flag (an RFC 6902 JSON Patch, the default type of
  every `patch <kind>`).
  A `setting`'s `value` is a STRING field even when the setting is a
  boolean or a number: patch it with `"value":"true"`, never a bare `true`;
  gated like any sensitive-kind patch (`--yes` required, since flipping a
  flag is destructive by policy).

- `rancher get authconfigs` lists the auth configs of the Rancher server (TYPE
  says which auth provider; NAME is the auth config's id), and `rancher get
  authconfig <id> -o json` shows one. Before Rancher v2.16 the list holds one
  auth config for every auth provider Rancher supports, most of them disabled
  and never configured. Do not report an auth config as a configured auth
  provider because it is listed: ENABLED says which ones users can log in with.
  The CLI hides fields that hold credentials and values that start with
  `cattle-global-data:` in get output and table cells: do not look for a
  credential there. The `--wait` and `--for` messages print the condition
  message the server sent, and the CLI hides no values in them. It is read-only:
  `patch`, `create resource` and `delete` fail with `VALIDATION`.

- `rancher create user <username> --password-stdin` creates a local user (a
  `v3 User`, its password secret, and a `v3 GlobalRoleBinding` for each global
  role the server marks `newUserDefault`, `user` on a default install) - the
  password only ever arrives on stdin, never a flag. A user with no global
  role cannot create a token and cannot log in, so pass `--global-role <id>`
  (repeatable, replaces the defaults) when the defaults are not the roles you
  want; the preview warns when none of the typed roles allows creating
  tokens. If a binding fails after the user was created, the error names each
  role not bound with the `rancher grant ... --global --role <id> --dry-run`
  command that grants it. `rancher create password-change-request <user id>
  --password-stdin` changes an existing local user's password the same way
  (completes synchronously - no wait needed). Both require a server that
  supports local user passwords; on a server without that support they
  refuse with `CAPABILITY_UNSUPPORTED` (exit `11`) rather than a raw error.
  Rancher requires the current password when you change your own password
  without permission to change every user's password (that is, permission to
  update `users.management.cattle.io` and `secrets` in
  `cattle-local-user-passwords`). Pipe two lines, line 1 the current password
  and line 2 the new password, and pass `--current-password-stdin` with
  `--password-stdin`, for example
  `printf '%s\n%s\n' "$CURRENTPASSWORD" "$NEWPASSWORD" | rancher create password-change-request <user id> --current-password-stdin --password-stdin --apply --yes`.
  In PowerShell 7.1 or later, read both passwords with `Read-Host -MaskInput`,
  which shows asterisks instead of what you type, and pipe them, the current
  password first:
  `$current = Read-Host -MaskInput 'Current password'; $new = Read-Host -MaskInput 'New password'; $current, $new | rancher create password-change-request <user id> --current-password-stdin --password-stdin --apply --yes`.
  Windows PowerShell 5.1 has no `Read-Host -MaskInput`, which hides what you
  type.
  PowerShell 7 sends each password as UTF-8 text followed by a newline, with no
  byte order mark by default and one when `$OutputEncoding` is
  `[System.Text.Encoding]::UTF8`. A password with non-ASCII characters arrives
  as UTF-8.
  Do not pipe a SecureString (`Read-Host -AsSecureString`): PowerShell 7 and
  Windows PowerShell 5.1 send the text `System.Security.SecureString`, not the
  password. Windows PowerShell 5.1 sends a byte order mark (the bytes
  `EF BB BF`) before piped text, and two of them when `$OutputEncoding` is set
  to `[System.Text.Encoding]::UTF8`. `rancher` drops every byte order mark at
  the start of stdin, which lets you pipe an ASCII password from Windows
  PowerShell 5.1.
  Windows PowerShell 5.1 sends `?` for each non-ASCII character with its default
  `$OutputEncoding` (US-ASCII), and rancher receives a different password than
  the one you typed.
  For a password with non-ASCII characters, set
  `$OutputEncoding = [System.Text.Encoding]::UTF8` before the command that pipes
  it, or use PowerShell 7.
  The dry-run does not check the current password. A refused current password
  is `VALIDATION` (exit `2`); changing another user's password without
  permission to change every user's password is `RBAC` (exit `6`). Neither
  error has a hint, and the password was not changed. A password piped to
  `--password-stdin` that has a newline inside it is refused, on both
  commands.

- `rancher create secret <name> --namespace <ns> --cluster <c> --from-literal k=v[,k=v...]` -
  an ordinary namespaced secret without hand-rolling base64.
- `rancher create secret <name> --project <p> --cluster <c> --from-literal k=v` -
  writes into the project's backing namespace for Rancher to propagate into
  every project namespace; a `warnings` entry means propagation isn't
  guaranteed on this server (requires Rancher >= 2.14, the supported
  minimum) and it won't propagate.

### OIDC clients

`create oidc-client <name> --redirect-uri <uri> [...] --apply --yes` creates
a client for Rancher's embedded OIDC provider (Sensitive kind,
Prime-classified, contract internal, stability beta - even `create` is
destructive, so `--yes` is always required, not just on patch/delete). The
`oidc-client` group manages its secrets: `add-secret`, `rotate-secret`,
`remove-secret` (all destructive, `--apply --yes`). Generic `get/patch/delete
oidcclient` (alias `oidcclients`) also work - `get` is read-only,
`patch`/`delete` escalate to `--yes` like any Sensitive kind.

`create oidc-client`, `add-secret`, and `rotate-secret` generate a client
secret and show it exactly once - capture it from the command's own output,
it is never readable again. `-o name` is refused on these three (there is no
id-only view of a secret you can only see once); table, wide, json, yaml,
and template all work.

`oidcclient` sits behind the `oidc-provider` feature: a disabled feature
fails any mutation with `VALIDATION` naming the feature - "enable feature
oidc-provider" on a Prime server, a neutral disabled statement on every
other edition. The flag is not dynamic: patching it
(`patch feature oidc-provider ... --apply --yes`) warns "this feature flag
is not dynamic: Rancher restarts to apply it - wait until the server is
available again before running more commands" - wait out the restart
before running oidc-client commands.

`rotate-secret <client> [secret-name]` may omit `secret-name` only when the
client has exactly one secret; with more than one, an omitted name fails
VALIDATION with a `candidates` list naming the secrets.

Worked flow - create, hand the relying app its config, rotate:

```
rancher create oidc-client my-app --redirect-uri https://example.invalid/callback --apply --yes -o json
# data: clientID, clientSecret (shown once), issuer, endpoints{authorization,token,jwks; userinfo when the server serves one}
# hand clientID/clientSecret/issuer/endpoints to the relying app now - none of it is shown again
rancher oidc-client rotate-secret my-app --apply --yes
```
