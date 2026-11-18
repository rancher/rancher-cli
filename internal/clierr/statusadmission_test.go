package clierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

// admissionRow is one 403 and the error the CLI reports for it. id names the
// admission text that must match, or is empty for a 403 that must stay RBAC.
// retryable is the class the row expects of a match: a retryable GENERIC,
// otherwise VALIDATION. It is written in the row, never read from the list
// under test. Every row runs with every kind of request.
type admissionRow struct {
	name      string
	id        string
	retryable bool
	se        *apierrors.StatusError
	// noJSON skips Rancher's error JSON, which cannot carry an empty message
	// and shows the message with its trailing space cut.
	noJSON bool
}

// kubernetesForbidden is the 403 apierrors.NewForbidden builds for a refusal
// of resource (the empty string for none) and name, with the given text after
// `forbidden: `.
func kubernetesForbidden(resource, name, text string) *apierrors.StatusError {
	return apierrors.NewForbidden(schema.ParseGroupResource(resource), name, errors.New(text))
}

// handBuiltForbidden is a 403 with the given message, for rows about the
// beginning of the message and for texts that do not begin as Kubernetes
// writes them.
func handBuiltForbidden(message string) *apierrors.StatusError {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden, Message: message,
	}}
}

// admissionStart is one way the server begins the message of a 403.
type admissionStart struct {
	name     string
	resource string
	object   string
}

// admissionStarts are the three beginnings apierrors.NewForbidden writes (with
// a name, with no name, bare), and a resource with a group.
var admissionStarts = []admissionStart{
	{"with a name", "pods", "p"},
	{"with no name", "pods", ""},
	{"with a group-qualified resource", "deployments.apps", "d"},
	{"with the bare beginning", "", ""},
}

// retryableAdmissionIDs are the four admission texts of a temporary 403. The
// list under test must hold exactly these as retryable.
var retryableAdmissionIDs = []string{"limitrange-error", "not-ready", "quota-previous-usage", "quota-status-unknown"}

// admissionSample is a text after `forbidden: ` that must match the
// list entry id, with the class that match gets.
type admissionSample struct {
	id    string
	label string
	text  string
	// allStarts runs the sample with every beginning of the message; otherwise
	// only with a name.
	allStarts bool
	// retryable is true for a retryable GENERIC; otherwise the class is VALIDATION.
	retryable bool
}

// aggregateText is the text the LimitRanger plugin returns for several
// violations: the aggregate error (utilerrors.NewAggregate) of one error for each message, which
// prints in brackets for two or more distinct messages.
func aggregateText(messages ...string) string {
	errs := make([]error, len(messages))
	for i, m := range messages {
		errs[i] = errors.New(m)
	}
	return utilerrors.NewAggregate(errs).Error()
}

const (
	limitRangeMinimumCPU    = `minimum cpu usage per Container is 1, but request is 500m`
	limitRangeMinimumMemory = `minimum memory usage per Container is 64Mi, but request is 32Mi`
	limitRangeMaximumCPU    = `maximum cpu usage per Container is 1, but limit is 2`
	limitRangeMaximumMemory = `maximum memory usage per Container is 64Mi, but limit is 128Mi`
	limitRangeRatioCPU      = `cpu max limit to request ratio per Container is 2, but provided ratio is 4.000000`
	limitRangeRatioMemory   = `memory max limit to request ratio per Container is 2, but provided ratio is 3.000000`
)

// admissionSamples are the texts of the list as the Kubernetes API server
// writes them. The comment of each names the module, file and line that
// writes it.
var admissionSamples = []admissionSample{
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:621-626
	{"quota-exceeded", "", `exceeded quota: q1, requested: count/configmaps=1, used: count/configmaps=1, limited: count/configmaps=1`, true, false},
	{"quota-exceeded", "a quota name with a dot", `exceeded quota: requests.storage, requested: requests.storage=2Gi, used: requests.storage=9Gi, limited: requests.storage=10Gi`, false, false},
	{"quota-exceeded", "a quota name with a slash", `exceeded quota: count/configmaps, requested: count/configmaps=1, used: count/configmaps=1, limited: count/configmaps=1`, false, false},
	{"quota-exceeded", "values with commas", `exceeded quota: compute, requested: cpu=1,memory=1Gi, used: cpu=2,memory=2Gi, limited: cpu=2,memory=2Gi`, false, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:580
	{"quota-insufficient", "", `insufficient quota to consume: requests.storage`, true, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:471, with the text after
	// the quota name from k8s.io/kubernetes@v1.37.1/pkg/quota/v1/evaluator/core/pods.go:170
	{"quota-constraint", "", `failed quota: compute: must specify limits.cpu for: web`, true, false},
	{"quota-constraint", "a quota name with a comma", `failed quota: cpu,memory: must specify limits.cpu for: web`, false, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:502
	{"quota-negative", "", `quota usage is negative for resource(s): requests.cpu`, true, false},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:315, :318, :321
	{"limitrange-minimum", "", `minimum memory usage per Container is 64Mi, but request is 32Mi`, true, false},
	{"limitrange-minimum", "the type PersistentVolumeClaim", `minimum storage usage per PersistentVolumeClaim is 1Gi, but request is 500Mi`, false, false},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:333, :336, :348, :351, :354
	{"limitrange-maximum", "", `maximum cpu usage per Container is 1, but limit is 2`, true, false},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:366, :369, :381
	{"limitrange-ratio", "", `cpu max limit to request ratio per Container is 2, but provided ratio is 4.000000`, true, false},
	// Two distinct violations print in brackets, built as the plugin builds them:
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:472 and :556
	// through k8s.io/apimachinery@v0.37.1/pkg/util/errors/errors.go:70-96.
	{"limitrange-list", "two minimum violations", aggregateText(limitRangeMinimumCPU, limitRangeMinimumMemory), true, false},
	{"limitrange-list", "two maximum violations", aggregateText(limitRangeMaximumCPU, limitRangeMaximumMemory), false, false},
	{"limitrange-list", "two ratio violations", aggregateText(limitRangeRatioCPU, limitRangeRatioMemory), false, false},
	// The same violation twice: the aggregate error (utilerrors.NewAggregate) prints it once, with no brackets.
	{"limitrange-maximum", "the same violation twice", aggregateText(limitRangeMaximumCPU, limitRangeMaximumCPU), false, false},
	// LimitRanger writes only its own items: this asserts that the CLI reads only the first item.
	{"limitrange-list", "a LimitRanger first item and another second item", aggregateText(limitRangeMaximumCPU, "something else"), false, false},
	// k8s.io/pod-security-admission@v0.37.1/admission/admission.go:480-484
	{"podsecurity", "", `violates PodSecurity "restricted:latest": privileged (container "c" must not set securityContext.privileged=true)`, true, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/namespace/lifecycle/admission.go:179
	{"namespace-terminating", "", `unable to create new content in namespace ns1 because it is being terminated`, true, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/namespace/lifecycle/admission.go:80
	{"namespace-immortal", "", `this namespace may not be deleted`, true, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/policy/validating/dispatcher.go:305
	{"validatingadmissionpolicy", "with a binding", `ValidatingAdmissionPolicy 'no-team' with binding 'no-team-binding' denied request: failed expression: has(object.metadata.labels.team)`, true, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/policy/validating/dispatcher.go:307
	{"validatingadmissionpolicy", "with no binding", `ValidatingAdmissionPolicy 'no-team' denied request: failed expression: has(object.metadata.labels.team)`, true, false},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/namespace/lifecycle/admission.go:117,
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/webhook/generic/webhook.go:421, :429 and
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/policy/generic/plugin.go:323, :331
	{"not-ready", "", `not yet ready to handle request`, true, true},
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/limitranger/admission.go:163; the resource is
	// printed as `/v1, Resource=pods` and holds a space
	{"limitrange-error", "", `unable to create /v1, Resource=pods at this time because there was an error enforcing limit ranges`, true, true},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:474
	{"quota-status-unknown", "", `status unknown for quota: q1`, true, true},
	// k8s.io/apiserver@v0.37.1/pkg/admission/plugin/resourcequota/controller.go:518
	{"quota-previous-usage", "", `unable to get previous usage since prior version of object was not found`, true, true},
}

// admissionRows are every row of the test.
func admissionRows() []admissionRow {
	var rows []admissionRow
	for _, s := range admissionSamples {
		starts := admissionStarts[:1]
		if s.allStarts {
			starts = admissionStarts
		}
		for _, st := range starts {
			name := s.id
			if s.label != "" {
				name += " " + s.label
			}
			rows = append(rows, admissionRow{
				name:      name + " " + st.name,
				id:        s.id,
				retryable: s.retryable,
				se:        kubernetesForbidden(st.resource, st.object, s.text),
			})
		}
	}

	rbac := func(name string, se *apierrors.StatusError) {
		rows = append(rows, admissionRow{name: name, se: se})
	}
	// A message that ends right after the fixed words keeps a trailing space,
	// which the Rancher error JSON removes.
	rbacNoJSON := func(name string, se *apierrors.StatusError) {
		rows = append(rows, admissionRow{name: name, se: se, noJSON: true})
	}
	const quota = `exceeded quota: q1, requested: pods=1, used: pods=1, limited: pods=1`

	// The fixed words of an admission text that must be the whole text, given
	// extra text after them.
	rbac("not-ready with text after it", kubernetesForbidden("pods", "p", `not yet ready to handle request, try again`))
	rbac("quota-previous-usage with text after it", kubernetesForbidden("pods", "p", `unable to get previous usage since prior version of object was not found: x`))
	rbac("namespace-immortal with text after it", kubernetesForbidden("namespaces", "default", `this namespace may not be deleted: x`))
	rbac("namespace-terminating with text after it", kubernetesForbidden("configmaps", "c1", `unable to create new content in namespace ns1 because it is being terminated: x`))
	rbac("namespace-terminating with an empty namespace", kubernetesForbidden("configmaps", "c1", `unable to create new content in namespace  because it is being terminated`))

	// The fixed words of an admission text that need a value after them, given
	// none.
	rbacNoJSON("quota-insufficient with nothing after it", kubernetesForbidden("pods", "p", `insufficient quota to consume: `))
	rbacNoJSON("quota-constraint with nothing after it", kubernetesForbidden("pods", "p", `failed quota: compute: `))
	rbacNoJSON("quota-negative with nothing after it", kubernetesForbidden("pods", "p", `quota usage is negative for resource(s): `))
	rbacNoJSON("limitrange-minimum with nothing after it", kubernetesForbidden("pods", "p", `minimum memory usage per Container is `))
	rbacNoJSON("limitrange-maximum with nothing after it", kubernetesForbidden("pods", "p", `maximum cpu usage per Container is `))
	rbacNoJSON("limitrange-ratio with nothing after it", kubernetesForbidden("pods", "p", `cpu max limit to request ratio per Container is `))
	rbac("podsecurity with a value that has no colon", kubernetesForbidden("pods", "p", `violates PodSecurity "restricted": privileged`))
	rbac("podsecurity with an empty level", kubernetesForbidden("pods", "p", `violates PodSecurity ":latest": privileged`))
	rbac("podsecurity with an empty version", kubernetesForbidden("pods", "p", `violates PodSecurity "restricted:": privileged`))
	rbac("podsecurity with a space in the value", kubernetesForbidden("pods", "p", `violates PodSecurity "a b:c": privileged`))
	rbac("validatingadmissionpolicy with no colon after denied request", kubernetesForbidden("pods", "p", `ValidatingAdmissionPolicy 'no-team' denied requestx failed expression: x`))
	rbac("validatingadmissionpolicy with a binding and no colon after denied request", kubernetesForbidden("pods", "p", `ValidatingAdmissionPolicy 'no-team' with binding 'b' denied requestx failed expression: x`))
	rbacNoJSON("podsecurity with nothing after it", kubernetesForbidden("pods", "p", `violates PodSecurity "restricted:latest": `))
	rbac("podsecurity with a name that is not quoted", kubernetesForbidden("pods", "p", `violates PodSecurity restricted:latest: privileged`))
	rbacNoJSON("quota-status-unknown with nothing after it", kubernetesForbidden("pods", "p", `status unknown for quota: `))

	// An empty or spaced value of a placeholder.
	rbac("quota-exceeded with an empty quota name", kubernetesForbidden("pods", "p", `exceeded quota: , requested: x, used: y, limited: z`))
	rbac("quota-exceeded with a space in the quota name", kubernetesForbidden("pods", "p", `exceeded quota: q 1, requested: x, used: y, limited: z`))
	rbac("quota-exceeded with the markers out of order", kubernetesForbidden("pods", "p", `exceeded quota: q1, requested: x, limited: z, used: y`))
	rbac("quota-exceeded with a marker missing", kubernetesForbidden("pods", "p", `exceeded quota: q1, requested: x, used: y`))
	rbac("quota-constraint with an empty quota name", kubernetesForbidden("pods", "p", `failed quota: : must specify limits.cpu`))
	rbac("limitrange-minimum with an empty resource", kubernetesForbidden("pods", "p", `minimum  usage per Container is 64Mi, but request is 32Mi`))
	rbac("limitrange-minimum with a space in the resource", kubernetesForbidden("pods", "p", `minimum a b usage per Container is 64Mi, but request is 32Mi`))
	rbac("limitrange-maximum with an empty type", kubernetesForbidden("pods", "p", `maximum cpu usage per  is 1, but limit is 2`))
	rbac("limitrange-ratio with a space in the type", kubernetesForbidden("pods", "p", `cpu max limit to request ratio per Con tainer is 2, but provided ratio is 4`))
	rbac("validatingadmissionpolicy with an empty policy name", kubernetesForbidden("pods", "p", `ValidatingAdmissionPolicy '' denied request: failed expression: x`))
	rbac("validatingadmissionpolicy with an empty binding name", kubernetesForbidden("pods", "p", `ValidatingAdmissionPolicy 'no-team' with binding '' denied request: failed expression: x`))

	// limitrange-error is retryable, never VALIDATION; it needs both its beginning and its end.
	rbac("limitrange-error with the beginning only", kubernetesForbidden("pods", "p", `unable to x at this time`))
	rbac("limitrange-error with the end only", kubernetesForbidden("pods", "p", `creating pods at this time because there was an error enforcing limit ranges`))
	rbac("limitrange-error with nothing between its beginning and its end", kubernetesForbidden("pods", "p", `unable to  at this time because there was an error enforcing limit ranges`))

	// The CLI reads only the first item of a bracketed list.
	rbac("a bracketed list whose first item is not a LimitRanger text", kubernetesForbidden("pods", "p", `[something else, maximum cpu usage per Container is 1, but limit is 2]`))
	rbac("a bracketed quota text", kubernetesForbidden("pods", "p", `[`+quota+`, `+quota+`]`))
	rbac("a bracket followed by a space and a LimitRanger text", kubernetesForbidden("pods", "p", `[ maximum cpu usage per Container is 1, but limit is 2]`))

	// The beginning of the message, built by hand.
	rbac("a resource with a space in it", handBuiltForbidden(`pod s "p" is forbidden: `+quota))
	rbac("an unquoted name", handBuiltForbidden(`pods p is forbidden: `+quota))
	rbac("an unterminated quote", handBuiltForbidden(`pods "p is forbidden: `+quota))
	rbac("an empty quoted name", handBuiltForbidden(`pods "" is forbidden: `+quota))
	rbac(`a message that says "was forbidden" instead of "is forbidden"`, handBuiltForbidden(`pods "p" was forbidden: `+quota))
	rbac(`a message that begins with "Forbidden: " with a capital F`, handBuiltForbidden(`Forbidden: `+quota))
	rbac("a message with no Kubernetes beginning", handBuiltForbidden(quota))
	rbac("a resource with a quote", handBuiltForbidden(`po"ds is forbidden: `+quota))

	// An admission text that is not at the beginning of the text that follows
	// `is forbidden: `.
	rbac("an admission text in the middle of the text", kubernetesForbidden("pods", "p", `the request failed: `+quota))

	// The authorizer, impersonation and other 403s that are not admission refusals.
	rbac("the authorizer in a namespace, with a name", kubernetesForbidden("pods", "p", `User "u" cannot delete resource "pods" in API group "" in the namespace "n"`))
	rbac("the authorizer at the cluster scope, with no name", kubernetesForbidden("pods", "", `User "u" cannot list resource "pods" in API group "" at the cluster scope`))
	rbac("the authorizer with text after its message that is an admission text", kubernetesForbidden("pods", "p", `User "u" cannot create resource "pods" in API group "" in the namespace "n": `+quota))
	rbac("the authorizer for a path with text after it that is an admission text", kubernetesForbidden("", "", `User "u" cannot get path "/x": `+quota))
	rbac("impersonation", kubernetesForbidden("users", "bob", `User "u" cannot impersonate resource "users" in API group "" at the cluster scope`))
	rbac("the Rancher ext API authorizer", kubernetesForbidden("testtypes.ext.cattle.io", "", `User "u" cannot list resource "testtypes" in API group "ext.cattle.io" at the cluster scope`))
	// k8s.io/apiserver@v0.37.1/pkg/endpoints/handlers/delete.go:423
	rbac("the unsafe delete option", kubernetesForbidden("pods", "p", `not permitted to do "unsafe-delete-ignore-read-errors", reason: the option is not enabled`))
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/noderestriction/admission.go:554
	rbac("noderestriction", kubernetesForbidden("nodes", "n1", `node "n2" is not allowed to modify node "n1"`))
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/gc/gc_admission.go:118
	rbac("the garbage collector plugin", kubernetesForbidden("pods", "p", `cannot set an ownerRef on a resource you can't delete: x`))
	// k8s.io/kubernetes@v1.37.1/plugin/pkg/admission/certificates/approval/admission.go:93
	rbac("a certificate plugin", kubernetesForbidden("certificatesigningrequests.certificates.k8s.io", "c", `user not permitted to approve requests with signerName "x"`))

	// A MutatingAdmissionPolicy configuration error does not begin with
	// `is forbidden: ` (k8s.io/apiserver@v0.37.1/pkg/admission/plugin/policy/generic/policy_dispatcher.go:126, :128).
	rbac("a MutatingAdmissionPolicy configuration error with a binding", handBuiltForbidden(`policy 'p' with binding 'b' denied request: failed to configure binding: x`))
	rbac("a MutatingAdmissionPolicy configuration error with no binding", handBuiltForbidden(`policy "p" denied request: failed to configure policy: x`))

	// An admission webhook's own 403 stays RBAC whatever its message says
	// (k8s.io/apiserver@v0.37.1/pkg/admission/plugin/webhook/errors/statuserror.go:28-57).
	rbac("a webhook denial with 403 whose text is an admission text", handBuiltForbidden(`admission webhook "x.example.io" denied the request: `+quota))
	rbac("a webhook denial with 403 whose text is an admission text, after `is forbidden: `", kubernetesForbidden("pods", "p", `admission webhook "x.example.io" denied the request: `+quota))

	// Rancher's own texts.
	rbac("Rancher's token text", kubernetesForbidden("tokens.ext.cattle.io", "", `user u-1 is not a Rancher user`))
	rbac("Rancher's kubeconfig backing store text", kubernetesForbidden("kubeconfigs.ext.cattle.io", "k", `backing store denied the request`))
	rbac("Rancher's kubeconfig request token text", kubernetesForbidden("kubeconfigs.ext.cattle.io", "", `error getting request token t-1: x`))
	rbac("Norman's text", handBuiltForbidden(`can not create clusters`))
	// A message longer than the 200 bytes that Rancher's error JSON shows is still read whole.
	const longQuota = `exceeded quota: compute-resources, requested: limits.cpu=500m,limits.memory=512Mi,requests.cpu=250m,requests.memory=256Mi, used: limits.cpu=7,limits.memory=14Gi,requests.cpu=3500m,requests.memory=7Gi, limited: limits.cpu=8,limits.memory=16Gi,requests.cpu=4,requests.memory=8Gi`
	rows = append(rows, admissionRow{name: "quota-exceeded longer than 200 bytes", id: "quota-exceeded", se: kubernetesForbidden("pods", "web-7c9d8f6b5-abcde", longQuota)})
	rows = append(rows, admissionRow{
		name: "limitrange-error longer than 200 bytes", id: "limitrange-error", retryable: true,
		se: kubernetesForbidden("pods", "p", "unable to create "+strings.Repeat("x", 200)+" at this time because there was an error enforcing limit ranges"),
	})
	rbac("a webhook denial with 403 longer than 200 bytes", handBuiltForbidden(`admission webhook "x.example.io" denied the request: `+strings.Repeat("x", 200)+" "+quota))
	rows = append(rows, admissionRow{name: "an empty 403", se: handBuiltForbidden(""), noJSON: true})
	return rows
}

// admissionExpectation is the code, retryability, exit code and message a
// row expects for a request.
type admissionExpectation struct {
	code      string
	retryable bool
	exit      int
	message   string
}

// unresolvedSentence is what the CLI adds to a 403 that is not an admission
// text, on a request to a cluster Rancher could not resolve.
const unresolvedSentence = "; Rancher answers 403 for a cluster the user cannot read and, to a user who cannot read every cluster, for one it does not know: check the --cluster value 'c-x'"

// expectation is what the row expects for req. show is how the code under test
// shows the server's text: Rancher's error JSON shows it cut to 200 bytes.
func (r admissionRow) expectation(t *testing.T, req *Request, show func(string) string) admissionExpectation {
	t.Helper()
	text := show(r.se.ErrStatus.Message)
	if r.id == "" {
		want := admissionExpectation{code: CodeRBAC, exit: 6, message: text}
		if text == "" {
			want.message = forbiddenText(req)
		}
		if req != nil && req.Unresolved && req.Cluster != "" && req.Cluster != "local" {
			want.message += unresolvedSentence
		}
		return want
	}
	if r.retryable {
		return admissionExpectation{code: CodeGeneric, retryable: true, exit: 1, message: text + " - retry later"}
	}
	return admissionExpectation{code: CodeValidation, exit: 2, message: text}
}

// admissionPaths are the ways the error reaches the classifier: the typed
// Status, the same behind a caller's prefix, and Rancher's error JSON.
var admissionPaths = []struct {
	name     string
	show     func(string) string
	classify func(se *apierrors.StatusError, req *Request) (*Error, string)
}{
	{"typed", keepText, func(se *apierrors.StatusError, req *Request) (*Error, string) {
		return FromStatus(se, req), ""
	}},
	{"behind a prefix", keepText, func(se *apierrors.StatusError, req *Request) (*Error, string) {
		return FromStatus(fmt.Errorf("listing the objects: %w", se), req), "listing the objects: "
	}},
	{"Rancher's error JSON", bounded, func(se *apierrors.StatusError, req *Request) (*Error, string) {
		body, err := json.Marshal(map[string]any{
			"type": "error", "status": http.StatusForbidden, "code": "Forbidden", "message": se.ErrStatus.Message,
		})
		if err != nil {
			panic(err)
		}
		return FromStatus(ResponseError(http.MethodPost, http.StatusForbidden, body), req), ""
	}},
}

// TestAdmissionTextsGetTheirCodeAndEveryOther403StaysRBAC asserts every
// admission text in the list gets its result and every other 403 stays RBAC
// with the Status message. A 403 that refuses the object (ResourceQuota,
// LimitRanger, PodSecurity, namespace lifecycle, ValidatingAdmissionPolicy) is
// VALIDATION at exit 2; a temporary 403 (the server is not ready, LimitRanger
// failed to enforce limit ranges, the ResourceQuota status is unknown, the
// previous version of the object was not found) is a retryable
// GENERIC with the Status message and " - retry later", also when the request
// names a server; both have no hint. A message that does not begin as
// Kubernetes writes it, an admission text anywhere but at the beginning of the
// text after `forbidden: `, the authorizer's texts, a webhook's own
// 403 and Rancher's texts stay RBAC. Every row runs through the typed Status,
// behind a caller's prefix and as Rancher's error JSON, with every kind of
// request: every verb, an action with no verb, a request that names a server,
// an unresolved cluster, and no request at all. The test fails for an entry
// with no row, a row for an id that is not in the list, a repeated id or row
// name, and a set of retryable entries other than the four.
//
// The test does not detect a Kubernetes release that changes the wording of an
// admission text, or a new 403 from a server other than Kubernetes admission
// whose text begins with an admission text. The rows build their texts as the
// Kubernetes API server writes them, with its own constructor for the
// beginning of the message.
func TestAdmissionTextsGetTheirCodeAndEveryOther403StaysRBAC(t *testing.T) {
	t.Parallel()
	rows := admissionRows()

	ids := map[string]bool{}
	for _, e := range admissionTexts {
		assert.False(t, ids[e.id], "the id %s appears only once in the list", e.id)
		ids[e.id] = true
	}
	var listRetryable []string
	for _, e := range admissionTexts {
		if e.retryable {
			listRetryable = append(listRetryable, e.id)
		}
	}
	slices.Sort(listRetryable)
	assert.Equal(t, retryableAdmissionIDs, listRetryable, "the list holds exactly these four texts as retryable")
	covered := map[string]bool{}
	names := map[string]bool{}
	for _, r := range rows {
		assert.False(t, names[r.name], "row names are unique: %s", r.name)
		names[r.name] = true
		if r.id != "" {
			assert.True(t, ids[r.id], "the row %q names the id %s, which is not in the list", r.name, r.id)
			covered[r.id] = true
		}
	}
	for id := range ids {
		assert.True(t, covered[id], "the list entry %s has no row", id)
	}

	shapes := append(conflictShapes(),
		conflictShape{
			name: "named server",
			req:  &Request{Server: "https://rancher.example.com", Verb: "create", Group: "apps", Resource: "deployments", Cluster: "local"},
		},
		conflictShape{
			name: "unresolved cluster",
			req:  &Request{Verb: "create", Group: "apps", Resource: "deployments", Cluster: "c-x", Unresolved: true},
		})
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			// The classifier trims the message; the untrimmed text, which keeps its
			// trailing space when nothing follows the fixed words, is matched
			// here as well.
			for _, text := range []string{r.se.ErrStatus.Message, strings.TrimSpace(r.se.ErrStatus.Message)} {
				entry, matched := matchAdmissionText(text)
				assert.Equal(t, r.id != "", matched, "a row matches an entry only when it names an id: %q", text)
				if matched {
					assert.Equal(t, r.id, entry.id)
				}
			}
			for _, shape := range shapes {
				for _, path := range admissionPaths {
					if r.noJSON && path.name == "Rancher's error JSON" {
						continue
					}
					want := r.expectation(t, shape.req, path.show)
					label := shape.name + ", " + path.name
					ce, prefix := path.classify(r.se, shape.req)
					require.NotNil(t, ce, label)
					assert.Equal(t, want.code, ce.Code, label)
					assert.Equal(t, want.retryable, ce.Retryable, label)
					assert.Equal(t, want.exit, ce.ExitCode(), label)
					assert.Empty(t, ce.Hint, label)
					assert.Equal(t, prefix+want.message, ce.Message, label)
				}
			}
		})
	}
}

// TestAdmissionTextOnAnUnresolvedClusterKeepsTheServersText asserts a 403
// that is an admission text on a request to a cluster Rancher could not
// resolve is VALIDATION, or a retryable GENERIC for the four texts of a
// temporary 403, with the server's text and no sentence about the
// cluster, because the cluster's API server responded; a 403 that is not an
// admission text on the same request keeps the sentence.
func TestAdmissionTextOnAnUnresolvedClusterKeepsTheServersText(t *testing.T) {
	t.Parallel()
	req := &Request{Cluster: "c-x", Unresolved: true}
	matched := kubernetesForbidden("pods", "p", `maximum cpu usage per Container is 1, but limit is 2`)
	ce := FromStatus(matched, req)
	require.NotNil(t, ce)
	assert.Equal(t, CodeValidation, ce.Code)
	assert.Equal(t, matched.ErrStatus.Message, ce.Message)

	retryable := kubernetesForbidden("pods", "p", `not yet ready to handle request`)
	ce = FromStatus(retryable, req)
	require.NotNil(t, ce)
	assert.Equal(t, CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, retryable.ErrStatus.Message+" - retry later", ce.Message)

	other := kubernetesForbidden("pods", "p", `User "u" cannot list resource "pods" in API group "" at the cluster scope`)
	ce = FromStatus(other, req)
	require.NotNil(t, ce)
	assert.Equal(t, CodeRBAC, ce.Code)
	assert.Equal(t, other.ErrStatus.Message+unresolvedSentence, ce.Message)
}

// TestAdmissionTextInAJSONErrorObjectLongerThan200BytesIsRead asserts a 403
// whose Status message is a JSON error object holding an admission text longer
// than 200 bytes is VALIDATION when the error ends with the Status message,
// though the message the CLI shows is the object's message cut to 200 bytes.
func TestAdmissionTextInAJSONErrorObjectLongerThan200BytesIsRead(t *testing.T) {
	t.Parallel()
	text := `pods "web-7c9d8f6b5-abcde" is forbidden: exceeded quota: compute-resources, requested: limits.cpu=500m,limits.memory=512Mi,requests.cpu=250m,requests.memory=256Mi, used: limits.cpu=7,limits.memory=14Gi,requests.cpu=3500m,requests.memory=7Gi, limited: limits.cpu=8,limits.memory=16Gi,requests.cpu=4,requests.memory=8Gi`
	require.Greater(t, len(text), maxServerMessage)
	body, err := json.Marshal(map[string]any{"type": "error", "status": http.StatusForbidden, "code": "Forbidden", "message": text})
	require.NoError(t, err)
	se := handBuiltForbidden(string(body))
	ce := FromStatus(fmt.Errorf("listing the objects: %w", se), &Request{Verb: "create", Cluster: "local"})
	require.NotNil(t, ce)
	assert.Equal(t, CodeValidation, ce.Code)
	assert.Equal(t, "listing the objects: "+bounded(text), ce.Message)
}

// TestWebhookDenialOnAWriteWithAResourceVersionIsNotRetryable asserts a 409
// on a write that sends a resourceVersion gets the retryable CONFLICT with the
// message that the resource changed since it was last read, unless the
// server's text says an admission webhook denied the request: that is a
// CONFLICT that is not retryable with the webhook's message, for an update and
// a delete, typed and as a body that is not a Status. A text that is
// Kubernetes' resourceVersion conflict, a denial with no quoted webhook name,
// one with an empty name, one with no colon after `denied the request`, and
// one whose name ends after the first 200 bytes of the text keep the retryable
// message.
func TestWebhookDenialOnAWriteWithAResourceVersionIsNotRetryable(t *testing.T) {
	t.Parallel()
	const denied = `admission webhook "x.example.io" denied the request`
	const versionConflict = `Operation cannot be fulfilled on widgets.example.io "w-x": the object has been modified; please apply your changes to the latest version and try again`
	longName := strings.Repeat("a", 250)
	rows := []struct {
		name  string
		text  string
		stale bool
	}{
		{"a webhook denial with a message", denied + ": duplicate not allowed", false},
		{"a webhook denial without explanation", denied + " without explanation", false},
		{"Kubernetes' resourceVersion conflict", versionConflict, true},
		{"a denial with no quoted webhook name", `admission webhook denied the request: x`, true},
		{"a denial with no colon after the request", `admission webhook "x.example.io" denied the requestly`, true},
		{"a denial with an empty webhook name", `admission webhook "" denied the request: x`, true},
		// The CLI reads the first 200 bytes of the text: this denial is not read as one
		// and keeps the retryable CONFLICT message.
		{"a denial whose webhook name ends after the first 200 bytes", `admission webhook "` + longName + `" denied the request: x`, true},
	}
	for _, r := range rows {
		for _, verb := range []string{"update", "delete"} {
			method := http.MethodPut
			if verb == "delete" {
				method = http.MethodDelete
			}
			req := &Request{Verb: verb, Group: "example.io", Resource: "widgets", Name: "w-x", Cluster: "local", Precondition: true}
			typed := &apierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure, Reason: metav1.StatusReasonConflict, Code: http.StatusConflict, Message: r.text,
			}}
			for _, path := range []struct {
				name string
				err  error
			}{
				{"typed", typed},
				{"a body that is not a Status", ResponseError(method, http.StatusConflict, []byte(r.text))},
			} {
				t.Run(r.name+", "+verb+", "+path.name, func(t *testing.T) {
					t.Parallel()
					ce := FromStatus(path.err, req)
					require.NotNil(t, ce)
					assert.Equal(t, CodeConflict, ce.Code)
					if r.stale {
						assert.True(t, ce.Retryable)
						assert.Equal(t, staleWriteText, ce.Message)
						return
					}
					assert.False(t, ce.Retryable)
					assert.Equal(t, r.text, ce.Message)
				})
			}
		}
	}
}

// TestResponseErrorRecordsTheFullMessageOnlyForAForbiddenCutTo200Bytes asserts
// which causes ResponseError records. A response whose body is not a Status
// carries the unexpected-server-response cause and the cause that says the
// message was already taken from the body. A 403 whose message was cut to
// maxServerMessage bytes carries a third cause with the whole message, decoded
// from a JSON error object when the body is one. A 403 with a short message,
// and a 409 or a 500 with a long one, carry only the first two.
func TestResponseErrorRecordsTheFullMessageOnlyForAForbiddenCutTo200Bytes(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 300)
	object := func(message string) []byte {
		body, err := json.Marshal(map[string]any{"type": "error", "message": message})
		require.NoError(t, err)
		return body
	}
	base := []string{string(metav1.CauseTypeUnexpectedServerResponse), string(causeServerMessageTaken)}
	full := append(slices.Clone(base), string(causeServerMessageFull))
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
		causes []string
		whole  string
	}{
		{"a 403 with a long message", http.StatusForbidden, []byte(long), full, long},
		{"a 403 with a long message in a JSON error object", http.StatusForbidden, object(long), full, long},
		{"a 403 with a short message", http.StatusForbidden, []byte("no"), base, ""},
		{"a 409 with a long message", http.StatusConflict, []byte(long), base, ""},
		{"a 500 with a long message", http.StatusInternalServerError, []byte(long), base, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			se := ResponseError(http.MethodPut, tc.status, tc.body)
			var types []string
			var whole string
			for _, c := range se.ErrStatus.Details.Causes {
				types = append(types, string(c.Type))
				if c.Type == causeServerMessageFull {
					whole = c.Message
				}
			}
			slices.Sort(types)
			want := slices.Clone(tc.causes)
			slices.Sort(want)
			assert.Equal(t, want, types)
			assert.Equal(t, tc.whole, whole)
		})
	}
}
