package joinedwordless

import "github.com/rancher/rancher-cli/internal/clierr"

func broken() string { return "something failed; use " + " access list --cluster x" }

func marked() string { return "something failed; use " + clierr.Rancher + " access list --cluster x" }
