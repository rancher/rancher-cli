// Package skills ships the agent skills this repository maintains as an
// embedded tree, so the rancher binary can install them without a network
// or a checkout. The go:embed directive lives here because an embed path
// cannot climb out of its package directory: the entry point in
// cmd/rancher reads the tree through Rancher.
package skills

import (
	"embed"
	"io/fs"
	"sync"
)

//go:embed rancher
var tree embed.FS

// Rancher returns the embedded rancher skill, rooted at the skills/rancher
// directory as it is on disk in the repository.
func Rancher() fs.FS { return rancher() }

// rancher resolves the sub-tree once. fs.Sub fails only on an invalid path,
// and "rancher" is a constant the build already resolved.
var rancher = sync.OnceValue(func() fs.FS {
	sub, err := fs.Sub(tree, "rancher")
	if err != nil {
		panic(err)
	}
	return sub
})
