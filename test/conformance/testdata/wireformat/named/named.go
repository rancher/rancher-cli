// Package named is a fixture for the wire format search: a constant built
// from named constants.
package named

const (
	prefix = "/v1/"
	name   = "logs"
)

// Path is built from named constants.
const Path = prefix + name
