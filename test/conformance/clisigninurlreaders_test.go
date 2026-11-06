package conformance

import (
	"go/scanner"
	"go/token"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signInURLParameterRE matches the name of a sign-in URL parameter that Go
// code reads, as a whole word inside a string literal: the literal alone, as
// in Query().Get("publicKey"), or part of a longer one, as in
// strings.Cut(raw, "publicKey=") or a regular expression. The name may follow
// a backslash and a letter, as in a regular expression starting with \b or a
// literal with \n before the name; Go's regexp has no lookbehind, so the left
// side lists the allowed characters.
var signInURLParameterRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_]|\\[A-Za-z])(requestId|publicKey)\b`)

// signInURLParameterAllowlist holds the files allowed to write those names in
// a string literal, each with the reason.
var signInURLParameterAllowlist = map[string]string{
	"test/conformance/clisigninurlreaders_test.go": "this scan and its snippets",
	"internal/testrancher/clisigninurl.go":         "ParseCLISignInURL, the one function that reads the parameters",
	"internal/testrancher/clisigninurl_test.go":    "the unit test of ParseCLISignInURL",
	"internal/token/token.go":                      "samlAuth, which writes the parameters into the sign-in URL",
}

// signInURLParameterLiterals returns one entry for each string literal of the
// Go source src that holds one of the parameter names, with its line. It
// splits src into tokens as the Go scanner does, so a raw string over several
// lines is found, and a name in a comment or an identifier is not. A name
// split across concatenated literals, as in "request" + "Id", is not found.
func signInURLParameterLiterals(path, src string) []string {
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, []byte(src), func(token.Position, string) {}, 0)
	var out []string
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.STRING && signInURLParameterRE.MatchString(lit) {
			out = append(out, path+":"+strconv.Itoa(fset.Position(pos).Line)+": "+strings.TrimSpace(lit))
		}
	}
}

// TestNoGoFileReadsTheSignInURLParametersItself asserts that no Go file reads
// the request id or public key of a sign-in URL except through
// testrancher.ParseCLISignInURL, which returns an error for a sign-in URL the
// dashboard sign-in page would refuse. Code that parses the sign-in URL on its
// own can pass a test with a sign-in URL the dashboard sign-in page would
// refuse. Non-test files are scanned too, because test helpers also live in
// the test-support packages.
func TestNoGoFileReadsTheSignInURLParametersItself(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var offenders []string
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if _, allowed := signInURLParameterAllowlist[path]; allowed {
			continue
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		offenders = append(offenders, signInURLParameterLiterals(path, string(data))...)
	}
	for _, o := range offenders {
		t.Errorf("read a sign-in URL with testrancher.ParseCLISignInURL, not by parameter name: %s", o)
	}
}

// TestSignInURLParameterAllowlistIsExact asserts the allowlist holds exactly
// the four expected files, each with a reason, and that every listed file
// exists.
func TestSignInURLParameterAllowlistIsExact(t *testing.T) {
	t.Parallel()
	want := []string{
		"test/conformance/clisigninurlreaders_test.go",
		"internal/testrancher/clisigninurl.go",
		"internal/testrancher/clisigninurl_test.go",
		"internal/token/token.go",
	}
	assert.ElementsMatch(t, want, slices.Collect(maps.Keys(signInURLParameterAllowlist)))
	for path, reason := range signInURLParameterAllowlist {
		assert.NotEmpty(t, reason, path)
		assert.FileExists(t, path)
	}
}

// TestSignInURLParameterScanFindsEverySpelling asserts the scan from source
// snippets: it finds each way to read the parameters except a name split
// across concatenated literals, reports each string literal once, and finds
// nothing in a comment or an identifier.
func TestSignInURLParameterScanFindsEverySpelling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"Query().Get of requestId", `id := u.Query().Get("requestId")`, 1},
		{"Query().Get of publicKey", `key := loginURL.Query().Get("publicKey")`, 1},
		{"both on one line", `onHandoff(u.Query().Get("requestId"), u.Query().Get("publicKey"))`, 2},
		{"a separate query variable", "q := u.Query()\nkey := q.Get(\"publicKey\")", 1},
		{"an index into the query map", `id := u.Query()["requestId"]`, 1},
		{"ParseQuery", "v, _ := url.ParseQuery(u.RawQuery)\nid := v.Get(\"requestId\")", 1},
		{"a backtick literal", "key := q.Get(`publicKey`)", 1},
		{"strings.Cut on the name and an equals sign", `_, rest, _ := strings.Cut(raw, "publicKey=")`, 1},
		{"a regular expression", "re := regexp.MustCompile(`publicKey=([^&]+)`)", 1},
		{"a regular expression starting with a word boundary", "re := regexp.MustCompile(`\\bpublicKey=([^&]+)`)", 1},
		{"a name after a newline escape", `x := "a\npublicKey="`, 1},
		{"a name after a tab escape", `x := "\trequestId"`, 1},
		{"a name inside a longer literal", `want := "requestId=r1"`, 1},
		{"a raw string over several lines", "re := `a\nb publicKey=x\nc`", 1},
		{"a name split across concatenated literals", `s := "request" + "Id"`, 0},
		{"two lines", "a := q.Get(\"requestId\")\nb := q.Get(\"publicKey\")", 2},
		{"the Go variable name", `requestID, publicKey := parse(u)`, 0},
		{"another parameter", `kind := u.Query().Get("responseType")`, 0},
		{"a longer word", `x := "myrequestIdentifier"`, 0},
		{"a comment", "// the requestId and publicKey of the sign-in URL\nx := 1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Len(t, signInURLParameterLiterals("x.go", tc.src), tc.want)
		})
	}
}
