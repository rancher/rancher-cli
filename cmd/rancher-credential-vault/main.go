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

const usageText = `rancher-credential-vault is a Vault/OpenBao credential helper for rancher.

Invocation by the rancher CLI:
  rancher-credential-vault get|store|erase  (JSON request on stdin)

Other uses:
  rancher-credential-vault version          print version and exit
  rancher-credential-vault --version        print version and exit
  rancher-credential-vault --help           print this message and exit

Environment:
  VAULT_ADDR              Vault/OpenBao address (required)
  VAULT_TOKEN             auth token; falls back to ~/.vault-token
  RANCHER_VAULT_MOUNT     KV v2 mount (default: secret)
  RANCHER_VAULT_PATH      path prefix under the mount (default: ` + defaultVaultPath + `)

See CREDENTIAL_HELPERS.md for the full protocol contract.`

// run handles the args that need no backend. It returns -1 when the arg is a
// protocol verb that must go through Serve.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stdout, usageText)
		return 0
	}
	// --help and -h are recognised in any argv position (standard CLI
	// convention): "get --help" shows usage rather than reporting --help as
	// an unknown flag. Bare-word spellings ("help", "version") and "--version"
	// are first-token-only: "get help" is a two-word invocation that Serve's
	// own arg-count check handles; only flag-shaped tokens unambiguously
	// signal a help request regardless of position.
	for _, a := range args {
		if a == "--help" || a == "-h" {
			_, _ = fmt.Fprintln(stdout, usageText)
			return 0
		}
	}
	switch args[0] {
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, VERSION)
		return 0
	case "help":
		_, _ = fmt.Fprintln(stdout, usageText)
		return 0
	}
	for _, a := range args {
		if len(a) > 0 && a[0] == '-' {
			_, _ = fmt.Fprintf(stderr, "rancher-credential-vault: unknown flag %s\n", a)
			_, _ = fmt.Fprintln(stderr, usageText)
			return 2
		}
	}
	return -1
}

func main() {
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code >= 0 {
		os.Exit(code)
	}
	h, err := newVaultHandlerFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rancher-credential-vault:", err)
		os.Exit(1)
	}
	credhelper.Serve(h)
}
