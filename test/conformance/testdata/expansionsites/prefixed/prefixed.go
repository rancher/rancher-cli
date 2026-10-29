package prefixed

import "github.com/rancher/rancher-cli/internal/clierr"

var atPackageLevel = clierr.ExpandRemedies(clierr.Rancher+" get pods", " --config 'd'")

func expand(s, prefix string) string { return clierr.ExpandRemedies(s, prefix) }

func reference() func(string, string) string { return clierr.ExpandRemedies }
