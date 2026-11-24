// Package originiface is a fixture of the exact-value scans.
package originiface

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// builder is an interface whose method leaves its parameter unnamed.
type builder interface{ Build(string) clierr.Command }

// users has the Build method of builder, with a named parameter.
type users struct{}

// Build passes its parameter to the command builder. A call through builder
// reaches it, and the walk cannot follow such a call.
func (users) Build(name string) clierr.Command { // want 3a
	return clierr.NewCommand("get users").Arg(name)
}

// viaBuilder calls Build through builder.
func viaBuilder(b builder, name string) clierr.Command { return b.Build(name) }
