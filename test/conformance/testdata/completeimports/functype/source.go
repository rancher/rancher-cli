// Declared behavior types: a func type, an interface type, and a struct
// with a func field, each a slot an owner's code could fill.
package complete

type Namer func() []string

type Reader interface{ Read() []string }

type Options struct {
	Filter func(string) bool
}
