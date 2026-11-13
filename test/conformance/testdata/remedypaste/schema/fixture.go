package fixture

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// texts holds the commands the schema check reads: a line marked hit must be
// reported and every other line must pass.
func texts() []string {
	return []string{
		"to ask, run: rancher access can get pods --cluster c1",
		"to ask, run: rancher access can get pods",     // hit
		"to ask, run: rancher access can --cluster c1", // hit
		"to ask, run: rancher get users --nope",        // hit
		clierr.NewCommand("get pod").Arg(name()).FlagValue("--cluster", name()).String(),
		clierr.NewCommand("get pod").Arg(name()).FlagValue("--nope", name()).String(), // hit
		clierr.NewCommand("inspekt cluster").Arg(name()),                              // hit
		clierr.NewCommand("config update-server"),                                     // hit
		clierr.NewCommand("config update-server").Arg(name()).FlagText(" --url <url>"),
		clierr.Text("to ask", clierr.Lead(", run: "), clierr.NewCommand("get nothing-here")), // hit
		"to ask, run: rancher get nothing-here",                                              // hit
		"to ask, run: rancher get user <Name>",                                               // hit
		"to ask, run: rancher get user <name>",
		"to see the options, run: rancher completion --help",
		"to ask, run: rancher get users -o json",
		clierr.Rancher + " get pod x --cluster c1",
		clierr.Rancher + " get user " + clierr.ShellQuote(name()) + " -o json",
		clierr.Rancher + " config update-server" + clierr.ShellQuoteArgs(" --url <url>", name()),
		clierr.Rancher + " config update-server", // hit
		clierr.Rancher + " revoke" + clierr.ShellQuoteArgs(" --role x --dry-run", name()),
		clierr.Rancher + " inspekt cluster" + clierr.ShellQuoteArgs("", name()), // hit
		builder(name(), flagsFrom()),                                            // hit
		builder(name(), " --role x"),
		clierr.Rancher + " app uninstall" + clierr.ShellQuoteArgs(" --cluster c1 --namespace n1 --dry-run", name()),
		clierr.Rancher + " app uninstall" + clierr.ShellQuoteArgs(" --cluster c1 -n n1 --dry-run", name()),
		clierr.Rancher + " app uninstall" + clierr.ShellQuoteArgs(" --cluster c1 --dry-run", name()), // hit
	}
}

func hints() []*clierr.Error {
	return []*clierr.Error{
		clierr.New("X", "m").WithHint(clierr.Rancher + " get user <name>"), // hit
		clierr.New("X", "m").WithHint(clierr.Rancher + " get users"),
	}
}

func name() string { return "" }

// flagsFrom is no builder: its flags are known at run time only.
func flagsFrom() string {
	flags := ""
	flags += " --x"
	return flags
}

// builder prints a command whose flags its caller gives.
func builder(who, flags string) string {
	return clierr.Rancher + " revoke" + clierr.ShellQuoteArgs(flags, who) // hit
}

var _ = fmt.Sprint
