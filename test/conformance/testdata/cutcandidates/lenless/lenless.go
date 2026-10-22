package lenless

import "github.com/rancher/rancher-cli/internal/clierr"

func full(c []clierr.Candidate) bool { return len(c) < 10 }
