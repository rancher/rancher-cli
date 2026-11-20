package importedsamename

import x "github.com/rancher/rancher-cli/test/conformance/testdata/wireformat/importedalt"

// Second uses a constant of importedalt through the import name x.
var Second = x.OTLPPath
