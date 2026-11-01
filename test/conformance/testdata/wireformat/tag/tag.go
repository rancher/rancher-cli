// Package tag is a fixture for the wire format search: a struct tag.
package tag

// Request holds a struct tag of the wire format.
type Request struct {
	Entries []string `json:"resourceLogs"`
}
