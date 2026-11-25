# Projects and their namespaces

`create project --cluster <c> --display-name <name>` lets Rancher generate the id (`p-xxxxx`); read it from `data.metadata.name` of the `--apply` result before `get`, `patch` or `delete project`.

- `delete project <id> --cluster <c>` lists the project's namespaces in the
  dry-run Effect and in the result: `data.objects`, one item per namespace with
  `kind`, `name`, `outcome` (and `code`, `message` for a failure). Rancher
  deletes the namespaces with the `field.cattle.io/creatorId` annotation
  (`rancher deletes`) and keeps the rest (`rancher keeps`); relay the lists and
  the warnings before applying. `--delete-namespaces` also deletes the
  namespaces, one at a time, waits until each is gone, then deletes the project
  and waits for it; `default`, `kube-system`, `kube-public` (and `local`,
  `fleet-local` on the local cluster) are `not deleted`. A namespace the command
  deleted and saw gone is `deleted` in `data.objects`; one that was already
  being deleted stays `being deleted`. A failure, or a namespace not gone within
  `--timeout`, stops the run and the project is not deleted; the error carries
  every item in `data.error.details.objects` (the project `not attempted`) and
  `data.error.details.recovery`, `rancher get project`; run the same command
  again. `delete project` also waits for the project's backing namespace when
  you can read it, with or without `--delete-namespaces`.

- `project remove-namespace <ns>` also accepts a namespace with the project
  label and no project annotation, and makes sure its project label is
  removed.

`create project` and `create resource -f` with a Project manifest give a
`creatorId annotation not set` warning when the CLI could not get your user
ID from the SelfUser request. The project is still created, without the
annotation, and Rancher then adds no owner binding for you on it: you may not
be able to see or manage the project afterwards. The warning shows on the
dry-run: log in again, or check that the account may create
`selfusers.ext.cattle.io`, and re-run the dry-run before `--apply`. A project
already created keeps no owner binding: a cluster owner or an administrator
grants the role. A Project manifest that sets `field.cattle.io/creatorId` to
another user is refused, and so is one that sets it when the SelfUser request
fails.

- **Project scope (`--project`):** `--project <id-or-name>` names a project on
  the cluster `--cluster` names; pass both. Only the commands whose `rancher
  schema` entry lists a `project` flag take it (generated `get`/`delete`/`patch`
  of Downstream namespaced kinds, namespaces and `projectroletemplatebinding`,
  `create resource|namespace|secret`, `project add-namespace`, `grant`, `revoke`,
  `usage`, `access can|list|who-can`); every other command refuses it with
  `VALIDATION` before any request. A list filters to the project's namespaces, a
  named op or mutation must be in one. A `projectroletemplatebinding` is the
  exception: it is in the project's backing namespace on the local cluster (the
  project's `status.backingNamespace`, or the project id when that is empty).
  `get|patch|delete projectroletemplatebinding --project <p> --cluster <c>`
  reads that namespace from the project. A `--namespace` typed together with
  `--project` and `--cluster` must be that backing namespace. `--cluster`
  without `--project` is refused with `VALIDATION`.
  `get namespaces --project <p>` is the membership listing: the server narrows
  the list by the project label and the CLI keeps the namespaces whose
  `field.cattle.io/projectId` annotation (`<cluster>:<project>`) names the
  project. An id wins over another project's display name, with a warning in
  `warnings`; duplicate display names are `AMBIGUOUS_SELECTION` - pass the id.
  Without `--cluster` the refusal names the clusters the project is on.
