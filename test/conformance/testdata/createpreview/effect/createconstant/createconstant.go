package createconstant

import "github.com/rancher/rancher-cli/internal/runtime/safety"

const act safety.Action = "create"

var _ = safety.Effect{Action: act}
