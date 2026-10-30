package prompterr

import "io"

// Prompter has an unexported field err, as ui.Prompter does.
type Prompter struct{ err io.Writer }

// Other has a field named err that is not the field of Prompter.
type Other struct{ err io.Writer }

func (p *Prompter) Notice() { _, _ = p.err.Write(nil) }

func New(w io.Writer) *Prompter { return &Prompter{err: w} }

func assign(p *Prompter, w io.Writer) { p.err = w }

func (o Other) otherType() { _, _ = o.err.Write(nil) }

func otherLiteral(w io.Writer) Other { return Other{err: w} }
