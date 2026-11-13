package conformance

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonicalServerURLFiles are the production files that call
// credhelper.CanonicalServerURL, with why each is safe: the function drops
// userinfo and a path, so it never decides whether a URL is accepted to
// connect to.
var canonicalServerURLFiles = map[string]string{
	"internal/cli/record.go":            "telemetry records the host of a server; no connection",
	"internal/config/credstore.go":      "the identity of a server for credential records; no connection",
	"internal/config/credstore_exec.go": "the exec helper's request carries the identity of a server; no connection",
	"internal/config/resolve.go":        "compares servers and reads the identity of the variable's URL after the check of the connection's URL",
}

// TestCanonicalServerURLIsCalledOnlyFromTheListedFiles asserts that every
// production use of credhelper.CanonicalServerURL is in a file of
// canonicalServerURLFiles. CanonicalServerURL accepts a URL with userinfo or
// an @ after the host, so a path that accepts a server URL to connect to
// calls credhelper.CheckServerURL instead; a new use of the lenient function
// needs a reviewed entry and a reason.
func TestCanonicalServerURLIsCalledOnlyFromTheListedFiles(t *testing.T) {
	t.Parallel()
	pkgs, err := fieldManagerPackages()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	var files []string
	for _, pkg := range pkgs {
		if strings.HasPrefix(pkg.PkgPath, modulePrefix+"internal/testrancher") {
			continue
		}
		for _, m := range serverURLRoutes(pkg, wd) {
			if strings.HasSuffix(m.text, ": credhelper.CanonicalServerURL") {
				file, _, _ := strings.Cut(m.key, ":")
				files = append(files, file)
			}
		}
	}
	require.NotEmpty(t, files, "the scan finds the uses of CanonicalServerURL")
	for _, file := range slices.Compact(slices.Sorted(slices.Values(files))) {
		_, listed := canonicalServerURLFiles[file]
		assert.True(t, listed, "%s calls credhelper.CanonicalServerURL and is not in the list of files that may", file)
	}
	for file, why := range canonicalServerURLFiles {
		assert.NotEmpty(t, why, file)
		assert.Contains(t, files, file, "%s is listed and no longer calls credhelper.CanonicalServerURL", file)
	}
}
