package clierr

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusArm names the branch of the status table a Status reaches.
type statusArm string

const (
	armNotFound      statusArm = "notfound"
	armForbidden     statusArm = "forbidden"
	armUnauthorized  statusArm = "unauthorized"
	armConflict      statusArm = "conflict"
	armAlreadyExists statusArm = "alreadyexists"
	armValidation    statusArm = "validation"
	armRetryable     statusArm = "retryable"
	armGeneric       statusArm = "generic"
)

// reasonRow is the row of one Status reason: the HTTP code the API
// server gives it, the branch that code reaches, and the branch it reaches
// when a webhook returns it with code 409.
type reasonRow struct {
	code      int
	canonical statusArm
	at409     statusArm
}

// statusReasonRows holds a row for every StatusReason that apimachinery
// defines, plus the empty reason and one a webhook invents. A reason added by
// an apimachinery upgrade fails the test until it has a row here.
var statusReasonRows = map[string]reasonRow{
	"":                      {http.StatusInternalServerError, armGeneric, armConflict},
	"Unauthorized":          {http.StatusUnauthorized, armUnauthorized, armUnauthorized},
	"Forbidden":             {http.StatusForbidden, armForbidden, armForbidden},
	"NotFound":              {http.StatusNotFound, armNotFound, armNotFound},
	"AlreadyExists":         {http.StatusConflict, armAlreadyExists, armAlreadyExists},
	"Conflict":              {http.StatusConflict, armConflict, armConflict},
	"Gone":                  {http.StatusGone, armGeneric, armGeneric},
	"Invalid":               {http.StatusUnprocessableEntity, armValidation, armValidation},
	"ServerTimeout":         {http.StatusInternalServerError, armGeneric, armGeneric},
	"StorageReadError":      {http.StatusInternalServerError, armGeneric, armGeneric},
	"Timeout":               {http.StatusGatewayTimeout, armRetryable, armGeneric},
	"TooManyRequests":       {http.StatusTooManyRequests, armRetryable, armRetryable},
	"BadRequest":            {http.StatusBadRequest, armValidation, armValidation},
	"MethodNotAllowed":      {http.StatusMethodNotAllowed, armGeneric, armGeneric},
	"NotAcceptable":         {http.StatusNotAcceptable, armGeneric, armGeneric},
	"RequestEntityTooLarge": {http.StatusRequestEntityTooLarge, armGeneric, armGeneric},
	"UnsupportedMediaType":  {http.StatusUnsupportedMediaType, armGeneric, armGeneric},
	"InternalError":         {http.StatusInternalServerError, armGeneric, armGeneric},
	"Expired":               {http.StatusGone, armGeneric, armGeneric},
	"ServiceUnavailable":    {http.StatusServiceUnavailable, armRetryable, armRetryable},
	"Denied":                {http.StatusForbidden, armForbidden, armConflict},
}

// extraStatusReasons are the rows of statusReasonRows that apimachinery does
// not define.
var extraStatusReasons = []string{"Denied"}

// statusReasonsInSource returns every constant of type StatusReason declared
// in apimachinery's types.go, read from the module the build uses.
func statusReasonsInSource(t *testing.T) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "k8s.io/apimachinery")
	cmd.Env = testfs.EnvWithoutGit()
	out, err := cmd.Output()
	require.NoError(t, err, "find the apimachinery module directory")
	dir := strings.TrimSpace(string(out))
	require.NotEmpty(t, dir)
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, "pkg", "apis", "meta", "v1", "types.go"), nil, 0)
	require.NoError(t, err)
	var reasons []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "StatusReason" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				require.True(t, ok, "a StatusReason constant that is not a string literal")
				s, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				reasons = append(reasons, s)
			}
		}
	}
	return reasons
}

// conflictShape is one way to describe the call a Status responds to.
type conflictShape struct {
	name string
	req  *Request
}

// conflictShapes returns every verb the callers name, a call with no verb
// that went to a server (a Rancher action), and no request, each with and
// without a precondition. A nil request has no precondition.
func conflictShapes() []conflictShape {
	var shapes []conflictShape
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
		for _, pre := range []bool{false, true} {
			shapes = append(shapes, conflictShape{
				name: fmt.Sprintf("%s precondition=%v", verb, pre),
				req: &Request{
					Verb: verb, Group: "example.io", Resource: "widgets", Name: "w-x",
					Cluster: "local", Precondition: pre,
				},
			})
		}
	}
	for _, pre := range []bool{false, true} {
		shapes = append(shapes, conflictShape{
			name: fmt.Sprintf("action precondition=%v", pre),
			req:  &Request{Server: "https://rancher.example.com", Precondition: pre},
		})
	}
	return append(shapes, conflictShape{name: "nil request", req: nil})
}

func (s conflictShape) precondition() bool { return s.req != nil && s.req.Precondition }

// expectedArm is the Code and Retryable of an arm.
func expectedArm(arm statusArm) (code string, retryable bool) {
	switch arm {
	case armNotFound:
		return CodeNotFound, false
	case armForbidden:
		return CodeRBAC, false
	case armUnauthorized:
		return CodeAuth, false
	case armConflict, armAlreadyExists:
		return CodeConflict, false
	case armValidation:
		return CodeValidation, false
	case armRetryable:
		return CodeGeneric, true
	default:
		return CodeGeneric, false
	}
}

const (
	staleWriteText = "the resource changed on the server since it was last read; retry the command"
	webhookText    = `admission webhook "x.example.io" denied the request: duplicate not allowed`
	// resourceVersionConflictText is the message Kubernetes writes for a write
	// whose resourceVersion is out of date.
	resourceVersionConflictText = `Operation cannot be fulfilled on widgets.example.io "w-x": the object has been modified; please apply your changes to the latest version and try again`
)

// conflictMessage is one 409 message for the conflict table: its text, the
// part of it every result must keep, and whether it is an admission webhook's
// denial.
type conflictMessage struct {
	name    string
	text    string
	keeps   string
	webhook bool
}

// conflictMessages are the two messages a 409 carries: Kubernetes'
// resourceVersion conflict, and a webhook's denial.
var conflictMessages = []conflictMessage{
	{"resourceVersion conflict", resourceVersionConflictText, "the object has been modified", false},
	{"webhook denial", webhookText, "duplicate not allowed", true},
}

// TestEveryReasonVerbAndPreconditionClassifiesAConflictOnlyWhenItCarriedOne
// asserts a 409 whose message is Kubernetes' resourceVersion conflict gets
// the retryable message that the resource changed since it was last read only
// for a request that sent a resourceVersion precondition, and a 409 whose
// message is an admission webhook's denial never gets it: it is a CONFLICT
// that is not retryable and keeps the webhook's message whether or not the
// request sent a precondition. This holds for every StatusReason apimachinery
// defines, the reason a webhook invents and the empty reason, under the
// canonical code of each and under 409, for every verb and no request. Every
// other 409 is a CONFLICT that is not retryable and keeps the Status message.
// Every other row of the table ignores the precondition and whether the
// message is a webhook's denial; a webhook's denial with a 403 stays RBAC.
func TestEveryReasonVerbAndPreconditionClassifiesAConflictOnlyWhenItCarriedOne(t *testing.T) {
	t.Parallel()
	inSource := statusReasonsInSource(t)
	require.NotEmpty(t, inSource)
	for _, reason := range inSource {
		_, ok := statusReasonRows[reason]
		assert.True(t, ok, "apimachinery defines StatusReason %q: add its row to statusReasonRows", reason)
	}
	for reason := range statusReasonRows {
		assert.True(t, slices.Contains(inSource, reason) || slices.Contains(extraStatusReasons, reason),
			"statusReasonRows has a row for %q, which apimachinery does not define", reason)
	}
	require.Equal(t, len(inSource)+len(extraStatusReasons), len(statusReasonRows))

	shapes := conflictShapes()
	for reason, row := range statusReasonRows {
		type codeArm struct {
			code int
			arm  statusArm
		}
		codeArms := []codeArm{{row.code, row.canonical}}
		if row.code != http.StatusConflict {
			codeArms = append(codeArms, codeArm{http.StatusConflict, row.at409})
		}
		type conflictCase struct {
			codeArm
			msg conflictMessage
		}
		var cases []conflictCase
		for _, ca := range codeArms {
			for _, msg := range conflictMessages {
				cases = append(cases, conflictCase{ca, msg})
			}
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("reason=%q code=%d %s", reason, tc.code, tc.msg.name), func(t *testing.T) {
				t.Parallel()
				newErr := func() error {
					return &apierrors.StatusError{ErrStatus: metav1.Status{
						Status: metav1.StatusFailure, Reason: metav1.StatusReason(reason),
						Code: int32(tc.code), Message: tc.msg.text,
					}}
				}
				// A 409 is read as a resourceVersion race on a write that sent
				// one, unless the message is a webhook's denial.
				stale := tc.arm == armConflict && !tc.msg.webhook
				code, retryable := expectedArm(tc.arm)
				results := make(map[string]*Error, len(shapes))
				for _, s := range shapes {
					ce := FromStatus(newErr(), s.req)
					require.NotNil(t, ce, s.name)
					results[s.name] = ce
					switch {
					case stale && s.precondition():
						assert.Equal(t, CodeConflict, ce.Code, s.name)
						assert.True(t, ce.Retryable, s.name)
						assert.Equal(t, staleWriteText, ce.Message, s.name)
					case tc.arm == armConflict:
						assert.Equal(t, CodeConflict, ce.Code, s.name)
						assert.False(t, ce.Retryable, s.name)
						assert.Equal(t, tc.msg.text, ce.Message, s.name)
					default:
						assert.Equal(t, code, ce.Code, s.name)
						assert.Equal(t, retryable, ce.Retryable, s.name)
						assert.Contains(t, ce.Message, tc.msg.keeps, s.name)
					}
				}
				for _, s := range shapes {
					ce := results[s.name]
					if tc.arm == armConflict {
						// The result depends on the precondition alone: never on
						// the verb, and a nil request reads as none.
						base := results[fmt.Sprintf("get precondition=%v", s.precondition())]
						assert.Equal(t, base.Code, ce.Code, s.name)
						assert.Equal(t, base.Retryable, ce.Retryable, s.name)
						assert.Equal(t, base.Message, ce.Message, s.name)
						assert.Equal(t, base.Hint, ce.Hint, s.name)
						continue
					}
					base := results["get precondition=false"]
					assert.Equal(t, base.Code, ce.Code, s.name)
					assert.Equal(t, base.Retryable, ce.Retryable, s.name)
					if s.req != nil && s.req.Precondition {
						off := *s.req
						off.Precondition = false
						other := FromStatus(newErr(), &off)
						assert.Equal(t, other.Code, ce.Code, s.name)
						assert.Equal(t, other.Retryable, ce.Retryable, s.name)
						assert.Equal(t, other.Message, ce.Message, s.name)
						assert.Equal(t, other.Hint, ce.Hint, s.name)
					}
				}
				if stale {
					assert.NotEqual(t, results["get precondition=true"].Retryable, results["get precondition=false"].Retryable)
				}
			})
		}
	}
}
