# Mapping the flat event to OTLP (contract 1.0.0)

The CLI sends this form. This table lists where each flat key goes in an OTLP logs export; a relay or an OpenTelemetry Collector can be built from it without a change to the contract.

An event is one OTLP log record with an event name. Who produced an event is stated as resource attributes. The scope name is the value of `service.name`; the scope version holds the contract version, a use this contract chooses for that field.

| Flat key | OTLP location | Notes |
|---|---|---|
| `event.name` | log record `eventName` | |
| `time` | log record `timeUnixNano` | A whole UTC hour. `observedTimeUnixNano` is never written. |
| `schema_version` | scope `version` | |
| `service.namespace` | resource attribute | `stringValue` |
| `service.name` | resource attribute | `stringValue` |
| `service.version` | resource attribute | `stringValue` |
| `service.instance.id` | resource attribute | `stringValue` |
| `io.rancher.cli.config_directory.id` | resource attribute | `stringValue` |
| `os.type` | resource attribute | `stringValue` |
| `host.arch` | resource attribute | `stringValue` |
| `io.rancher.client.type` | log record attribute | `stringValue` |
| `io.rancher.client.agent` | log record attribute | `stringValue` |
| `io.rancher.client.ci` | log record attribute | `boolValue` |
| `io.rancher.cli.exec_plugin` | log record attribute | `boolValue` |
| `log.record.uid` | log record attribute | `stringValue` |
| `io.rancher.cli.command` | log record attribute | `stringValue` |
| `io.rancher.cli.command.flags` | log record attribute | `arrayValue` of `stringValue` |
| `io.rancher.cli.command.outcome` | log record attribute | `stringValue` |
| `error.type` | log record attribute | `stringValue` |
| `process.exit.code` | log record attribute | `intValue` |
| `io.rancher.cli.command.duration.bucket` | log record attribute | `stringValue` |
| `io.rancher.manager.version` | log record attribute | `stringValue` |
| `io.rancher.manager.edition` | log record attribute | `stringValue` |
| `io.rancher.manager.install.id` | log record attribute | `stringValue` |
| `io.rancher.cli.install_id.missing_reason` | log record attribute | `stringValue` |
| `io.rancher.cli.events.dropped.count` | log record attribute | `intValue` |

Resource attributes state who produced an event. How many events a producer puts under one resource is that producer's rule; the rule of the CLI is in the section below.

## JSON encoding

The CLI writes events in the JSON Protobuf encoding of OTLP. A request goes to `/v1/logs` with the content type `application/json`; the body is compact JSON and is not compressed.

The body holds one `resourceLogs` entry per event, in the order of the events. Entries are never grouped: `service.instance.id` is different for every process, and the events of one hour come from several processes. An entry holds `resource`, `scopeLogs` and `schemaUrl`. Its one `scopeLogs` entry holds `scope` (with `name` and `version`), `logRecords` and `schemaUrl`. Its one record holds `timeUnixNano`, `eventName` and `attributes`. Each attribute is a `key` and a `value`. `resourceLogs[].schemaUrl` and `resourceLogs[].scopeLogs[].schemaUrl` are both `https://opentelemetry.io/schemas/1.44.0`.

- Keys are lowerCamelCase. `timeUnixNano` and `intValue` are decimal strings.
- `scope.name` equals `service.name`, `scope.version` is `schema_version` and `eventName` is `event.name`. A fixed key that is absent or not text leaves its field out.
- `timeUnixNano` is the `time` of the event as nanoseconds since the Unix epoch: an event whose `time` cannot be read is sent without `timeUnixNano`, and so is one whose `time` is not after 1970-01-01T00:00:00Z or is after the year 2261.
- The encoder writes no `body`, `severityNumber`, `severityText`, `observedTimeUnixNano`, `traceId`, `spanId`, `flags` or `droppedAttributesCount`.
- A value is written in one of four forms: `stringValue`, `boolValue`, `intValue` or `arrayValue` (an object with `values`, a list of `stringValue`). `false`, `0` and the empty string are written, and an empty list is written as an empty `values`. An integer is written only when its magnitude is at most 2^53-1; any other number, a null, an object and a list that holds anything but text have no form, and the attribute is not sent.
- Resource attributes and log record attributes go where the table above puts them, and attributes follow the order of the table above.
- A stored event may hold other keys: keys this contract does not list are not sent.
- The directory `examples/otlp/` holds the pretty-printed OTLP form of each flat example, under the same file name. A receiver compares their structure; the request body is compact.
- A receiver's answer states the number of records it refused in `partialSuccess.rejectedLogRecords`. An answer without that number counts none refused.
