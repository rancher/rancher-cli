package markcopy

import "github.com/rancher/rancher-cli/internal/clierr"

// removalMarkCopy is the one package-level variable the fixture rules let
// copy the removal mark.
var removalMarkCopy = []byte(clierr.RemovalMark)

// Copy reads the copy, not the mark.
func Copy() []byte { return removalMarkCopy }
