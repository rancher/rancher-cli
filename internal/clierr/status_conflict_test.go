package clierr

import (
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// storageResourceVersionText is the message of the resourceVersion
	// precondition the storage layer checks on an update or a delete: the
	// storage error text wraps it.
	storageResourceVersionText = `Operation cannot be fulfilled on widgets.example.io "w-x": StorageError: invalid object, Code: 4, Key: /registry/example.io/widgets/default/w-x, ResourceVersion: 0, AdditionalErrorMsg: Precondition failed: ResourceVersion in precondition: 41, ResourceVersion in object meta: 42`
	// storageUIDText is the message of the uid precondition the storage layer
	// checks on an update whose body has a different uid from the stored
	// object's. Its words ", UID in object meta: " start past the 200 bytes
	// the CLI cuts a message to.
	storageUIDText = `Operation cannot be fulfilled on widgets.example.io "w-x": StorageError: invalid object, Code: 4, Key: /registry/example.io/widgets/default/w-x, ResourceVersion: 0, AdditionalErrorMsg: Precondition failed: UID in precondition: 00000000-0000-0000-0000-000000000000, UID in object meta: 119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f`
	// deleteResourceVersionText and deleteUIDText are the messages of the
	// preconditions a delete request carries.
	deleteResourceVersionText = `Operation cannot be fulfilled on widgets.example.io "w-x": the ResourceVersion in the precondition (41) does not match the ResourceVersion in record (42). The object might have been modified`
	deleteUIDText             = `Operation cannot be fulfilled on widgets.example.io "w-x": the UID in the precondition (00000000-0000-0000-0000-000000000000) does not match the UID in record (119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f). The object might have been deleted and then recreated`
	// uidMismatchText is the message of a patch that names a uid when no
	// object exists.
	uidMismatchText = `Operation cannot be fulfilled on widgets.example.io "w-x": uid mismatch: the provided object specified uid 00000000-0000-0000-0000-000000000000, and no existing object was found`
	// namespaceUIDText is the message of the uid precondition a namespace
	// delete checks itself (k8s.io/kubernetes pkg/registry/core/namespace/storage/storage.go:159);
	// the update of the scale of a ReplicationController, ReplicaSet, Deployment
	// and StatefulSet writes the same sentence (scaleUIDText; for example
	// pkg/registry/apps/deployment/storage/storage.go:467). crdUIDText and
	// crdResourceVersionText are the preconditions of a CustomResourceDefinition
	// delete, which write the sentence with a lower-case "precondition failed: ".
	namespaceUIDText       = `Operation cannot be fulfilled on widgets.example.io "w-x": Precondition failed: UID in precondition: 00000000-0000-0000-0000-000000000000, UID in object meta: 119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f`
	crdUIDText             = `Operation cannot be fulfilled on widgets.example.io "w-x": precondition failed: UID in precondition: 00000000-0000-0000-0000-000000000000, UID in object meta: 119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f`
	crdResourceVersionText = `Operation cannot be fulfilled on widgets.example.io "w-x": precondition failed: ResourceVersion in precondition: 41, ResourceVersion in object meta: 42`
	scaleUIDText           = `Operation cannot be fulfilled on widgets.example.io "w-x": Precondition failed: UID in precondition: 00000000-0000-0000-0000-000000000000, UID in object meta: 119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f`
	// evictionUIDText is the message of the uid precondition a pod eviction
	// checks (k8s.io/kubernetes pkg/registry/core/pod/storage/eviction.go:329):
	// the sentence of the delete uid precondition.
	evictionUIDText = `Operation cannot be fulfilled on widgets.example.io "w-x": the UID in the precondition (00000000-0000-0000-0000-000000000000) does not match the UID in record (119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f). The object might have been deleted and then recreated`
	// rancherUIDText and rancherResourceVersionText are the messages the
	// kubeconfig store of Rancher's ext API writes for a delete precondition:
	// the sentences of the API server without their last sentence.
	rancherUIDText             = `Operation cannot be fulfilled on widgets.example.io "w-x": the UID in the precondition (00000000-0000-0000-0000-000000000000) does not match the UID in record (119d52bc-3f5e-4c1a-9a0e-7b6f1c2d3e4f)`
	rancherResourceVersionText = `Operation cannot be fulfilled on widgets.example.io "w-x": the ResourceVersion in the precondition (41) does not match the ResourceVersion in record (42)`
)

// conflictMessage is one 409 message for the conflict table: its text, the
// part of it every result must keep, and whether it is a resourceVersion race.
// A 409 that is a race gets the retryable message on a request that sent a
// resourceVersion. A webhook's denial and a uid conflict are not races: no
// fresh read resolves them, and they never get the retryable message.
type conflictMessage struct {
	name  string
	text  string
	keeps string
	race  bool
}

// conflictMessages are the messages a 409 carries on an update or a delete:
// every resourceVersion race text, each of which a fresh read-modify-write
// resolves, and every text that says the uid sent with the request is
// different from the object's or that a webhook denied the request.
var conflictMessages = []conflictMessage{
	{"resourceVersion conflict", resourceVersionConflictText, "the object has been modified", true},
	{"storage resourceVersion precondition", storageResourceVersionText, "StorageError: invalid object", true},
	{"delete resourceVersion precondition", deleteResourceVersionText, "the ResourceVersion in the precondition", true},
	{"Rancher delete resourceVersion precondition", rancherResourceVersionText, "the ResourceVersion in the precondition", true},
	{"CRD delete resourceVersion precondition", crdResourceVersionText, "ResourceVersion in precondition", true},
	{"webhook denial", webhookText, "duplicate not allowed", false},
	{"storage uid precondition", storageUIDText, "StorageError: invalid object", false},
	{"delete uid precondition", deleteUIDText, "the UID in the precondition", false},
	{"namespace delete uid precondition", namespaceUIDText, "UID in precondition", false},
	{"CRD delete uid precondition", crdUIDText, "UID in precondition", false},
	{"Rancher delete uid precondition", rancherUIDText, "the UID in the precondition", false},
	{"patch uid mismatch", uidMismatchText, "uid mismatch", false},
	{"scale update uid precondition", scaleUIDText, "UID in precondition", false},
	{"eviction uid precondition", evictionUIDText, "the UID in the precondition", false},
}

// TestEveryReasonVerbAndPreconditionClassifiesAConflictOnlyWhenItCarriedOne
// asserts a 409 whose message is a resourceVersion race text of Kubernetes
// gets the retryable message that the resource changed since it was last read
// only for a request that sent a resourceVersion precondition. A 409 whose
// message is an admission webhook's denial, or says the uid sent with the
// request is different from the object's, never gets that message. It is a
// CONFLICT that is not retryable and keeps the server's message, whether or
// not the request sent a precondition.
//
// The table runs every StatusReason apimachinery defines, the reason a webhook
// invents and the empty reason, under the canonical code of each and under
// 409, for every verb and no request. Every other 409 is a CONFLICT that is
// not retryable and keeps the Status message. Where the reason and the code
// reach another arm of the status table, the result ignores the precondition
// and the message; a webhook's denial with a 403 stays RBAC.
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
				// one, when the message is a resourceVersion race.
				stale := tc.arm == armConflict && tc.msg.race
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

// TestConflictMessagesAreTheTextsKubernetesWrites asserts each fixture of
// conflictMessages that Kubernetes writes through apierrors.NewConflict equals
// what apierrors.NewConflict builds from the text after the fixture's prefix,
// and that a storage text is longer than the 200 bytes the CLI cuts a message
// to. The words that tell a uid precondition from a resourceVersion race lie
// past that cut, and a match that reads the cut text misses them.
func TestConflictMessagesAreTheTextsKubernetesWrites(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: "example.io", Resource: "widgets"}
	const prefix = `Operation cannot be fulfilled on widgets.example.io "w-x": `
	for _, m := range conflictMessages {
		if m.text == webhookText {
			continue
		}
		inner, ok := strings.CutPrefix(m.text, prefix)
		require.True(t, ok, m.name)
		assert.Equal(t, m.text, apierrors.NewConflict(gr, "w-x", errors.New(inner)).ErrStatus.Message, m.name)
		if strings.HasPrefix(inner, "StorageError: ") {
			assert.Greater(t, len(m.text), maxServerMessage, m.name)
		}
	}
}

// TestEveryOtherConflictShapeIsClassified asserts the 409s whose body is
// empty, is not a Status or is AlreadyExists, and a Status whose message is a
// JSON error object. A 409 with an empty body, or a body that is not a Status
// (Rancher's facades send these), is a resourceVersion race on a write that
// sent a resourceVersion: retryable, with the stale write text. Such a 409 is a
// CONFLICT that is not retryable, with the server's message, on a write that
// sent none. AlreadyExists is a CONFLICT that is not retryable in both.
//
// A Status whose message is a JSON error object with the text of Kubernetes is
// read whole: the uid words are found in it although the message shown is cut
// to 200 bytes. A body that is not a Status, a JSON error object or plain text,
// keeps its message cut: a uid text whose words end within its first 200 bytes
// is not a race, and one whose uid words start past the cut is still read as a
// race.
func TestEveryOtherConflictShapeIsClassified(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: "example.io", Resource: "widgets"}
	jsonObject := func(text string) error {
		body, err := json.Marshal(map[string]any{"type": "error", "code": "Conflict", "message": text})
		require.NoError(t, err)
		return &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Reason: metav1.StatusReasonConflict,
			Code: http.StatusConflict, Message: string(body),
		}}
	}
	bareBody := func(body string) func() error {
		return func() error { return ResponseError(http.MethodPut, http.StatusConflict, []byte(body)) }
	}
	bareObject := func(text string) string {
		body, err := json.Marshal(map[string]any{"type": "error", "code": "Conflict", "message": text})
		require.NoError(t, err)
		return string(body)
	}
	require.LessOrEqual(t, len(rancherUIDText), maxServerMessage)
	require.Greater(t, len(storageUIDText), maxServerMessage)
	rows := []struct {
		name      string
		err       func() error
		retryable bool
		message   string
	}{
		{"empty body", func() error {
			return apierrors.NewGenericServerResponse(http.StatusConflict, http.MethodPut, gr, "w-x", "", 0, true)
		}, true, "Conflict"},
		{"body that is not a Status", func() error {
			return apierrors.NewGenericServerResponse(http.StatusConflict, http.MethodPut, gr, "w-x", "<html>conflict</html>", 0, true)
		}, true, "<html>conflict</html>"},
		{"already exists", func() error { return apierrors.NewAlreadyExists(gr, "w-x") }, false, `widgets.example.io "w-x" already exists`},
		{"JSON error object with a resourceVersion race text", func() error { return jsonObject(resourceVersionConflictText) }, true, "the object has been modified"},
		{"JSON error object with a uid precondition text", func() error { return jsonObject(storageUIDText) }, false, "StorageError: invalid object"},
		{"JSON error object body with a short uid text", bareBody(bareObject(rancherUIDText)), false, "the UID in the precondition"},
		{"text body with a short uid text", bareBody(rancherUIDText), false, "the UID in the precondition"},
		{"JSON error object body with a long uid text", bareBody(bareObject(storageUIDText)), true, ""},
		{"text body with a long uid text", bareBody(storageUIDText), true, ""},
		{"text body with a resourceVersion race text", bareBody(resourceVersionConflictText), true, ""},
	}
	for _, row := range rows {
		for _, pre := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s precondition=%v", row.name, pre), func(t *testing.T) {
				t.Parallel()
				req := &Request{Verb: "update", Group: "example.io", Resource: "widgets", Name: "w-x", Cluster: "local", Precondition: pre}
				ce := FromStatus(row.err(), req)
				require.NotNil(t, ce)
				assert.Equal(t, CodeConflict, ce.Code)
				if row.retryable && pre {
					assert.True(t, ce.Retryable)
					assert.Equal(t, staleWriteText, ce.Message)
					return
				}
				assert.False(t, ce.Retryable)
				assert.Contains(t, ce.Message, row.message)
			})
		}
	}
}
