package othervariable

import "os"

// Read reads another variable.
func Read() string { return os.Getenv("RANCHER_CLI_URL") }
