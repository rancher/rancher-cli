package markcopyrefused

import "github.com/rancher/rancher-cli/internal/clierr"

// A variable with another name, a variable with two names and a local
// variable with the accepted name may not copy the removal mark.
var other = []byte(clierr.RemovalMark)

var removalMarkCopy, second = clierr.RemovalMark, 1

func local() []byte {
	var removalMarkCopy = []byte(clierr.RemovalMark)
	return removalMarkCopy
}
