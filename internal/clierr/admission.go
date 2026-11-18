package clierr

import (
	"strconv"
	"strings"
)

// An admission text is the message the Kubernetes API server returns when
// admission refuses a write, or when it could not check the write yet. A 403
// whose message is one is not a missing permission: the server refused the
// object (VALIDATION), or the 403 is temporary because the server could not
// finish its check (retryable GENERIC). Any other 403 stays RBAC.
//
// The server writes a 403 of this kind with apierrors.NewForbidden, which
// begins the message with one of
//
//	forbidden: <text>
//	<resource> is forbidden: <text>
//	<resource> "<name>" is forbidden: <text>
//
// and the authorizer writes its own 403 the same way, with a <text> that
// always begins `User "`. An admission text is therefore matched only at the
// beginning of the <text>, never anywhere inside the message, and a message
// that does not begin with one of these three forms matches nothing.
//
// The list below names, for each admission text, the file and line that writes
// it at k8s.io/apiserver v0.37.1, k8s.io/kubernetes v1.37.1 or
// k8s.io/pod-security-admission v0.37.1. The wording of every admission text is
// the same from Kubernetes 1.32 to 1.37; only the line numbers move. If a
// Kubernetes release changes the wording of an admission text, the CLI reports
// that 403 as RBAC again, and no test here detects that. A new 403 from a
// server other than Kubernetes admission whose text begins with an admission
// text would be reported as VALIDATION, and no test here detects that either.
//
// In an admission text, <q>, <res>, <type>, <ns>, <p> and <b> each stand for
// one or more characters with no space, ending at the first place where the
// next fixed words of the admission text follow. The other words must match
// exactly.

// admissionText is one entry of the admissionTexts list.
type admissionText struct {
	// id is a short stable name; a test requires a row for every id.
	id string
	// retryable is true for a temporary 403: the error is a retryable GENERIC.
	// Otherwise the error is VALIDATION.
	retryable bool
	// match reports whether the text after `forbidden: ` is this admission
	// text.
	match func(text string) bool
}

// admissionTexts is the list of admission texts, in the order they are tried.
var admissionTexts = []admissionText{
	// ResourceQuota: k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:621-626.
	{id: "quota-exceeded", match: quotaExceeded},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:580.
	{id: "quota-insufficient", match: prefixThenText("insufficient quota to consume: ")},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:471; the
	// text after the quota name is written by
	// k8s.io/kubernetes@v1.37.1/pkg/quota/v1/evaluator/core/pods.go:170.
	{id: "quota-constraint", match: quotaConstraint},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:502.
	{id: "quota-negative", match: prefixThenText("quota usage is negative for resource(s): ")},
	// LimitRanger: several violations of one request print in brackets
	// (k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:152, :472,
	// :556 through k8s.io/apimachinery@v0.37.1/pkg/util/errors/errors.go:70-96) and the
	// CLI reads only the first item. It checks the bracketed list first,
	// because a ratio text accepts any resource name, a bracket included.
	{id: "limitrange-list", match: limitRangeList},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:315, :318, :321.
	{id: "limitrange-minimum", match: limitRangeMinimum},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:333, :336, :348, :351, :354.
	{id: "limitrange-maximum", match: limitRangeMaximum},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:366, :369, :381.
	{id: "limitrange-ratio", match: limitRangeRatio},
	// PodSecurity: k8s.io/pod-security-admission@v0.37.1/admission/admission.go:480-484.
	{id: "podsecurity", match: podSecurity},
	// Namespace lifecycle: k8s.io/apiserver@v0.37.1/pkg/admission/plugin/namespace/lifecycle/admission.go:179.
	{id: "namespace-terminating", match: namespaceTerminating},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/namespace/lifecycle/admission.go:80.
	{id: "namespace-immortal", match: exactText("this namespace may not be deleted")},
	// ValidatingAdmissionPolicy: k8s.io/apiserver@v0.37.1/pkg/admission/plugin/policy/validating/dispatcher.go:305, :307.
	{id: "validatingadmissionpolicy", match: validatingAdmissionPolicy},

	// The texts below do not refuse the object: they are temporary 403s, the
	// server could not check the request yet.

	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/namespace/lifecycle/admission.go:117,
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/webhook/generic/webhook.go:421, :429 and
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/policy/generic/plugin.go:323, :331.
	{id: "not-ready", retryable: true, match: exactText("not yet ready to handle request")},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:163; the
	// resource between the two parts is printed as `/v1, Resource=pods` and
	// holds a space.
	{id: "limitrange-error", retryable: true, match: limitRangeError},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:474.
	{id: "quota-status-unknown", retryable: true, match: prefixThenText("status unknown for quota: ")},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:518.
	{id: "quota-previous-usage", retryable: true, match: exactText("unable to get previous usage since prior version of object was not found")},
}

// matchAdmissionText returns the list entry that matches the 403 message, and
// false when none does. It reads the beginning of the message that
// apierrors.NewForbidden writes, then tries every entry on the text after
// `forbidden: `.
func matchAdmissionText(reason string) (admissionText, bool) {
	text, ok := textAfterForbidden(reason)
	if !ok {
		return admissionText{}, false
	}
	for _, entry := range admissionTexts {
		if entry.match(text) {
			return entry, true
		}
	}
	return admissionText{}, false
}

// textAfterForbidden returns the text that follows the beginning of a message
// apierrors.NewForbidden wrote: `forbidden: `, `<resource> is forbidden: `
// or `<resource> "<name>" is forbidden: `. The resource has no space and no
// double quote; the name is a Go-quoted string. ok is false for any other
// message, a name with no quotes or an unterminated quote included.
func textAfterForbidden(reason string) (text string, ok bool) {
	if rest, found := strings.CutPrefix(reason, "forbidden: "); found {
		return rest, true
	}
	resource, rest, found := strings.Cut(reason, " ")
	if !found || resource == "" || strings.Contains(resource, `"`) {
		return "", false
	}
	if text, found = strings.CutPrefix(rest, "is forbidden: "); found {
		return text, true
	}
	quoted, _, found := quotedName(rest)
	if !found {
		return "", false
	}
	return strings.CutPrefix(rest[len(quoted):], " is forbidden: ")
}

// quotedName reads a non-empty Go double-quoted string at the beginning of text,
// limited to the same length as the webhook name in a failed webhook call: it
// returns the quoted text as written and the string it holds.
func quotedName(text string) (quoted, name string, ok bool) {
	if !strings.HasPrefix(text, `"`) {
		return "", "", false
	}
	quoted, err := strconv.QuotedPrefix(text[:min(len(text), maxQuotedWebhookName)])
	if err != nil {
		return "", "", false
	}
	name, err = strconv.Unquote(quoted)
	if err != nil || name == "" {
		return "", "", false
	}
	return quoted, name, true
}

// webhookDenial reports whether text is the message the API server writes
// when an admission webhook denies a request:
//
//	admission webhook "<name>" denied the request: <message>
//	admission webhook "<name>" denied the request without explanation
//
// with a non-empty Go-quoted name. The text without a quoted name is not a
// denial. text is the server's message as the CLI shows it, which is cut to
// 200 bytes: a name long enough that its closing quote falls past the cut does
// not parse, and that text is not read as a denial.
func webhookDenial(text string) bool {
	rest, ok := strings.CutPrefix(text, "admission webhook ")
	if !ok {
		return false
	}
	quoted, _, ok := quotedName(rest)
	if !ok {
		return false
	}
	rest = rest[len(quoted):]
	return strings.HasPrefix(rest, " denied the request: ") || rest == " denied the request without explanation"
}

// exactText matches a text that is exactly the given words.
func exactText(words string) func(string) bool {
	return func(text string) bool { return text == words }
}

// prefixThenText matches a text that begins with the given words and has more
// text after them.
func prefixThenText(words string) func(string) bool {
	return func(text string) bool { return nonEmptyAfter(text, words) }
}

// nonEmptyAfter reports whether text begins with the given words and has more
// text after them.
func nonEmptyAfter(text, words string) bool {
	rest, ok := strings.CutPrefix(text, words)
	return ok && rest != ""
}

// cutPlaceholder reads a placeholder at the beginning of text: one or more
// characters with no space, ending at the first place where the literal
// `after` follows. It returns the text that follows the placeholder and the
// literal `after`, and false when the placeholder does not end there.
func cutPlaceholder(text, after string) (rest string, ok bool) {
	for i := 0; i <= len(text); i++ {
		if i > 0 && strings.HasPrefix(text[i:], after) {
			return text[i+len(after):], true
		}
		if i == len(text) || text[i] == ' ' {
			return "", false
		}
	}
	return "", false
}

// cutWords matches the literal at the beginning of text, then a placeholder
// followed by each literal in the list `then`, in order. It returns the text
// after the last literal.
func cutWords(text, literal string, then ...string) (rest string, ok bool) {
	rest, ok = strings.CutPrefix(text, literal)
	if !ok {
		return "", false
	}
	for _, next := range then {
		if rest, ok = cutPlaceholder(rest, next); !ok {
			return "", false
		}
	}
	return rest, true
}

// quotaExceeded matches `exceeded quota: <q>, requested: ..., used: ...,
// limited: ...`: the three markers in this order.
func quotaExceeded(text string) bool {
	rest, ok := cutWords(text, "exceeded quota: ", ", requested: ")
	if !ok {
		return false
	}
	_, rest, ok = strings.Cut(rest, ", used: ")
	if !ok {
		return false
	}
	_, _, ok = strings.Cut(rest, ", limited: ")
	return ok
}

// quotaConstraint matches `failed quota: <q>: ` and more text.
func quotaConstraint(text string) bool {
	rest, ok := cutWords(text, "failed quota: ", ": ")
	return ok && rest != ""
}

// limitRangeMinimum matches `minimum <res> usage per <type> is ` and more
// text.
func limitRangeMinimum(text string) bool {
	rest, ok := cutWords(text, "minimum ", " usage per ", " is ")
	return ok && rest != ""
}

// limitRangeMaximum matches `maximum <res> usage per <type> is ` and more
// text.
func limitRangeMaximum(text string) bool {
	rest, ok := cutWords(text, "maximum ", " usage per ", " is ")
	return ok && rest != ""
}

// limitRangeRatio matches `<res> max limit to request ratio per <type> is `
// and more text.
func limitRangeRatio(text string) bool {
	rest, ok := cutPlaceholder(text, " max limit to request ratio per ")
	if !ok {
		return false
	}
	rest, ok = cutPlaceholder(rest, " is ")
	return ok && rest != ""
}

// limitRangeList matches a list in brackets whose first item is a LimitRanger
// minimum, maximum or ratio text. Later items are not read.
func limitRangeList(text string) bool {
	rest, ok := strings.CutPrefix(text, "[")
	return ok && (limitRangeMinimum(rest) || limitRangeMaximum(rest) || limitRangeRatio(rest))
}

// limitRangeError matches `unable to <operation> <resource> at this time
// because there was an error enforcing limit ranges`: the beginning and the end,
// with something between them.
func limitRangeError(text string) bool {
	const start, end = "unable to ", " at this time because there was an error enforcing limit ranges"
	rest, ok := strings.CutPrefix(text, start)
	if !ok {
		return false
	}
	middle, ok := strings.CutSuffix(rest, end)
	return ok && middle != ""
}

// podSecurity matches `violates PodSecurity "<level>:<version>": ` and more
// text.
func podSecurity(text string) bool {
	rest, ok := strings.CutPrefix(text, "violates PodSecurity ")
	if !ok {
		return false
	}
	quoted, value, ok := quotedName(rest)
	if !ok {
		return false
	}
	level, version, ok := strings.Cut(value, ":")
	if !ok || level == "" || version == "" || strings.Contains(value, " ") {
		return false
	}
	return nonEmptyAfter(rest[len(quoted):], ": ")
}

// namespaceTerminating matches exactly `unable to create new content in
// namespace <ns> because it is being terminated`.
func namespaceTerminating(text string) bool {
	rest, ok := cutWords(text, "unable to create new content in namespace ", " because it is being terminated")
	return ok && rest == ""
}

// validatingAdmissionPolicy matches `ValidatingAdmissionPolicy '<p>' with
// binding '<b>' denied request: ...` and `ValidatingAdmissionPolicy '<p>'
// denied request: ...`.
func validatingAdmissionPolicy(text string) bool {
	rest, ok := cutWords(text, "ValidatingAdmissionPolicy '", "'")
	if !ok {
		return false
	}
	if binding, found := strings.CutPrefix(rest, " with binding '"); found {
		rest, ok = cutPlaceholder(binding, "'")
		if !ok {
			return false
		}
	}
	return strings.HasPrefix(rest, " denied request: ")
}
