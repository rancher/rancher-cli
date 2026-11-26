package tokenliteral

import "os"

// Read reads the token variable by its name.
func Read() string { return os.Getenv("RANCHER_CLI_TOKEN") }
