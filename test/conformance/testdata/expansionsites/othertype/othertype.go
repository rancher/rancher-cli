package othertype

type IO struct{}

func (IO) ExpandText(s string) string  { return s }
func (IO) ExpandError(s string) string { return s }

func use(io IO) string { return io.ExpandText("a") + io.ExpandError("b") }
