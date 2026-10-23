// Package freestring is a fixture for the free-string surface test: an event
// type with an exported field and a setter that takes a free string. The test
// asserts the check reports both.
package freestring

// Event is an event type with an exported field.
type Event struct {
	Name string
	n    int
}

// SetCommand takes a free string.
func (e *Event) SetCommand(s string) { e.Name = s }

// SetBytes takes bytes, which can be text.
func (e *Event) SetBytes(b []byte) { e.Name = string(b) }

// SetAny takes any value, which can be text.
func (e *Event) SetAny(v any) { e.n = len(v.(string)) }

// Count takes no string.
func (e *Event) Count() int { return e.n }

// Make takes a free string.
func Make(label string) Event { return Event{Name: label} }

// Names takes a list of free strings.
func Names(all []string) int { return len(all) }
