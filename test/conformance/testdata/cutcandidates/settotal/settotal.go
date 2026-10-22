package settotal

import "github.com/rancher/rancher-cli/internal/clierr"

func set(e *clierr.Error) { e.CandidatesTotal = 3 }
