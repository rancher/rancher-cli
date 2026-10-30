# Rancher usage events: what is recorded (contract 1.0.0)

This file is generated; do not edit it by hand.
A version directory that holds a file named `DRAFT` is not published and may still change; see `published.json` in the directory above.

## Keys outside JSON Schema

`event.schema.json` carries two keys that are not part of JSON Schema: `x-stability`, the stability level of an attribute, and `x-absent`, what the attribute's absence means. A validator in strict mode may need them declared as annotations; the events never contain them.

## The flat event

One event is one flat JSON object: its keys hold single values or a list of strings, never nested objects. The fixed keys are `event.name`, `time` (RFC 3339, the start of the UTC hour: a whole hour, never a precise time) and `schema_version` (the contract version). Every other key is an attribute name from the tables below. `event.schema.json` describes the same object. Consumers must accept keys they do not know; producers must write only the keys listed.

An attribute is optional unless the table says required. A producer may stop sending an optional attribute at any time within a major version; an optional attribute may always be absent, and the last column gives what its absence means. Booleans and the flag list are always written, never left absent.

Every attribute has a stability level. All start as `development`; one becomes `stable` once a consumer depends on it, and every attribute that a consumer's metric uses is `stable` before any upload starts. A `development` attribute may be renamed or removed in a minor version; a `stable` or required attribute only in a new major version.

## Limits

- Each event is written to a slot: a numbered file the process creates for it in the directory of its UTC hour. An hour has 1024 slots (N = 1024). A process tries 8 random slots (k = 8) and drops the event when every one is taken; events can be dropped before the hour is full. A process that drops an event leaves a drop marker.
- Once an hour, a process that records an event also runs a short pass. It copies the events of one finished hour into one file, and each file it writes after reading the whole hour contains at most one `io.rancher.cli.events.dropped` event with the number of drop markers that pass read (at most 1024 markers per file). A process that dies between writing a file and cleaning up can make a later file count the same drops again. Consumers report usage as lower bounds when such an event exists. The version, operating system and architecture of an `io.rancher.cli.events.dropped` event are those of the process that ran the pass, not of the processes that dropped events; its `service.instance.id` is a random id of its own and matches no run.
- The pass stops reading slots after 150 milliseconds and writes what it read; a file written that way contains no `io.rancher.cli.events.dropped` event, and the next hourly pass reads the rest of the hour.
- Events are deleted no earlier than 72 hours after their hour ends, and only while the CLI runs; an hour that gets a late event has a second file.
- An uploaded event is at most 30 days old; older events are not uploaded.

## Never recorded

Resource, namespace, cluster and project names; host names and server URLs; user names; tokens; flag and argument values (the value of `-o` included); file paths; error messages. Every value is from a fixed list in this contract, matches a pattern in it, or is a count, a bucket or a random id.

## Never uploaded

Events recorded while nothing is uploaded stay on the machine. When uploading is added, events are recorded in a separate directory that starts empty; the events recorded before then are never uploaded.

## Command and flag names

The names change with every release; this contract does not list them. The valid names for an event are in the `rancher schema` manifest of the event's `service.version`; the release attaches it as the asset `rancher-schema.json`. The name rule is part of the contract: a command is one to three words, each matching `[a-z][a-z0-9-]*` and at most 32 characters; a flag is its long name without dashes, matching `[a-z][a-z0-9-]*`, at most 32 characters; at most 32 flags per event, the first 32 in sorted order kept.

## Version rule

`service.version` and `io.rancher.manager.version` hold `MAJOR.MINOR.PATCH` with a leading `v` removed, optionally followed by `-alphaN`, `-betaN` or `-rcN`, with or without a dot before the number; the development forms `dev`, `dev-<hex sha>`, `dev-unknown` and `unknown` as they are; a hotfix tag `X.Y.Z-hotfix-<id>` as `X.Y.Z-hotfix` with the id removed, because a hotfix id can belong to one customer. Anything else, including a build suffix, other text or more than 64 characters, is `_OTHER`.

## Event `io.rancher.cli.command.run`

One finished command run.

| Attribute | Type | Required | Values | Stability | Standard | Why it is recorded | When absent |
|---|---|---|---|---|---|---|---|
| `event.name` | string | yes | `io.rancher.cli.command.run` | development | new | The event name. | Never absent. |
| `time` | string | yes | whole UTC hour, RFC 3339 | development | OpenTelemetry | The event time, truncated to the hour. | Never absent. |
| `schema_version` | string | yes | `1.0.0` | development | new | The contract version. | Never absent. |
| `service.namespace` | string | no | `io.rancher` | development | OpenTelemetry stable | Groups every Rancher product's events under one namespace. | The producer did not state a namespace; every producer of this contract is in io.rancher. |
| `service.name` | string | yes | `io.rancher.cli` | development | OpenTelemetry stable | Names the producer; the producer is identified by this attribute alone. | Never absent; the event is invalid without it. |
| `service.version` | string | yes | matches `^(?:[0-9]+\.[0-9]+\.[0-9]+(?:-(?:alpha\|beta\|rc)\.?[0-9]+\|-hotfix)?\|dev\|dev-[0-9a-f]{1,40}\|dev-unknown\|unknown\|_OTHER)$` | development | OpenTelemetry stable | Tells which release of the producer wrote the event. A customer-specific hotfix id is removed; a version that matches none of the forms this contract lists becomes _OTHER. | Never absent; the event is invalid without it. |
| `service.instance.id` | string | yes | matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` | development | OpenTelemetry stable | A random id for one process, the same on every event that describes that process. It names no person and no machine. | Never absent; the event is invalid without it. |
| `io.rancher.cli.config_directory.id` | string | no | matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` | development | new | A random id per CLI config directory. It is not a user and not a machine: coding agents use several directories at once, and CI and test runs use new ones. | The config directory id did not exist yet, or another process was creating it; treat the event as from an unknown config directory. |
| `os.type` | string | no | `aix`, `darwin`, `dragonflybsd`, `freebsd`, `linux`, `netbsd`, `openbsd`, `solaris`, `windows`, `z_os`, `_OTHER` | development | OpenTelemetry development | Tells which operating systems the producer runs on. | The producer did not report an operating system. |
| `host.arch` | string | no | `amd64`, `arm32`, `arm64`, `ppc64`, `s390x`, `x86`, `_OTHER` | development | OpenTelemetry development | Tells which processor architectures the producer runs on. | The producer did not report an architecture. |
| `io.rancher.client.type` | string | no | `person`, `agent`, `automation` | development | new | A hint, never proof, of who ran the command: a coding agent, a person at a terminal, or automation (CI, scripts, kubectl calling the exec plugin, output piped to another program). | The producer did not report who ran the command. |
| `io.rancher.client.agent` | string | no | `claude-code`, `codex`, `gemini-cli`, `copilot-cli`, `opencode`, `cline`, `roo-code`, `copilot-vscode`, `copilot-cloud`, `crush`, `pi`, `agent` | development | new | Tells which coding-agent harness ran the command, from a fixed list; agent stands for a harness not on the list. | No coding-agent harness was detected. |
| `io.rancher.client.ci` | boolean | no | true or false | development | new | True when an environment variable that a CI system sets was present; recorded beside the client type and never changes the client type. | Always written; never absent. |
| `io.rancher.cli.exec_plugin` | boolean | no | true or false | development | new | True when kubectl ran the command as its exec plugin. | Always written; never absent. |
| `log.record.uid` | string | yes | matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` | development | OpenTelemetry development | A random id per event, never time-ordered; lets a consumer remove duplicates. | Never absent; the event is invalid without it. |
| `io.rancher.cli.command` | string | no | matches `^[a-z][a-z0-9-]{0,31}( [a-z][a-z0-9-]{0,31}){0,2}$` | development | new | The command name (an alias is replaced by the name it stands for); never an argument value. Valid names are in the rancher schema manifest of the event's service.version. | The name did not follow the name rule and was left out. |
| `io.rancher.cli.command.flags` | array of string | no | at most 32 names, each matching `^[a-z][a-z0-9-]{0,31}$`, sorted | development | new | The long names, without dashes and sorted, of the flags typed on the command line; never values and never flags set from the environment. At most 32 are kept (the first 32 sorted). | Always written; an empty list means no flag was typed. |
| `io.rancher.cli.command.outcome` | string | no | `ok`, `error` | development | new | Whether the command succeeded. An exit code that the command lists as success in the rancher schema manifest (`successExitCodes`) counts as ok. | The producer did not report an outcome. |
| `error.type` | string | no | `NOT_LOGGED_IN`, `CONFIRMATION_REQUIRED`, `DRY_RUN_NOT_APPLICABLE`, `AMBIGUOUS_SELECTION`, `CONFLICT`, `LOCK_TIMEOUT`, `AUTH`, `CREDENTIAL_REPLACED`, `LOGIN_INCOMPLETE`, `CREDENTIAL_TRANSITION_INCOMPLETE`, `CREDENTIAL_STORE_UNAVAILABLE`, `NOT_FOUND`, `RBAC`, `VALIDATION`, `PRIME_REQUIRED`, `TLS_TRUST`, `CAPABILITY_UNSUPPORTED`, `INTERRUPTED`, `GENERIC`, `_OTHER` | development | OpenTelemetry stable | The CLI error code of a failed command, or _OTHER when it has none. | The command did not fail. |
| `process.exit.code` | integer | no | integer from 0 to 255 | development | OpenTelemetry release candidate | The exit code the process returns to its caller. | The producer did not report an exit code. |
| `io.rancher.cli.command.duration.bucket` | string | no | `lt1s`, `1to5s`, `gt5s` | development | new | How long the command took, in three buckets; never an exact time. | The producer did not report a duration. |
| `io.rancher.manager.version` | string | no | matches `^(?:[0-9]+\.[0-9]+\.[0-9]+(?:-(?:alpha\|beta\|rc)\.?[0-9]+\|-hotfix)?\|dev\|dev-[0-9a-f]{1,40}\|dev-unknown\|unknown\|_OTHER)$` | development | new | The version of the Rancher server the command targeted, by the same version rule. | The server version is unknown. |
| `io.rancher.manager.edition` | string | no | `community`, `prime` | development | new | The edition of the Rancher server the command targeted. | The server edition is unknown. |
| `io.rancher.manager.install.id` | string | no | matches `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$` | development | new | The install id of the Rancher server the command targeted (its install-uuid setting), when the CLI could read it. It joins CLI use to a server installation. | The CLI did not have the id; io.rancher.cli.install_id.missing_reason says why. |
| `io.rancher.cli.install_id.missing_reason` | string | no | `no_server`, `not_permitted`, `not_fetched`, `invalid` | development | new | Why the install id is absent: no_server (the run had no server), not_permitted (the server's response was 403 or 404), not_fetched (no fresh response yet), invalid (the value was not a UUID). Present exactly when the install id is absent. | The install id is present. |

## Event `io.rancher.cli.events.dropped`

One finished hour in which events were dropped: every slot (a numbered file) the producer tried was taken.

| Attribute | Type | Required | Values | Stability | Standard | Why it is recorded | When absent |
|---|---|---|---|---|---|---|---|
| `event.name` | string | yes | `io.rancher.cli.events.dropped` | development | new | The event name. | Never absent. |
| `time` | string | yes | whole UTC hour, RFC 3339 | development | OpenTelemetry | The event time, truncated to the hour. | Never absent. |
| `schema_version` | string | yes | `1.0.0` | development | new | The contract version. | Never absent. |
| `service.namespace` | string | no | `io.rancher` | development | OpenTelemetry stable | Groups every Rancher product's events under one namespace. | The producer did not state a namespace; every producer of this contract is in io.rancher. |
| `service.name` | string | yes | `io.rancher.cli` | development | OpenTelemetry stable | Names the producer; the producer is identified by this attribute alone. | Never absent; the event is invalid without it. |
| `service.version` | string | yes | matches `^(?:[0-9]+\.[0-9]+\.[0-9]+(?:-(?:alpha\|beta\|rc)\.?[0-9]+\|-hotfix)?\|dev\|dev-[0-9a-f]{1,40}\|dev-unknown\|unknown\|_OTHER)$` | development | OpenTelemetry stable | Tells which release of the producer wrote the event. A customer-specific hotfix id is removed; a version that matches none of the forms this contract lists becomes _OTHER. | Never absent; the event is invalid without it. |
| `service.instance.id` | string | yes | matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` | development | OpenTelemetry stable | A random id for one process, the same on every event that describes that process. It names no person and no machine. | Never absent; the event is invalid without it. |
| `io.rancher.cli.config_directory.id` | string | no | matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` | development | new | A random id per CLI config directory. It is not a user and not a machine: coding agents use several directories at once, and CI and test runs use new ones. | The config directory id did not exist yet, or another process was creating it; treat the event as from an unknown config directory. |
| `os.type` | string | no | `aix`, `darwin`, `dragonflybsd`, `freebsd`, `linux`, `netbsd`, `openbsd`, `solaris`, `windows`, `z_os`, `_OTHER` | development | OpenTelemetry development | Tells which operating systems the producer runs on. | The producer did not report an operating system. |
| `host.arch` | string | no | `amd64`, `arm32`, `arm64`, `ppc64`, `s390x`, `x86`, `_OTHER` | development | OpenTelemetry development | Tells which processor architectures the producer runs on. | The producer did not report an architecture. |
| `log.record.uid` | string | yes | matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` | development | OpenTelemetry development | A random id per event, never time-ordered; lets a consumer remove duplicates. | Never absent; the event is invalid without it. |
| `io.rancher.cli.events.dropped.count` | integer | yes | integer, at least 1 | development | new | How many events were dropped in one finished hour because every slot (a numbered file) the producer tried was taken. Consumers report usage as lower bounds when this event exists. | Never absent; the event is invalid without it. |

## Rules between attributes

- `io.rancher.cli.install_id.missing_reason` is present exactly when `io.rancher.manager.install.id` is absent.
- `error.type` is present only when `io.rancher.cli.command.outcome` is `error`.
