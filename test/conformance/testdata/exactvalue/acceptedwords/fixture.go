// Package acceptedwords is a fixture of the exact-value scans.
package acceptedwords

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// flag is a struct that holds a command word.
type flag struct {
	Spelling clierr.CommandWord
}

// verbs is constant command words joined by +.
const verbs clierr.CommandWord = "get" + " users"

// spelling returns a command word.
func spelling(project bool) clierr.CommandWord {
	if project {
		return "--project"
	}
	return "--cluster"
}

// build uses command words from constants, a field and a function.
func build(id string, project bool) clierr.Command {
	f := flag{Spelling: spelling(project)}
	return clierr.NewCommand(verbs).FlagValue(f.Spelling, id)
}
