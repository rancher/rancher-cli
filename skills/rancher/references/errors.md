# Errors in detail

- `retryable: true` (timeouts, a 429, a 502/503/504, another 5xx whose body is
  not a Kubernetes Status, a 409 on an update or delete that sent a
  resourceVersion (except the two cases named below), the two CONFLICTs from a
  check in the CLI named below, a temporary 403 from Kubernetes admission (see
  the next bullet), and a 500 the API server returns for a failed admission
  webhook call that did not reach the webhook or came back with a 503 that is
  not a Kubernetes Status)
  → retry is reasonable; otherwise do not repeat the identical call. A failed
  admission webhook call did not reach the webhook when the connection was
  refused, there was no route to host, the host name did not resolve,
  connecting to the webhook's address timed out, or the webhook's service had
  no endpoints. A webhook call that timed out in any other way, or came back
  with a 504, is not retryable. A 409 on
  an update or delete that sent a resourceVersion is `CONFLICT` with the message
  "the resource changed on the server since it was last read; retry the
  command". An update here is a command that changes an existing object, such as
  `rancher patch`. An admission webhook's 409 on such a write (its message
  starts `admission webhook "<name>" denied the request:` or is `admission
  webhook "<name>" denied the request without explanation`) is `CONFLICT` with the
  webhook's message and is not retryable: running the command again gets the
  same denial. A 409 on such a write whose message says the uid sent with the
  request is different from the object's (the message has
  `UID in precondition`, `the UID in the precondition` or `uid mismatch`) is
  `CONFLICT` with the server's message and is not retryable: running the
  command again fails the same way. For a `patch` that changes
  `metadata.uid`, remove that change. If the object was deleted and created
  again after the command read it, try again to read the new object. A 409 on
  any other request is `CONFLICT` and is not retryable. Its message is the
  server's message. The one
  exception is a create whose response body is not a Kubernetes Status: its
  message ends with "the server reported a conflict". Two CONFLICTs come from a
  check in the CLI, not from a 409, and are retryable: `app install` or
  `app uninstall` with `--apply` finds that what the dry-run preview read has
  changed (the message ends "run the command again to see a new preview"), and
  the exec plugin finds that `config.yaml` changed during a call and `--name`
  now identifies another entry or none (the message starts "config.yaml changed
  while this call was running"). Any other CONFLICT from a check in the CLI
  carries its own `retryable`. A timeout, a 502 or a 504 on a write whose
  request was sent is not retryable, nor is a 500 or a 503 whose body is not a
  Kubernetes Status: see `outcomeUnknown` below.

- `outcomeUnknown: true` means a write's request was sent and the CLI does not
  know whether the server applied it (the connection was lost, the request
  timed out or was stopped, or the server responded with 502 or 504, or with
  500 or 503 and a body that is not a Kubernetes Status). The change may be on
  the server. Do not run the command again yet: run the `hint`, a `rancher get`
  command that reads the object (or lists the objects of its kind when the
  create sent no name). If the object shows the change, do not repeat it; if it
  does not, run the command again. For a delete, an object that is gone, or
  that has a `metadata.deletionTimestamp`, means the delete was applied. For a
  create that sent no name, look in the list for the object this command
  created. With no `hint`, the CLI cannot read the object back: check whether
  the change is on the server with a command of your own, or tell the user; a
  delete of an object you cannot get says to ask a user who can get it. `create
  user` keeps the field on an error after the user was created. `revoke` and
  `delete project` can leave the same doubt in the `message` inside `details`;
  `app install`, `app upgrade`, `app uninstall`, `extension install`,
  `extension upgrade`, `extension uninstall`, `node drain`, `node stop-drain`
  and the cloud credential writes in their `message`, in each case without the
  field.

- A temporary 403 from Kubernetes admission (the server could not check the
  request yet) is `GENERIC` and retryable. The message is the Status message,
  followed by " - retry later". The five are: the server is not ready to handle
  the request, LimitRanger failed to enforce limit ranges, the ResourceQuota
  status is unknown, ResourceQuota cannot get the previous usage because the
  previous version of the object was not found, and the ServiceAccount
  admission plugin could not look up the pod's ServiceAccount because of a
  timeout, a refused or reset connection, or an API server that was unavailable
  or overloaded. A ServiceAccount lookup that client-go refuses because the
  namespace or name has a slash or a percent sign, or is `.` or `..`, is
  `VALIDATION`, as is a ServiceAccount that does not exist; a lookup that fails
  for any other reason is `RBAC`.

- `TLS_TRUST` (exit `10`) means the server's certificate failed
  verification: act on the remedy the message names (a different CA, the
  server URL, or the server's administrator), or tell the user; never retry the
  same call. It is not retryable.

- `CREDENTIAL_STORE_UNAVAILABLE` (exit `14`, retryable) means the keychain
  or credential helper this directory uses cannot be read or written right
  now: a locked keychain, a cancelled keychain prompt, no D-Bus session bus
  on Linux (rancher-cli does not start one), a helper that is missing or
  failing. The message names the store, the entry, the cause and
  the remedy (on macOS: unlock the keychain with `security unlock-keychain`,
  or approve the prompt). That remedy needs the person at the keyboard: tell
  the user, and run the command again only after they have acted. It is
  never a reason to try another server entry, store or `--config`.
- A `NOT_FOUND` list that was cut (`candidatesTotal` is present): `candidates`
  has the first 10 for a project, cluster, cloud credential or pod (the entries
  that contain what was typed first, each part by id), and the first 10 that
  contain what was typed (or the first 10 of all when none does) for a user or
  role, and `candidatesTotal` is the size of the full list. A `--project` typed
  without `--cluster` is a `VALIDATION` error that lists the first 10 clusters
  that have the project, by id; when it tells you which clusters to pass, its
  `hint` lists every cluster you can read, not only those that have the
  project (when several projects on one cluster have that display name, it
  lists the projects on that cluster). When the error has a `hint`, run it to
  see the full list (for example `rancher access projects --cluster <id>`); a
  pod has no `hint`; ask the user for a closer name.
- When you type a username, display name or node name that several objects have,
  `get`, `patch` and `delete` of a user, project, role template, global role,
  node driver, auth config or management node return a `NOT_FOUND` that lists
  every one of them, with no `candidatesTotal`. A project member gets `RBAC`
  with the same list.
