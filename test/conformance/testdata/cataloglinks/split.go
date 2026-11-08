package fixture

const prefix = "?link"

// split builds the link request from a named constant and a literal.
func split() string { return prefix + "=index" }

// grouped splits the parameter name inside parentheses.
func grouped() string { return ("?li" + "nk") + "=index" }
