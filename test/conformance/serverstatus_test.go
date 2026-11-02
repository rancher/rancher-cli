package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A server answer's HTTP status reaches the user through one table,
// clierr.FromStatus, which reads the status from a *apierrors.StatusError
// in the error's chain. The scan below rejects each source shape that
// keeps a status from that table: a function that reads a response's
// status without building the typed error (clierr.ResponseError or
// clierr.FromResponse), a second classifier over client-go's errors, an
// error type with a status field of its own, and a GENERIC error built
// from another error's text or a type assertion or type switch on
// *clierr.Error, each of which drops whatever status or code a wrapped
// error carried. The scan walks declared functions, and every
// package-level declaration (type, var and const), function literals held
// by package-level values included.
//
// Known gaps: a GENERIC error whose text holds another error behind a prefix
// ("x: "+err.Error(), fmt.Sprintf("x: %v", err)) is not flagged, under
// clierr.New or clierr.Retryable; the tree holds many such errors, made from
// local file, decoding and transport failures that carry no status. A
// response returned by a call and read at once (http.Do(req).Status) is not
// flagged either: without type information the scan cannot tell it from any
// other value with a Status field. The same limit leaves these shapes
// unflagged: a response read through a call result with no Body or Header
// read (resp, _ := c.Do(r), then resp.Status); a response held by value
// (var resp http.Response) or in a struct field (x.resp.Status); a response
// built in place (resp := &http.Response{}); a Kubernetes Status copied to a
// local first (st := se.Status(), then st.Code); and a type assertion on a
// named interface that declares a Status method.

const (
	clierrPath    = "github.com/rancher/rancher-cli/internal/clierr"
	apierrorsPath = "k8s.io/apimachinery/pkg/api/errors"
	metav1Path    = "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusAllowlist names the functions, as "<file>:<func>", that read a
// status and deliberately build no error from it, and the error
// types, as "<file>:<type>", that carry a status of their own; each value
// says why.
var statusAllowlist = map[string]string{
	"cmd/rancher-credential-vault/vault.go:do":               "a separate binary talking to Vault, not Rancher",
	"internal/runtime/completion/connection.go:GetJSON":      "completion is best effort; its errors never reach the pipeline",
	"internal/runtime/completion/projects.go:GetBody":        "completion is best effort; its errors never reach the pipeline",
	"internal/runtime/completion/projects.go:StatusError":    "completion is a leaf of the standard library and cannot import clierr",
	"internal/client/provider/review.go:aggregatedResources": "a non-200 only selects the legacy walk",
	"internal/engine/access/fanout.go:fanOutFailure":         "records a review's status beside the classified error it carries",
	"internal/engine/access/review.go:postReview":            "decides on ReviewResponse.Status, the typed status beside the classified Mapped error",
	"internal/token/directwait.go:probeDirect":               "the status selects a readiness probe outcome, never an error",
	"internal/token/identity.go:classifyCreate":              "the status selects a create outcome, never an error",
	"internal/token/revoke.go:revokeToken":                   "the status is recorded as text in a revoke outcome, never an error",
	"internal/token/revoke.go:postLogout":                    "returns the logout answer's status, which selects the revoke outcome, never an error",
	"internal/token/clustertoken.go:createV3ClusterToken":    "a 404 answers whether the server serves v3 cluster tokens",
	"internal/token/token.go:CreateDerivedToken":             "a 404 answers whether the server serves ext.cattle.io tokens",
	"internal/client/httpx/answered.go:RequestVersion":       "the status goes to the server facts rule, which stores an outcome per status, never an error",
	"internal/client/provider/installid.go:InstallIDFetch":   "the status goes to the server facts rule, which stores an outcome per status, never an error",
	"internal/cli/refresh.go:versionAnswer":                  "the status goes to the server facts rule, which stores an outcome per status, never an error",
	"internal/telemetrysend/request.go:sendOne":              "the status selects whether the sent events are kept or deleted, never an error",
}

// scanServerStatus returns one message per violation in f, at path.
func scanServerStatus(fset *token.FileSet, path string, f *ast.File) []string {
	return scanServerStatusWith(fset, path, f, statusAllowlist)
}

// scanServerStatusWith is scanServerStatus with the allowlist given.
func scanServerStatusWith(fset *token.FileSet, path string, f *ast.File, allow map[string]string) []string {
	var out []string
	report := func(pos token.Pos, msg string) {
		out = append(out, fset.Position(pos).String()+": "+msg)
	}
	names := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." && (p == apierrorsPath || p == clierrPath) {
			report(imp.Pos(), "dot import of "+p+" hides the status table from the scan")
		}
		names[name] = p
	}
	pkgOf := func(x ast.Expr) string {
		if id, ok := x.(*ast.Ident); ok {
			return names[id.Name]
		}
		return ""
	}
	inClierr := strings.HasPrefix(path, "internal/clierr/")

	// The error types of this file, for the carrier rule.
	errorTypes := map[string]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != "Error" || len(fd.Recv.List) != 1 {
			continue
		}
		t := fd.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if id, ok := t.(*ast.Ident); ok {
			errorTypes[id.Name] = true
		}
	}

	// scanNode applies the rules to one node: the body of a declared
	// function, or a package-level type, var or const value. fields are the
	// parameters and results of the function, none for a package-level node.
	// A function literal inside the node is scanned with it, once.
	scanNode := func(key string, fields []*ast.Field, body ast.Node) {
		var reads []token.Pos
		builds := false
		// Identifiers that are an *http.Response (declared so, or read for
		// a Body or Header field): their Status field is the status text.
		responses := map[string]bool{}
		declare := func(names []*ast.Ident, typ ast.Expr) {
			if !isHTTPResponse(typ, pkgOf) {
				return
			}
			for _, n := range names {
				responses[n.Name] = true
			}
		}
		for _, p := range fields {
			declare(p.Names, p.Type)
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				if e.Sel.Name == "Body" || e.Sel.Name == "Header" {
					if id, ok := e.X.(*ast.Ident); ok {
						responses[id.Name] = true
					}
				}
			case *ast.ValueSpec:
				declare(e.Names, e.Type)
			case *ast.FuncLit:
				for _, p := range slices.Concat(fieldList(e.Type.Params), fieldList(e.Type.Results)) {
					declare(p.Names, p.Type)
				}
			}
			return true
		})
		ast.Inspect(body, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				switch {
				case e.Sel.Name == "StatusCode":
					reads = append(reads, e.Pos())
				case e.Sel.Name == "Status" && isResponse(e.X, responses):
					reads = append(reads, e.Pos())
				case e.Sel.Name == "ErrStatus" && !inClierr:
					report(e.Pos(), "reads a Kubernetes Status outside clierr; classify with clierr.FromStatus")
				case e.Sel.Name == "APIStatus" && pkgOf(e.X) == apierrorsPath && !inClierr:
					report(e.Pos(), "reads a Kubernetes Status through APIStatus outside clierr; classify with clierr.FromStatus")
				case isStatusCall(e.X) && !inClierr:
					report(e.Pos(), "reads a Kubernetes Status field outside clierr; classify with clierr.FromStatus")
				case pkgOf(e.X) == metav1Path && strings.HasPrefix(e.Sel.Name, "StatusReason") && !inClierr:
					report(e.Pos(), "names a Kubernetes StatusReason outside clierr; classify with clierr.FromStatus")
				case pkgOf(e.X) == apierrorsPath && !inClierr &&
					(strings.HasPrefix(e.Sel.Name, "Is") || e.Sel.Name == "ReasonForError"):
					report(e.Pos(), "classifies a client-go error outside clierr; use clierr.FromStatus")
				}
			case *ast.TypeAssertExpr:
				if isClierrError(e.Type, pkgOf) {
					report(e.Pos(), "a type assertion misses a wrapped *clierr.Error; use errors.AsType or clierr.From")
				}
				if isStatusInterface(e.Type) && !inClierr {
					report(e.Pos(), "asserts a Status method outside clierr; classify with clierr.FromStatus")
				}
			case *ast.IndexExpr:
				if isAsType(e.X, pkgOf) && isStatusInterface(e.Index) && !inClierr {
					report(e.Pos(), "asks errors.AsType for a Status method outside clierr; classify with clierr.FromStatus")
				}
			case *ast.TypeSwitchStmt:
				for _, st := range e.Body.List {
					if cc, ok := st.(*ast.CaseClause); ok && slices.ContainsFunc(cc.List, func(t ast.Expr) bool { return isClierrError(t, pkgOf) }) {
						report(cc.Pos(), "a type switch misses a wrapped *clierr.Error; use errors.AsType or clierr.From")
					}
					if cc, ok := st.(*ast.CaseClause); ok && !inClierr && slices.ContainsFunc(cc.List, isStatusInterface) {
						report(cc.Pos(), "a type switch on a Status method outside clierr; classify with clierr.FromStatus")
					}
				}
			case *ast.CallExpr:
				sel, ok := e.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkgOf(sel.X) == clierrPath && (sel.Sel.Name == "ResponseError" || sel.Sel.Name == "FromResponse") {
					builds = true
				}
				if pkgOf(sel.X) == clierrPath && (sel.Sel.Name == "New" || sel.Sel.Name == "Retryable") && len(e.Args) == 2 &&
					isGenericCode(e.Args[0], pkgOf) && isErrorCall(e.Args[1]) {
					report(e.Pos(), "flattens an error that may carry a server status; use clierr.From")
				}
			}
			return true
		})
		if len(reads) > 0 && !builds && allow[key] == "" {
			report(reads[0], "reads a status and builds no clierr.ResponseError or clierr.FromResponse")
		}
	}

	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.GenDecl:
			for _, s := range x.Specs {
				switch sp := s.(type) {
				case *ast.TypeSpec:
					scanNode(path+":"+sp.Name.Name, nil, sp)
					st, ok := sp.Type.(*ast.StructType)
					if !ok || !errorTypes[sp.Name.Name] {
						continue
					}
					for _, fl := range st.Fields.List {
						id, ok := fl.Type.(*ast.Ident)
						if !ok || (id.Name != "int" && id.Name != "int32") {
							continue
						}
						for _, n := range fl.Names {
							if (n.Name == "Status" || n.Name == "StatusCode" || n.Name == "Code") &&
								allow[path+":"+sp.Name.Name] == "" {
								report(sp.Pos(), "error type "+sp.Name.Name+" has a status field of its own; return clierr.ResponseError")
							}
						}
					}
				case *ast.ValueSpec:
					// A package-level value, and a function literal inside
					// it, is scanned like a declared function, under the
					// name of the var it initializes.
					if sp.Type != nil && len(sp.Names) > 0 {
						scanNode(path+":"+sp.Names[0].Name, nil, sp.Type)
					}
					for i, v := range sp.Values {
						name := "_"
						if i < len(sp.Names) {
							name = sp.Names[i].Name
						}
						scanNode(path+":"+name, nil, v)
					}
				}
			}
		case *ast.FuncDecl:
			if x.Body != nil {
				scanNode(path+":"+x.Name.Name, slices.Concat(fieldList(x.Type.Params), fieldList(x.Type.Results)), x.Body)
			}
		}
	}
	return out
}

// fieldList returns the fields of l, or none when l is nil.
func fieldList(l *ast.FieldList) []*ast.Field {
	if l == nil {
		return nil
	}
	return l.List
}

// isHTTPResponse reports whether e is the type *http.Response.
func isHTTPResponse(e ast.Expr, pkgOf func(ast.Expr) string) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Response" && pkgOf(sel.X) == "net/http"
}

// isStatusCall reports whether e is a call of a Status method with no
// arguments, as in err.Status().Code.
func isStatusCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Status"
}

// isStatusInterface reports whether e is an anonymous interface type with a
// Status method.
func isStatusInterface(e ast.Expr) bool {
	it, ok := e.(*ast.InterfaceType)
	if !ok {
		return false
	}
	return slices.ContainsFunc(it.Methods.List, func(m *ast.Field) bool {
		_, isFunc := m.Type.(*ast.FuncType)
		return isFunc && slices.ContainsFunc(m.Names, func(n *ast.Ident) bool { return n.Name == "Status" })
	})
}

// isAsType reports whether e is errors.AsType.
func isAsType(e ast.Expr, pkgOf func(ast.Expr) string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "AsType" && pkgOf(sel.X) == "errors"
}

// isResponse reports whether e is an identifier read like an
// *http.Response.
func isResponse(e ast.Expr, responses map[string]bool) bool {
	id, ok := e.(*ast.Ident)
	return ok && responses[id.Name]
}

// isClierrError reports whether e is the type *clierr.Error.
func isClierrError(e ast.Expr, pkgOf func(ast.Expr) string) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Error" && pkgOf(sel.X) == clierrPath
}

// isGenericCode reports whether e is clierr.CodeGeneric.
func isGenericCode(e ast.Expr, pkgOf func(ast.Expr) string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "CodeGeneric" && pkgOf(sel.X) == clierrPath
}

// isErrorCall reports whether e is a call of a value's Error method.
func isErrorCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Error"
}

// TestServerStatusReachesTheOneTable asserts the production tree holds no
// path where a server status escapes clierr's table, and that every
// allowlist entry still names a function or type the scan would flag.
func TestServerStatusReachesTheOneTable(t *testing.T) {
	t.Parallel()
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var offenders []string
	for i, f := range tree.files {
		// internal/testrancher is the test-only fake and oracle of Rancher;
		// reading statuses is its job.
		if strings.HasPrefix(tree.paths[i], "internal/testrancher/") {
			continue
		}
		offenders = append(offenders, scanServerStatus(tree.fset, tree.paths[i], f)...)
	}
	assert.Empty(t, offenders)

	// An entry naming a function or type that no longer exists is stale.
	present := map[string]bool{}
	for i, f := range tree.files {
		maps.Copy(present, declaredKeys(tree.paths[i], f))
	}
	for _, k := range slices.Sorted(maps.Keys(statusAllowlist)) {
		assert.True(t, present[k], "allowlist entry %s names nothing", k)
	}
}

// declaredKeys returns "<path>:<name>" for each function, type, var and const
// declared at package level in f.
func declaredKeys(path string, f *ast.File) map[string]bool {
	keys := map[string]bool{}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			keys[path+":"+x.Name.Name] = true
		case *ast.GenDecl:
			for _, s := range x.Specs {
				switch sp := s.(type) {
				case *ast.TypeSpec:
					keys[path+":"+sp.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						keys[path+":"+n.Name] = true
					}
				}
			}
		}
	}
	return keys
}

// probeHead is the import block and the unused-import guard that the scan
// probes share. Nothing in it is a violation.
const probeHead = "package p\nimport (\n\t\"errors\"\n\t\"fmt\"\n\t\"net/http\"\n\t\"strconv\"\n\tkerrors \"k8s.io/apimachinery/pkg/api/errors\"\n\tmetav1 \"k8s.io/apimachinery/pkg/apis/meta/v1\"\n\t\"github.com/rancher/rancher-cli/internal/clierr\"\n)\n" +
	"var _, _, _, _, _, _, _ = errors.New, fmt.Errorf, http.StatusOK, strconv.Itoa, kerrors.NewNotFound, metav1.Now, clierr.New\n"

// probeScan parses probeHead followed by src and returns the scan's messages,
// with the allowlist given.
func probeScan(t *testing.T, src string, allow map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/probe/p.go", probeHead+src, parser.AllErrors)
	require.NoError(t, err)
	return scanServerStatusWith(fset, "internal/probe/p.go", f, allow)
}

// TestServerStatusScanProbeHeadIsClean asserts the shared probe head, and
// source that only looks like a violation, give no hit, so a probe that
// fails to flag proves its rule is missing.
func TestServerStatusScanProbeHeadIsClean(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"head alone":                        "",
		"interface with another method":     "func f(err error) bool { _, ok := err.(interface{ Timeout() bool }); return ok }",
		"AsType of another interface":       "func f(err error) bool { _, ok := errors.AsType[interface{ Timeout() bool }](err); return ok }",
		"type switch on another method":     "func f(err error) bool { switch err.(type) { case interface{ Timeout() bool }: return true }; return false }",
		"Status call with no selector":      "type reader interface{ Status() metav1.Status }\nfunc f(r reader) metav1.Status { return r.Status() }",
		"metav1 name without a reason":      "var now = metav1.Now()",
		"package-level literal that builds": "var g = func(resp *http.Response) error { return clierr.ResponseError(resp.Request.Method, resp.StatusCode, nil) }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, probeScan(t, src, statusAllowlist))
		})
	}
}

// TestServerStatusScanReportsEachViolationOnce asserts a function literal
// inside a package-level value is scanned once, not once per enclosing node.
func TestServerStatusScanReportsEachViolationOnce(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"literal in a var":           "var g = func(err error) bool { _, ok := err.(*clierr.Error); return ok }",
		"literal in a composite":     "var g = map[string]func(error) bool{\"a\": func(err error) bool { _, ok := err.(*clierr.Error); return ok }}",
		"literal in a call":          "var g = fmt.Sprint(func(err error) bool { _, ok := err.(*clierr.Error); return ok })",
		"nested literal":             "var g = func() func(error) bool { return func(err error) bool { _, ok := err.(*clierr.Error); return ok } }",
		"status reason in a literal": "var g = func(r metav1.Status) bool { return r.Reason == metav1.StatusReasonForbidden }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Len(t, probeScan(t, src, statusAllowlist), 1)
		})
	}
}

// TestServerStatusAllowlistNamesPackageLevelVars asserts a "<file>:<var>" key
// is found by the stale-entry check and exempts a package-level literal that
// reads a status.
func TestServerStatusAllowlistNamesPackageLevelVars(t *testing.T) {
	t.Parallel()
	const src = "var g = func(resp *http.Response) int { return resp.StatusCode }\nconst k = 1\ntype T struct{}\nfunc F() {}"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/probe/p.go", probeHead+src, parser.AllErrors)
	require.NoError(t, err)
	keys := declaredKeys("internal/probe/p.go", f)
	for _, k := range []string{"g", "k", "T", "F"} {
		assert.True(t, keys["internal/probe/p.go:"+k], k)
	}
	assert.False(t, keys["internal/probe/p.go:missing"])

	assert.NotEmpty(t, probeScan(t, src, nil))
	assert.Empty(t, probeScan(t, src, map[string]string{"internal/probe/p.go:g": "a reason"}))
}

// TestServerStatusScanFlagsEachShape asserts the scan flags each source
// shape that keeps a status from the table.
func TestServerStatusScanFlagsEachShape(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"status number in an Errorf":                "func f(resp *http.Response) error { return fmt.Errorf(\"answered %d\", resp.StatusCode) }",
		"status copied to a local first":            "func f(resp *http.Response) error { c := resp.StatusCode; return fmt.Errorf(\"answered %d\", c) }",
		"status text in errors.New":                 "func f(resp *http.Response) error { return errors.New(http.StatusText(resp.StatusCode)) }",
		"status in a clierr message":                "func f(r *http.Response) error { return clierr.New(clierr.CodeGeneric, fmt.Sprintf(\"%d\", r.StatusCode)) }",
		"switch on the status, no text":             "func f(resp *http.Response) error { switch resp.StatusCode { case 403: return errors.New(\"forbidden\") }; return nil }",
		"status read in a func literal":             "func f(do func() *http.Response) func() error { return func() error { return errors.New(strconv.Itoa(do().StatusCode)) } }",
		"own carrier type":                          "type statusErr struct{ Code int }\nfunc (statusErr) Error() string { return \"\" }",
		"own carrier, pointer receiver":             "type statusErr struct{ Status int }\nfunc (*statusErr) Error() string { return \"\" }",
		"second classifier, renamed":                "func f(err error) error { if kerrors.IsForbidden(err) { return clierr.New(clierr.CodeAuth, \"\") }; return nil }",
		"Status read off a client-go err":           "func f(err *kerrors.StatusError) int { return int(err.ErrStatus.Code) }",
		"GENERIC built from an error's text":        "func f(err error) error { return clierr.New(clierr.CodeGeneric, err.Error()) }",
		"dot import of the client-go errors":        "var _ = 0",
		"type assertion on a clierr error":          "func f(err error) bool { _, ok := err.(*clierr.Error); return ok }",
		"type switch on a clierr error":             "func f(err error) bool { switch err.(type) { case *clierr.Error: return true }; return false }",
		"status text of a response":                 "func f(resp *http.Response) error { defer resp.Body.Close(); return errors.New(resp.Status) }",
		"status text, no Body or Header read":       "func f(resp *http.Response) error { return errors.New(resp.Status) }",
		"status text of a declared response":        "func f() error { var resp *http.Response; return errors.New(resp.Status) }",
		"status read in a package-level literal":    "var g = func(resp *http.Response) error { return fmt.Errorf(\"%d\", resp.StatusCode) }",
		"type assertion in a package-level literal": "var h = func(err error) bool { _, ok := err.(*clierr.Error); return ok }",
		"APIStatus assertion":                       "func f(err error) bool { s, ok := err.(kerrors.APIStatus); return ok && s != nil }",
		"APIStatus as a type argument":              "func f(err error) bool { _, ok := errors.AsType[kerrors.APIStatus](err); return ok }",
		"Status().Code of a status reader":          "type reader interface{ Status() struct{ Code int32 } }\nfunc f(err reader) int32 { return err.Status().Code }",
		"retryable GENERIC from an error's text":    "func f(err error) error { return clierr.Retryable(clierr.CodeGeneric, err.Error()) }",
		"Status().Reason of a status reader":        "type reader interface{ Status() metav1.Status }\nfunc f(r reader) metav1.StatusReason { return r.Status().Reason }",
		"Status().Message of a status reader":       "type reader interface{ Status() metav1.Status }\nfunc f(r reader) string { return r.Status().Message }",
		"StatusReason compared":                     "func f(s metav1.Status) bool { return s.Reason == metav1.StatusReasonForbidden }",
		"StatusReason in a switch":                  "func f(s metav1.Status) int { switch s.Reason { case metav1.StatusReasonNotFound: return 1 }; return 0 }",
		"anonymous Status interface assertion":      "func f(err error) bool { _, ok := err.(interface{ Status() metav1.Status }); return ok }",
		"anonymous Status interface type switch":    "func f(err error) bool { switch err.(type) { case interface{ Status() metav1.Status }: return true }; return false }",
		"anonymous Status interface in AsType":      "func f(err error) bool { _, ok := errors.AsType[interface{ Status() metav1.Status }](err); return ok }",
		"package-level APIStatus alias":             "type apiStatus = kerrors.APIStatus",
		"package-level APIStatus definition":        "type apiStatus kerrors.APIStatus",
		"package-level APIStatus var type":          "var target kerrors.APIStatus",
		"package-level APIStatus alias used":        "type apiStatus = kerrors.APIStatus\nfunc f(err error) bool { _, ok := err.(apiStatus); return ok }",
		"package-level StatusReason value":          "var reason = metav1.StatusReasonForbidden",
		"package-level StatusReason const":          "const reason = metav1.StatusReasonForbidden",
		"package-level classifier value":            "var isGone = kerrors.IsGone",
		"package-level Status().Code value":         "var code = func(err interface{ Status() metav1.Status }) int32 { return err.Status().Code }",
		"package-level anonymous Status assertion":  "var ok = func(err error) bool { _, ok := err.(interface{ Status() metav1.Status }); return ok }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			full := probeHead + src
			if strings.HasPrefix(name, "dot import") {
				full = "package p\nimport . \"k8s.io/apimachinery/pkg/api/errors\"\nvar _ = IsNotFound\n" + src
			}
			f, err := parser.ParseFile(fset, "internal/probe/p.go", full, parser.AllErrors)
			require.NoError(t, err)
			assert.NotEmpty(t, scanServerStatus(fset, "internal/probe/p.go", f))
		})
	}
}
