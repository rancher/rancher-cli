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
