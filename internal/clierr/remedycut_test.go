package clierr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// cutTexts are the texts of the recovery builders of recovery.go and
// loginfailure.go that hold a command, each a command the sentence that holds
// it opens with: the recovery commands of a token, the sequence that logs in
// and deletes it, and the line of the login failure details that clears the
// local credential.
func cutTexts(t *testing.T) map[string][]string {
	t.Helper()
	ext := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-a"}
	v3 := clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-b"}
	var recoveryCommands []string
	for _, ref := range []clierr.TokenRef{ext, v3} {
		list, revoke := clierr.RecoveryCommands(ref)
		recoveryCommands = append(recoveryCommands, list, revoke)
	}
	var credentialLines []string
	for _, line := range (clierr.LoginFailureDetails{CredentialRecovery: &clierr.CredentialRecovery{
		Command: clierr.Rancher + " auth logout 'prod'", DataLossRisk: "deletes the stored login",
	}}).TextDetails() {
		if text := line.String(); clierr.HasRemedy(text) {
			credentialLines = append(credentialLines, text)
		}
	}
	return map[string][]string{
		"RecoveryCommands":                recoveryCommands,
		"RecoverySequence":                clierr.RecoverySequence("https://r.example.test", ext),
		"LoginFailureDetails.TextDetails": credentialLines,
	}
}

// TestNoCommandSurvivesTheCutOfTheRecoveryTexts asserts, for every text the
// recovery builders write, that with a config directory that has characters a
// terminal cannot show the commands are left out and the sentence the text
// opens with is the note that says why.
func TestNoCommandSurvivesTheCutOfTheRecoveryTexts(t *testing.T) {
	io := runtime.IO{RemedyDir: "/d\x01"}
	texts := cutTexts(t)
	for _, name := range []string{"RecoveryCommands", "RecoverySequence", "LoginFailureDetails.TextDetails"} {
		require.NotEmpty(t, texts[name], name)
		for _, text := range texts[name] {
			require.True(t, clierr.HasRemedy(text), "%s: %q", name, text)
			cut := io.ExpandText(text)
			assert.NotContains(t, cut, "rancher ", "%s: no command is left: %q", name, cut)
			assert.Equal(t, runtime.UnprintableConfigDirNote, cut, "%s", name)
		}
	}
}

// TestEveryRecoveryBuilderHasACutRow asserts every function of recovery.go
// and loginfailure.go that writes a command of the CLI is a row of the cut
// table; a new builder fails until it has one.
func TestEveryRecoveryBuilderHasACutRow(t *testing.T) {
	rows := cutTexts(t)
	var found []string
	for _, file := range []string{"recovery.go", "loginfailure.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			writes := false
			ast.Inspect(fn, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "Rancher" {
					writes = true
				}
				return !writes
			})
			if writes {
				found = append(found, fn.Name.Name)
			}
		}
	}
	slices.Sort(found)
	assert.Equal(t, []string{"RecoveryCommands", "RecoverySequence"}, found)
	for _, name := range found {
		assert.NotEmpty(t, rows[name], "%s has a cut row", name)
	}
}
