# Mapping the flat event to OTLP (contract 1.0.0)

Documentation only: nothing in the CLI encodes OTLP. The wire format is decided with the upload stage. This table says where each flat key would sit in an OTLP logs export; a relay or a Collector can be built from it without a change to the contract.

An event is one OTLP log record with an event name. Who produced a batch is stated once, as resource attributes. The scope name is the value of `service.name`; the scope version is the contract version, which is this contract's own use of that field. `schemaUrl` is `https://opentelemetry.io/schemas/1.44.0` on the resource and on the scope.

| Flat key | OTLP location | Notes |
|---|---|---|
| `event.name` | log record `eventName` | |
| `time` | log record `timeUnixNano` | A whole UTC hour. `observedTimeUnixNano` is never written. |
| `schema_version` | scope `version` | |
| `service.namespace` | resource attribute | string value |
| `service.name` | resource attribute | string value |
| `service.version` | resource attribute | string value |
| `service.instance.id` | resource attribute | string value |
| `io.rancher.cli.config_directory.id` | resource attribute | string value |
| `os.type` | resource attribute | string value |
| `host.arch` | resource attribute | string value |
| `io.rancher.client.type` | log record attribute | string value |
| `io.rancher.client.agent` | log record attribute | string value |
| `io.rancher.client.ci` | log record attribute | `boolValue` |
| `io.rancher.cli.exec_plugin` | log record attribute | `boolValue` |
| `log.record.uid` | log record attribute | string value |
| `io.rancher.cli.command` | log record attribute | string value |
| `io.rancher.cli.command.flags` | log record attribute | `arrayValue` of `stringValue` |
| `io.rancher.cli.command.outcome` | log record attribute | string value |
| `error.type` | log record attribute | string value |
| `process.exit.code` | log record attribute | `intValue` |
| `io.rancher.cli.command.duration.bucket` | log record attribute | string value |
| `io.rancher.manager.version` | log record attribute | string value |
| `io.rancher.manager.edition` | log record attribute | string value |
| `io.rancher.manager.install.id` | log record attribute | string value |
| `io.rancher.cli.install_id.missing_reason` | log record attribute | string value |
| `io.rancher.cli.events.dropped.count` | log record attribute | `intValue` |

A resource attribute is the same for every event of a batch; a producer that writes events from several processes groups them by their resource attributes.
