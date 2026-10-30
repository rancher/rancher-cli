package plainuse

func plainRemedies(s string) string { return s }

func call(s string) string { return plainRemedies(s) }

func value() func(string) string { return plainRemedies }
