// Command rancher-credential-vault is the reference rancher credential
// helper, backed by HashiCorp Vault / OpenBao KV v2. See CREDENTIAL_HELPERS.md.
package main

import (
	"fmt"
	"os"

	"github.com/rancher/rancher-cli/credhelper"
)

func main() {
	h, err := newVaultHandlerFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rancher-credential-vault:", err)
		os.Exit(1)
	}
	credhelper.Serve(h)
}
