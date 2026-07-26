// Command rancher-credential-vault is the reference rancher credential
// helper, backed by HashiCorp Vault / OpenBao KV v2. See CREDENTIAL_HELPERS.md.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/rancher/rancher-cli/credhelper"
)

// VERSION is stamped by scripts/build via -ldflags; "dev" otherwise.
var VERSION = "dev"

// run handles the verbs that need no backend. It returns -1 when the verb
// belongs to the credential protocol and must go through Serve.
func run(args []string, stdout io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		_, _ = fmt.Fprintln(stdout, VERSION)
		return 0
	}
	return -1
}

func main() {
	if code := run(os.Args[1:], os.Stdout); code >= 0 {
		os.Exit(code)
	}
	h, err := newVaultHandlerFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rancher-credential-vault:", err)
		os.Exit(1)
	}
	credhelper.Serve(h)
}
