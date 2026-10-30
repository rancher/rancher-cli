package engine

import (
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Redacted is what a diff shows in place of a value the redaction hides.
const Redacted = "<redacted>"

// RedactDiff hides, in place, the values a redaction covers in diff and
// returns it. An entry at or under a hidden path keeps its path; each side
// that has a value becomes Redacted and a missing side stays nil, so an
// addition still reads as an addition and a removal as a removal. An entry
// above a hidden path (a whole object or a whole metadata map) carries a
// copy of its value with the hidden values replaced. The patch preview and
// every create preview hide values through this one function.
func RedactDiff(diff []safety.DiffEntry, r kinds.Redaction) []safety.DiffEntry {
	if !r.Hides() {
		return diff
	}
	for i, e := range diff {
		keys := SplitPointer(e.Path)
		if r.Covers(keys) {
			diff[i].From = redactedSide(e.From)
			diff[i].To = redactedSide(e.To)
			continue
		}
		diff[i].From, _ = replaceCovered(keys, e.From, r.Covers, Redacted)
		diff[i].To, _ = replaceCovered(keys, e.To, r.Covers, Redacted)
	}
	return diff
}

func redactedSide(v any) any {
	if v == nil {
		return nil
	}
	return Redacted
}

// checkRedaction refuses a preview of action (create or update) built
// without the redaction of the resource it previews: a zero redaction, or
// the redaction of another resource, or a redaction built for a read, which
// leaves a Secret's payload visible. Each is a programming error, refused
// before any request.
func checkRedaction(gvr schema.GroupVersionResource, r kinds.Redaction, action string) error {
	switch {
	case r.IsZero():
		return clierr.New(clierr.CodeGeneric,
			"internal error: "+action+" preview of "+gvr.Resource+" has no redaction for its kind")
	case r.IsRead():
		return clierr.New(clierr.CodeGeneric,
			"internal error: "+action+" preview of "+gvr.Resource+" uses the redaction for reads")
	case r.GVR() != gvr:
		return clierr.New(clierr.CodeGeneric,
			"internal error: "+action+" preview of "+gvr.Resource+" uses the redaction for "+r.GVR().Resource)
	}
	return nil
}
