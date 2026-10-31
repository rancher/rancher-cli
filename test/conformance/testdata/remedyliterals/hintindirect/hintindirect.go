package hintindirect

type failure struct{}

func (f *failure) WithHint(hint string) *failure { return f }

func hintFor() string { return "use access clusters" }

func fromConstant(f *failure) *failure { return f.WithHint(hintText) }

func fromFunction(f *failure) *failure { return f.WithHint(hintFor()) }
