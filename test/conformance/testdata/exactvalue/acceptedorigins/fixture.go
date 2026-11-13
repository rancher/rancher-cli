// Package acceptedorigins is a fixture of the exact-value scans.
package acceptedorigins

import (
	"cmp"
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"strings"
)

// canonical is a fixture canonicalization, allowlisted.
func canonical(s string) string { return strings.ToLower(s) }

// ref is a reference read from an object.
type ref struct{ name string }

// listed is used as a value and allowlisted.
func listed(name string) clierr.Command { return clierr.NewCommand("get users").Arg(name) }

// Listed holds listed as a value.
var Listed = listed

// trimmed has an allowlisted site.
func trimmed(name string) clierr.Command {
	return clierr.NewCommand("get users").Arg(strings.TrimSpace(name))
}

// sources builds commands from values read without a change.
func sources(obj *unstructured.Unstructured, names []string, m map[string]any, r ref, server string) []clierr.Command {
	var out []clierr.Command
	out = append(out, clierr.NewCommand("get users").Arg(obj.GetName()))
	out = append(out, clierr.Revoke(clierr.TokenRef{Name: obj.GetName()}))
	for _, n := range names {
		out = append(out, clierr.NewCommand("get users").Arg(n))
	}
	all := append([]string{}, names...)
	out = append(out, clierr.NewCommand("get users").Arg(all[0]))
	if s, ok := m["name"].(string); ok {
		out = append(out, clierr.NewCommand("get users").Arg(s))
	}
	out = append(out, clierr.NewCommand("get users").Arg(r.name))
	out = append(out, clierr.NewCommand("get users").Arg(string(clierr.TokenKindV3)))
	out = append(out, clierr.NewCommand("get users").Arg(canonical(server)))
	out = append(out, clierr.NewCommand("get users").Arg(cmp.Or(server, obj.GetName())))
	out = append(out, followed(obj.GetNamespace()))
	frag, _ := clierr.FlagFragment("--cluster", obj.GetName())
	_ = frag
	return out
}

// followed passes its parameter on; its one caller passes a source.
func followed(ns string) clierr.Command {
	return clierr.NewCommand("get users").FlagValue("--namespace", ns)
}
