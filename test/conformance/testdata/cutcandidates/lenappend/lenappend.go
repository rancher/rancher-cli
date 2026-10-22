package lenappend

import "github.com/rancher/rancher-cli/internal/clierr"

func collect(in []clierr.Candidate) []clierr.Candidate {
	var out []clierr.Candidate
	for _, c := range in {
		if len(out) < 10 {
			out = append(out, c)
		}
	}
	return out
}
