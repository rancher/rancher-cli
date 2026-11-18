package requestnoname

import "github.com/rancher/rancher-cli/internal/engine/steve"

func list() steve.Request { return steve.Request{Type: steve.ClustersType} }
