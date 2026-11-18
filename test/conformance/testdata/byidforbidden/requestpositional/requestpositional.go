package requestpositional

import "github.com/rancher/rancher-cli/internal/engine/steve"

func read(v string) steve.Request { return steve.Request{"", steve.ClustersType, "", v, ""} }
