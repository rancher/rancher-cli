package incrementtotal

import "github.com/rancher/rancher-cli/internal/clierr"

func bump(e *clierr.Error) { e.CandidatesTotal++ }
