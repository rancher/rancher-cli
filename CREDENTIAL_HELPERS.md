# Credential helpers

`rancher` can delegate login-token storage to an external program - a
*credential helper* - so tokens live in your secret infrastructure
(HashiCorp Vault, OpenBao, 1Password, pass, …) instead of the OS keychain or
a local file.

## Configuration

```yaml
# config.yaml
credentialStore: exec:vault
```

or with `RANCHER_CLI_CREDENTIAL_STORE=exec:vault`, which chooses the store
of a directory that declares none and must agree with a declared one: a
variable naming a different store than `credentialStore` is refused. The
name after `exec:` must match `^[a-z0-9][a-z0-9-]*$`; the CLI runs the
binary `rancher-credential-<name>` found on `PATH`.

There is **no fallback**: if the helper is missing or fails, the command
fails with the helper's error. `RANCHER_CLI_TOKEN` still bypasses credential
storage entirely (the CI escape hatch). `auto` never selects a helper.

## Protocol (version 1)

Invocation: `rancher-credential-<name> <verb>` with verb `get`, `store`, or
`erase`. One JSON request on stdin; one JSON response on stdout. Exit 0 on
success; on failure exit nonzero with a plain-language message on stderr
(stdout is ignored then). Unknown verbs and unsupported protocol versions
MUST exit nonzero.

### Request

| Field | Type | Notes |
|---|---|---|
| `protocolVersion` | int | Always `1`. |
| `serverURL` | string | **Canonical** server URL (see below). |
| `name` | string | The user's config-entry name for the server. |
| `token` | string | `store` only - the credential to persist. |

The CLI canonicalizes `serverURL` before every request: lowercase scheme and
host, default port (`:443` https / `:80` http) stripped,
path/query/fragment/userinfo removed. A helper is guaranteed byte-identical
`serverURL`+`name` across the store/get/erase lifetime of a credential - key
your storage on those fields verbatim.

### Responses

`get` → `{"protocolVersion":1,"token":"...","expiresAt":"..."}`.
No credential is **not an error**: exit 0 with `{"protocolVersion":1}`
(absent `token` = not found). `expiresAt` (optional, RFC 3339) marks the
token short-lived: the CLI caches it in memory only (never on disk),
refreshes 30 seconds before expiry, retries once if a token arrives already
expired, and hard-errors on a malformed timestamp. This lets a helper *create*
short-lived credentials rather than store long-lived ones.

`store` → exit 0 with empty output or `{"protocolVersion":1}`. A read-only
(credential-creating) helper responds `{"protocolVersion":1,"unsupported":true}`:
`rancher auth login` then succeeds with a warning that the token was not
persisted.

`erase` → exit 0; erasing a credential that does not exist is success
(erase is idempotent). A helper that can tell whether a credential was
actually deleted says so with the optional `erased` field -
`{"protocolVersion":1,"erased":true}` for a real deletion, `"erased":false`
for a no-op. When the field is absent the CLI reports the erase as a
deletion (the safe direction); `rancher auth logout` uses it to keep its
`deletedFrom` report honest. The field is best-effort: a helper whose
backend has no atomic delete-and-report operation may answer from a read
that a concurrent store immediately staled - prefer `erased` over silence,
but do not treat it as a transactional guarantee.

### Compatibility rules

- Within a supported `protocolVersion`, helpers MUST ignore unknown request
  fields and the CLI ignores unknown response fields.
- Any change a helper cannot safely ignore arrives as a new
  `protocolVersion`; reject versions you do not support (nonzero + stderr).

### Security rules

- stdout carries only the response JSON. Prompts (PIN, biometric, unlock)
  must use the TTY.
- The **Rancher credential** carried by this protocol travels only via
  stdin/stdout - the CLI never puts it in argv or the environment, and
  helpers must not either. How a helper authenticates to its *own backend*
  (e.g. `VAULT_TOKEN`, `op` session state) is outside the protocol's scope
  and follows that backend's conventions.
- The CLI enforces a 30-second timeout per invocation.
- A helper must not invoke `rancher` commands that modify the CLI's own
  configuration; the calling command may hold the config write lock during
  helper execution, so such a call waits and then fails.

## Writing a helper in Go

```go
package main

import "github.com/rancher/rancher-cli/credhelper"

type memory struct{ m map[string]string }

func key(u, n string) string { return u + "\n" + n }

func (s *memory) Get(u, n string) (*credhelper.Credentials, error) {
	if t, ok := s.m[key(u, n)]; ok {
		return &credhelper.Credentials{Token: t}, nil
	}
	return nil, nil // not found: NOT an error
}
func (s *memory) Store(u, n, tok string) error { s.m[key(u, n)] = tok; return nil }
func (s *memory) Erase(u, n string) (credhelper.EraseOutcome, error) {
	if _, ok := s.m[key(u, n)]; !ok {
		return credhelper.EraseNothingStored, nil
	}
	delete(s.m, key(u, n))
	return credhelper.EraseDeleted, nil // EraseUnknown is always a safe answer
}

func main() { credhelper.Serve(&memory{m: map[string]string{}}) }
```

For a backend with no atomic delete-and-report operation, build `Erase` on
`credhelper.ProbeErase(probe, erase)` - it probes through your own `Get`,
treats a denied read as unknown (never a failure), and always runs the
delete; the vault helper is the reference caller.

`credhelper.Serve` handles verbs, JSON, versioning, and exit codes. Return
`credhelper.ErrStoreUnsupported` from `Store` to declare a read-only helper.

## The Vault helper

`rancher-credential-vault` ships with the CLI and works unchanged against
OpenBao. Configuration (environment):

| Variable | Default | Purpose |
|---|---|---|
| `VAULT_ADDR` | - (required) | Vault/OpenBao address. |
| `VAULT_TOKEN` | `~/.vault-token` | Auth token (env wins). |
| `RANCHER_VAULT_MOUNT` | `secret` | KV v2 mount. |
| `RANCHER_VAULT_PATH` | `rancher-cli` | Path prefix under the mount. |

One KV v2 secret per credential at
`<mount>/data/<path>/sha256hex(serverURL + "\n" + name)` with fields `token`,
`serverURL`, `name`. Erase uses the **metadata** endpoint - all versions are
permanently removed. Required policy:

```hcl
path "secret/data/rancher-cli/*"     { capabilities = ["create", "update", "read"] }
path "secret/metadata/rancher-cli/*" { capabilities = ["delete"] }
```

The paths above use the default mount (`secret`) and path prefix
(`rancher-cli`); adjust them if `RANCHER_VAULT_MOUNT` or `RANCHER_VAULT_PATH`
are overridden.

(`update` matters: without it the first login works and every re-login
fails.)

## Wrapping other password managers

Any manager with a CLI is a small script away. The rules that make a wrapper
*safe*, not just working: validate `protocolVersion`; key on
`serverURL`+`name` (a name alone collides across servers and can hand one
server's token to another); report not-found ONLY when the backend
specifically said not-found - any other failure (locked vault, auth,
transport) must exit nonzero, or the CLI silently degrades; `erase` must
exit nonzero unless the credential is gone.

Both wrappers hash the identity with `openssl dgst -sha256` - present by
default on macOS and Linux (`sha256sum` is GNU-only) - and use the FULL
digest: a truncated hash is a small keyspace and colliding identities would
hand one server's token to another.

1Password (read-only helper - the CLI warns at login instead of persisting).
Items are named `rancher-<64-hex>` from the full identity hash:

```sh
#!/bin/sh
# rancher-credential-op: get-only helper backed by `op`.
set -u
req=$(cat)
ver=$(printf '%s' "$req" | jq -r '.protocolVersion')
[ "$ver" = "1" ] || { echo "unsupported protocol version $ver" >&2; exit 1; }
url=$(printf '%s' "$req" | jq -r '.serverURL')
name=$(printf '%s' "$req" | jq -r '.name')
id=$(printf '%s\n%s' "$url" "$name" | openssl dgst -sha256 -r | cut -d' ' -f1)
item="rancher-$id"
case "$1" in
get)
  if out=$(op item get "$item" --vault Private --fields token --reveal 2>&1); then
    jq -cn --arg t "$out" '{"protocolVersion":1,"token":$t}'
  elif printf '%s' "$out" | grep -q "isn't an item"; then
    printf '{"protocolVersion":1}'   # genuinely not found
  else
    printf '%s\n' "$out" >&2; exit 1 # locked vault / auth / transport: HARD error
  fi
  ;;
store) printf '{"protocolVersion":1,"unsupported":true}' ;;
erase)
  if out=$(op item delete "$item" --vault Private 2>&1); then
    printf '{"protocolVersion":1}'
  elif printf '%s' "$out" | grep -q "isn't an item"; then
    printf '{"protocolVersion":1}'   # already gone: idempotent success
  else
    printf '%s\n' "$out" >&2; exit 1 # deletion NOT confirmed: never exit 0
  fi
  ;;
*) echo "unknown verb $1" >&2; exit 1 ;;
esac
```

pass (full store; entries keyed by the identity hash):

```sh
#!/bin/sh
# rancher-credential-pass
set -u
req=$(cat)
ver=$(printf '%s' "$req" | jq -r '.protocolVersion')
[ "$ver" = "1" ] || { echo "unsupported protocol version $ver" >&2; exit 1; }
url=$(printf '%s' "$req" | jq -r '.serverURL')
name=$(printf '%s' "$req" | jq -r '.name')
entry="rancher-cli/$(printf '%s\n%s' "$url" "$name" | openssl dgst -sha256 -r | cut -d' ' -f1)"
case "$1" in
get)
  if out=$(pass show "$entry" 2>&1); then
    jq -cn --arg t "$out" '{"protocolVersion":1,"token":$t}'
  elif printf '%s' "$out" | grep -q "not in the password store"; then
    printf '{"protocolVersion":1}'   # genuinely not found
  else
    printf '%s\n' "$out" >&2; exit 1 # GPG/store failure: HARD error
  fi
  ;;
store)
  printf '%s' "$req" | jq -r '.token' | pass insert -f -e "$entry" >/dev/null || exit 1
  printf '{"protocolVersion":1}'
  ;;
erase)
  # Classify the existence check exactly like get: only a backend-reported
  # not-found is "already absent". A GPG/permissions/store failure must
  # hard-fail - exit 0 here would let the CLI drop its cleanup record while
  # the credential stays live.
  if out=$(pass show "$entry" 2>&1); then
    pass rm -f "$entry" >/dev/null || exit 1  # exists: removal MUST succeed
    printf '{"protocolVersion":1}'
  elif printf '%s' "$out" | grep -q "not in the password store"; then
    printf '{"protocolVersion":1}'            # absent already = idempotent success
  else
    printf '%s\n' "$out" >&2; exit 1          # store unreadable: deletion unconfirmed
  fi
  ;;
*) echo "unknown verb $1" >&2; exit 1 ;;
esac
```
