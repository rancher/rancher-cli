# Output formats, flags and running unattended

Read `data`; surface `warnings` to the user. Other formats: `table` (default,
for users), `wide` (table family - adds extra columns to the same rows and
never filters rows), `yaml`, `name` (only the NAME column, one per line), or
`template=<go-template>` - a Go template over the `data` of the JSON output
(same shape as `-o json`) - often shorter than piping JSON into another
interpreter, e.g. names and creation times of every kubeconfig token object:
`rancher get kubeconfigs -o 'template={{range .}}{{.metadata.name}} {{.metadata.creationTimestamp}}{{"\n"}}{{end}}'`.
Anything else is a `VALIDATION` error. `metadata.managedFields` never appears in CLI
output across all formats. To narrow a list by label, use `--selector`
(`-l`) with a Kubernetes label selector (`-l app=web,tier!=cache`): the
server filters, except that the CLI applies the selector itself when `get` of clusters, projects, provisioning clusters, etcd snapshots, management nodes or namespaces reads through the Steve API (`/v1`, the API the Rancher UI uses) after the Kubernetes API refuses it with 403. Reach for it before `-o template`. It does
not combine with a name. `get namespaces` of a project owner or member is read this way too; without `--project`, a warning says
so and names `rancher access projects --cluster <c>`, which lists the projects you can see.

A list's `data` is a bare array - never `.items` - regardless of kind. Real
`get clusters -o json` output, trimmed to one cluster and the fields that
matter for triage:

```json
{
  "apiVersion": "cli.rancher.io/v1",
  "kind": "ClusterList",
  "data": [
    {
      "apiVersion": "management.cattle.io/v3",
      "kind": "Cluster",
      "metadata": { "name": "local", "resourceVersion": "3769" },
      "spec": { "displayName": "local", "internal": true },
      "status": {
        "conditions": [{ "type": "Ready", "status": "True" }],
        "version": { "gitVersion": "v1.35.5+k3s1" }
      }
    }
  ]
}
```

`data[].metadata.name` is the id; `data[].spec.displayName` is the name shown
in tables. `get pods -o json` follows the same shape, trimmed here to the
fields most triage reads need:

```json
{
  "apiVersion": "cli.rancher.io/v1",
  "kind": "PodList",
  "data": [
    {
      "apiVersion": "v1",
      "kind": "Pod",
      "metadata": {
        "name": "rancher-webhook-65fcbccf9f-8ckhj",
        "namespace": "cattle-system",
        "labels": { "app": "rancher-webhook" }
      },
      "status": {
        "phase": "Running",
        "containerStatuses": [{ "name": "rancher-webhook", "ready": true }]
      }
    }
  ]
}
```

- **Empty values:** a flag typed with an empty or blank value (`--flag ''`,
  `--flag=`) is refused with `VALIDATION` before any request, never treated as
  omitted; build `--flag "$VAR"` only when `VAR` is set. A bool flag typed with
  `=` and an empty or blank value (`--apply="$APPLY"` with `APPLY` unset) is
  refused too; write `--apply` or `--apply=true|false`.
- **Arguments and bool flags:** extra positional arguments are refused with
  `VALIDATION`; a bool flag takes its value only as `--flag=value`
  (`--dry-run=false`), and a separate word typed after it that the flag
  could take as its value (`true`, `false`, `1`, `0`, ...) is refused - put
  an argument such as `true` or `false` before the flags or after `--`. The
  words after a `-` on its own are not used: a word after it is refused
  (`--help` after a command's name, or `--version` at the root, before the
  `-` still shows the help or the version); keep `-` last or as a flag's
  value (`-f -`).

- **Isolation for scripted sessions:** point `RANCHER_CLI_CONFIG_DIR` at a
  temporary directory and set `RANCHER_CLI_CREDENTIAL_STORE=file` so a scripted run
  never touches the operator's real config or OS keychain. A kubeconfig written
  in such a session records that directory and stops working when it is
  removed.
- **Agent name in the User-Agent:** the CLI sends the Rancher server a
  User-Agent that names the agent harness the session runs in. A harness the
  CLI does not detect can set `AI_AGENT` to its name: a name on the list in
  REFERENCE.md ("What the CLI sends to the server") is reported as is, any
  other as `agent`.

- **Read-only mode:** `--read-only` (or config `readOnly: true` / a
  non-empty `RANCHER_CLI_READONLY` - including the literal `"false"`) refuses
  every command that makes changes before any network call, and nothing in
  that run turns it off. A typed `--read-only` on a local command
  (`auth status`, `config *`, `kubeconfig`, ...) is refused with
  `VALIDATION`; to keep local commands usable in every run, set
  `RANCHER_CLI_READONLY` instead of typing the flag on every call.
  `rancher schema -o json` reports `readOnly`/`readOnlySources`. For real
  enforcement pair it with a Rancher token bound to read-only RBAC - the
  CLI-side gate is a courtesy, not a security boundary, and a read-only RBAC
  token can't `--dry-run` a mutation either (dry-run still needs write verbs
  server-side), so read-only mode refuses those outright too.

- Migrating from the legacy `rancher` CLI? `MIGRATING.md` in the release maps
  every old command to its replacement.
