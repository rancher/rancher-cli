# rancher-ai-mcp kind fixture

`mcp-kinds.json` holds the map `K8sKindsToGVRs` of `pkg/converter/grv.go` in
github.com/rancher/rancher-ai-mcp, the Model Context Protocol (MCP) server for
Rancher. The map has the lower-case Kubernetes kind names the server resolves,
each with the group, version and resource it resolves to.
`TestConformsToMCPVocabulary` holds the kind table to it.

- The file is `{"commit": "<40 hex digits>", "kinds": {"<key>": {"group",
  "version", "resource"}}}`: the commit of rancher-ai-mcp the map was read at,
  then the kinds sorted by key. The indent is two spaces and the file ends with
  a newline.
- `make mcp-kinds` compares the `kinds` object with the main branch of
  rancher-ai-mcp and ignores the commit. `make mcp-kinds-update` writes the
  file; never edit it by hand.
- After an update, a key added to the map fails the test until it gets a row in
  the kind table or an entry in `mcpNotYetKinds`
  (`internal/engine/kinds/kinds_test.go`).
- A key dropped from the map fails the test until you remove its row, or keep
  the row and add the key to `cliOnlyKinds`, or, when the key has no row,
  remove it from `mcpNotYetKinds`.

# Auth config fixtures

`authconfig-fixtures.json` holds the AuthConfig objects that the tests of the
authconfig row read. The tests run the redaction of the authconfig row on each
object and check that every value marked as a credential is hidden and every
other value is shown.

## File format

- A JSON array with two entries for every auth provider type: the
  `reference` variant first (each credential value starts with
  `cattle-global-data:`), then the `plaintext` variant (each credential value
  starts with `SECRET-plain-`).
- Each entry has `name` (`<type>/<variant>`), `type` (the AuthConfig `type`
  value, for example `githubConfig`), `variant` and `object` (the AuthConfig).
- Entries are sorted by type. Keys are sorted, the indent is two spaces and
  the file ends with a newline.

## Markers

- A leaf is a value that is not a map or a list: a string, number, boolean
  or null. A leaf that the row must hide has `SECRET-` in its path or in its
  value. A leaf that must stay shown has no `SECRET-` anywhere.
- Every leaf that stays shown has `SHOWN-` in its path or in its value,
  except `apiVersion`, `kind`, `type`, `enabled`, `metadata.name` and
  `metadata.creationTimestamp`.
- A value that starts with exactly `cattle-global-data:` is a reference: the
  namespace and name of a Secret. The row hides it wherever it sits.

## What every object holds

- The base fields: `apiVersion`, `kind`, `metadata` (the name, the
  `management.cattle.io/auth-provider-cleanup` and `field.cattle.io/description`
  annotations and `creationTimestamp`), `type`, `enabled`, `accessMode`,
  `allowedPrincipalIds` and `displayName`.
- The credential fields of the type, all listed in `RedactPaths` of the
  authconfig row. In the `reference` variant each value is a reference. In the
  `plaintext` variant each value is text that starts with `SECRET-plain-`.
  `additionalClientIds` is a map whose key also has `SECRET-`.
- For `adfsConfig`, `oktaConfig` and `shibbolethConfig` only: the field
  `openLdapConfig` with a `serviceAccountPassword`.
- The fields of the type that hold no credential (client ids, hostnames,
  certificates and similar), each with a `SHOWN-` value.
- Fields that no path in `RedactPaths` names, each holding a reference. The
  row hides each one because of its value, not its path: `unlistedReference`,
  a reference deeper in `unlistedNested`, a reference as an item of
  `unlistedList` and a reference in a map inside `unlistedListOfMaps`. Their
  siblings stay shown.
- Values that stay shown although they look like references: `mapKeyWithPrefix`
  (a map key that starts with `cattle-global-data:`), `nonStrings` (a number
  and a boolean) and `otherCase` (the prefix in another letter case).

`localConfig` has no credential field.

## Adding a type

When the golden `internal/cli/testdata/rancher-api-fields.golden.json` gains
an auth config type, the conformance test that reads it names the missing
fixture. Add the two entries for the type. Build them like the entries of a
similar type: the same base fields, the credential fields of the type that
`RedactPaths` lists, the fields that hold no credential, and the fields that
hold a reference no path in `RedactPaths` names.

A type that only Rancher main has gets its fixtures before the golden has the
type. Its entry in `authConfigMainOnlyFixtures`
(`internal/cli/createfields_conformance_test.go`) gives the reason. After the
golden gains the type, the test fails until the entry is removed.
