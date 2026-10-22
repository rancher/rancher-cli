package readfields

import "github.com/rancher/rancher-cli/internal/clierr"

func read(e *clierr.Error) ([]clierr.Candidate, int) { return e.Candidates, e.CandidatesTotal }
