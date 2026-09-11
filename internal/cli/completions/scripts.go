// Package completions holds the four in-repo shell completion scripts
// `rancher completion <shell>` emits, adapted from urfave/cli's own embedded
// ones (MIT License, Copyright (c) 2023 urfave/cli maintainers; see the
// attribution header in each script). urfave's embedded scripts are not
// reused directly because its embed.FS is unexported, and because emitting
// them through the registry (rather than unhiding urfave's own built-in
// completion command) is the only way `completion <shell>` appears in
// `rancher schema`.
package completions

import _ "embed"

//go:embed bash.sh
var bashScript []byte

//go:embed zsh.sh
var zshScript []byte

//go:embed fish.sh
var fishScript []byte

//go:embed pwsh.ps1
var pwshScript []byte

var scripts = map[string][]byte{
	"bash": bashScript,
	"zsh":  zshScript,
	"fish": fishScript,
	"pwsh": pwshScript,
}

// Script returns the completion script for shell verbatim, and whether shell
// is a known one.
func Script(shell string) ([]byte, bool) {
	b, ok := scripts[shell]
	return b, ok
}
