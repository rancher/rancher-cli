package commands

import "github.com/rancher/rancher-cli/internal/runtime"

type testView struct {
	ctxNames []string
	srvNames []string
	dir      string
}

func (v testView) ContextNames() []string { return v.ctxNames }
func (v testView) ServerNames() []string  { return v.srvNames }
func (v testView) ConfigDir() string      { return v.dir }

var _ runtime.ConfigView = testView{}
