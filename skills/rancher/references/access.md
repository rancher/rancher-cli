# Roles, permissions, grants and removing access

### Role grants and secrets

Roles are facts about bindings that exist - what `grant`/`revoke` create and
delete, what the `inspect project`/`inspect cluster` MEMBERS sections list,
and what `inspect user`'s ROLE BINDINGS section enumerates across every scope for one
subject. Permissions are what a subject can actually do, and only the server
can answer that (a warning starting "discovery for API group(s)" means the
resource name was resolved while some API groups could not be read; qualify
it as `<resource>.<group>` to make the question exact):
`rancher access can <verb> <resource> --cluster <c> [-n <ns>]
[--as <subject>]` asks the authorizer for one decision (exit 0 allowed, 12
denied, 13 unknown; `--as` takes a user, a group principal, or a service
account) and `rancher access list --cluster <c> [-n <ns>] [--as <user or
service account>]` lists the rules it reports (never a group: ask a point
question instead). Read `data.decision`,
`data.reason` (the server's own words), `data.resultSource`, and
`data.identity`; `unknown` means the server could not say, never "no".
`rancher access who-can <verb> <resource> --cluster <c> [-n <ns> | --project <p> | -A] [--show-denied]`
lists the binding subjects the authorizer allows (one SubjectAccessReview
each; `--project` and `-A` give one row per subject per namespace). Read
`data.subjects[].decision`; a row `unknown` with note `groups unknown` is a
subject Rancher knows no groups for - "could not be ruled out", never
"no" - and does not change the exit code; `data.rollup.outsideCandidateSet`
names what the answer cannot see.
`rancher access explain <verb> <resource> --cluster <c> [-n <ns>] [--as <subject>]`
says why: read `data.decision` and `data.reason` first, then `data.tree`
(the binding the server accepted, its role's covering rules, and the
ClusterRoleTemplateBinding, ProjectRoleTemplateBinding, GlobalRoleBinding,
GlobalRole and RoleTemplates behind it) and `data.chain` with
`data.chainIssues` - `partial` means a read was refused or an object was
missing, never that the decision is in doubt. Quote nodes as facts about
bindings; the rules shown are a display filter, the server chose the
binding.
`rancher access clusters [--as <user>]` says where Rancher lets a subject
in: one row per cluster the caller can see, `data.clusters[].access` is
`reachable`, `refused`, `unknown`, or `notApplicable` (the management
cluster for another user; never counts against completeness). Read
`data.reachable[]` for the answer, `data.clusters[].bindings` for what
grants it, and `data.bindingsDiscovery.status` before treating an empty
GRANTED BY as "no binding". Reachable means the proxy admits the identity,
not that it may do anything inside; ask `access list --cluster <c>` for
that. A group or service account is refused here.
`rancher access projects [--cluster <c>] [--as <user>]` says which
projects a subject can see into: `data.clusters[].projects[].visibility`
is `all`, `partial`, `none`, or `unknown` (the authorizer's answer to
getting each namespace of the project, summarized per project),
`data.visibleProjects[]` the compact list, `data.memberOf.projects[]` the
subset the subject is a Rancher member of (a ProjectRoleTemplateBinding
names it) and `data.inheritedAccess[]` what it reaches through cluster or
global grants without membership. Visibility is not membership and
membership is not permission; a `null` `data.memberOf` means the binding
read failed, not "no memberships". A member asking about themselves gets
visibility only. Every `access` command runs under one 60 s time limit for
the whole command; on "fan-out budget ... exhausted" (a terminal error, nothing
partial is reported) rerun with `--timeout <duration>` (for example
`--timeout 5m`) or narrow the question: `access projects --cluster <c>`, or
`-n <ns>` / `--project <p>` on `access can|list|who-can` (`access clusters`
takes neither).
Never present a role list as permissions.

- `rancher grant <subject> (--project <p> --cluster <c> | --cluster <c> | --global) --role <r>` -
  grant a role at project, cluster, or global scope. Subject is a user id,
  username, or a user or group principal (for example `local://<id>`,
  `<provider>_user://<id>`, `<provider>_group://<id>`). A subject that is not a
  principal is matched by user id, then by username. A display name is never
  matched: when the value is the display name of one or more users and the user
  id or username of no user, the command fails with NOT_FOUND and lists those
  users with their user ids. Groups must be passed as a group principal (for
  example `okta_group://<id>`); `--type` only
  disambiguates principals whose provider prefix cannot be inferred.
  Principals with no matching user trigger a provisioning warning;
  Rancher auto-creates the user when the binding applies. Dry-run/apply --yes.
  A role template whose `hidden` field is true (Rancher tells its UI not to
  display it) is accepted when typed in full, with a warning in the preview and
  in the result: `role "admin" is a hidden role template: Rancher tells its UI
  not to display it`. Relay the warning to the user and check that the role is
  the one they mean. `revoke` does not warn.
  When Rancher refuses the read of users (a project owner, for example), the CLI
  warns and uses the subject exactly as typed if it is `local://<id>` or an id
  that looks like `u-lpnxw` (`u-`, `m-` or `user-` followed by at least five
  characters from `bcdfghjklmnpqrstvwxz2456789`). The server-side dry-run checks
  only that Rancher accepts the create: if no user has that id, the create still
  succeeds and the binding has no effect. A username, an id that does not look
  like `u-lpnxw`, or a user principal other than `local://<id>` then fails: pass
  the user id as `local://<id>`; an administrator can list ids with
  `rancher get users`. When Rancher refuses the
  read of the existing bindings, grant continues with a warning and does not
  check whether the subject already has the role.

- `rancher revoke <subject> (--project <p> --cluster <c> | --cluster <c> | --global) [--role <r>]` -
  revoke role binding(s). Omitting --role revokes every role at the scope
  (destructive, needs `--yes`). Partial failures leave already-removed
  bindings gone. Revoke removes the binding only; an auto-provisioned user
  is removed with `rancher delete user <id> --apply --yes` (destructive,
  cascades role bindings). When Rancher refuses the read of the user, revoke
  warns and uses the id exactly as typed. It must also list the bindings (it
  fails if Rancher refuses that), and for an id without `local://` it matches
  `userName` only.

- Removing all of a user's access has three parts; do and report each:
  1. Bindings: `rancher inspect user <username> -o json` lists them at global, cluster
     and project scope, and gives the user id in `data.user.id`. `revoke` at each scope,
     then read `inspect user` again. A project is listed as `<cluster id>:<project id>`;
     pass the part after the colon to `--project` and the part before it to `--cluster`.
  2. Tokens: read both kinds, as an administrator. `rancher get management-tokens -l
     authn.management.cattle.io/token-userId=<user id> -o json` lists the user's v3
     tokens (the `management-token` kind); `rancher get tokens -l
     cattle.io/user-id=<user id> -o json` lists the user's ext tokens (the `token` kind,
     which `rancher create token` creates). A token still authenticates after every
     binding is revoked. Delete the user's tokens (`rancher delete management-token <name>
     --allow-deprecated`, `rancher delete token <name>`; preview each with `--dry-run`
     first, then apply with `--apply --yes`), or say which of the two reads you could not
     make. "None found" is a claim about both kinds.
  3. The account: a user with no bindings can still log in and still has what their
     groups give them. Disabling is `rancher patch user <id> --type merge --patch
     '{"enabled":false}'` (preview with `--dry-run` first, then apply with
     `--apply --yes`). Disable only when the request says to close, disable or deactivate
     the account. "Remove their access" and "has left" are not that: leave the account as
     it is and report that it is still enabled.

  Rancher creates the ClusterRoleBinding `grb-<user id>-view` on the local cluster so
  the user can read their own User object. It is deleted with the user; it is not a
  binding you failed to revoke.

- `rancher get users` hides Rancher's internal system accounts (cluster agent
  and provisioning accounts) by default, matching what Rancher itself hides;
  pass `--include-system` to see them. Cluster teardown removes its agent
  account automatically - never delete a system account by hand.

- `rancher get rolebindings --cluster <c> --namespace <ns>` and `rancher get
  clusterrolebindings --cluster <c>` list the Kubernetes role bindings of a
  cluster (`get roles` and `get clusterroles` list the roles), and `rancher get
  rolebinding <name> --cluster <c> --namespace <ns> -o json` shows one, with its
  `roleRef` and `subjects`. Rancher creates many of these objects from its role
  templates, global roles and Rancher role bindings: change access with
  `rancher grant` and `rancher revoke`, never by editing these objects. To learn
  what a subject may do, ask `rancher access can`; do not work it out from these
  objects. These kinds are read-only: `patch`, `create resource` and `delete`
  fail with `VALIDATION`.

- `rancher get roletemplates -o json` - filter `context=="cluster"` or
  `context=="project"` to find role names for `--role`; `-o table` shows the
  context in a CONTEXT column and the `hidden` field in a HIDDEN column. Once a
  scope flag (`--cluster`, `--project`, or `--global`) is on the line,
  `--role <TAB>` completes live (requires the network-completion opt-in).
  `grant --role <TAB>` leaves out role templates whose `locked` field is true
  and, at global scope, global roles that inherit a locked role template or one
  that does not exist (Rancher does not allow a new binding to such a role).
  It also leaves out role templates whose `hidden` field is true (Rancher tells
  its UI not to display them); `grant --role` typed in full accepts one, with a
  warning. `revoke --role <TAB>` still offers all of them. The candidates in a
  NOT_FOUND error of `grant` follow the same rule, and
  `create user --global-role` completion and its NOT_FOUND candidates leave out
  the same global roles.
  Positional completion may return the full candidate list and rely on the
  shell's client-side prefix filtering.
