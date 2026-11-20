package clierr

import (
	"slices"
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
// that 403 as RBAC again; the weekly make admission-text-check fails when a
// Kubernetes module no longer writes that text at the versions the Rancher
// commit of each pinned Rancher image requires or at the versions in go.mod. A
// new 403 from a server other than Kubernetes admission whose text begins with
// an admission text would be reported as VALIDATION, and no test here detects
// that.
//
// In an admission text, <q>, <res>, <type>, <ns>, <p> and <b> each stand for
// one or more characters with no space, ending at the first place where the
// next fixed words of the admission text follow. The other words must match
// exactly. Each matcher from serviceAccountNotFound on names its own
// placeholders in its comment. Two exceptions: in
// runtimeclass-node-selector-conflict one of the two values after `] = ` may be
// empty, and serviceaccount-lookup-error has no fixed words after `: `, because
// the text there is the error of a lookup: the matcher reads it for the texts
// of serviceAccountTemporaryTexts anywhere in it. The texts after the colon of
// serviceaccount-invalid-name and serviceaccount-lookup-error are written by
// client-go, apimachinery and the Go standard library, not by a module the
// weekly check reads; tests in this package build them from those libraries.

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
	// ServiceAccount: k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/serviceaccount/admission.go:164, :228 and :389; the
	// text after the colon is written at :314 of the same file and by
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:155. A namespace or name that client-go refuses is the entry
	// below, a temporary error is serviceaccount-lookup-error in the group of temporary 403s, and any other lookup
	// error is not matched.
	{id: "serviceaccount-not-found", match: serviceAccountNotFound},
	// The same lookup with a namespace or a name that k8s.io/client-go@v0.37.1 refuses before it sends a request, one that
	// has `/` or `%` or is `.` or `..`: rest/request.go:362 for the namespace and :345 for the name, called in this
	// order by gentype/type.go:159-163, and returned at :310 of the admission.go above. A retry gets the same response:
	// the 403 is not temporary.
	{id: "serviceaccount-invalid-name", match: serviceAccountInvalidName},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/serviceaccount/admission.go:223 and :384; a defensive check, because
	// Admit sets `default` for a pod with no ServiceAccount name first.
	{id: "serviceaccount-none-specified", match: serviceAccountNoneSpecified},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/serviceaccount/admission.go:330, :338, :345, :355, :362 and :375.
	{id: "serviceaccount-secret-reference", match: serviceAccountSecretReference},
	// Priority: k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/priority/admission.go:363.
	{id: "priorityclass-not-found", match: priorityClassNotFound},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/priority/admission.go:185.
	{id: "priority-set-in-pod", match: prioritySetInPod},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/priority/admission.go:193.
	{id: "preemptionpolicy-set-in-pod", match: preemptionPolicySetInPod},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/priority/admission.go:300.
	{id: "priorityclass-default-exists", match: priorityClassDefaultExists},
	// RuntimeClass: k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/runtimeclass/admission.go:158.
	{id: "runtimeclass-not-found", match: runtimeClassNotFound},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/runtimeclass/admission.go:179 and :227.
	{id: "runtimeclass-overhead-mismatch", match: exactText("pod rejected: Pod's Overhead doesn't match RuntimeClass's defined Overhead")},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/runtimeclass/admission.go:232.
	{id: "runtimeclass-overhead-unexpected", match: exactText("pod rejected: Pod Overhead set without corresponding RuntimeClass defined Overhead")},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/runtimeclass/admission.go:205.
	{id: "runtimeclass-node-selector-conflict", match: runtimeClassNodeSelectorConflict},
	// PersistentVolumeClaimResize: k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/storage/persistentvolume/resize/admission.go:102.
	{id: "pvc-resize-unbound", match: exactText("Only bound persistent volume claims can be expanded")},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/storage/persistentvolume/resize/admission.go:108-109.
	{id: "pvc-resize-unsupported", match: exactText("only dynamically provisioned pvc can be resized and the storageclass that provisions the pvc must support resize")},
	// CertificateSubjectRestriction: k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/certificates/subjectrestriction/admission.go:87-88.
	{id: "csr-system-masters", match: exactText("use of kubernetes.io/kube-apiserver-client signer with system:masters group is not allowed")},

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
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/serviceaccount/admission.go:164, :228 and :389; the lookup failed with
	// one of the temporary errors of serviceAccountTemporaryTexts, which :290-292 and :309-311 return unchanged. Any
	// other lookup error is not matched.
	{id: "serviceaccount-lookup-error", retryable: true, match: serviceAccountLookupError},
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

// serviceAccountNotFound matches exactly `error looking up service account
// <ns>/<name>: serviceaccount "<sa>" not found`, with a non-empty <sa> in Go
// double quotes. <ns>/<name> is one or more characters with no space. Any other
// lookup error is not matched, and neither is a text with anything after
// `not found`.
func serviceAccountNotFound(text string) bool {
	rest, ok := cutWords(text, "error looking up service account ", ": serviceaccount ")
	if !ok {
		return false
	}
	quoted, _, ok := quotedName(rest)
	return ok && rest[len(quoted):] == " not found"
}

// serviceAccountInvalidName matches `error looking up service account
// <ns>/<name>: invalid resource name "<value>": ` and `error looking up service
// account <ns>/<name>: invalid namespace "<value>": `, each followed by more
// text, with a non-empty <value> in Go double quotes. <ns>/<name> is one or more
// characters with no space.
func serviceAccountInvalidName(text string) bool {
	rest, ok := cutWords(text, "error looking up service account ", ": invalid ")
	if !ok {
		return false
	}
	for _, kind := range []string{"resource name ", "namespace "} {
		if value, found := strings.CutPrefix(rest, kind); found {
			quoted, _, ok := quotedName(value)
			return ok && nonEmptyAfter(value[len(quoted):], ": ")
		}
	}
	return false
}

// serviceAccountTemporaryTexts are the texts that mark a failed ServiceAccount
// lookup as temporary. serviceAccountLookupError reads them anywhere after
// `error looking up service account <ns>/<name>: `, because client-go writes the
// cause after the request (`Get "<url>": <cause>`) and writes its own text of a 503,
// 504 or 429 before ` (get serviceaccounts <name>)`. A lookup error with none of
// them is not matched. Each text has a row in the admission test.
var serviceAccountTemporaryTexts = []string{
	// context.DeadlineExceeded: context/context.go:176 of the Go standard library. The message of
	// k8s.io/apiserver@v0.37.1/pkg/endpoints/handlers/finisher/finisher.go:146 ends with it, from ctx.Err().
	"context deadline exceeded",
	// os.ErrDeadlineExceeded: internal/poll/fd.go:60 of the Go standard library, the end of a failed dial or read.
	"i/o timeout",
	// net/http/client.go:749 of the Go standard library.
	"Client.Timeout exceeded",
	// net/http/transport.go:3421 of the Go standard library.
	"TLS handshake timeout",
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:484, the text apimachinery writes for a 504.
	"the server was unable to return a response in the time allotted",
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:401-406, which formats the message that
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/webhook/validating/dispatcher.go:122 passes to it.
	"request did not complete within requested timeout",
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:401-406, which formats the message that
	// k8s.io/apiserver@v0.37.1/pkg/server/filters/timeout.go:59, the timeout filter of the API server, passes to it.
	"Timeout: request did not complete within the allotted timeout",
	// go.etcd.io/etcd/api/v3@v3.7.0/v3rpc/rpctypes/error.go:82, whose longer forms at :83-85 begin with it;
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:395 writes it after `Internal error occurred: `.
	"etcdserver: request timed out",
	// ECONNREFUSED and ECONNRESET: syscall/zerrors_linux_amd64.go:1466 and :1459 of the Go standard library.
	"connection refused",
	"connection reset by peer",
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:481, the text apimachinery writes for a 503.
	"the server is currently unable to handle the request",
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:421.
	"Too many requests",
	// k8s.io/apimachinery@v0.37.1/pkg/api/errors/errors.go:487, the text apimachinery writes for a 429.
	"the server has received too many requests and has asked us to try again later",
}

// serviceAccountLookupError matches `error looking up service account
// <ns>/<name>: ` followed by a text that has one of serviceAccountTemporaryTexts
// anywhere in it: the lookup failed with a temporary error. <ns>/<name> is one
// or more characters with no space. Any other lookup error, for example an
// Unauthorized response or a Forbidden response to the lookup, is not matched.
// The plugin runs before validation, and the pod's ServiceAccount name can have
// `: ` in it. <ns>/<name> then ends at that `: ` and the rest of the name is
// read as part of the error text. A name that has `: ` followed by one of
// serviceAccountTemporaryTexts makes any lookup error read as temporary.
// The matcher is case-sensitive.
func serviceAccountLookupError(text string) bool {
	rest, ok := cutWords(text, "error looking up service account ", ": ")
	if !ok {
		return false
	}
	return slices.ContainsFunc(serviceAccountTemporaryTexts, func(temporary string) bool {
		return strings.Contains(rest, temporary)
	})
}

// serviceAccountNoneSpecified matches exactly `no service account specified
// for pod <ns>/<name>`. <ns>/<name> is one or more characters with no space,
// with a `/` among them; <name> is empty for a pod that has a generateName only.
func serviceAccountNoneSpecified(text string) bool {
	rest, ok := strings.CutPrefix(text, "no service account specified for pod ")
	return ok && rest != "" && !strings.Contains(rest, " ") && strings.Contains(rest, "/")
}

// serviceAccountSecretTexts are the six texts of the ServiceAccount admission
// plugin that name a secret the ServiceAccount does not reference. Each is a
// list of fixed words: the words the text begins with, then the words that
// follow each placeholder. The text must end right after the last of them.
var serviceAccountSecretTexts = [][]string{
	// A volume.
	{`volume with secret.secretName="`, `" is not allowed because service account `, ` does not reference that secret`},
	// An init container or a container, with an environment variable or envFrom.
	{"init container ", " with envVar ", ` referencing secret.secretName="`, `" is not allowed because service account `, ` does not reference that secret`},
	{"init container ", ` with envFrom referencing secret.secretName="`, `" is not allowed because service account `, ` does not reference that secret`},
	{"container ", " with envVar ", ` referencing secret.secretName="`, `" is not allowed because service account `, ` does not reference that secret`},
	{"container ", ` with envFrom referencing secret.secretName="`, `" is not allowed because service account `, ` does not reference that secret`},
	// An image pull secret.
	{"imagePullSecrets[", `].name="`, `" is not allowed because service account `, ` does not reference that imagePullSecret`},
}

// serviceAccountSecretReference matches one of the texts in
// serviceAccountSecretTexts, for example `volume with secret.secretName="<s>"
// is not allowed because service account <sa> does not reference that secret`.
// <s> (the secret name between literal double quotes), <sa>, <c> (a container
// name), <e> (an environment variable name) and <i> (the index in
// imagePullSecrets) are each one or more characters with no space. A text about
// an ephemeral container is not matched: the server returns it with a 500, not
// a 403.
func serviceAccountSecretReference(text string) bool {
	for _, words := range serviceAccountSecretTexts {
		if rest, ok := cutWords(text, words[0], words[1:]...); ok && rest == "" {
			return true
		}
	}
	return false
}

// priorityClassNotFound matches exactly `no PriorityClass with name <name> was
// found`. <name> is one or more characters with no space.
func priorityClassNotFound(text string) bool {
	rest, ok := cutWords(text, "no PriorityClass with name ", " was found")
	return ok && rest == ""
}

// prioritySetInPod matches exactly `the integer value of priority (<priority>)
// must not be provided in pod spec; priority admission controller computed
// <computed priority> from the given PriorityClass name`. <priority> and
// <computed priority> are each one or more characters with no space. The
// texts about a pod group start differently and are not matched.
func prioritySetInPod(text string) bool {
	rest, ok := cutWords(text, "the integer value of priority (", ") must not be provided in pod spec; priority admission controller computed ", " from the given PriorityClass name")
	return ok && rest == ""
}

// preemptionPolicySetInPod matches exactly `the string value of
// PreemptionPolicy (<policy>) must not be provided in pod spec; priority
// admission controller computed <computed policy> from the given PriorityClass
// name`. <policy> and <computed policy> are each one or more characters with
// no space. The texts about a pod group are not matched.
func preemptionPolicySetInPod(text string) bool {
	rest, ok := cutWords(text, "the string value of PreemptionPolicy (", ") must not be provided in pod spec; priority admission controller computed ", " from the given PriorityClass name")
	return ok && rest == ""
}

// priorityClassDefaultExists matches exactly `PriorityClass <name> is already
// marked as default. Only one default can exist`. <name> is one or more
// characters with no space.
func priorityClassDefaultExists(text string) bool {
	rest, ok := cutWords(text, "PriorityClass ", " is already marked as default. Only one default can exist")
	return ok && rest == ""
}

// runtimeClassNotFound matches exactly `pod rejected: RuntimeClass "<name>"
// not found`, with a non-empty <name> in Go double quotes.
func runtimeClassNotFound(text string) bool {
	rest, ok := strings.CutPrefix(text, "pod rejected: RuntimeClass ")
	if !ok {
		return false
	}
	quoted, _, ok := quotedName(rest)
	return ok && rest[len(quoted):] == " not found"
}

// runtimeClassNodeSelectorConflict matches exactly `conflict:
// runtimeClass.scheduling.nodeSelector[<k>] = <v>; pod.spec.nodeSelector[<k>] =
// <w>`, with the same <k> on both sides. <k> is one or more characters with no
// space. <v> and <w> are each zero or more characters with no space: a label
// value can be empty, and the server writes the text only when they differ, so
// at most one is empty. The server writes a space after `] =` even when <w> is
// empty, and the CLI trims the message. A message that ends right after `] =`
// is matched, and so is one that ends with `] = `.
func runtimeClassNodeSelectorConflict(text string) bool {
	rest, ok := strings.CutPrefix(text, "conflict: runtimeClass.scheduling.nodeSelector[")
	if !ok {
		return false
	}
	key, rest, ok := strings.Cut(rest, "] = ")
	if !ok || key == "" || strings.Contains(key, " ") {
		return false
	}
	runtimeValue, rest, ok := strings.Cut(rest, "; pod.spec.nodeSelector["+key+"] =")
	if !ok || strings.Contains(runtimeValue, " ") {
		return false
	}
	podValue := ""
	if rest != "" {
		podValue, ok = strings.CutPrefix(rest, " ")
		if !ok || strings.Contains(podValue, " ") {
			return false
		}
	}
	return runtimeValue != "" || podValue != ""
}
