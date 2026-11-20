// Package importeduse is a fixture for the wire format search: uses of
// constants of other packages, through a renamed import and through an import
// whose package name differs from its directory.
package importeduse

import (
	"net/http"

	"github.com/rancher/rancher-cli/test/conformance/testdata/wireformat/importedchain"
	def "github.com/rancher/rancher-cli/test/conformance/testdata/wireformat/importeddef"
)

// SetContentType sets the content type of a request to another server.
func SetContentType(h http.Header) {
	h.Set("Content-Type", def.MediaType)
}

// Endpoint is a constant of another package built from a constant of a third.
var Endpoint = chain.Endpoint

// Kind holds a constant of another package that holds no searched text.
var Kind = def.Unrelated
