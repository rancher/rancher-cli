package lenreversed

import "github.com/rancher/rancher-cli/internal/clierr"

func full(c []clierr.Candidate) bool { return 10 <= len(c) }
