package token

import "github.com/rancher/rancher-cli/internal/clierr"

// hasCommand reports whether c is a command, left out or not, as opposed to
// the zero Command that stands for a text with no command.
func hasCommand(c clierr.Command) bool { return !c.OK() || c.Field() != "" }
