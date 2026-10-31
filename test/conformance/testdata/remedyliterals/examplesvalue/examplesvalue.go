package examplesvalue

type descriptor struct {
	Examples []string
	Cmds     []string
}

var listed = []string{"x"}

var literal = descriptor{Examples: []string{"rancher get users"}}

var byName = descriptor{Examples: listed}

var copyOf = descriptor{Examples: literal.Examples}

var other = descriptor{Examples: literal.Cmds}
