package zeroslice

import "github.com/rancher/rancher-cli/internal/clierr"

func reset(c []clierr.Candidate) []clierr.Candidate { return c[:0] }
