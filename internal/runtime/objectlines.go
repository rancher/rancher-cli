package runtime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// maxNamesPerOutcome is how many names one outcome line shows before it
// counts the rest. JSON output carries every object.
const maxNamesPerOutcome = 5

// objectOutcomeOrder is the order outcome lines appear in. An outcome not
// listed follows them in the order it first appears.
var objectOutcomeOrder = []string{
	safety.OutcomeRancherKeeps,
	safety.OutcomeRancherDeletes,
	safety.OutcomeUnknown,
	safety.OutcomeWillBeDeleted,
	safety.OutcomeNotDeleted,
	safety.OutcomeBeingDeleted,
	safety.OutcomeDeletionStarted,
	safety.OutcomeAlreadyGone,
	safety.OutcomeFailed,
	safety.OutcomeNotAttempted,
}

// ObjectDetailLines renders the objects of a preview or result as lines, with
// every kind, outcome, name and message as a value and nothing removed or
// escaped: per kind other than headlineKind (the kind the command's own
// headline already names), a "<kind>s:" line, then one line per outcome two
// spaces in, with at most five names and a count of the rest. A name whose
// object carries a Message shows it in parentheses for the outcomes that give
// a reason (not deleted, failed). The grouping by kind, the match of an
// outcome and the condition on a message use the values as given.
func ObjectDetailLines(objs []safety.Object, headlineKind string) []clierr.DetailLine {
	var kindOrder []string
	byKind := map[string][]safety.Object{}
	for _, o := range objs {
		if o.Kind == headlineKind {
			continue
		}
		if _, seen := byKind[o.Kind]; !seen {
			kindOrder = append(kindOrder, o.Kind)
		}
		byKind[o.Kind] = append(byKind[o.Kind], o)
	}
	var lines []clierr.DetailLine
	for _, kind := range kindOrder {
		lines = append(lines, clierr.Line(clierr.ValuePart("", kind), clierr.LabelPart("s:")))
		for _, outcome := range outcomesOf(byKind[kind]) {
			parts := []clierr.DetailPart{clierr.ValuePart("", outcome)}
			shown := 0
			for _, o := range byKind[kind] {
				if o.Outcome != outcome {
					continue
				}
				shown++
				if shown > maxNamesPerOutcome {
					continue
				}
				sep := ", "
				if shown == 1 {
					sep = ": "
				}
				parts = append(parts, clierr.ValuePart(sep, o.Name))
				if o.Message != "" && (outcome == safety.OutcomeNotDeleted || outcome == safety.OutcomeFailed) {
					parts = append(parts, clierr.ValuePart(" (", o.Message), clierr.LabelPart(")"))
				}
			}
			if shown > maxNamesPerOutcome {
				parts = append(parts, clierr.LabelPart(fmt.Sprintf(" and %d more", shown-maxNamesPerOutcome)))
			}
			lines = append(lines, clierr.DetailLine{Indent: "  ", Parts: parts})
		}
	}
	return lines
}

// ObjectLines is ObjectDetailLines as text lines for TextLines, each passed
// through output.Printable for printing. The caller indents the lines.
func ObjectLines(objs []safety.Object, headlineKind string) []string {
	var lines []string
	for _, l := range ObjectDetailLines(objs, headlineKind) {
		lines = append(lines, output.Printable(l.String()))
	}
	return lines
}

// outcomesOf lists the distinct outcomes of objs: the known ones in their
// fixed order, then any other in first-appearance order.
func outcomesOf(objs []safety.Object) []string {
	present := map[string]bool{}
	var extra []string
	for _, o := range objs {
		if !present[o.Outcome] && !slices.Contains(objectOutcomeOrder, o.Outcome) {
			extra = append(extra, o.Outcome)
		}
		present[o.Outcome] = true
	}
	var out []string
	for _, outcome := range objectOutcomeOrder {
		if present[outcome] {
			out = append(out, outcome)
		}
	}
	return append(out, extra...)
}

// NameList joins the first five names with ", " and counts the rest ("and 3
// more").
func NameList(names []string) string {
	if len(names) <= maxNamesPerOutcome {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxNamesPerOutcome], ", "), len(names)-maxNamesPerOutcome)
}
