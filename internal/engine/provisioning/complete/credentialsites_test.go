package complete

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// displayNameProjections are the functions that build an offered list of
// display names for a cloud credential (completeCloudCredentials and
// cloudCredentialNames), the two that reach them (the read command's completion
// and its source), and the function that reads one credential's display name
// (storedDisplayName). A mutation that takes a cloud credential takes the
// credential id only, so only the read command's completion may offer display
// names. The scan does not see two things; TestMutationsOfferOnlyCredentialIDs
// covers them: a display name offered as a value from inside credentialIDs or
// storedDriverPrefix (the two users of storedDisplayName), and a direct read of
// the field.cattle.io/name annotation (cloudCredentialDisplay) in
// cloudCredentialNames and storedDisplayName.
var displayNameProjections = []string{"completeCloudCredentials", "cloudCredentialNames", "completeGetCloudCredential", "GetCloudCredentialSource", "storedDisplayName"}

// displayNameProjectionUsers are the functions that may use the projections
// other than storedDisplayName, one flat list: the read command's source, which
// names its completion, that completion, and the two functions that build the
// list, each of which calls the next. The one use of GetCloudCredentialSource
// outside this package is getCloudCredCmd.LiveSource, which the scan of package
// provisioning allows.
var displayNameProjectionUsers = []string{"GetCloudCredentialSource", "completeGetCloudCredential", "completeCloudCredentials", "cloudCredentialNames"}

// storedDisplayNameUsers are the only functions that may use storedDisplayName:
// storedDriverPrefix compares a display name with the typed value, and
// credentialIDs keeps it as the description of an id and tests for an ambiguous
// id; neither offers it as a value. They are not in displayNameProjectionUsers,
// so neither may use the other projections.
var storedDisplayNameUsers = []string{"storedDriverPrefix", "credentialIDs"}

// projectionUseAllowed reports whether function may use the projection name.
func projectionUseAllowed(function, name string) bool {
	if name == "storedDisplayName" {
		return slices.Contains(storedDisplayNameUsers, function)
	}
	return slices.Contains(displayNameProjectionUsers, function)
}

// findProjectionUses parses the Go source of path (read from the file when src
// is nil) and returns "function name" for every use of a name in
// displayNameProjections: a call, a function value or any other mention. A
// declaration is not a use. It does not see a display name offered from inside
// a function that may use storedDisplayName. It does not see reflection.
// TestMutationsOfferOnlyCredentialIDs and TestEveryMutationSelectsACredentialByID
// cover the three mutations that take a cloud credential.
func findProjectionUses(path string, src any) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		return nil, err
	}
	var uses []string
	for _, decl := range file.Decls {
		function := ""
		declared := map[*ast.Ident]bool{}
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = fd.Name.Name
			declared[fd.Name] = true
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && !declared[id] && slices.Contains(displayNameProjections, id.Name) {
				uses = append(uses, function+" "+id.Name)
			}
			return true
		})
	}
	return uses, nil
}

// TestOnlyTheReadCommandOffersDisplayNames asserts that only the functions of
// displayNameProjectionUsers use the display name projections of a credential
// completion, the read command's completion and its source included, and only
// the functions of storedDisplayNameUsers use storedDisplayName, so no source
// of a mutation that takes a cloud credential offers a value that mutation
// refuses.
func TestOnlyTheReadCommandOffersDisplayNames(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		uses, err := findProjectionUses(name, nil)
		require.NoError(t, err)
		for _, u := range uses {
			function, projection, _ := strings.Cut(u, " ")
			assert.True(t, projectionUseAllowed(function, projection), "%s: %s; only the read command's completion offers display names, and only storedDriverPrefix and credentialIDs use storedDisplayName", name, u)
		}
	}
	assert.NotZero(t, checked)
}

// TestTheDisplayNameProjectionScanFindsEveryUse asserts the rule of
// findProjectionUses and projectionUseAllowed on small sources: a call, a
// function value and a mention outside the listed functions are found, a
// declaration is not, and storedDisplayName is allowed only in its two users,
// which may not use the other projections.
func TestTheDisplayNameProjectionScanFindsEveryUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src string
		want      []string
		allowed   bool
	}{
		{"a call in a patch function", "package p\n\nfunc completePatchCloudCredential() { return completeCloudCredentials(ctx, conn) }\n", []string{"completePatchCloudCredential completeCloudCredentials"}, false},
		{"a call in the get function", "package p\n\nfunc completeGetCloudCredential() { return completeCloudCredentials(ctx, conn) }\n", []string{"completeGetCloudCredential completeCloudCredentials"}, true},
		{"a name projection call", "package p\n\nfunc completeDeleteCloudCredential() { _ = cloudCredentialNames(items) }\n", []string{"completeDeleteCloudCredential cloudCredentialNames"}, false},
		{"a function value", "package p\n\nvar sites = map[string]any{\"a\": completeCloudCredentials}\n", []string{" completeCloudCredentials"}, false},
		{"the read command's completion called in a delete function", "package p\n\nfunc completeDeleteCloudCredential() { return completeGetCloudCredential(ctx, conn, req) }\n", []string{"completeDeleteCloudCredential completeGetCloudCredential"}, false},
		{"the read command's source called in another function", "package p\n\nfunc f() { _ = GetCloudCredentialSource() }\n", []string{"f GetCloudCredentialSource"}, false},
		{"the completion as a function value in the read command's source", "package p\n\nfunc GetCloudCredentialSource() completion.Source { return completion.Source{Complete: completeGetCloudCredential} }\n", []string{"GetCloudCredentialSource completeGetCloudCredential"}, true},
		{"storedDisplayName in storedDriverPrefix", "package p\n\nfunc storedDriverPrefix() { _ = storedDisplayName(item) == name }\n", []string{"storedDriverPrefix storedDisplayName"}, true},
		{"storedDisplayName in credentialIDs", "package p\n\nfunc credentialIDs() { _ = entry{display: storedDisplayName(item)} }\n", []string{"credentialIDs storedDisplayName"}, true},
		{"storedDisplayName in a delete function", "package p\n\nfunc completeDeleteCloudCredential() { res.Names = append(res.Names, storedDisplayName(item)) }\n", []string{"completeDeleteCloudCredential storedDisplayName"}, false},
		{"a display name list built in credentialIDs", "package p\n\nfunc credentialIDs() { _ = cloudCredentialNames(items) }\n", []string{"credentialIDs cloudCredentialNames"}, false},
		{"the declaration of storedDisplayName", "package p\n\nfunc storedDisplayName(item map[string]any) string { return \"\" }\n", nil, true},
		{"a declaration only", "package p\n\nfunc completeCloudCredentials() {}\n", nil, true},
		{"another function", "package p\n\nfunc f() { credentialIDs(items) }\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := findProjectionUses("p.go", tc.src)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			allowed := true
			for _, u := range got {
				function, projection, _ := strings.Cut(u, " ")
				allowed = allowed && projectionUseAllowed(function, projection)
			}
			assert.Equal(t, tc.allowed, allowed)
		})
	}
}
