package conformance

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The packages that record usage events and set file access lists read no
// credential and open no connection. Each package has a closed list of
// direct imports, and the full import closure of each, computed for every
// platform the release builds, holds no HTTP, TLS, client, configuration,
// token or keychain package. The standard library's net package, which
// golang.org/x/sys/windows pulls in, is tolerated only when reached through
// that package.

const (
	telemetryDir     = "internal/telemetry"
	telemetrysendDir = "internal/telemetrysend"
	fsaclDir         = "internal/fsacl"
	windowsSys       = "golang.org/x/sys/windows"
)

// bannedImports are the packages no closure may hold: full import paths and
// path prefixes (a prefix ends in "/").
var bannedImports = []string{
	"net/http", "crypto/tls",
	modulePrefix + "internal/client/",
	modulePrefix + "internal/config",
	modulePrefix + "internal/token",
	modulePrefix + "credhelper",
	"github.com/zalando/go-keyring",
}

// directRule is the closed list of direct imports of one package.
type directRule struct {
	// standard says whether a standard-library path is allowed.
	standard func(path string) bool
	// others are the allowed non-standard import paths.
	others []string
}

func isStandard(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func isNetworkPath(path string) bool {
	return path == "net" || strings.HasPrefix(path, "net/") || path == "crypto/tls"
}

var directRules = map[string]directRule{
	// The check of the experiment endpoint and the encoder of stored events
	// make no request and start no process: the standard library without the
	// network packages and os/exec, except net/url and net/netip, which parse
	// the value, credhelper's URL check, and internal/telemetry, whose stored
	// events and attribute metadata the encoder reads. internal/telemetry
	// never imports internal/telemetrysend.
	telemetrysendDir: {
		standard: func(p string) bool {
			if p == "net/url" || p == "net/netip" {
				return true
			}
			return !isNetworkPath(p) && p != "os/exec"
		},
		others: []string{modulePrefix + "credhelper", modulePrefix + "internal/telemetry"},
	},
	telemetryDir: {
		standard: func(p string) bool { return !isNetworkPath(p) },
		others:   []string{modulePrefix + "internal/invocation", modulePrefix + "internal/fsacl"},
	},
	fsaclDir: {
		standard: func(string) bool { return true },
		others:   []string{windowsSys},
	},
	// The telemetry commands make no request and read no credential: the
	// standard library without network packages, the error type, the config
	// package (its use is limited by TestTelemetryCommandsUseOnlyTheAllowedConfigNames),
	// the command runtime and the telemetry package.
	telemetrycmdDir: {
		standard: func(p string) bool { return !isNetworkPath(p) },
		others: []string{
			modulePrefix + "internal/clierr", modulePrefix + "internal/config",
			modulePrefix + "internal/runtime", modulePrefix + "internal/telemetry",
		},
	},
}

// directImportFindings parses every Go file of dir, whatever its build
// constraint, tests excluded, and returns one line per import the rule does
// not allow.
func directImportFindings(t *testing.T, dir string, rule directRule) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			ok := slices.Contains(rule.others, p)
			if isStandard(p) {
				ok = rule.standard(p)
			}
			if !ok {
				out = append(out, filepath.ToSlash(filepath.Join(dir, n))+" imports "+p)
			}
		}
	}
	slices.Sort(out)
	return out
}

func isBanned(path string) bool {
	for _, b := range bannedImports {
		if path == b || (strings.HasSuffix(b, "/") && strings.HasPrefix(path, b)) || (!strings.HasSuffix(b, "/") && strings.HasPrefix(path, b+"/")) {
			return true
		}
	}
	return false
}

func isNetFamily(path string) bool { return path == "net" || strings.HasPrefix(path, "net/") }

// closureFindings loads the packages at patterns for one platform and
// returns one line per banned package in their import closure, and per
// package outside the net family and golang.org/x/sys/windows that imports
// a net package.
func closureFindings(t *testing.T, goos, goarch string, patterns ...string) []string {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps,
		Env:  append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0"),
	}
	roots, err := packages.Load(cfg, patterns...)
	require.NoError(t, err)
	var out []string
	seen := map[string]bool{}
	var walk func(p *packages.Package)
	walk = func(p *packages.Package) {
		if seen[p.PkgPath] {
			return
		}
		seen[p.PkgPath] = true
		for _, e := range p.Errors {
			out = append(out, goos+"/"+goarch+": "+p.PkgPath+": "+e.Msg)
		}
		if isBanned(p.PkgPath) {
			out = append(out, goos+"/"+goarch+": closure holds "+p.PkgPath)
		}
		for path, imp := range p.Imports {
			if isNetFamily(path) && !isNetFamily(p.PkgPath) && p.PkgPath != windowsSys {
				out = append(out, goos+"/"+goarch+": "+p.PkgPath+" imports "+path)
			}
			walk(imp)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestTelemetryDirectImportsAreClosed asserts internal/telemetry imports the
// standard library without network packages, internal/invocation and
// internal/fsacl, internal/fsacl imports the standard library and
// golang.org/x/sys/windows, and the telemetry commands import the standard
// library without network packages, internal/clierr, internal/config,
// internal/runtime and internal/telemetry, in every file whatever its build
// constraint.
func TestTelemetryDirectImportsAreClosed(t *testing.T) {
	for dir, rule := range directRules {
		assert.Empty(t, directImportFindings(t, dir, rule), dir)
	}
}

// TestDirectImportCheckCatchesFixtures asserts the check reports a network
// package of the standard library and a module package outside the list.
func TestDirectImportCheckCatchesFixtures(t *testing.T) {
	rule := directRules[telemetryDir]
	got := directImportFindings(t, "test/conformance/testdata/importguard/netdirect", rule)
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0], "netdirect.go imports net"), got)
	got = directImportFindings(t, "test/conformance/testdata/importguard/config", rule)
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0], "imports "+modulePrefix+"internal/config"), got)
	assert.Empty(t, directImportFindings(t, "test/conformance/testdata/importguard/clean", rule))

	got = directImportFindings(t, "test/conformance/testdata/importguard/httpx", directRules[telemetrycmdDir])
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0], "imports "+modulePrefix+"internal/client/httpx"), got)
	assert.Empty(t, directImportFindings(t, "test/conformance/testdata/importguard/clean", directRules[telemetrycmdDir]))

	sendRule := directRules[telemetrysendDir]
	got = directImportFindings(t, "test/conformance/testdata/importguard/netdirect", sendRule)
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0], "netdirect.go imports net"), got)
	assert.Empty(t, directImportFindings(t, "test/conformance/testdata/importguard/clean", sendRule))
	assert.Empty(t, directImportFindings(t, "test/conformance/testdata/importguard/telemetry", sendRule))
	got = directImportFindings(t, "test/conformance/testdata/importguard/token", sendRule)
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0], "imports "+modulePrefix+"internal/token"), got)
	got = directImportFindings(t, "test/conformance/testdata/importguard/httpx", sendRule)
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0], "imports "+modulePrefix+"internal/client/httpx"), got)
	for _, p := range []string{"net/url", "net/netip", "errors", "strings", "strconv"} {
		assert.True(t, sendRule.standard(p), p)
	}
	for _, p := range []string{"net", "net/http", "net/http/httptrace", "net/smtp", "crypto/tls", "os/exec"} {
		assert.False(t, sendRule.standard(p), p)
	}
}

// TestTelemetryImportClosureHasNoNetworkOrCredentialPackage asserts, for
// each of the six platforms the release builds, that the import closure of
// internal/telemetry and internal/fsacl holds no HTTP, TLS, client,
// configuration, token or keychain package, and reaches the net package only
// through golang.org/x/sys/windows.
func TestTelemetryImportClosureHasNoNetworkOrCredentialPackage(t *testing.T) {
	for _, p := range ciPlatforms {
		assert.Empty(t, closureFindings(t, p.goos, p.goarch, "./"+telemetryDir, "./"+fsaclDir), p.goos+"/"+p.goarch)
	}
}

// TestImportClosureCheckCatchesFixtures asserts the closure check reports a
// package that imports net/http, one that reaches the configuration package,
// and one that imports net, and accepts a clean one.
func TestImportClosureCheckCatchesFixtures(t *testing.T) {
	const base = "./test/conformance/testdata/importguard/"
	nethttp := closureFindings(t, "linux", "amd64", base+"nethttp")
	assert.Contains(t, nethttp, "linux/amd64: closure holds net/http")
	config := closureFindings(t, "linux", "amd64", base+"config")
	assert.Contains(t, config, "linux/amd64: closure holds "+modulePrefix+"internal/config")
	netdirect := closureFindings(t, "windows", "amd64", base+"netdirect")
	assert.Contains(t, netdirect, "windows/amd64: "+modulePrefix+"test/conformance/testdata/importguard/netdirect imports net")
	assert.Empty(t, closureFindings(t, "linux", "amd64", base+"clean"))
}
