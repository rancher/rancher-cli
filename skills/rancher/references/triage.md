# Waits, triage composites, logs and the support bundle

Grammar: `state=<value>` or `condition=<Type>[=<Status>]` (default Status
`True`). JSONPath is deliberately unsupported - for that, use `kubectl wait`
via `rancher kubeconfig`. Waits fail fast when Rancher marks the object
`transitioning=error` (non-retryable, with the server's message) instead of
using up the timeout. An authentication (`AUTH`) or certificate (`TLS_TRUST`)
failure of a request also ends the wait at once, with that error, and so does
`NOT_FOUND` (exit 4); any other error is retried, and a timeout names the last
one. A wait sees a change within a fraction of a second when you may list and
watch the object; with only the `get` permission it polls every 5 seconds. On
success the `-o json`/`-o yaml` envelope carries a one-line `summary` (`wait:
condition=Ready satisfied after 53s`) so pass/fail needs no object parsing;
default/table format prints the same summary as its own stdout line above the
table header. When only the verdict matters, the exit code is authoritative;
`-o name` keeps a successful wait's output to the bare resource name - no need
to receive or parse the full object.

### Triage composites

One call each, with graceful degradation (missing metrics/logs become
`warnings`, never failures).

A composite answers with what you can read. A section you cannot read
is named in `data.degraded` - `{section, state: unavailable|partial, code,
target?}` - with one matching `warnings` entry, and the command still exits
0. Exit 0 therefore does not mean "complete": read `data.degraded` (and
`warnings`) before concluding a section is empty, absent, or zero. Only a
command's required reads - its identity, scope, and substance reads,
listed in each command's `rancher schema <command>` card - fail the command
outright.

- `rancher inspect pod <name>` - status, owners, events, logs, metrics
- `rancher inspect deployment <name>` - rollout + its pods (the pods its selector matches)
- `rancher inspect project <name-or-id>` - project namespaces, workload counts, members, resource quota, usage
- `rancher inspect user <id|username|principal>` - id and names, groups, and role bindings at global, cluster and project level; bindings that exist only, never permissions
- `rancher usage` - capacity and consumption, grouped by `--by namespace|project|node|pod` and narrowed by `--namespace`/`--project`/`--node`; a `--namespace` or `--node` that matches no pods is read; a missing one is NOT_FOUND (a warning when you cannot read it, an RBAC error when you cannot list pods there)
- `rancher images [--by image|workload] [--match substr]` - active-pod image inventory; repeatable `--match` substring-filters (OR), `--by workload` maps each image to its owning workload (CVE blast radius: `--match <affected-repo>: --by workload --all-clusters`)
- `rancher get projects` - without `--cluster`, lists the projects you can read on every cluster; `rancher get project <id>` needs `--cluster`. Only `rancher images` takes `--all-clusters`.

### Observability & support

- `rancher logs <target> [--cluster <c>] [--tail 200] [--since <dur>]
  [--previous] [--container <name>] [--timestamps] [--pod <name>]` -
  verified-location logs of Rancher components for `server`, `agent`,
  `fleet-agent`, `kube-api-auth`, `webhook`, or `api-extension` (the
  imperative API extension server that backs `ext.cattle.io` tokens and
  kubeconfigs - read it when a token or kubeconfig operation misbehaves).
  Multi-pod targets merge every matched pod/container into one
  `[pod/container]`-prefixed body; `--tail` defaults to 200 lines, `--since`
  takes a duration (`30m`, `2h`), `--previous` reads the last-terminated
  container instance, and `--container` narrows a multi-container pod, and
  `--pod` selects ONE pod of a multi-pod target (to read the logs of a single
  replica - e.g. one Rancher server replica during HA triage; an unknown pod
  name is NOT_FOUND listing the real ones). A missing target degrades to an
  honest `NOT_FOUND` (if Rancher runs as a single Docker container, read the
  server logs with `docker logs <container>`; a cluster with no Authorized
  Cluster Endpoint has no `kube-api-auth`). Triage example: `rancher logs
  server --since 1h --timestamps`. Real `-o json` shape:

  ```json
  {
    "apiVersion": "cli.rancher.io/v1",
    "kind": "TargetLogs",
    "data": {
      "pods": ["rancher-webhook-65fcbccf9f-8ckhj"],
      "entries": [
        {
          "pod": "rancher-webhook-65fcbccf9f-8ckhj",
          "container": "rancher-webhook",
          "line": "2026-08-26T19:23:33.127639464Z time=\"2026-08-26T19:23:33Z\" level=info msg=\"...\"\n"
        }
      ]
    }
  }
  ```

- `rancher support-bundle --output-file bundle.tar.gz [--targets ...]
  [--clusters ...]` - packs the logs of every Rancher component target plus
  cluster/node state, events, and a snapshot of a fixed list of settings,
  feature flags and the schema into one tar.gz; `manifest.json` inside records
  each item as collected, skipped (not applicable), or errored, and partial
  bundles are normal - read `errors` and `warnings` before treating the
  archive as complete: a log the server refused is an error item and a warning
  naming the pod and container, and `errors: 0` means every requested log is
  inside. A setting read that fails with anything but not found is an errored
  item too; `errors: 0` also means every setting read got an answer (not found
  counts as one). A run that collects no item from the server fails with the
  code of those requests (the code they share, `GENERIC` when they differ) and
  writes the archive anyway: `data.error.details.outputFile` is its path and
  its `manifest.json` lists each failure. A run that collects at least one
  item from the server exits 0 whatever else failed; a `--clusters` value
  naming an offline cluster therefore still exits 0 while the local cluster
  answers. An interrupted run (exit code 130) keeps the archive and its
  message names the path. A rerun in the same second gets the same default
  name and fails with `VALIDATION`; wait a second or pass a new
  `--output-file`. Complements
  rancherlabs/support-tools' node-level collector, not a replacement for it.
