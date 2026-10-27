package plain

import "github.com/rancher/rancher-cli/internal/clierr"

func build(name string) []clierr.Candidate {
	return []clierr.Candidate{
		{ID: "c-1", Name: name},
		{ID: "rancher", Name: "rancher get cluster"},
		{"c-2", clierr.ShellQuote(name)},
	}
}

func message() string {
	return clierr.Rancher + " get cluster"
}

func assign(c *clierr.Candidate, name string) {
	text := "rancher get " + name
	c.Name = text
	command := clierr.Rancher + " get cluster"
	_ = command
}
