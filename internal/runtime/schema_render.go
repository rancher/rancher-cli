package runtime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// renderCommandCard builds the operator-facing card for one command (SE-1):
// usage line with args, the command-declared flags, the contract line, the
// gate line (SE-3), Desc as body, examples, and a footer of the global
// exit-code strip plus one pointer line to the global flags. Per-command
// error codes are deliberately absent — no per-command source of truth
// exists yet (SE-16). Rendered via Result.Text under default/table only;
// the structured formats keep serving Result.Data.
func renderCommandCard(m Manifest, c SchemaCommand) string {
	var b strings.Builder
	b.WriteString("rancher " + c.Name + argSummary(c.Args) + "\n")
	if c.Usage != "" {
		b.WriteString("  " + c.Usage + "\n")
	}
	if len(c.Flags) > 0 {
		b.WriteString("\nflags:\n")
		for _, f := range c.Flags {
			b.WriteString("  " + flagLine(f) + "\n")
		}
	}
	b.WriteString("\n" + contractLine(c) + "\n")
	b.WriteString(gateLine(c.CommandType, c.Destructive, c.Stability) + "\n")
	if c.Desc != "" {
		b.WriteString("\n" + strings.TrimRight(c.Desc, "\n") + "\n")
	}
	if len(c.Examples) > 0 {
		b.WriteString("\nexamples:\n")
		for _, e := range c.Examples {
			b.WriteString("  " + e + "\n")
		}
	}
	b.WriteString("\n" + exitCodeStrip(m.ExitCodes) + "\n")
	b.WriteString("global flags: see `rancher schema -o json` (globalFlags)\n")
	return b.String()
}

// argSummary renders the positional args for the usage line: <name> when
// required, [name] otherwise; an enum lists its values in place of the name.
func argSummary(args []ArgSpec) string {
	var b strings.Builder
	for _, a := range args {
		token := a.Name
		if len(a.Enum) > 0 {
			token = strings.Join(a.Enum, "|")
		}
		if a.Required {
			b.WriteString(" <" + token + ">")
		} else {
			b.WriteString(" [" + token + "]")
		}
	}
	return b.String()
}

// flagLine renders one command-declared flag: name with short aliases, the
// type (an enum lists its values), required marker, description, and any
// backing env vars.
func flagLine(f ArgSpec) string {
	name := "--" + f.Name
	for _, a := range f.Aliases {
		name += "/-" + a
	}
	typ := f.Type
	if len(f.Enum) > 0 {
		typ = strings.Join(f.Enum, "|")
	}
	line := name + " " + typ
	if f.Required {
		line += " (required)"
	}
	if f.Desc != "" {
		line += "  " + f.Desc
	}
	if len(f.Env) > 0 {
		line += "  [env: " + strings.Join(f.Env, ", ") + "]"
	}
	return line
}

// contractLine renders the command's contract facts in SE-1's order —
// commandType · tier · stability · destructive · network · auth — omitting
// segments with no value (tier/stability unresolved, destructive unset,
// network/auth false).
func contractLine(c SchemaCommand) string {
	parts := []string{string(c.CommandType)}
	if c.Tier != "" {
		parts = append(parts, c.Tier)
	}
	if c.Stability != "" {
		parts = append(parts, c.Stability)
	}
	if c.Destructive != "" {
		parts = append(parts, "destructive="+c.Destructive)
	}
	if c.Network {
		parts = append(parts, "network")
	}
	if c.Auth {
		parts = append(parts, "auth")
	}
	return "contract: " + strings.Join(parts, " · ")
}

// gateLine is SE-3's total function of (commandType, destructive, stability);
// the wordings are contract, pinned verbatim by TestSchemaGateLineMatrix.
// A mutating destructive value other than "true"/"dependsOnInput" (including
// unset) carries no destructive gate; a non-mutating commandType other than
// Local reads as read-only, matching the pipeline (only Mutating is gated).
func gateLine(ct CommandType, destructive, stability string) string {
	switch ct {
	case Local:
		return "gates: none (local command)"
	case Mutating:
		var g string
		switch destructive {
		case "true":
			g = "gates: --apply --yes"
		case "dependsOnInput":
			g = "gates: --apply; --yes when the input is destructive (see description)"
		default:
			g = "gates: --apply"
		}
		if stability == string(safety.Deprecated) {
			g += " --allow-deprecated"
		}
		return g
	default:
		return "gates: none (read-only)"
	}
}

// exitCodeStrip renders the global code→exit map as one footer line, ordered
// by exit status then code name so the strip is deterministic.
func exitCodeStrip(exits map[string]int) string {
	codes := make([]string, 0, len(exits))
	for code := range exits {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool {
		if exits[codes[i]] != exits[codes[j]] {
			return exits[codes[i]] < exits[codes[j]]
		}
		return codes[i] < codes[j]
	})
	parts := make([]string, len(codes))
	for i, code := range codes {
		parts[i] = fmt.Sprintf("%s=%d", code, exits[code])
	}
	return "exit codes: " + strings.Join(parts, " · ")
}
