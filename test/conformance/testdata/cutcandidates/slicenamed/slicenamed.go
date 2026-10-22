package slicenamed

import "github.com/rancher/rancher-cli/internal/clierr"

type list []clierr.Candidate

func cut(c list) list { return c[:3] }
