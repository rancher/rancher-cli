package fixture

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// texts holds the commands the end check reads: a line marked hit must be
// reported and every other line must pass.
func texts() []string {
	return []string{
		"run: rancher get users",
		"run: rancher get users; then check it",
		"use `rancher get users` now",
		"use 'rancher get users' now",
		"run:\nrancher get users\nthen check it",
		"run: rancher get users, then check it",                                             // hit
		"(use: rancher config use-server x)",                                                // hit
		"run rancher config delete-server x again",                                          // hit
		"run: rancher get users # note",                                                     // hit
		"run: rancher get users rancher get pods",                                           // hit
		clierr.Text("run", clierr.Lead(": "), clierr.NewCommand("get users"), " again now"), // hit
		clierr.Text("run", clierr.Lead(": "), clierr.NewCommand("get users"), "; then check it"),
		"after it ends, rancher get users.", // hit
		"run: " + clierr.Rancher + " get users",
		fmt.Sprintf("run: %s get users and look", clierr.Rancher), // hit
		fmt.Sprintf("run: %s get users", clierr.Rancher),
		fmt.Sprintf("the entry is left; run %s again to finish", command("x")), // hit
		fmt.Sprintf("the entry is left; run it again: %s", command("x")),
		"stop; " + guarded("x") + ", then more", // hit
		"stop; " + guarded("x") + "; then more",
	}
}

// command is a builder: one returned value.
func command(name string) string {
	return clierr.Rancher + " config delete-server" + clierr.ShellQuoteArgs("", name)
}

// guarded is a builder with a guard that returns early.
func guarded(name string) string {
	if name == "" {
		return ""
	}
	return clierr.Rancher + " config delete-server" + clierr.ShellQuoteArgs("", name)
}
