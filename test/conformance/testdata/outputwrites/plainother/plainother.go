package plainother

// plainRemedies is a function of the same name in another package.
func plainRemedies(s string) string { return s }

func call(s string) string { return plainRemedies(s) }
