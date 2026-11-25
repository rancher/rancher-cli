# kubeconfigs and the kubectl exec plugin

The kubeconfig uses an exec-credential plugin that calls `rancher auth
get-token` - the `rancher` binary must stay on PATH for kubectl to
authenticate, or pass `--exec-command <path>` for one that is not. The exec
entry records the config directory (absolute) and `RANCHER_CLI_CREDENTIAL_STORE`
(empty when the directory declares its store), so kubectl authenticates through
the directory the file was written from, whatever its own environment names;
the file works for this user on this machine only, and moving the directory
means writing its kubeconfigs again. Every command the plugin and `rancher
kubeconfig` print as a remedy that reads a config directory names that
directory with `--config` (`rancher schema` reads none). When the cluster has the Authorized Cluster Endpoint
enabled, the file also carries direct contexts (`<entry>-fqdn`, or one
`<entry>-<node>` per control-plane node) and makes one current; the proxy
context `<entry>` is always present, so `--context <entry>` is the
fallback when the direct endpoint is unreachable from where you run.
The exec entry's `--name <entry>-<cluster>` picks the server entry whose login
is used, so entries for one URL as different users stay apart. When a
kubeconfig names no such entry, the plugin uses the current server's entry if it
is a user entry with a login, else the one user entry for the URL that has one,
else the plugin's own login entry while token reuse is on. When several entries
tie for first it fails and lists them, and when the store cannot read an entry
that would come first or tie for it, it fails with that error. Fix
a tie by adding `--name <entry>-<cluster>` to the exec entry (changes nothing
else), or by regenerating the kubeconfig: `rancher config use-server <entry>`
switches the directory's current server for every process using it (switch
back with `rancher config use-server <previous>`), then
`rancher kubeconfig --cluster <id>`.
The exec entry also carries `--user-id <id>` when that login records its
owner: the plugin then hands kubectl only that user's tokens. Rancher writes
the form `auth get-token --server <url> [--cluster <id>] --user-id <id>
[--auth-provider <auth config>]` with no `--name`; `--cluster` may be left out
for a token for Rancher itself. With `--auth-provider` beside `--user-id` the
value must be an auth config's exact name (`VALIDATION` for a provider type,
`NOT_FOUND` before any prompt for a sign-in when none is named so), the plugin
reads the entries that record that auth config first and then the ones that
record none, never one that records another, and hands kubectl only tokens made
through it: an entry whose login was made through another is refused before
any request (`AUTH`) when its `--name` identifies it, and a token created
through another auth config, or one whose response names none, is revoked
(`AUTH`). The first token created from an entry that recorded none records the
auth config; `rancher kubeconfig` writes `--auth-provider` when the login records
one.
With no `--name` (or one that identifies no entry) the plugin reads the
entries for the URL that record that user - user entries first, the current
server first, then by name, the plugin's own login entries only with token
reuse on - and the first with a login answers; an entry that records no owner
or another user is never read. It refuses (`AUTH`) the entry its `--name`
identifies when that login records another user, revokes a created
token whose owner is not the user (`AUTH`; `GENERIC` when the response
names none), fails closed on a Rancher without `ext.cattle.io`
(`CAPABILITY_UNSUPPORTED`), and after an interactive sign-in checks the
user from the sign-in's token create, with one `SelfSubjectReview` only when it
names no owner. The plugin-created login (token reuse on)
writes `exec-login-<host>-<user id>` (`-<auth config>` after it with an auth
config), never over an entry of another user or auth config. The
refusal of an entry whose login records another user or auth config names a login
of a user entry with `--token-stdin` (a sign-in through the auth config with
`auth login <entry> --auth-provider <auth config>` too, for another auth config),
or, for the plugin's own login entry, its removal
with `auth logout` and `config delete-server`. Without a terminal the error
names a token of that user, made through the call's auth config, piped to `auth login`. With no `--name` no remedy
names `--config`. With token reuse on, a call without `--user-id` is refused
(`AMBIGUOUS_SELECTION`) when no user entry answers and the URL has more than
one plugin login entry, since there is one per user. A notice says
when the login records no owner (log in again without `--no-verify`, then
regenerate the kubeconfig). `--name` values starting with `user-id:` are
reserved.
Cached credentials: `rancher auth list-tokens` (a TOKEN column names each
record's token) / `rancher auth delete-token` (deletes the cache records,
then revokes the cluster tokens the plugin created and recorded; one that
could not be revoked stays valid until `rancher delete token <name>`, or
expiry; the plugin's default interactive sign-in (`--cluster` or `--user-id`,
token reuse off) creates an ext token its user can delete; on a server without ext
tokens its cluster token is a cluster-scoped v3 token the CLI cannot delete,
an administrator can: `rancher delete management-token <name> --apply --yes
--allow-deprecated`; a sign-in token that cannot be deleted (forced single
logout) is reported in a warning). An unscoped v3
cache token and a v3 login are deleted through Rancher's logout endpoint with
the token itself.
`rancher auth logout`, `auth logout --all` and `config delete-server` revoke
the tokens the CLI created and recorded the same way. Their data carries
`revoked` and `notRevoked`; the exit code is 0 whenever the local deletes
succeed, so read `notRevoked`. A Ctrl-C or termination signal that stops
the local delete, or leaves a token unreached, ends the command with
`INTERRUPTED` (exit 130) and both lists in the error's details
(`config delete-server` keeps the entry; run it again to remove it); a
signal after the local delete that leaves no token unsent does not stop
it. A failed login-token delete fails the command with both lists in the
details, even when the login token was already gone from the store the CLI
reads (it is then revoked like any deleted login): another backend may
still hold a copy, so fix the step the error names and run the command
again. Each entry has a `reason` and a `retry`:
`nextLogin` needs nothing beyond the next login of its `entry`
(`rancher auth login <entry>`, or the kubectl sign-in); `manual` means run
its `recovery`, logging in as the user in `loginAs` when it is present (or the
reason's remedy, as `unproven` says: find the token with `rancher get tokens`);
`none` is a pasted token, nothing to do. An
entry with `code` `TLS_TRUST` failed because the server's certificate did not
verify: read its `error` for the fix - a CA to save, a server URL to correct,
or a certificate only the server's administrator can fix (`certificate` holds
the certificate's facts) - then follow `retry`. Revokes
run under config or environment read-only mode too.

- **`kubeconfig` vs. `create kubeconfig`:** `kubeconfig` renders a LOCAL
  exec-credential kubeconfig from config alone (no server call, no embedded
  secret) - the interactive default. `create kubeconfig` generates a
  SERVER-SIDE, embedded, revocable kubeconfig via the ext Kubeconfig store -
  for handing the kubeconfig to CI or another party where an exec plugin
  isn't usable. Every ext.cattle.io operation here is also plain Kubernetes
  API - `kubectl get/create tokens.ext.cattle.io` and
  `kubeconfigs.ext.cattle.io` work too - the CLI adds the auth bootstrap, the
  ms-vs-seconds TTL conversion, and shown-once handling on top.
  On Rancher before 2.15, `create kubeconfig` refuses with
  `CAPABILITY_UNSUPPORTED` (exit `11`) when the token it sends is one the CLI
  created for a login or an ext token in `RANCHER_CLI_TOKEN`; use an API
  token (`rancher auth login <name> --token-stdin`, or `RANCHER_CLI_TOKEN`);
  the error has no `hint`, and its message names the login command only when
  the stored login's entry accepts `auth login` (a plugin login entry does
  not).
