package elided

import "github.com/rancher/rancher-cli/internal/clierr"

var inSlice = []*clierr.Request{{Server: "s"}}

var inMap = map[string]clierr.Request{"a": {Server: "s"}}
