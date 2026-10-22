package zerolenvariants

import "github.com/rancher/rancher-cli/internal/clierr"

func some(c []clierr.Candidate) bool { return len(c) > 0 }

func nonzero(c []clierr.Candidate) bool { return len(c) != 0 }

func reversed(c []clierr.Candidate) bool { return 0 < len(c) }

func grow(c []clierr.Candidate) int { return len(c) + 1 }
