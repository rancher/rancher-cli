package runtime

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/output/style"
)

// styleSchemaTOC applies the schema TOC's bold spans to text, the plain
// rendering renderTOC/renderTOCText/renderFilteredTOC already produced from
// groups: the header line (bold, whole line) and every named group label
// (bold). Command rows and every other line are left untouched.
func styleSchemaTOC(text string, groups []SchemaGroup, s style.Style) string {
	out := BoldFirstLine(text, s)
	return BoldExactLines(out, groupLabelSet(groups), s)
}

// groupLabelSet builds the set of rendered group-label lines ("<name>:")
// for every named group — the unnamed ("") sections render no label line.
func groupLabelSet(groups []SchemaGroup) map[string]bool {
	names := map[string]bool{}
	for _, g := range groups {
		if g.Name != "" {
			names[g.Name+":"] = true
		}
	}
	return names
}

// labelRule anchors one section-label token to the structural position the
// card renderer gives it, so a Desc line that happens to start with the
// same word can never steal the bold: labels rendered BEFORE the free-text
// Desc block anchor on their first occurrence, labels rendered AFTER it on
// their last, and list headers must match the whole line exactly.
type labelRule struct {
	label string
	exact bool // the line must equal label exactly (a list header)
	last  bool // anchor on the last occurrence (the label renders after Desc)
}

// flagRowIndexes returns the line indexes of the flag rows belonging to the
// flags section that starts at headerIdx: the consecutive lines after the
// header up to the first blank line. Anchoring to the section range keeps a
// Desc line that merely LOOKS like a flag row ("  --word ...") plain — the
// same rule the labels follow, applied to the rows.
func flagRowIndexes(text string, headerIdx int) map[int]bool {
	rows := map[int]bool{}
	lines := strings.SplitAfter(text, "\n")
	for i := headerIdx + 1; i < len(lines); i++ {
		body, _ := strings.CutSuffix(lines[i], "\n")
		if body == "" {
			break
		}
		rows[i] = true
	}
	return rows
}

// labelIndexes resolves each rule to the line index it anchors to, using
// the same SplitAfter line numbering StyleLines walks.
func labelIndexes(text string, rules []labelRule) map[int]string {
	at := map[int]string{}
	lines := strings.SplitAfter(text, "\n")
	for _, r := range rules {
		idx := -1
		for i, line := range lines {
			body, _ := strings.CutSuffix(line, "\n")
			if r.exact {
				if body != r.label {
					continue
				}
			} else if !strings.HasPrefix(body, r.label) {
				continue
			}
			idx = i
			if !r.last {
				break
			}
		}
		if idx >= 0 {
			at[idx] = r.label
		}
	}
	return at
}

// styleSchemaGroupCard applies the group card's bold spans to text, the
// plain rendering renderGroupCard already produced: the "group: <name>"
// header line (bold, whole line) and the "commands:" label line. The
// group's Desc renders before the commands label, so the label anchors on
// its last exact occurrence — and only when the section actually rendered
// (hasCommands), so an absent section can never donate its bold to a Desc
// line that happens to equal the label.
func styleSchemaGroupCard(text string, hasCommands bool, s style.Style) string {
	if !s.Enabled || text == "" {
		return text
	}
	var rules []labelRule
	if hasCommands {
		rules = append(rules, labelRule{label: "commands:", exact: true, last: true})
	}
	at := labelIndexes(text, rules)
	return StyleLines(text, s, func(i int, body string) (string, bool) {
		if i == 0 {
			return s.Bold(body), true
		}
		if p, ok := at[i]; ok {
			return s.Bold(p) + body[len(p):], true
		}
		return "", false
	})
}

// styleSchemaCommandCard applies the command card's bold spans to text, the
// plain rendering renderCommandCard already produced: the title line (bold,
// whole line), each section label token, and every flag name. The values
// following a label, the examples block, and the exit-codes row are left
// plain. Anchoring follows the card's render order — contract/gates precede
// the Desc block (first occurrence), examples/exit codes/global flags
// follow it (last occurrence) — and the conditional sections (flags,
// examples) anchor only when the caller says they rendered, so an absent
// section can never donate its bold to a Desc line that equals the label.
// The gates line's destructive prose carries no marker token in the
// current rendering, so nothing on it is styled beyond the "gates:" label.
func styleSchemaCommandCard(text string, hasFlags, hasExamples bool, s style.Style) string {
	if !s.Enabled || text == "" {
		return text
	}
	rules := []labelRule{
		{label: "contract:"},
		{label: "gates:"},
		{label: "exit codes:", last: true},
		{label: "global flags:", last: true},
	}
	if hasFlags {
		rules = append(rules, labelRule{label: "flags:", exact: true})
	}
	if hasExamples {
		rules = append(rules, labelRule{label: "examples:", exact: true, last: true})
	}
	at := labelIndexes(text, rules)
	flagRows := map[int]bool{}
	if hasFlags {
		for i, p := range at {
			if p == "flags:" {
				flagRows = flagRowIndexes(text, i)
				break
			}
		}
	}
	return StyleLines(text, s, func(i int, body string) (string, bool) {
		if i == 0 {
			return s.Bold(body), true
		}
		if p, ok := at[i]; ok {
			return s.Bold(p) + body[len(p):], true
		}
		if !flagRows[i] {
			return "", false
		}
		rest, ok := strings.CutPrefix(body, "  --")
		if !ok {
			return "", false
		}
		name, tail, found := strings.Cut(rest, " ")
		if !found {
			return "", false
		}
		return "  " + s.Bold("--"+name) + " " + tail, true
	})
}
