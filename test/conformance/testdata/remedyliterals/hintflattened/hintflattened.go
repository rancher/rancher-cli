package hintflattened

import "fmt"

type failure struct{}

func (f *failure) WithHint(hint string) *failure { return f }

type payload struct{ Hint string }

func variable(f *failure) *failure {
	p := "list them with: "
	return f.WithHint(p + "get users")
}

func chain(f *failure) *failure {
	return f.WithHint("list them with: " + "get roletemplates")
}

func format(f *failure, n string) *failure {
	return f.WithHint(fmt.Sprintf("use access clusters %s", n))
}

func field() payload {
	q := "see: "
	return payload{Hint: q + "access list"}
}
