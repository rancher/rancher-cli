# Migrating from the legacy Rancher CLI

Config auto-migrates on first run: a legacy `~/.rancher/cli2.json` (or
`$RANCHER_CONFIG_DIR/cli2.json`) is imported into the new config + credential
store the first time you run against that config dir (only when the dir was
not explicitly overridden). Run `rancher config purge-legacy` afterward to
remove the plaintext original.

Output is enveloped JSON/YAML/table (`-o table|json|yaml`, plus `-o name` for
IDs only); table is the default for interactive use. Mutating commands need
`--apply` (and `--yes` for destructive ones) — there is no implicit write.
Run `rancher schema` to discover the full command surface, including flags,
tiers, and stability, without reading docs.

## Command disposition

Every legacy command below maps to exactly one of: a new spelling, a
replacement recipe, or a documented drop.

| Legacy | Disposition |
|---|---|
| `login` | → `rancher auth login` |
| `context switch` | → `rancher config use-context` |
| `server` (ls/switch) | → `rancher config get-servers` / `use-server` |
| `clusters` / `nodes` / `projects` / `namespaces` (+create/delete/rm where they existed) | → `rancher get/create/delete <kind>` |
| `machines` | → `rancher get machines` |
| `settings` | → `rancher get settings` / `get setting <name>` |
| `inspect <type> <id>` | → `rancher get <kind> <name> -o json` |
| `ps` | → `rancher get deployments/pods` (+ `rancher inspect deployment`) |
| `wait <type> <id>` | → `rancher get <kind> <name> --wait` |
| `kubectl …` | → one-time `rancher kubeconfig --cluster <id>` + native `kubectl` (merge recipe below); `auth get-token` handles credentials transparently |
| `token` | → `rancher auth get-token` (already the exec-credential plugin; the legacy command is named `token`) |
| `token delete` | → `rancher auth delete-token` |
| `ssh <node>` | DROPPED — RKE1/node-driver only, Norman-only; keep the legacy binary for this |
| `up -f compose.yml` | DROPPED — compose-era, no replacement |

## `rancher kubeconfig`

`rancher kubeconfig --cluster <id-or-name>` emits a bare kubeconfig YAML
document to stdout. Its user entry runs `rancher auth get-token` as a
kubectl exec-credential plugin, so no long-lived token is ever written to
disk — only the CLI's own short-lived cache. It replaces the legacy
`kubectl` passthrough's purpose; the CLI itself does not wrap kubectl.

**PATH note:** the exec stanza runs `rancher` by name, so the `rancher`
binary must be on `PATH` for kubectl to authenticate.

**Merging into your default kubeconfig** (the command only writes to
stdout — it never touches `~/.kube/config`):

```
KUBECONFIG=~/.kube/config:<(rancher kubeconfig --cluster X) kubectl config view --flatten > merged && mv merged ~/.kube/config
```

**No `rancher` binary on the target machine?** (CI, teammates without the
CLI) `rancher kubeconfig`'s exec mode is not the only option — the
Dashboard's "Download KubeConfig" mints a server-side kubeconfig with an
embedded, revocable token that needs no local binary. A future `rancher
kubeconfig --embed-token` will cover that mode from the CLI directly.

## `--wait` on named `get`

`rancher get <kind> <name> --wait [--for state=<value>|condition=<Type>[=<Status>]] [--timeout <duration>]`
replaces the legacy standalone `wait` command. Unlike `kubectl wait`, it
fails fast on a Rancher-reported provisioning error instead of burning the
full timeout. For anything `--wait` doesn't cover well (arbitrary JSONPath
conditions, non-Rancher resources), use `kubectl wait` via `rancher
kubeconfig`.
