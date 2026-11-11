package observe

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
)

// itemFailure is an item read from the server that was not collected.
type itemFailure struct {
	name    string
	cluster string
	err     error
}

// serverOutcome counts the items the rule reads: those read from the
// server, which is every item not marked outside and not a failure of the
// archive or of an entry name, and the ones among them that were not
// collected, in manifest order. A local failure is in neither count and
// never changes the code of the error.
func serverOutcome(items []manifestItem) (attempted int, failed []itemFailure) {
	for _, it := range items {
		if it.outcome.outside || it.outcome.local {
			continue
		}
		attempted++
		if it.outcome.failure != nil {
			failed = append(failed, itemFailure{name: it.Name, cluster: it.Cluster, err: it.outcome.failure})
		}
	}
	return attempted, failed
}

// firstFailure is the first item that was not collected.
func firstFailure(failed []itemFailure) itemFailure { return failed[0] }

// sharedCode is the code every failure carries; agree is false, and the code
// GENERIC, when they differ.
func sharedCode(failed []itemFailure) (code string, agree bool) {
	for _, f := range failed {
		c := clierr.From(f.err).Code
		switch {
		case code == "":
			code = c
		case code != c:
			return clierr.CodeGeneric, false
		}
	}
	return code, code != ""
}

// itemLabel names an item and its cluster for a message.
func itemLabel(f itemFailure) string {
	if f.cluster == "" {
		return quoteValue(f.name)
	}
	return quoteValue(f.name) + " of cluster " + quoteValue(f.cluster)
}

// noServerItemError is the error of a run in which every attempted item
// failed. It carries the first failure's classification (code, retryable,
// hint and details) when every failure shares its code, and GENERIC with the
// first failure as its cause when they differ. The archive is written; the
// message and the details name it. The first failure's own message is last,
// because it can end in a command to run.
func noServerItemError(path string, attempted int, failed []itemFailure) *clierr.Error {
	first := firstFailure(failed)
	msg := fmt.Sprintf("support-bundle collected no item from the server: all %d items it attempted failed; the archive is kept at %s and its manifest.json lists each failure; first failure, %s: %v",
		attempted, clierr.IDText(path), itemLabel(first), first.err)
	firstErr := clierr.From(first.err)
	var out *clierr.Error
	if code, agree := sharedCode(failed); agree {
		out = firstErr.WithMessage(msg)
	} else {
		out = clierr.New(code, msg).WithCause(first.err)
	}
	return out.WithDetails(BundleFailureDetails{OutputFile: path, Attempted: attempted, Failed: len(failed), First: firstCertificate(firstErr.Details)})
}

// interruptedError is the error of a run whose context ended before the
// bundle was complete. The archive is closed and kept.
func interruptedError(path string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("support-bundle stopped before it finished; the archive of what was collected until then is kept at %s", clierr.IDText(path))).
		WithDetails(BundleFailureDetails{OutputFile: path})
}

// BundleFailureDetails is the details payload of an error from a run that
// left its archive behind: the path written, the items attempted and the
// ones that failed, and the certificate facts when the first failure was a
// certificate failure. It holds no field that can hold a command: a field
// that can makes the type take MapText and leave the allowlist of
// payloads without one.
type BundleFailureDetails struct {
	OutputFile string            `json:"outputFile"`
	Attempted  int               `json:"attempted,omitempty"`
	Failed     int               `json:"failed,omitempty"`
	First      *tlstrust.Details `json:"first,omitempty"`
}

// firstCertificate is the certificate details of a first failure, nil when
// its details are of any other type.
func firstCertificate(details any) *tlstrust.Details {
	if d, ok := details.(tlstrust.Details); ok {
		return &d
	}
	return nil
}

// TextDetails lists the path and the counts on their own lines, then the
// lines of the certificate details.
func (d BundleFailureDetails) TextDetails() []clierr.DetailLine {
	lines := []clierr.DetailLine{clierr.Line(clierr.ValuePart("archive: ", d.OutputFile))}
	if d.Attempted > 0 {
		lines = append(lines, clierr.Line(clierr.LabelPart(fmt.Sprintf("items attempted: %d, failed: %d", d.Attempted, d.Failed))))
	}
	if d.First != nil {
		lines = append(lines, d.First.TextDetails()...)
	}
	return lines
}
