package slicenonzero

import "github.com/rancher/rancher-cli/internal/clierr"

func cut(c []clierr.Candidate, n int) []clierr.Candidate { return c[:n] }
