package slicehigh

import "github.com/rancher/rancher-cli/internal/clierr"

func cut(c []clierr.Candidate) []clierr.Candidate { return c[:10] }
