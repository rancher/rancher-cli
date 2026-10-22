package slicelow

import "github.com/rancher/rancher-cli/internal/clierr"

func rest(c []clierr.Candidate) []clierr.Candidate { return c[2:] }
