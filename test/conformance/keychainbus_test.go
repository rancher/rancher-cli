package conformance

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rancher-cli's own D-Bus calls never start a session bus. With no session
// bus address the D-Bus library starts one through dbus-launch, and the
// Secret Service lookup of the keychain then starts gnome-keyring-daemon;
// internal/config sets the address to a value the library rejects at once
// before its first keychain call. The scan below keeps every other path
// from reaching the library:
//   - no production file imports the D-Bus library, so none calls its
//     entry points that start a bus (SessionBus, ConnectSessionBus,
//     SessionBusPrivate, SessionBusPrivateHandler) or refers to them;
//   - the keychain package go-keyring, which connects through the library,
//     is imported only in the directories of keychainImportDirs, and its
//     subpackages, such as secret_service, which connect without the check,
//     nowhere; its Set, Get, Delete and DeleteAll are referred to only in
//     the seam file internal/config/keyring_backend.go and in
//     internal/commands/cmdtest;
//   - no production file imports internal/commands/cmdtest, which calls
//     go-keyring without the seam.
//
// Known limit: the scan reads direct imports only. go-keyring is the only
// module that imports the D-Bus library today; a new dependency that does
// is reviewed when it is added.

const (
	dbusImport    = "github.com/godbus/dbus/v5"
	keyringImport = "github.com/zalando/go-keyring"
	cmdtestImport = "github.com/rancher/rancher-cli/internal/commands/cmdtest"

	// keychainSeamFile is the one production file outside the test support
	// package that calls go-keyring's Set, Get, Delete or DeleteAll.
	keychainSeamFile = "internal/config/keyring_backend.go"
)

// dbusLaunchingEntryPoints are the names in the D-Bus library that start a
// session bus when no address exists.
var dbusLaunchingEntryPoints = []string{"SessionBus", "ConnectSessionBus", "SessionBusPrivate", "SessionBusPrivateHandler"}

// keyringCalls are the names in go-keyring that reach the keychain.
var keyringCalls = []string{"Set", "Get", "Delete", "DeleteAll"}

// keychainImportDirs maps each directory whose production files may import
// go-keyring to the reason.
var keychainImportDirs = map[string]string{
	"internal/config":           "the keychain seam, which makes sure go-keyring cannot start a session bus, and the error values of its callers",
	"internal/commands/cmdtest": "test support imported only by tests, which swaps go-keyring for its in-memory mock",
}

// keychainBusFindings returns one line per breach of the rule in f, a parsed
// file that the lines name reported.
func keychainBusFindings(reported string, f *ast.File) []string {
	var out []string
	dbusName, keyringName := "", ""
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch p {
		case dbusImport:
			out = append(out, reported+" imports "+dbusImport)
			dbusName = cmp.Or(name, "dbus")
		case keyringImport:
			if _, ok := keychainImportDirs[path.Dir(reported)]; !ok {
				out = append(out, reported+" imports "+keyringImport+" outside "+allowedKeychainDirs())
			}
			keyringName = cmp.Or(name, "keyring")
		case cmdtestImport:
			out = append(out, reported+" imports "+cmdtestImport+", test support that only tests may import")
		default:
			if strings.HasPrefix(p, keyringImport+"/") {
				out = append(out, reported+" imports "+p+", which connects to the session bus without the check in "+keychainSeamFile)
			}
		}
	}
	seamMayCall := reported == keychainSeamFile || path.Dir(reported) == "internal/commands/cmdtest"
	refers := func(local, name string, n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			x, ok := v.X.(*ast.Ident)
			return ok && local != "" && local != "_" && local != "." && x.Name == local && v.Sel.Name == name
		case *ast.Ident:
			return local == "." && v.Name == name
		}
		return false
	}
	ast.Inspect(f, func(n ast.Node) bool {
		for _, name := range dbusLaunchingEntryPoints {
			if refers(dbusName, name, n) {
				out = append(out, reported+" refers to "+displayName(dbusName, name)+", which starts a session bus")
			}
		}
		if !seamMayCall {
			for _, name := range keyringCalls {
				if refers(keyringName, name, n) {
					out = append(out, reported+" refers to "+displayName(keyringName, name)+", which reaches the keychain without the session bus check in "+keychainSeamFile)
				}
			}
		}
		return true
	})
	return out
}

// displayName spells a reference as the file does: local.name, or name for
// a dot import.
func displayName(local, name string) string {
	if local == "." {
		return name
	}
	return local + "." + name
}

func allowedKeychainDirs() string {
	dirs := slices.Sorted(maps.Keys(keychainImportDirs))
	return strings.Join(dirs, " and ")
}

func fixtureFindings(t *testing.T, fixture, reportedAs string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), fixture, nil, 0)
	require.NoError(t, err)
	return keychainBusFindings(reportedAs, f)
}

// TestNoProductionCodeStartsASessionBus asserts that no production file
// imports the D-Bus library or refers to its entry points that start a
// session bus, and that go-keyring is imported only in the directories
// keychainImportDirs lists, each of which still imports it.
func TestNoProductionCodeStartsASessionBus(t *testing.T) {
	tree := parseSourceTree()
	require.NoError(t, tree.err)
	var findings []string
	importers := map[string]bool{}
	for i, f := range tree.files {
		findings = append(findings, keychainBusFindings(tree.paths[i], f)...)
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == keyringImport {
				importers[path.Dir(tree.paths[i])] = true
			}
		}
	}
	assert.Empty(t, findings)
	for dir, reason := range keychainImportDirs {
		assert.True(t, importers[dir], "%s is allowed to import go-keyring (%s) but no file there does; remove the row", dir, reason)
	}
}

// TestKeychainBusScanFlagsEveryCase asserts the scan reports an import of
// the D-Bus library under an alias with a reference to an entry point that
// is not a call, go-keyring outside its allowed directories, a direct
// keychain call anywhere but the seam and the test support package, and an
// import of the test support package, and passes each of them where it is
// allowed.
func TestKeychainBusScanFlagsEveryCase(t *testing.T) {
	const fixtures = "test/conformance/testdata/importguard/"
	got := fixtureFindings(t, fixtures+"dbus/dbus.go", "internal/engine/x.go")
	assert.Equal(t, []string{
		"internal/engine/x.go imports " + dbusImport,
		"internal/engine/x.go refers to bus.SessionBus, which starts a session bus",
	}, got)

	outside := "internal/engine/x.go imports " + keyringImport + " outside internal/commands/cmdtest and internal/config"
	call := func(path string) string {
		return path + " refers to keyring.Get, which reaches the keychain without the session bus check in " + keychainSeamFile
	}
	assert.Equal(t, []string{outside, call("internal/engine/x.go")}, fixtureFindings(t, fixtures+"keyringcall/keyringcall.go", "internal/engine/x.go"))
	assert.Equal(t, []string{call("internal/config/x.go")}, fixtureFindings(t, fixtures+"keyringcall/keyringcall.go", "internal/config/x.go"))
	assert.Equal(t, []string{call("internal/config/x.go")}, fixtureFindings(t, fixtures+"keyring/keyring.go", "internal/config/x.go"))
	assert.Empty(t, fixtureFindings(t, fixtures+"keyringcall/keyringcall.go", keychainSeamFile))
	assert.Empty(t, fixtureFindings(t, fixtures+"keyringcall/keyringcall.go", "internal/commands/cmdtest/x.go"))
	assert.Empty(t, fixtureFindings(t, fixtures+"clean/clean.go", "internal/engine/x.go"))

	for _, reported := range []string{"internal/config/x.go", "internal/engine/x.go"} {
		assert.Contains(t, fixtureFindings(t, fixtures+"secretservice/secretservice.go", reported),
			reported+" imports "+keyringImport+"/secret_service, which connects to the session bus without the check in "+keychainSeamFile, reported)
	}

	assert.Equal(t, []string{"internal/engine/x.go imports " + cmdtestImport + ", test support that only tests may import"},
		fixtureFindings(t, fixtures+"cmdtest/cmdtest.go", "internal/engine/x.go"))
}
