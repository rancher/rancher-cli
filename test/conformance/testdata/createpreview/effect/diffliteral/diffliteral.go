package diffliteral

import "github.com/rancher/rancher-cli/internal/runtime/safety"

var _ = safety.Effect{Action: safety.ActionUpdate, Diff: nil}
