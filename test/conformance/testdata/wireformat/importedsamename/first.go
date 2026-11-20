// Package importedsamename is a fixture for the wire format search: two files
// of one directory that import different packages under the same import name.
package importedsamename

import x "github.com/rancher/rancher-cli/test/conformance/testdata/wireformat/importeddef"

// First uses a constant of importeddef through the import name x.
var First = x.OTLPPath
