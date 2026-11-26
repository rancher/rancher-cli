# Surface conventions

The conventions every command, flag and argument of `rancher` follows. A new or changed command, flag or argument
follows these rules; if it cannot, the same commit changes the rule, its test and every command the rule covers.

A rule with a test names it on a `Checked by:` line. A rule with no test yet says so on a `Not checked yet:` line,
with the change that adds the test. Seven of the tests are in `internal/cli/surfaceconventions_test.go`: they read the
schema manifest (see Terms) and fail `make validate` on a flag, argument or command that does not follow its rule and
has no allowlist row. Each of the seven has an allowlist in that file: one row, with its reason, for each flag,
argument or command that does not follow the rule today.

A rule's `Kept on purpose:` line names flags, arguments and commands that do not follow the rule and stay that way,
with the reason; an allowlist row for one of them stays. Every other allowlist row is for a flag, argument or command
that a later change makes follow the rule. A command can also differ today from a rule marked `Not checked yet`: such
a rule may describe behavior that a later change adds. A change that adds, renames or removes a flag name also edits
the concept table of rule 1; the comment above that table lists the steps.

## Terms

- Schema manifest: the output of `rancher schema --all -o json`, recorded in
  `internal/cli/testdata/schema.golden.json`.
- Kind: a Kubernetes or Rancher resource type the CLI knows by name, as `rancher schema` lists it (`pod`,
  `provisioning-cluster`).
- Argument: a value given to a command by its place on the command line, not after a flag name (the command's `args`
  in the schema manifest).
- Bool flag: a flag that is on when given; `--x=false` turns it off.
- Value flag: any other flag; it takes a value. A string flag is a value flag of schema type `string`.
- Placeholder: the word that stands for a value in help, such as `id|name` in `--cluster <id|name>`.
- Enum: a finite set of accepted values, listed in the schema manifest.
- Gate flags: `--apply`, `--yes` and `--allow-deprecated`, listed per command in the `gates` of the schema manifest.
- Mode flags: `--dry-run` and `-o`.
- Mutating command: a command whose command type in the schema manifest is `mutating`.
- Fleet workspace: a namespace on Rancher's local cluster that has Fleet objects and provisioning clusters (the
  `provisioning-cluster` kind); `fleet-default` is the default, and `fleet-local` has the local cluster.

## 1. One flag name per concept

One concept has one flag name on every command. The test has a table of every flag name and the one name of its
concept (the concept table). It fails on a flag name that is not in the table, on a row that no flag has, and on a
flag whose name differs from the name the table gives its concept.

Cluster selection: `--cluster <id|name>` selects one cluster on every command. A command that works across clusters
accepts it once per cluster, never as a comma list. `--all-clusters` means the clusters Rancher reports the user can
access and is never combined with `--cluster`. No other flag selects clusters. `--namespace` (`-n`) selects a Fleet
workspace.

Kept on purpose: the flags of `auth get-token` and `token`, which are the flags of the `exec` block of a kubeconfig;
Rancher writes them, and existing kubeconfig files have them. `--current-context` makes one of the clusters the
command selects the current context of the kubeconfig it writes; it never selects a cluster.

Checked by: `TestEveryFlagNameIsInTheConceptTable` (internal/cli/surfaceconventions_test.go)

Not checked yet: the cluster-selection part; its test is added when `--cluster` becomes repeatable.

## 2. One meaning and one kind of value per flag name

A flag name means the same thing, and takes the same kind of value (for example a duration, a number, or an id or
name), on every command. A command that copies a flag of kubectl or Helm keeps that tool's meaning. `--type` is the
type of the object the command writes. `--timeout` is the longest time the command runs before it gives up. `--all`
means every object that the command's argument can name. `--force` is used only on a command that copies the
`--force` of kubectl or Helm, with that tool's meaning.

Kept on purpose: `--scope` on `create oidc-client` (Rancher's OIDC client field is `scopes`) and on the `skill`
commands (the install location, which coding-agent harnesses call a scope); `--force` on `app upgrade` (Helm) and on
`node drain` (kubectl drain).

Not checked yet: a test is added with one shared description per flag name used on two or more commands.

## 3. An object is named by its id or its name

A flag or argument that names a Rancher or Kubernetes object accepts its id or the name in the NAME column of `get`.
Its placeholder says which it accepts: `id|name` for either, `name` or `id` for one. A command that acts on an
existing cluster accepts the cluster's id or its display name (the DISPLAY NAME column of `get cluster`), as
`--cluster` does.

Not checked yet: a test is added with the `placeholder` field in the schema manifest.

## 4. Lists are repeatable flags

A list of identifiers or of `key=value` pairs is a repeatable flag, one value per occurrence, never split on commas. A
comma list is allowed only for a grammar Kubernetes defines (the label selector of `--selector` and `-l`). A repeatable
flag has a singular name; Helm's `--values` keeps its name. The test fails on a string flag whose description contains
`key=value`, "comma-separated" or "separated by".

Checked by: `TestEveryListFlagIsRepeatable` (internal/cli/surfaceconventions_test.go)

Not checked yet: that a repeatable flag has a singular name, and that no command splits a value on commas when its
description does not say so. Their tests are added with a repeatable flag type in the flag parser.

## 5. A flag given twice is refused

A command refuses a second occurrence of a flag that takes one value, with a usage error that names the flag. Only
repeatable flags take more than one value.

Not checked yet: a test is added when the flag parser refuses a repeated flag.

## 6. One duration form

Every duration flag takes a duration such as `90s`, `2h` or `30d12h`; a leading number of days is allowed. Its schema
type is `duration` and its placeholder is `duration`. A flag that copies kubectl's integer seconds (`--grace-period`)
keeps kubectl's form.

Not checked yet: a test is added with the `duration` flag type in the schema manifest.

## 7. Counts are numbers

A flag that takes a count, such as a number of nodes or lines, has schema type `int` and placeholder `number`. A flag
that takes a number or a percentage has the placeholder `number|percent`.

Not checked yet: a test is added with the `int` flag type in the schema manifest.

## 8. On/off values are bool flags

An on/off value is a bool flag (`--x`, `--x=false`). A bool flag that is not given leaves the server's default, where
its description says so. `--no-x` is used only where the kubectl or Helm flag it copies starts with `no-` (Helm's
`--no-hooks`). A command whose only input is one on/off value may take it as `on` or `off`: as a word of its name
(`telemetry on`) or as its argument (`config set-token-reuse on`). The test fails on a string flag whose enum is
`true` and `false`, or whose description contains `true|false` or `false|true`.

Checked by: `TestEverySwitchIsABoolFlag` (internal/cli/surfaceconventions_test.go)

Not checked yet: that a `--no-x` name exists only where the kubectl or Helm flag it copies starts with `no-`; no
change adds this test yet.

Not checked yet: that a bool flag that is not given leaves the server's default; a test is added with one shared
description per flag name.

## 9. A finite set of values is an enum in the schema manifest

Every finite set of values is an enum in the schema manifest. Help and completion print the enum's values (`a|b|c`)
with the default. Values are lowercase with a hyphen between words. The test fails on a value flag with no enum whose
description, outside backticks, has a `|` between two letters or digits, or a list of three or more lowercase words in
parentheses, separated by commas.

Kept on purpose: `--restore-mode` of `cluster restore-etcd-snapshot` takes Rancher's values of `restoreRKEConfig`
(`kubernetesVersion`), which the operator also writes in the field and in an `etcd-snapshot-restore` manifest.

Checked by: `TestEveryValueListIsAnEnum` (internal/cli/surfaceconventions_test.go)

Not checked yet: the help rows that print the enum, its default and the command's own `-o` values; their test is
added with help pages built from the schema manifest.

## 10. Short flags mean what kubectl means

A short flag exists only for kubectl's meaning of its letter: `-n` namespace, `-o` output, `-l` selector, `-p` patch,
`-f` filename, `-A` all namespaces, `-c` container, `-v` verbosity level, `-h` help. A short flag with another letter
needs a change to this rule first. The test fails on a short flag that is not one of these letters for its long name,
and when the letters listed here differ from the test's own list.

Kept on purpose: the `-h` and `-v` the CLI adds itself, outside the schema manifest. Today `-v` is the short form of
`--version`; a later change makes it the verbosity level.

Checked by: `TestEveryShortFlagMeansWhatKubectlMeans` (internal/cli/surfaceconventions_test.go)

## 11. The object is an argument; its cluster, namespace or project is a flag

The object a command acts on or creates is an argument. The cluster, namespace, project or Fleet workspace that the
object belongs to is a flag. A server entry in the config directory is the argument `<name>`, and a server's address
is `--url`. An argument's placeholder is the kind of value (`<name>`, `<id>`, `<id|name>`), never the concept
(`<cluster>`). The test fails on an argument named `cluster`, `namespace`, `project` or `server`, and on a flag named
`--name`.

Kept on purpose: the `cluster` commands (the cluster is their object), `project add-namespace` and
`project remove-namespace` (the namespace is their object), `create project [id]` (optional: Rancher generates the id)
and the exec plugin's `auth get-token --name`.

Checked by: `TestEveryPositionalIsTheObject` (internal/cli/surfaceconventions_test.go)

Not checked yet: the placeholders of arguments; their test is added with the `placeholder` field.

## 12. Command shape and verbs

The commands that create, read, change and delete an object of a kind put the verb first:
`create|get|patch|delete <kind>`. A command group is for one of three things: a feature with local state (`auth`,
`config`, `skill`, `telemetry`), a set of Rancher actions on one object (`app`, `cluster`, `extension`, `node`,
`oidc-client`, `project`), or a set of read-only questions to the server (`access`). Every command's verb is in the
table below: the first word of a `create`, `get`, `patch`, `delete` or `inspect` command, the second word of a command
in another group, and the whole name of a top-level command. A top-level noun is a top-level command named for a
thing, such as `schema`; it never changes server state. A local group is a group whose commands all have the command
type `local`. The test fails on a verb that is not in the table, on a local-state verb outside a local group, on a
top-level noun that changes server state, and when this table differs from the test's own copy.

| Verb | Meaning |
|---|---|
| `create`, `get`, `patch`, `delete` | create, read, change, delete an object of a kind |
| `inspect` | show one object with its events and related objects |
| `add-<x>`, `remove-<x>`, `rotate-<x>` | add, remove or regenerate one item of a set on one object (a project's namespaces, an OIDC client's secrets) |
| `install`, `upgrade`, `uninstall` | Helm's words, for apps, extensions and skills |
| `grant`, `revoke` | create or delete Rancher role bindings |
| a Rancher action word | one Rancher action on one object, named as the Rancher UI or API names it; the test lists these commands |
| `set-<x>`, `clear-<x>` | set one setting, or remove it |
| `status` | report the state of what the group manages |
| local-state verbs, in a local group only: `add-`, `update-`, `delete-`, `use-`, `list-`, `edit`, `path`, `login`, `logout`, `prune` | change or read files and credentials of the config directory |
| `on`, `off` | the on or off word of a command whose only input is an on/off value (rule 8) |
| `show`, `reset`, `upload` | print, delete and send the recorded telemetry events |
| a top-level noun | print that thing; never changes server state |

Kept on purpose: `auth get-token` (the exec plugin name Rancher writes into kubeconfigs), `token` (the older exec plugin
name), the `access` commands (each is named after the question it asks, as kubectl's `auth can-i` is) and
`extension list` (it lists the extensions the ClusterRepos offer and the installed ones, which no single kind has).

Checked by: `TestEveryCommandUsesTheVerbTable` (internal/cli/surfaceconventions_test.go)

## 13. Kind names

A kind's name is the singular, lowercase name of its Kubernetes resource. A hyphenated name tells apart two resources
with one name (`provisioning-cluster`, `management-node`, `management-token`) or splits a resource name written as one
word (`fleet-workspace`, `etcd-snapshot`). Every kind accepts its singular, plural and short names, hyphenated and
unhyphenated, in any case. A command with a verb that a kind does not support prints the note the schema manifest has
for that kind and verb.

Not checked yet: a test is added with the change that makes every kind accept every spelling.

## 14. Gate and mode flags only where they apply

A command lists, in help and in the schema manifest, only the gate and mode flags that change what it does: `--apply`
and `--dry-run` on every mutating command; `--yes` on a command whose `gates` list `--yes` or whose `destructive` is
`dependsOnInput`; `--allow-deprecated` on a command whose `gates` list it; and `-o` with only the values the command
accepts. Such a flag typed where it is not listed is refused with a usage error that names the flag and the command's
`gates`. `--no-input` is on every command.

Not checked yet: a test is added with the change that removes the gate flags a command does not use.

## 15. Waiting and timeouts

A mutating command whose server work continues after the request returns offers `--wait` (default on) and `--timeout`.
On `get`, `--wait` or `--for` starts a wait (default off). `--timeout` is the longest time the command runs before it
gives up. It is a `duration`, with its default last in its description. Completion offers the same suggested values
for every `--timeout`, scaled to the flag's default. The CLI's flag for Helm's own wait is `--helm-wait`.

Not checked yet: a test is added with a field in the schema manifest for commands whose server work continues after
the request returns.

## 16. Placeholders

A value flag's placeholder is an explicit field of its declaration and of the schema manifest (`placeholder`). Help and
completion never take it from a backtick span in the description. It is one of the words in two lists: a word for the
form of the value (`id|name`, `name`, `id`, `namespace`, `duration`, `number`, `number|percent`, `file`, `path`, `url`,
`key=value`, `text`), or a noun for the value, Kubernetes' or Rancher's own where one exists (`selector`, `patch`,
`condition`, `version`, `container`, `pod`, `image`, `region`, `host`, `address`, `scope`, `type`, `command`,
`kind/name`, `agent`, `target`, `tag`, `store`, `flow`, `size`). It is never `string`, a default value or a plural.

Not checked yet: a test is added with the `placeholder` field.

## 17. Flag descriptions

A flag used on two or more commands takes its description from one table in code, with a suffix only for a real
difference. Every description:

- starts lowercase, unless it starts with a proper noun;
- has no final period;
- is one sentence of at most about 100 characters;
- ends with "(default X)", with no colon; a default given in words instead of a value is written "(default: <words>)";
- uses "e.g.", "control plane" and "omit for X".

The test refuses the words listed in `test/conformance/rejectedwording_test.go` in command text and in the user
documents it reads.

Checked by: `TestRejectedWordingStaysOut` (test/conformance/rejectedwording_test.go)

Not checked yet: one shared description per flag name and the style rules; their test is added with the table of
shared descriptions.

## 18. A help page prints what the schema manifest has

Every help page prints the command's DESCRIPTION (the manifest's `desc`), ARGUMENTS (the description of each
argument), EXAMPLES, and a required marker on each required flag. Group pages print the manifest's group description.
A list of commands marks deprecated and Rancher Prime commands the same way on every row, and so does the NAME line of
the command's own page.

Not checked yet: a test is added with help pages built from the schema manifest.

## 19. Every value completes

Completion offers candidates for every value flag and argument (from an enum, a list of suggested values or a request
to the server), or the flag or argument has an allowlist row with a reason. Every candidate is accepted by the command
exactly as offered. Completion never offers the names of Secrets or cloud credentials.

Not checked yet: a test is added with the completion allowlist for every value flag and argument.

## 20. One usage error form

A usage error has the form `error (VALIDATION): <command>: <what>`, names the flag or argument as typed, has the hint
`hint: rancher <command> --help` (`rancher <group> --help` for an unknown subcommand), and the command exits 2. Exit 2
is for usage errors, gate refusals (`CONFIRMATION_REQUIRED`, `DRY_RUN_NOT_APPLICABLE`) and `AMBIGUOUS_SELECTION`.
`NOT_LOGGED_IN` exits 3, like the other login errors; it exits 2 today, and a later change makes it 3.

Not checked yet: a test is added with the change that gives every usage error this form.
