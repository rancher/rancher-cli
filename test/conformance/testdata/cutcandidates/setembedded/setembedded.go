package setembedded

import "github.com/rancher/rancher-cli/internal/clierr"

type wrapped struct{ *clierr.Error }

func set(w wrapped, c []clierr.Candidate) { w.Candidates = c }
