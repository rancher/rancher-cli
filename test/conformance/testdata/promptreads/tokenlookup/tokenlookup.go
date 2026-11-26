package tokenlookup

import "os"

// Read looks the token variable up.
func Read() bool {
	_, ok := os.LookupEnv("RANCHER_CLI_TOKEN")
	return ok
}
