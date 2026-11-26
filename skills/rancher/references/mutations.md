# Previews, patches and deletes in detail

- `delete` waits until the object is gone (default `--timeout 5m`);
  `--wait=false` returns at once and prints `deletion started`. Read
  `data.waited` and the warnings. Without `--wait=false`, a `data.waited`
  of `false` means the deletion was requested and the command did not see
  every object deleted; a warning says which object and why. On an error with
  `data.error.details.committed` true (a timeout, an interrupt, `AUTH`,
  `TLS_TRUST`), run `data.error.details.recovery`, a `rancher get` command;
  never run the delete again.

- Every `delete` asks Kubernetes to delete the object's dependents in the
  background (`propagationPolicy: Background`, as `kubectl delete` does): the
  pods of a deleted Job are deleted shortly after the Job is gone. A
  `data.waited` of `true` means the Job is gone, not its pods; read
  `rancher get pod --cluster <c> --namespace <ns>` until they are gone.

- Patch and create previews include a field-level diff (`data.diff`) - show
  it. Its `path` is an RFC 6901 pointer (`~1` for `/`, `~0` for `~`), usable
  verbatim as the `path` of an operation in a `--patch` JSON Patch. A
  create's diff lists the fields the command sends, with `from` null; a
  create that sets nothing past its name has no `diff`. In the diff of a
  preview, `from` is left out when the CLI did not read the stored value, and
  is null when the field had no value.

- A patch preview may carry a warning that a changed field is applied by
  another manager (Flux, Argo CD) which may revert the change on its next
  reconcile; relay it before applying, and suggest changing the source that
  manager applies instead.

- `--type merge` takes an RFC 7386 JSON merge patch (an object), which
  replaces each list it names as a whole. Its preview warns when entries of
  a list are removed; relay it, and to change one entry use a JSON Patch
  operation on that entry's path instead.

- A preview hides secret values by design: the entry keeps its path and each
  side that has a value reads `<redacted>`; a side that has no value stays
  null. Hidden values are a secret's `data` and `stringData`, a user's
  `password`, a management token's `token`, an ext token's `status.bearerToken`
  and `status.value`, an ext kubeconfig's `status.value`, a password change
  request's `spec.currentPassword` and `spec.newPassword`, and the password
  and key fields of a machine config (for example `secretKey`,
  `sshKeyContents`, `sessionToken`, `sshPassword`). The `kubectl.kubernetes.io/last-applied-configuration` and
  `objectset.rio.cattle.io/applied` annotations, which hold a copy of the
  object as it was last applied, are hidden too for every sensitive kind and
  every kind with hidden fields. Field names match in any
  case. Other fields print as given, so a description or a cloud-init text shows
  in full. The `--apply` result in json, yaml and template leaves out the same
  values. The exceptions are the commands that show a secret once on purpose:
  `create token`, `create oidc-client`, `oidc-client add-secret` and
  `oidc-client rotate-secret`; `create kubeconfig` prints its kubeconfig and
  accepts only table output. `get` in json, yaml and template leaves out the
  same values, except that `get secret` shows `data` and `stringData`; kubectl
  with a kubeconfig from `rancher create kubeconfig` shows a value the
  server returns on a read.

- `--dry-run` is a real server dry-run: it needs the same RBAC verb as the
  write. An `RBAC` error on dry-run means the write would be denied too. The
  exceptions are a Rancher action the CLI previews from server reads (`app
  install`, `app upgrade`, `app uninstall`, `extension install`, `extension
  upgrade` and `extension uninstall`, and `node drain` and `node stop-drain` for
  a caller without access to Rancher's local cluster), and the cloud credential
  writes through Norman (`create cloud-credential`, `patch cloud-credential`,
  `delete cloud-credential`): their Effects have `previewSource: serverReads`,
  built from server reads, and `conflictDetection` false, with no
  `resourceVersion` except in the Effects of `patch cloud-credential` and
  `delete cloud-credential`, which carry the `resourceVersion` the preview read.
  A ResourceQuota, LimitRanger,
  PodSecurity or ValidatingAdmissionPolicy refusal on dry-run is `VALIDATION`:
  the server refused the object, not your access.

- A delete by a role that grants `delete` and not `get` still works: its Effect
  has `uid` set, no `resourceVersion`, `conflictDetection` false and a warning,
  the delete carries a uid precondition, and the command does not wait
  (`deletion started`, `data.waited` false). `delete project`, `delete token`,
  `delete kubeconfig`, the day-2 operation deletes and the delete of the token
  the command uses need `get`: their `RBAC` error's hint names the missing
  verb. When a delete fails with a `CONFLICT` that says the object was deleted
  and created again after the preview, nothing was deleted: run the delete
  with `--dry-run` again before you apply it.
- `patch` reads the object before it updates it, and needs `get` as well as
  `update`. The same is true of `cluster scale`, `cluster set-autoscaling`,
  `cluster clear-autoscaling`, `cluster pause-autoscaling`, `cluster
  resume-autoscaling`, `cluster save-etcd-snapshot` and `cluster
  restore-etcd-snapshot` (they read the
  provisioning cluster), the three `oidc-client` secret commands (they read the
  OIDC client) and `project add-namespace|remove-namespace` (they read the
  namespace). `cluster upgrade` and `cluster set-upgrade-strategy` read the
  provisioning cluster and need `get` on it; for an imported cluster they also
  read and update its management cluster. `create secret --project` and the
  commands for a project's role bindings with `--project` read the project and
  need `get` on it; `create cluster` reads the Secret of its cloud credential
  and needs `get` on `secrets`, as do `patch cloud-credential` and `delete
  cloud-credential` (they read the credential Secret, and read it again before
  the write with `--apply`). When your role cannot get the object, the command
  stops with `RBAC` (exit 6) before any write, and the hint names the missing
  verb (`patch cluster` keeps its own hint, `rancher inspect cluster <name>`).

- The dry-run's `resourceVersion`/`conflictDetection` fields describe THAT
  invocation only. An Effect with `previewSource: serverReads` has no
  server precondition; for an action and for `patch cloud-credential` and `delete cloud-credential`, `--apply` reads
  the state again and refuses with `CONFLICT` when it changed (`create cloud-credential` does not
  read again), and a change after that read is not detected; without `--wait` nothing checks after the request. `--apply` re-plans from scratch - fresh read, fresh
  resourceVersion - and its conflict check spans its own plan-to-execute
  window, not the gap since your dry-run. If the object may have changed
  since the preview, re-run `--dry-run` and re-read the diff before applying.
  The same holds for `uid`: `--apply` sends the uid in the response to its own
  dry-run delete, not the one an earlier `--dry-run` showed.
