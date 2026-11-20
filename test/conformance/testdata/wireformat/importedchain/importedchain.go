// Package chain is a fixture for the wire format search: a constant built
// from a constant of another package, in a directory whose name differs from
// the package name.
package chain

import "github.com/rancher/rancher-cli/test/conformance/testdata/wireformat/importeddef"

// Endpoint is built from a constant of another package.
const Endpoint = "https://h" + importeddef.OTLPPath
