# Mapping the flat event to OTLP (contract 1.0.0)

Documentation only: nothing in the CLI encodes OTLP. The wire format is decided when uploading is added. This table lists where each flat key goes in an OTLP logs export; a relay or an OpenTelemetry Collector can be built from it without a change to the contract.

An event is one OTLP log record with an event name. Who produced a batch is stated once, as resource attributes. The scope name is the value of `service.name`; the scope version holds the contract version, a use this contract chooses for that field. `schemaUrl` is `https://opentelemetry.io/schemas/1.44.0` on the resource and on the scope.

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

A resource attribute is the same for every event of a batch; a producer that writes events from several processes groups them by their resource attributes.
