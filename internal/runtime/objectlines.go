package runtime

import (
	"fmt"
	"slices"
	"strings"

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

// ObjectLines renders the objects of a preview or result as text lines for
// TextLines: per kind other than headlineKind (the kind the command's own
// headline already names), a "<kind>s:" line, then one line per outcome
// with at most five names and a count of the rest. A name whose object
// carries a Message shows it in parentheses for the outcomes that give a
// reason (not deleted, failed). Kind, outcome, name and message all pass
// output.Printable for printing; the grouping by kind, the match of an outcome
// and the condition on a message use the values as given. The caller indents
// the lines.
func ObjectLines(objs []safety.Object, headlineKind string) []string {
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
	var lines []string
	for _, kind := range kindOrder {
		lines = append(lines, output.Printable(kind)+"s:")
		for _, outcome := range outcomesOf(byKind[kind]) {
			var names []string
			for _, o := range byKind[kind] {
				if o.Outcome != outcome {
					continue
				}
				name := output.Printable(o.Name)
				if o.Message != "" && (outcome == safety.OutcomeNotDeleted || outcome == safety.OutcomeFailed) {
					name += " (" + output.Printable(o.Message) + ")"
				}
				names = append(names, name)
			}
			lines = append(lines, "  "+output.Printable(outcome)+": "+NameList(names))
		}
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
