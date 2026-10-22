package otherfield

import "github.com/rancher/rancher-cli/internal/clierr"

type holder struct {
	Candidates      []clierr.Candidate
	CandidatesTotal int
}

func set(h *holder, c []clierr.Candidate) {
	h.Candidates = c
	h.CandidatesTotal++
	_ = holder{Candidates: c, CandidatesTotal: 1}
}
