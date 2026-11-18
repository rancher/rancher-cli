package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// scripts/admission-text-check reads the fixed words of every admission text
// of internal/clierr/admission.go from the words file below and looks for them
// in the Kubernetes modules. The tests in this file fail when that file has no
// row for a text of the list, or a row is wrong. They do not read the matchers'
// source: each variant of a row has a sample, the example message of one form
// of the text as the server writes it, and the tests run the samples through
// clierr.FromStatus.
//
// The limit: a listed word is shown to be needed, and the other words of a
// sample are shown not to be. A word a matcher needs that no sample shows (a
// rare form of a text) is found only by reading the matchers in
// internal/clierr/admission.go.

const (
	admissionListFile  = "internal/clierr/admission.go"
	admissionWordsFile = "test/conformance/testdata/admission-words.json"
)

// admissionReasonIDs are the ids whose row may have a reason in place of
// words: no code in the checked modules writes the text.
var admissionReasonIDs = []string{"limitrange-list"}

// admissionStartCase is one way the server begins the message of a 403.
type admissionStartCase struct {
	name     string
	resource string
	object   string
}

// admissionStartCases are the beginnings apierrors.NewForbidden writes: with a
// name, with no name, with a group-qualified resource and bare.
var admissionStartCases = []admissionStartCase{
	{"with a name", "pods", "p"},
	{"with no name", "pods", ""},
	{"with a group-qualified resource", "deployments.apps", "d"},
	{"with the bare beginning", "", ""},
}

type admissionWords struct {
	Modules []string             `json:"modules"`
	Texts   []admissionWordsText `json:"texts"`
}

type admissionWordsText struct {
	ID        string                 `json:"id"`
	Module    string                 `json:"module"`
	Dir       string                 `json:"dir"`
	Retryable bool                   `json:"retryable"`
	Variants  []admissionWordsSample `json:"variants"`
	Reason    string                 `json:"reason"`
}

type admissionWordsSample struct {
	Dir    string   `json:"dir"`
	Sample string   `json:"sample"`
	Words  []string `json:"words"`
}

func readAdmissionWords(t *testing.T) admissionWords {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(admissionWordsFile))
	require.NoError(t, err)
	var words admissionWords
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&words))
	return words
}

// admissionListIDs returns the id of every element of the admissionTexts
// literal of internal/clierr/admission.go, in order. It fails when the
// literal, or the id of an element, cannot be found.
func admissionListIDs(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.FromSlash(admissionListFile), nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var ids []string
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name.Name != "admissionTexts" || i >= len(spec.Values) {
				continue
			}
			list, ok := spec.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			found = true
			for _, element := range list.Elts {
				entry, ok := element.(*ast.CompositeLit)
				require.True(t, ok, "an element of admissionTexts is not a composite literal")
				id := ""
				for _, field := range entry.Elts {
					pair, ok := field.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := pair.Key.(*ast.Ident); !ok || key.Name != "id" {
						continue
					}
					lit, ok := pair.Value.(*ast.BasicLit)
					require.True(t, ok && lit.Kind == token.STRING, "the id of an element of admissionTexts is not a string literal")
					id, err = strconv.Unquote(lit.Value)
					require.NoError(t, err)
				}
				require.NotEmpty(t, id, "an element of admissionTexts has no id key")
				ids = append(ids, id)
			}
		}
		return true
	})
	require.True(t, found, "%s has no admissionTexts literal", admissionListFile)
	require.NotEmpty(t, ids, "the admissionTexts literal of %s has no element", admissionListFile)
	return ids
}

// validAdmissionDir reports whether dir is a non-empty relative path with no
// parent element.
func validAdmissionDir(dir string) bool {
	if dir == "" || path.IsAbs(dir) || filepath.IsAbs(dir) || strings.Contains(dir, `\`) {
		return false
	}
	return !slices.Contains(strings.Split(dir, "/"), "..")
}

// admissionForbidden is the 403 apierrors.NewForbidden builds with the text
// after `forbidden: `.
func admissionForbidden(start admissionStartCase, text string) *apierrors.StatusError {
	return apierrors.NewForbidden(schema.ParseGroupResource(start.resource), start.object, errors.New(text))
}

// admissionCode reports the code and retryability the CLI gives a 403 with the
// text after `forbidden: `.
func admissionCode(start admissionStartCase, text string) *clierr.Error {
	return clierr.FromStatus(admissionForbidden(start, text), &clierr.Request{Verb: "create", Resource: "pods"})
}

// wordPositions returns the offset of each word in the sample, found in order
// with no overlap, and false when a word is missing.
func wordPositions(sample string, words []string) ([]int, bool) {
	positions := make([]int, 0, len(words))
	from := 0
	for _, word := range words {
		i := strings.Index(sample[from:], word)
		if i < 0 {
			return nil, false
		}
		positions = append(positions, from+i)
		from += i + len(word)
	}
	return positions, true
}

// admissionVariants calls fn with every variant of every text that has words.
func admissionVariants(t *testing.T, fn func(t *testing.T, text admissionWordsText, v admissionWordsSample)) {
	t.Helper()
	for _, text := range readAdmissionWords(t).Texts {
		for i, v := range text.Variants {
			t.Run(text.ID+"/"+strconv.Itoa(i), func(t *testing.T) {
				fn(t, text, v)
			})
		}
	}
}

// TestAdmissionTextWordsCoverTheList asserts the words file has one row for
// each entry of the admissionTexts list of internal/clierr/admission.go and no
// other, with no id repeated on either side; that each row names a module of
// the file and a relative directory with no parent element; and that only the
// ids of admissionReasonIDs carry a reason in place of words. The list is read
// as source, so an entry added to it, or a list that moves or is renamed, fails
// here and not in the weekly check.
func TestAdmissionTextWordsCoverTheList(t *testing.T) {
	t.Parallel()
	words := readAdmissionWords(t)
	listIDs := admissionListIDs(t)

	seen := map[string]bool{}
	for _, id := range listIDs {
		assert.False(t, seen[id], "the id %s appears twice in the list", id)
		seen[id] = true
	}
	rows := map[string]bool{}
	for _, text := range words.Texts {
		assert.False(t, rows[text.ID], "the id %s has two rows in the words file", text.ID)
		rows[text.ID] = true
		assert.True(t, seen[text.ID], "the words file has a row for the id %s, which is not in the list", text.ID)
	}
	for _, id := range listIDs {
		assert.True(t, rows[id], "the list entry %s has no row in the words file", id)
	}

	for _, text := range words.Texts {
		assert.Contains(t, words.Modules, text.Module, "the module of the row %s", text.ID)
		assert.True(t, validAdmissionDir(text.Dir), "the directory %q of the row %s must be a relative path with no \"..\" element", text.Dir, text.ID)
		hasWords := len(text.Variants) > 0
		hasReason := text.Reason != ""
		assert.NotEqual(t, hasWords, hasReason, "the row %s needs \"variants\" or \"reason\", one of the two", text.ID)
		if hasReason {
			assert.Contains(t, admissionReasonIDs, text.ID, "the row %s has a reason in place of words, which only %v may", text.ID, admissionReasonIDs)
		}
		for i, v := range text.Variants {
			if v.Dir != "" {
				assert.True(t, validAdmissionDir(v.Dir), "the directory %q of variant %d of the row %s must be a relative path with no \"..\" element", v.Dir, i, text.ID)
			}
			assert.NotEmpty(t, v.Sample, "variant %d of the row %s has no sample", i, text.ID)
			assert.NotEmpty(t, v.Words, "variant %d of the row %s has no words", i, text.ID)
			for _, word := range v.Words {
				assert.NotEmpty(t, word, "variant %d of the row %s has an empty word", i, text.ID)
			}
		}
	}
	for _, id := range admissionReasonIDs {
		assert.True(t, rows[id], "the reason allowlist names %s, which has no row", id)
	}
}

// TestAdmissionTextWordsAppearInTheirSamples asserts the words of each variant
// occur in its sample, in order.
func TestAdmissionTextWordsAppearInTheirSamples(t *testing.T) {
	t.Parallel()
	admissionVariants(t, func(t *testing.T, text admissionWordsText, v admissionWordsSample) {
		t.Helper()
		_, ok := wordPositions(v.Sample, v.Words)
		assert.True(t, ok, "the words %q of %s do not occur in order in the sample %q", v.Words, text.ID, v.Sample)
	})
}

// TestAdmissionTextSamplesReportTheirErrorCode asserts the CLI reports each
// sample, with every beginning of the message that apierrors.NewForbidden
// writes, as VALIDATION, or as a retryable GENERIC when the row says it is
// retryable. The row's retryable value is read from the words file, never from
// the list.
func TestAdmissionTextSamplesReportTheirErrorCode(t *testing.T) {
	t.Parallel()
	admissionVariants(t, func(t *testing.T, text admissionWordsText, v admissionWordsSample) {
		t.Helper()
		for _, start := range admissionStartCases {
			ce := admissionCode(start, v.Sample)
			require.NotNil(t, ce, "%s %s", text.ID, start.name)
			if text.Retryable {
				assert.Equal(t, clierr.CodeGeneric, ce.Code, "%s %s must be a retryable GENERIC", text.ID, start.name)
				assert.True(t, ce.Retryable, "%s %s must be retryable", text.ID, start.name)
			} else {
				assert.Equal(t, clierr.CodeValidation, ce.Code, "%s %s must be VALIDATION", text.ID, start.name)
				assert.False(t, ce.Retryable, "%s %s must not be retryable", text.ID, start.name)
			}
		}
	})
}

// TestAdmissionTextEveryWordIsNeeded asserts that changing the first byte of
// any listed word in its sample makes the CLI report the 403 as RBAC. A word
// whose change still gives VALIDATION or GENERIC for the 403 is a word the
// matcher does not need, and the words file must not list it.
func TestAdmissionTextEveryWordIsNeeded(t *testing.T) {
	t.Parallel()
	admissionVariants(t, func(t *testing.T, text admissionWordsText, v admissionWordsSample) {
		t.Helper()
		positions, ok := wordPositions(v.Sample, v.Words)
		require.True(t, ok, "the words of %s do not occur in order in the sample", text.ID)
		for i, word := range v.Words {
			altered := []byte(v.Sample)
			require.NotEqual(t, byte('Z'), altered[positions[i]], "the word %q of %s begins with Z", word, text.ID)
			altered[positions[i]] = 'Z'
			ce := admissionCode(admissionStartCases[0], string(altered))
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeRBAC, ce.Code, "the word %q of %s is not needed: %q is still reported as %s", word, text.ID, string(altered), ce.Code)
		}
	})
}

// admissionUnlistedTokens are the tokens of a sample, outside every listed
// word, whose change legitimately breaks the match: the matcher reads the
// token's form and not its words.
var admissionUnlistedTokens = []struct{ id, token, reason string }{
	{"podsecurity", `"restricted:latest"`, "the matcher reads the quoted level and version, so the quote is part of the form"},
	{"serviceaccount-not-found", "serviceaccount", "k8s.io/apimachinery writes these words, which are not in a checked module"},
	{"serviceaccount-not-found", `"sa1"`, "k8s.io/apimachinery writes the quoted name, which is not in a checked module"},
	{"serviceaccount-not-found", "not", "k8s.io/apimachinery writes these words, which are not in a checked module"},
	{"serviceaccount-not-found", "found", "k8s.io/apimachinery writes these words, which are not in a checked module"},
	{"runtimeclass-not-found", `"gvisor"`, "the matcher reads the quoted name; the quotes belong to the text it matches"},
	{"runtimeclass-node-selector-conflict", "disktype", "the matcher needs the same key on both sides"},
	{"csr-system-masters", "kubernetes.io/kube-apiserver-client", "a constant of another module gives the signer name, which is not in a checked module"},
}

// TestAdmissionTextNoOtherWordIsNeeded asserts that changing the first byte of
// any token of a sample that lies outside every listed word leaves the code
// the sample has. A change that alters the code means the matcher reads that
// token and the words file does not list it. The tokens in
// admissionUnlistedTokens are the only exceptions.
func TestAdmissionTextNoOtherWordIsNeeded(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	admissionVariants(t, func(t *testing.T, text admissionWordsText, v admissionWordsSample) {
		t.Helper()
		positions, ok := wordPositions(v.Sample, v.Words)
		require.True(t, ok, "the words of %s do not occur in order in the sample", text.ID)
		covered := make([]bool, len(v.Sample))
		for i, word := range v.Words {
			for j := positions[i]; j < positions[i]+len(word); j++ {
				covered[j] = true
			}
		}
		want := admissionCode(admissionStartCases[0], v.Sample)
		require.NotNil(t, want)
		for start := 0; start < len(v.Sample); start++ {
			if covered[start] || v.Sample[start] == ' ' || (start > 0 && !covered[start-1] && v.Sample[start-1] != ' ') {
				continue
			}
			end := start
			for end < len(v.Sample) && !covered[end] && v.Sample[end] != ' ' {
				end++
			}
			token := v.Sample[start:end]
			altered := []byte(v.Sample)
			altered[start] = 'Z'
			if v.Sample[start] == 'Z' {
				altered[start] = 'Y'
			}
			ce := admissionCode(admissionStartCases[0], string(altered))
			require.NotNil(t, ce)
			if ce.Code == want.Code && ce.Retryable == want.Retryable {
				continue
			}
			allowed := false
			for _, exception := range admissionUnlistedTokens {
				if exception.id == text.ID && exception.token == token {
					allowed = true
					used[exception.id+" "+exception.token] = true
				}
			}
			assert.True(t, allowed, "the token %q of %s is read by the matcher and is not listed: changing it makes %q a %s", token, text.ID, string(altered), ce.Code)
		}
	})
	if !t.Failed() {
		for _, exception := range admissionUnlistedTokens {
			assert.True(t, used[exception.id+" "+exception.token], "the exception %q of %s (%s) is never needed", exception.token, exception.id, exception.reason)
		}
	}
}
