# Clusters, nodes, etcd snapshots, certificate rotations and encryption key rotations

Kind names have kubectl-style aliases (e.g. `get pods`,
`get cm`). `get kubernetes-versions` always marks the newest release per
distro (`newest`); when the server's default version for a distro is empty
or missing, or is not one of the releases the server lists, no version is
marked as the default, `newest` is the only sign of which version to pick,
and a warning says so.

- `cluster upgrade <cluster> --k8s-version <version>` upgrades an RKE2 or K3s
  cluster. Preview first (`--dry-run`) and read the preview: the warnings say how
  Rancher runs the upgrade, and one names the newest successful etcd snapshot
  of the cluster, because the upgrade cannot be reversed with this command (the
  way to an earlier version is an etcd snapshot restore of type Kubernetes
  version and etcd). Then `--apply --yes`; the command waits until Rancher
  reports the upgrade done (default `--timeout 30m`, `--wait=false` returns at
  once). A refusal names the rule: the version must be a release of the cluster's
  own distribution that `rancher get kubernetes-versions` lists and not lower
  than the current one. A retryable error of code `GENERIC` (an operation in
  progress, a cluster that has not reported its version) means: try again later.
  A timeout leaves the upgrade running: run the same command again with the same
  version to wait again.

- `cluster set-upgrade-strategy <cluster>` sets how Rancher upgrades the nodes of
  an RKE2 or K3s cluster: `--control-plane-concurrency` and
  `--worker-concurrency`, `--control-plane-drain` and `--worker-drain`, and for a
  provisioned cluster the drain options `--control-plane-drain-force`,
  `--control-plane-delete-emptydir`, `--control-plane-grace-period` and
  `--control-plane-drain-timeout` (and the same four with `--worker-` in place of `--control-plane-`). Preview
  first (`--dry-run`) and read the warnings. A provisioned cluster needs
  `--apply --yes`, an imported cluster `--apply`. An imported cluster takes only
  the two concurrency flags (whole numbers of 1 or more) and the two drain
  flags. The command does not wait for an upgrade. When it turns drain on for a provisioned
  cluster, it writes the Rancher UI's values for the drain options the cluster
  does not have yet.

- `node drain <node> --cluster <c>` asks Rancher to drain a node: run it with
  `--dry-run` first, then `--apply --yes`. The dry-run preview lists the pods on
  the node in `data.objects` (`kind` pod, `outcome` `will be evicted`, `skipped` or
  `blocks the drain`, with `message`) and warns that Rancher runs the drain as
  its system user; relay both before applying. By default it waits until
  Rancher reports the drain done or failed. `node stop-drain <node> --cluster
  <c>` stops a running drain with `--apply` alone; the node stays cordoned. Read
  the state with `rancher get management-node --cluster <c>`. An error with
  `data.error.details.committed` true means the request was accepted: run
  `data.error.details.recovery`, never the command again. `writeVia` in the
  Effect says which path the write takes. `kubernetesAPI` (a caller with access
  to Rancher's local cluster) has a server-side dry-run and a resourceVersion
  precondition. `rancherAction` (a caller without that access, such as a Cluster
  Owner or Manage Nodes user) has `previewSource: serverReads`: Rancher accepts
  the request in any state and the request is not checked against the preview.
  Relay its warnings before applying.

- An etcd snapshot, an etcd snapshot restore, an encryption key rotation and a
  certificate rotation of an imported RKE2 or K3s cluster are manifests of the
  kinds `etcd-snapshot-save`, `etcd-snapshot-restore`, `encryption-key-rotation`
  and `certificate-rotation`, created with `create resource -f <file>`: the
  object the Rancher UI creates, in the namespace named by the cluster's id with
  `spec.clusterRef` and `spec.ttl: 60` (REFERENCE.md, "Day-2 operations on
  imported clusters", has the manifest). Run `--dry-run` first and relay its
  warnings: the command checks the cluster before the preview, and a refusal
  names what is wrong (the cluster is not ready, day-2 operations are off, the
  snapshot is not one the Rancher UI restores). A snapshot needs `--apply`; a
  restore and the two rotations need `--apply --yes`, and the preview says
  `reversible` is false for all four. A restore stops Kubernetes on every node
  and loses every change made after the snapshot: confirm the snapshot with the
  user. Take a snapshot before an encryption key rotation; the command refuses a
  rotation of a cluster with no etcd snapshot. Wait with `get <kind> <name>
  --cluster <id> --wait`, which ends with an error when Rancher reports the
  operation Failed or Canceled. `patch` changes `spec.paused` and `spec.ttl`
  only. A server older than Rancher v2.15 (v2.16 for `certificate-rotation`)
  answers `CAPABILITY_UNSUPPORTED`.

Node-driver provisioning is two steps - author the machine config, then
reference it from the cluster:

```
rancher create machine-config digitaloceanconfig web-pool --region nyc3 --size s-2vcpu-4gb --dry-run -o json
rancher create machine-config digitaloceanconfig web-pool --region nyc3 --size s-2vcpu-4gb --apply
rancher create cluster do-1 --machine-config digitaloceanconfig/web-pool --cloud-credential cc-abc12 --k8s-version v1.30.5+rke2r1 --etcd --controlplane --worker --dry-run -o json
rancher create cluster do-1 --machine-config digitaloceanconfig/web-pool --cloud-credential cc-abc12 --k8s-version v1.30.5+rke2r1 --etcd --controlplane --worker --apply
```

`create cluster` sets less than the Rancher UI cluster form does for a custom or
machine-pool cluster. It does not set the machine pool's `drainBeforeDelete`
(with it unset, Rancher ignores the drain when it deletes a machine of the
pool). It does not set `cni` (for RKE2, Rancher uses `calico` when nothing sets
it), `ingress-controller`, `chartValues`, the Pod Security Admission template,
the etcd snapshot retention and schedule, `localClusterAuthEndpoint`,
registries, agent environment variables, agent scheduling customization or data
directories. Set them with `create resource -f` and a full manifest, which is
also the only way to get several machine pools. The CLI adds no cluster
members; add them with `rancher grant` afterwards. A cloud credential must
already exist: `rancher create cloud-credential <driver> <display-name>` creates one
(run it with `--dry-run` first; give a secret field only with
`--from-file <field>=<path>`, or `--from-file <field>=-` for stdin, never as a `--set` value),
and `rancher get cloud-credentials` lists the ones that exist; its NAME column is the credential id.
`--cloud-credential` and the `<credential>` of `patch` and `delete cloud-credential` take that id (with or without
`cattle-global-data:`), never a display name; a value that is the id of one credential and the display name of another
is refused with `AMBIGUOUS_SELECTION`: give the id as `cattle-global-data:<id>`.
`rancher patch cloud-credential <credential>` changes one (`--set`, `--from-file`,
`--display-name`, `--description`; fields not given keep their values; `--yes` when it
replaces a secret value) and `rancher delete cloud-credential <credential>` deletes one
(`--apply --yes`; refused while a provisioning cluster uses it); run each with `--dry-run` first. The CLI has
no command or flag for hosted clusters (EKS, AKS, GKE). Rancher creates the
management cluster, its default and system projects and the registration token
`default-token`; for a cluster created without the
`field.cattle.io/no-creator-rbac` annotation it also creates the creator's
owner binding on the cluster.

The uncurated kinds (amazonec2config, azureconfig, harvesterconfig,
vmwarevsphereconfig) take driver fields via repeatable `--set field=value`.
Credential fields are rejected on a machine config - they belong on a
cloud credential (`cc-*` secret), referenced via `--cloud-credential`.
Completion on `--set` returns only statically-known driver fields for curated
kinds; uncurated kinds return no completion candidates by design, not by
failure.

A machine config is SINGLE-USE: the first cluster that references it
takes ownership (a controller ownerReference) and it is deleted with
that cluster. Never reuse an existing machine config for a new
cluster - even a perfectly matching one, even within the same
cluster - create a fresh config per cluster instead. After a cluster
delete, its machine configs are cascade-deleted with it: do not report
them as orphaned or try to delete them separately.

Unset curated machine-config fields inherit the node-driver's declared defaults,
which may be stale (a retired image slug will loop provisioning forever). The
`create machine-config` dry-run preview lists the unset value fields in its
`warnings`; set them explicitly rather than trusting the driver's defaults.

A `creatorId annotation not set` warning on `create cluster` means the
command could not set the `field.cattle.io/creatorId` annotation before the
server-side dry-run (the SelfUser request in ext.cattle.io failed or
returned no valid user ID). rancher-webhook may then reject the dry-run
with an admission error. That denial is final for this invocation and for
`--apply` alike: every invocation plans first, and the plan repeats the
SelfUser request and the dry-run, so `--apply` returns the same denial
before any real create is attempted. Recover by logging in again, or
checking that the account may create `selfusers.ext.cattle.io`, then
re-run. When the dry-run passes despite the warning, the create proceeds
and the warning rides along.

Follow through to readiness and access - and read the registration
(join) commands from `inspect cluster --registration`, never from the UI:

```
rancher create cluster web --custom --k8s-version v1.32.4+rke2r1 --apply
rancher inspect cluster web --registration --etcd --controlplane --worker -o json   # data.registration.withOptions.nodeCommand: run on each node
rancher get cluster web --wait --timeout 15m   # kind default: condition=Ready
rancher kubeconfig --cluster web > kc.yaml
```

The node commands (`nodeCommand`, `insecureNodeCommand`) carry no role flag,
and run as printed they stop with `You must select at least one role.` Never
edit a printed command: pass the node command options to `inspect cluster <name>
--registration` and run the command it composes, from
`data.registration.withOptions`. The options are `--etcd`, `--controlplane`,
`--worker` (all three when none is given), `--node-name`, `--address`,
`--internal-address`, `--label key=value` and `--taint key=value:effect`; they
apply only to a custom cluster. A value with a character the Rancher UI would
remove, a value that breaks the Kubernetes rule for its flag, or a label key
Rancher sets on the node itself is refused with VALIDATION naming the flag: fix
the value and run again. For a Windows worker
add `--windows` (no role flag, no `--label`, at most one `--taint`) and run
`data.registration.withOptions.windowsNodeCommand` in PowerShell; a retryable
`GENERIC` refusal means the cluster is not ready for Windows nodes yet. Ask the
user which roles each node takes when that is not stated.

`inspect cluster <name-or-id>` is the one-call cluster triage: management
identity and version, the provisioning CR's ready flag and conditions, machine
pools, and the registration commands (node command and import command) plus
`manifestUrl`. The management read is mandatory: an unknown or ambiguous
cluster, or a failed management read, is a structured error with a non-zero
exit. Only the provisioning and registration sections degrade - to `warnings`,
or a `provisioningNote` for a confirmed missing provisioning CR. Check
`data.status` presence before reading conditions. `data.agent` carries the
management Cluster's agent connectivity (`connected`/`ready` + failing
message) - an agent that is not connected or a cluster that is not ready also
lands in `warnings`, so check it before trusting `data.status.ready`. The
registration section holds only a message and a hint by default, and the
commands are left out (they contain the cluster registration token); pass
`--registration` to fetch and show them. The commands are also available with
`rancher get clusterregistrationtoken default-token --namespace <cluster-id>`.
Without the flag `data.registration` carries `{"withheld":true,"hint":"..."}`
- check `data.registration.withheld` before reading `nodeCommand`. On an
imported cluster `data.registration.notApplicable` names the node command
fields that do not apply (they stay present - pick the import
`command`/`manifestUrl` instead), and member entries with `system: true` are
accounts Rancher uses internally, not bindings an administrator added.

`cluster upgrade` waits for Rancher to report the upgrade done, not for
`Ready`: `rancher get provisioning-cluster --wait` returns when the cluster's
`Ready` condition is `True`, which can be before or during an upgrade. The
timeout error of `cluster upgrade` (`data.error.details.committed` true) says the
upgrade goes on; wait again by running the same command with the same version.

- `rancher get etcd-snapshot` lists the etcd snapshots of a cluster that Rancher
  provisions or imports. `--namespace` defaults to `fleet-default`, the
  namespace of provisioning clusters in the default Fleet workspace. For a
  provisioning cluster in another Fleet workspace, pass that workspace's name.
  For an imported cluster, or a cluster imported through Rancher Turtles, pass
  the cluster's id: `--namespace <cluster id>`. Rancher v2.15 and later creates
  etcd snapshot objects for these clusters; Rancher v2.14 creates them only for
  provisioning clusters. `--selector rke.cattle.io/cluster-name=<name>` lists
  the snapshots of one cluster. For a provisioning cluster, `<name>` is the
  cluster's name. For an imported cluster, or a cluster imported through Rancher
  Turtles, `<name>` is the cluster's id (`c-...`). It is read-only: `patch`,
  `create resource` and `delete` fail with `VALIDATION`. To take a snapshot of an
  imported cluster, create an `etcd-snapshot-save`.

- `rancher cluster save-etcd-snapshot <cluster>` takes an etcd snapshot of a
  cluster that Rancher provisions with RKE2 or K3s. Preview it with `--dry-run`,
  then run it with `--apply --yes`; the command waits until Rancher reports the
  snapshot taken and lists the cluster's snapshots. Take a snapshot before an
  upgrade or another change that is hard to undo. It refuses while another
  operation is in progress on the cluster (the error is retryable: try again
  after the operation ends) and while the cluster is not ready. If you run it
  again while its own snapshot is in progress, it changes nothing and waits for
  that snapshot. `--wait=false` returns once the server accepts the request. It
  refuses an imported cluster: an etcd snapshot of one is a manifest of the kind
  `etcd-snapshot-save`, created with `create resource -f`.
