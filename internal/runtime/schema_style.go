package runtime

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/output/style"
)

// styleSchemaTOC applies the schema TOC's bold spans to text, the plain
// rendering renderTOC/renderTOCText/renderFilteredTOC already produced from
// m: the header line (bold, whole line) and every named group label (bold);
// primeGroups is allPrimeGroups of the unfiltered manifest.
// It then colors "[Prime]" green wherever it structurally belongs - an
// all-Prime group's own label, and a Prime command's row - anchored on the
// same line indexes computed from the still-plain text, so a Desc line that
// happens to contain the literal token is never touched. Command rows are
// otherwise left unbolded.
func styleSchemaTOC(text string, m Manifest, primeGroups map[string]bool, s style.Style) string {
	labels, marked := groupLabelSets(m.Groups, primeGroups)
	primeLineAt := primeRowIndexes(text, primeCommandLines(m.Commands))
	for i := range labelIndexesExact(text, marked) {
		primeLineAt[i] = true
	}
	out := BoldFirstLine(text, s)
	out = BoldExactLines(out, labels, s)
	return StyleLines(out, s, func(i int, body string) (string, bool) {
		if !primeLineAt[i] {
			return "", false
		}
		return ColorPrimeMarker(body, s), true
	})
}

// ColorPrimeMarker colors the first PrimeMarker in text green and leaves
// every other byte alone; text without the marker comes back unchanged.
// Callers pass only lines they have already identified as carrying the
// marker structurally, so the first occurrence is always the marker itself.
func ColorPrimeMarker(text string, s style.Style) string {
	return strings.Replace(text, PrimeMarker, s.Green(PrimeMarker), 1)
}

// plainLines splits text into its lines without their trailing newline,
// numbered exactly as StyleLines numbers them (SplitAfter on "\n", so a
// trailing newline yields one final empty line). Every structural scan in
// this file walks lines through it, so the split rule lives in one place.
func plainLines(text string) []string {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		lines[i], _ = strings.CutSuffix(line, "\n")
	}
	return lines
}

// primeCommandLines returns the exact tocLine rendering of every Prime
// command in cmds - the row text primeRowIndexes matches against to find
// which rendered lines carry a "[Prime]" marker to colorize.
func primeCommandLines(cmds []SchemaCommand) []string {
	var lines []string
	for _, c := range cmds {
		if c.Prime {
			lines = append(lines, tocLine(c))
		}
	}
	return lines
}

// primeRowIndexes finds, in text's ORIGINAL (unstyled) line layout, the
// index of every line whose content (after stripping the group indent)
// starts with one of rows - the exact set of Prime command rows to
// colorize. A prefix match, rather than exact equality, so a filtered-TOC
// search fragment appended after the usage text still colorizes correctly.
// Two rows can never be prefixes of each other: every needle is the whole
// command name followed by the " - " separator, and command names are
// unique, so a needle matches exactly its own row.
func primeRowIndexes(text string, rows []string) map[int]bool {
	at := map[int]bool{}
	if len(rows) == 0 {
		return at
	}
	for i, body := range plainLines(text) {
		plain := strings.TrimPrefix(body, "  ")
		for _, row := range rows {
			if strings.HasPrefix(plain, row) {
				at[i] = true
				break
			}
		}
	}
	return at
}

// labelIndexesExact is labelIndexes for a set of whole-line labels: one
// exact rule per name, returning the matched line indexes so a caller can
// layer a second, independent transform (coloring) onto the same lines
// BoldExactLines bolds. Group labels are unique lines, so the first match
// per name is the only match.
func labelIndexesExact(text string, names map[string]bool) map[int]bool {
	rules := make([]labelRule, 0, len(names))
	for n := range names {
		rules = append(rules, labelRule{label: n, exact: true})
	}
	at := map[int]bool{}
	for i := range labelIndexes(text, rules) {
		at[i] = true
	}
	return at
}

// groupLabelSets builds, in one pass over groups, the set of every
// rendered group-label line and the subset that carries the Prime marker.
// Each named group contributes its plain form ("<name>:"); a group named in
// primeGroups also contributes its marked form ("<name>: [Prime]") to both
// sets. primeGroups is the caller's verdict from the FULL manifest
// (allPrimeGroups), the same one the renderer used, so a filtered subset can
// never make the styler and the renderer disagree. Whichever form the
// renderer actually emitted is the one these sets match.
func groupLabelSets(groups []SchemaGroup, primeGroups map[string]bool) (labels, marked map[string]bool) {
	labels = map[string]bool{}
	marked = map[string]bool{}
	for _, g := range groups {
		if g.Name == "" {
			continue
		}
		labels[g.Name+":"] = true
		if primeGroups[g.Name] {
			line := g.Name + ": " + PrimeMarker
			labels[line] = true
			marked[line] = true
		}
	}
	return labels, marked
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
// Desc line that merely LOOKS like a flag row ("  --word ...") plain - the
// same rule the labels follow, applied to the rows.
func flagRowIndexes(text string, headerIdx int) map[int]bool {
	rows := map[int]bool{}
	lines := plainLines(text)
	for i := headerIdx + 1; i < len(lines); i++ {
		if lines[i] == "" {
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
	lines := plainLines(text)
	for _, r := range rules {
		idx := -1
		for i, body := range lines {
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
// its last exact occurrence - and only when the section actually rendered
// (hasCommands), so an absent section can never donate its bold to a Desc
// line that happens to equal the label. isPrime - the same flag
// renderGroupCard used to append " [Prime]" to the header - additionally
// colors that marker green within the header's bold span; bespoke is the
// group's own member commands, used to colorize each Prime member's row.
func styleSchemaGroupCard(text string, hasCommands, isPrime bool, bespoke []SchemaCommand, s style.Style) string {
	if !s.Enabled || text == "" {
		return text
	}
	var rules []labelRule
	if hasCommands {
		rules = append(rules, labelRule{label: "commands:", exact: true, last: true})
	}
	at := labelIndexes(text, rules)
	primeRowAt := primeRowIndexes(text, primeCommandLines(bespoke))
	return StyleLines(text, s, func(i int, body string) (string, bool) {
		if i == 0 {
			bolded := s.Bold(body)
			if isPrime {
				bolded = ColorPrimeMarker(bolded, s)
			}
			return bolded, true
		}
		if p, ok := at[i]; ok {
			return s.Bold(p) + body[len(p):], true
		}
		if primeRowAt[i] {
			return ColorPrimeMarker(body, s), true
		}
		return "", false
	})
}

// styleSchemaCommandCard applies the command card's bold spans to text, the
// plain rendering renderCommandCard already produced: the title line (bold,
// whole line), each section label token, and every flag name. The values
// following a label, the examples block, and the exit-codes row are left
// plain. Anchoring follows the card's render order - contract/gates precede
// the Desc block (first occurrence), examples/exit codes/global flags
// follow it (last occurrence) - and the conditional sections (flags,
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
		if body == PrimeSentence {
			return s.Green(body), true
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
