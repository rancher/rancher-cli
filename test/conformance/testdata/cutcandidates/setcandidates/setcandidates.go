package setcandidates

import "github.com/rancher/rancher-cli/internal/clierr"

func set(e *clierr.Error, c []clierr.Candidate) { e.Candidates = c }
