package requestliteral

import "github.com/rancher/rancher-cli/internal/engine/steve"

func read(v string) steve.Request { return steve.Request{Type: steve.ClustersType, Name: v} }
