package conformance

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lowercaseExtensionsAPI matches the term Rancher writes "Extensions API
// version" in its lowercase form: "extensions" with a lowercase e, then
// "API", on one line or with the line wrapped between the two words (a Go or
// shell comment marker may start the next line). The match is case-sensitive.
var lowercaseExtensionsAPI = regexp.MustCompile(`\bextensions(?:[ \t]+|\n[ \t]*(?://|#)?[ \t]*)API`)

// lowercaseExtensionsAPIUses returns "path:line" for each place in text that
// writes the term of Rancher's Extensions API version with a lowercase e.
// Line ends are normalized first, so a document checked out with CRLF line
// ends reads the same as one with LF.
func lowercaseExtensionsAPIUses(path, text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var uses []string
	for _, loc := range lowercaseExtensionsAPI.FindAllStringIndex(text, -1) {
		uses = append(uses, fmt.Sprintf("%s:%d", path, 1+strings.Count(text[:loc[0]], "\n")))
	}
	return uses
}

// TestExtensionsAPIVersionIsWrittenWithACapitalE asserts that no file of the
// repository writes the first word of the term with a lowercase e. Rancher
// writes the term "Extensions API version" with a capital E everywhere,
// mid-sentence too, and the code, the comments, the documents and the scripts
// match it. Identifiers such as ExtensionsAPIVersion hold no space and are
// not read.
func TestExtensionsAPIVersionIsWrittenWithACapitalE(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var uses []string
	for _, path := range files {
		raw, err := os.ReadFile(path)
		require.NoError(t, err, path)
		if bytes.IndexByte(raw, 0) >= 0 {
			continue
		}
		uses = append(uses, lowercaseExtensionsAPIUses(path, string(raw))...)
	}
	assert.Empty(t, uses, "write the term with a capital E: Extensions API version, as Rancher does")
}

// TestExtensionsAPIScanReportsALowercaseForm asserts the scan reports the
// lowercase form in a string, in a comment wrapped across two lines, in a
// shell comment and in a document with CRLF line ends, with the line of each,
// and reports neither the capital form nor an identifier.
func TestExtensionsAPIScanReportsALowercaseForm(t *testing.T) {
	t.Parallel()
	lower := "extensions" + " API"
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"a string", "x := \"the " + lower + " version\"\n", []string{"a.go:1"}},
		{"a wrapped Go comment", "// the extensions\n// API version\n", []string{"a.go:1"}},
		{"a wrapped shell comment", "x\n# the extensions\n# API version\n", []string{"a.go:2"}},
		{"a document with CRLF line ends", "one\r\ntwo\r\nthe " + lower + " version\r\n", []string{"a.go:3"}},
		{"two uses", lower + "\n" + lower + "\n", []string{"a.go:1", "a.go:2"}},
		{"the capital form", "the Extensions API version\n", nil},
		{"an identifier", "ExtensionsAPIVersion, extensionsApi, extensions-api\n", nil},
		{"another word before API", "the extensions of the API\n", nil},
	} {
		assert.Equal(t, tc.want, lowercaseExtensionsAPIUses("a.go", tc.text), tc.name)
	}
}
