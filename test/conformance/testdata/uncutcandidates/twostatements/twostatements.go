package twostatements

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(c []clierr.Candidate) *clierr.Error {
	e := clierr.New(clierr.CodeNotFound, "not found")
	e = e.WithCandidates(c)
	return e
}
