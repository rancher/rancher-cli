package zerolen

import "github.com/rancher/rancher-cli/internal/clierr"

func none(c []clierr.Candidate) bool { return len(c) == 0 }
