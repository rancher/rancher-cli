package createliteral

import "github.com/rancher/rancher-cli/internal/runtime/safety"

var _ = safety.Effect{Action: safety.ActionCreate, DisplayKind: "x"}
