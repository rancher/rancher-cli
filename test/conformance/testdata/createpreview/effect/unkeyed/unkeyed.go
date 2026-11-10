package unkeyed

import "github.com/rancher/rancher-cli/internal/runtime/safety"

var _ = safety.Effect{safety.ActionUpdate, "t", "k", "n", safety.PreviewServerDryRun, "", "", nil, "", false, false, false, nil, nil}
