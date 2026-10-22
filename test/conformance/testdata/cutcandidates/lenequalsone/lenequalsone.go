package lenequalsone

import "github.com/rancher/rancher-cli/internal/clierr"

func single(c []clierr.Candidate) bool { return len(c) == 1 }
