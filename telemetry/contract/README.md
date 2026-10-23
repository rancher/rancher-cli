# Rancher usage event contract

This directory holds the published contract for Rancher usage events: what
an event is called, which attributes it carries, the values each attribute
may take, and what a missing attribute means. The CLI is the first producer.
It records events on the local machine only and sends nothing
anywhere. Other Rancher components can write the same events.

## Layout

- `<version>/event.schema.json`: JSON Schema (2020-12) of one event in its
  flat form.
- `<version>/fields.md`: the same facts as a table a person can read: each
  attribute, its values, its stability, why it is recorded and what absent
  means.
- `<version>/otlp-mapping.md`: where each flat key sits in an OTLP logs
  export. Documentation only; nothing encodes OTLP yet.
- `<version>/examples/valid/` and `<version>/examples/invalid/`: events the
  schema accepts, and events it refuses, one rule each.
- `<version>/DRAFT`: an empty file. A version directory that has it is a
  draft and may still change.
- `published.json`: the versions that are published, each with the SHA-256
  of every file in its directory. A published version never changes; a
  test compares each file with its recorded hash and fails on any
  difference, and on a version directory that is neither listed nor marked
  `DRAFT`. A change goes into a new version directory.

`event.schema.json`, `fields.md` and `otlp-mapping.md` are generated from
the attribute metadata in `internal/telemetry` by a test; edit the
metadata, never these files, then run:

    go test ./internal/cli/ -run TestTelemetryContract -update

## Check an event against the schema

Any JSON Schema 2020-12 validator works. One example, which needs Python
and is not part of the repository's build or tests:

    uvx check-jsonschema \
      --schemafile telemetry/contract/1.0.0/event.schema.json \
      telemetry/contract/1.0.0/examples/valid/*.json

Every file in `examples/invalid/` must fail the same command.

## Rules in short

- An event carries `event.name`, `time` (a whole UTC hour) and
  `schema_version`, then one key per attribute.
- Producers write only the keys the schema lists. Consumers accept keys they
  do not know.
- Command and flag names are not listed here; they come from the
  `rancher schema` manifest of the event's `service.version`, which each
  release publishes as the asset `rancher-schema.json`.
- Events recorded while nothing is sent are never uploaded.
