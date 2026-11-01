package fixture

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// operands holds the values the positional check reads: a line marked hit
// must be reported and every other line must pass.
func operands(name, id string) []string {
	q := clierr.ShellQuote(name)
	return []string{
		clierr.Rancher + " delete token" + clierr.ShellQuoteArgs(" --dry-run", name),
		clierr.Rancher + " get users --cluster " + clierr.ShellQuote(name),
		clierr.Rancher + " get users --cluster=" + clierr.ShellQuote(name),
		clierr.Rancher + " delete token " + clierr.ShellQuote(name),                        // hit
		clierr.Rancher + " get users " + id,                                                // hit
		clierr.Rancher + " get user " + q,                                                  // hit
		clierr.Rancher + " get users --namespace " + id,                                    // hit
		clierr.Rancher + " get user" + clierr.ShellQuoteArgs("", name) + " --cluster y",    // hit
		fmt.Sprintf("%s config delete-server %s", clierr.Rancher, clierr.ShellQuote(name)), // hit
		fmt.Sprintf("run: %s config delete-server%s", clierr.Rancher, clierr.ShellQuoteArgs("", name)),
		"no command here: " + clierr.ShellQuote(name),
		clierr.Rancher + " oidc-client rotate-secret" + clierr.ShellQuoteArgs(" "+clierr.ShellQuote(id)+" --dry-run", name), // hit
		clierr.Rancher + " oidc-client rotate-secret" + clierr.ShellQuoteArgs(" --timeout "+clierr.ShellQuote(id), name),
		clierr.Rancher + " get users" + clierr.ShellQuoteArgs(" --cluster "+id, name), // hit
		clierr.Rancher + " delete token --dry-run " + clierr.ShellQuote(name),         // hit
	}
}
