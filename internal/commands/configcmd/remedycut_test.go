package configcmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// cutRow is the lines one builder writes and what the lines that hold no
// command say: a line with a command is left out when the config directory has
// characters a terminal cannot show, and a line without one stays as it is.
type cutRow struct {
	lines []string
	// kept is a phrase a line that holds no command has; empty when every line
	// of the builder is a command.
	kept string
}

// cutRows are the lines of the builders of token_records.go and
// config_info.go that write a command.
func cutRows(t *testing.T) map[string]cutRow {
	t.Helper()
	const server = "https://r.example.test"
	ext := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}
	v3 := clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}
	entry := config.Server{URL: server, Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-a",
		SupersededTokens: []config.SupersededToken{
			{Ref: ext, UserID: "user-b", SupersededAt: "2026-09-01T00:00:00Z"},
			{Ref: v3, UserID: "user-b", SupersededAt: "2026-09-01T00:00:00Z"},
		}}
	recovery, ok := recoveryField("exec-login-r.example.test", server, server, "user-b", ext).val.([]string)
	require.True(t, ok)
	return map[string]cutRow{
		"tokenRecovery":     {lines: tokenRecovery("exec-login-r.example.test", server, server, "user-b", ext)},
		"administratorLine": {lines: []string{administratorLine(server, server, v3)}, kept: "an administrator logged in to"},
		"recordLines":       {lines: recordLines("exec-login-r.example.test", entry, "  "), kept: "log in as user 'user-b' (the token's owner)"},
		"recoveryField":     {lines: recovery, kept: "log in as user 'user-b' (the token's owner)"},
		"pauseReasonText":   {lines: []string{pauseReasonText("/x", "prod", config.PauseReasonNoLock)}, kept: "noLock - the change ran with"},
	}
}

// TestTheFactBeforeACommandSurvivesTheCutInTheConfigRecoveryTexts asserts, for
// every line the recovery builders of config status and the token records
// write, that a line with a command holds none when the config directory has
// characters a terminal cannot show, and keeps the phrase named for its
// builder, and that a line without a command is written as it is.
func TestTheFactBeforeACommandSurvivesTheCutInTheConfigRecoveryTexts(t *testing.T) {
	io := runtime.IO{RemedyDir: "/d\x01"}
	for name, row := range cutRows(t) {
		require.NotEmpty(t, row.lines, name)
		commands, facts := 0, 0
		for _, line := range row.lines {
			cut := io.ExpandText(line)
			assert.NotContains(t, cut, "rancher ", "%s: no command is left: %q", name, cut)
			switch {
			case clierr.HasRemedy(line):
				commands++
				if row.kept != "" && strings.Contains(line, row.kept) {
					assert.Contains(t, cut, row.kept, "%s keeps its fact", name)
				}
			default:
				assert.Equal(t, line, cut, "%s: a line without a command is written as it is", name)
			}
			if row.kept != "" && strings.Contains(line, row.kept) {
				facts++
			}
		}
		assert.Positive(t, commands, "%s has a command", name)
		if row.kept != "" {
			assert.Positive(t, facts, "%s has a line with its fact: %q", name, row.lines)
		}
	}
}

// TestEveryRecoveryBuilderOfConfigStatusHasACutRow asserts every function of
// token_records.go and config_info.go that writes a command of the CLI is a
// row of the cut table; a new builder fails until it has one.
func TestEveryRecoveryBuilderOfConfigStatusHasACutRow(t *testing.T) {
	rows := cutRows(t)
	var found []string
	for _, file := range []string{"token_records.go", "config_info.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			writes := false
			ast.Inspect(fn, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "RecoveryCommands", "AdviseRecord", "LoginHintFor", "LogoutHintFor":
						writes = true
					}
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "clierr" && sel.Sel.Name == "Rancher" {
						writes = true
					}
				}
				return !writes
			})
			if writes {
				found = append(found, fn.Name.Name)
			}
		}
	}
	for _, name := range found {
		assert.Contains(t, rows, name, "%s writes a command and has a cut row", name)
	}
	assert.True(t, slices.Contains(found, "tokenRecovery"))
}

// TestEveryCommandOfTheConfigRecoveryTextsIsOneOfTheRecoveryCommands asserts
// each line of the recovery builders that holds a command ends with a command
// of the recovery sequence or the administrator's delete of internal/clierr,
// whose commands the exec plugin parse test of internal/token runs through the
// command tree.
func TestEveryCommandOfTheConfigRecoveryTextsIsOneOfTheRecoveryCommands(t *testing.T) {
	const server = "https://r.example.test"
	ext := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}
	v3 := clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}
	commands := append(clierr.RecoverySequence(server, ext).Lines(), clierr.Revoke(v3).String())
	for name, row := range cutRows(t) {
		if name == "pauseReasonText" {
			continue
		}
		for _, line := range row.lines {
			if !clierr.HasRemedy(line) {
				continue
			}
			assert.True(t, slices.ContainsFunc(commands, func(c string) bool { return strings.HasSuffix(strings.TrimSpace(line), c) }),
				"%s: %q ends with none of the recovery commands", name, line)
		}
	}
}
