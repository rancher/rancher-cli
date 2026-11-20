// Package importedsamedir is a fixture for the wire format search: a
// constant declared in one file from a constant of another package and used
// in another file of the same package that has no imports.
package importedsamedir

import "github.com/rancher/rancher-cli/test/conformance/testdata/wireformat/importeddef"

// Endpoint is built from a constant of another package.
const Endpoint = "https://h" + importeddef.OTLPPath
