// Package skillinstall is a fixture for the import guard: its closure holds
// the skill installer package, which takes a file lock.
package skillinstall

import "github.com/rancher/rancher-cli/internal/skillinstall"

// Use is a use of the import.
var Use = skillinstall.Rendered
