package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// docApplyGateFiles lists the Markdown files whose command examples are
// scanned. They are read from the module root.
var docApplyGateFiles = []string{
	"README.md",
	"MIGRATING.md",
	"COMMANDS.md",
	"REFERENCE.md",
	"skills/rancher/SKILL.md",
}

// docApplyGateAllowed lists the examples that have --apply and leave out a
// gate on purpose, such as one that shows the refusal. The key is
// <file>:<command>:<example text>, where the example text is the example's
// words joined by single spaces, up to the first word that ends it (|, ||,
// &&, >, >>, \| or a word that starts with #) or the first ; (the text before
// the ; is the last word), with a trailing ), ; " or ' removed from each word.
// The command is "unknown" for an example no manifest command matches. The
// value is the reason.
var docApplyGateAllowed = map[string]string{}

// docGateCommand is the part of a schema manifest command the scan reads.
type docGateCommand struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases"`
	Gates       []string `json:"gates"`
	Destructive string   `json:"destructive"`
}

// docUnit is a piece of Markdown the scan reads for examples: an inline code
// span, a line of a fenced block, or a line of an indented block. The line is
// the 1-based line where the unit starts. A code span that wraps over several
// lines starts at the first line of its paragraph (a list item, a table row
// or a run of non-blank lines).
type docUnit struct {
	line int
	text string
}

// docExample is one rancher command found in a unit. The words start with
// "rancher".
type docExample struct {
	line  int
	words []string
}

func (e docExample) text() string { return strings.Join(e.words, " ") }

// docIndented reports whether a line starts with four spaces or a tab.
func docIndented(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

// docUnits returns the units of the Markdown text: every inline code span
// (wrapped lines joined with a space, and \| read as | in a table row),
// every line of a fenced block and every line of an indented block. A fenced
// or indented line that ends in a backslash, or that leaves a single or double
// quote open, is joined with the next line; a blank line ends the join. An
// indented line that has a backtick is read twice: as code, and for its
// inline code spans, because under a list item it can be a paragraph. Prose
// outside code is not returned. CRLF line endings are read as LF. The line of
// a joined fenced or indented line is the first line of the join, not the line
// of the example.
func docUnits(text string) []docUnit {
	var units []docUnit
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}

	var para []string
	paraLine := 0
	paraTable := false
	flushPara := func() {
		if len(para) > 0 {
			joined := strings.Join(para, " ")
			if paraTable {
				joined = strings.ReplaceAll(joined, `\|`, "|")
			}
			for _, span := range inlineCodeSpans(joined) {
				units = append(units, docUnit{line: paraLine, text: span})
			}
		}
		para, paraTable = nil, false
	}

	var code []string
	codeLine := 0
	codeSpans := false
	flushCode := func() {
		if len(code) > 0 {
			joined := strings.Join(code, " ")
			units = append(units, docUnit{line: codeLine, text: joined})
			if codeSpans && strings.Contains(joined, "`") {
				for _, span := range inlineCodeSpans(joined) {
					units = append(units, docUnit{line: codeLine, text: span})
				}
			}
		}
		code, codeSpans = nil, false
	}
	addCode := func(n int, line string, spans bool) {
		line = strings.TrimSpace(line)
		if line == "" {
			flushCode()
			return
		}
		if len(code) == 0 {
			codeLine = n
		}
		codeSpans = codeSpans || spans
		if rest, ok := strings.CutSuffix(line, `\`); ok {
			code = append(code, strings.TrimSpace(rest))
			return
		}
		code = append(code, line)
		if docOpenQuote(strings.Join(code, " ")) {
			return
		}
		flushCode()
	}

	var fenceChar byte
	fenceLen := 0
	previousBlank := true
	previousIndented := false
	for i, line := range lines {
		n := i + 1
		trimmed := strings.TrimSpace(line)
		if fenceLen > 0 {
			closing := strings.TrimSpace(line)
			char, length := fenceRun(closing)
			if char == fenceChar && length >= fenceLen && closing[length:] == "" {
				flushCode()
				fenceChar, fenceLen = 0, 0
				continue
			}
			addCode(n, line, false)
			continue
		}
		if char, length := fenceRun(strings.TrimLeft(line, " \t")); length >= 3 {
			flushPara()
			flushCode()
			fenceChar, fenceLen = char, length
			previousBlank, previousIndented = false, false
			continue
		}
		isList := markdownListItem.MatchString(line)
		if docIndented(line) && trimmed != "" && !isList && (previousBlank || previousIndented) {
			flushPara()
			addCode(n, line, true)
			previousBlank, previousIndented = false, true
			continue
		}
		flushCode()
		previousIndented = false
		switch {
		case trimmed == "":
			flushPara()
			previousBlank = true
			continue
		case strings.HasPrefix(trimmed, "|"):
			flushPara()
			para, paraLine, paraTable = []string{trimmed}, n, true
			flushPara()
		case isList:
			flushPara()
			para, paraLine = []string{trimmed}, n
		default:
			if len(para) == 0 {
				paraLine = n
			}
			para = append(para, trimmed)
		}
		previousBlank = false
	}
	flushPara()
	flushCode()
	return units
}

// docOpenQuote reports whether a single or double quote is still open at the
// end of the text. A backslash escapes the next character outside single
// quotes, and a word that starts with # outside quotes begins a comment that
// runs to the end of the text.
func docOpenQuote(text string) bool {
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || text[i-1] == ' ' || text[i-1] == '\t'):
			return false
		}
	}
	return quote != 0
}

// docExampleStarts returns the offsets in text of every word "rancher" that
// starts the text or follows whitespace, a pipe, an opening parenthesis (as in
// "$(") or a quote, and that is followed by whitespace and a lowercase letter
// or a flag. A flag after rancher is a global flag before the command. A
// rancher that follows ;, & or = is not an example.
func docExampleStarts(text string) []int {
	var starts []int
	for i := 0; i+len("rancher") < len(text); i++ {
		if !strings.HasPrefix(text[i:], "rancher") {
			continue
		}
		if i > 0 && !strings.ContainsRune(" \t|('\"", rune(text[i-1])) {
			continue
		}
		j := i + len("rancher")
		k := j
		for k < len(text) && (text[k] == ' ' || text[k] == '\t') {
			k++
		}
		if k == j || k >= len(text) || (text[k] != '-' && (text[k] < 'a' || text[k] > 'z')) {
			continue
		}
		starts = append(starts, i)
	}
	return starts
}

// docExampleEnd reports whether a word ends an example: a pipe (also \| in a
// table row that does not start with a pipe), an operator, a redirection or a
// comment. A word with a ; is handled in docExamples.
func docExampleEnd(word string) bool {
	switch word {
	case "|", "||", "&&", ">", ">>", `\|`:
		return true
	}
	return strings.HasPrefix(word, "#")
}

// docExamples returns the rancher commands in a unit. An example runs to the
// next rancher command, to the first word that ends it, or to the end of the
// unit. A word with a ; ends its example: the text before the ; is the last
// word, so --apply;echo is --apply, and a ; inside quotes ends it too. A
// trailing ), ; " or ' is removed from each word: --apply) is the flag --apply.
func docExamples(unit docUnit) []docExample {
	starts := docExampleStarts(unit.text)
	var examples []docExample
	for i, start := range starts {
		end := len(unit.text)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		var words []string
		for word := range strings.FieldsSeq(unit.text[start:end]) {
			if docExampleEnd(word) {
				break
			}
			before, _, last := strings.Cut(word, ";")
			if trimmed := strings.TrimRight(before, `);"'`); trimmed != "" {
				words = append(words, trimmed)
			}
			if last {
				break
			}
		}
		examples = append(examples, docExample{line: unit.line, words: words})
	}
	return examples
}

// docHasWord reports whether the words contain the flag as a whole word, with or
// without =true.
func docHasWord(words []string, flag string) bool {
	return slices.Contains(words, flag) || slices.Contains(words, flag+"=true")
}

// docCommandOf returns the manifest command an example runs: the longest
// prefix of the words after "rancher" that is a command name, or a name with
// its last word replaced by one of the command's aliases.
func docCommandOf(commands []docGateCommand, words []string) (docGateCommand, bool) {
	var best docGateCommand
	bestLen := 0
	for _, c := range commands {
		name := strings.Fields(c.Name)
		forms := [][]string{name}
		if len(name) > 0 {
			for _, alias := range c.Aliases {
				forms = append(forms, append(slices.Clone(name[:len(name)-1]), alias))
			}
		}
		for _, form := range forms {
			if len(form) > bestLen && len(form) <= len(words) && slices.Equal(form, words[:len(form)]) {
				best, bestLen = c, len(form)
			}
		}
	}
	return best, bestLen > 0
}

// docApplyScan returns, per file, the number of examples that have --apply,
// and the problems: an unknown command, a missing gate, an allowlist entry
// with no reason, and an allowlist entry that matches no example with a
// problem. For a command whose destructive value is dependsOnInput (create
// resource), whether --yes is needed depends on the input; only --apply counts
// as a gate.
func docApplyScan(commands []docGateCommand, names []string, texts map[string]string, allowed map[string]string) (map[string]int, []string) {
	counts := map[string]int{}
	var problems []string
	used := map[string]bool{}
	report := func(file, command string, example docExample, problem string) {
		key := file + ":" + command + ":" + example.text()
		if _, ok := allowed[key]; ok {
			used[key] = true
			return
		}
		problems = append(problems, fmt.Sprintf("%s:%d: %s", file, example.line, problem))
	}
	for _, file := range names {
		for _, unit := range docUnits(texts[file]) {
			for _, example := range docExamples(unit) {
				if !docHasWord(example.words, "--apply") {
					continue
				}
				counts[file]++
				command, ok := docCommandOf(commands, example.words[1:])
				if !ok {
					report(file, "unknown", example, "unknown command "+strings.Join(example.words[1:], " "))
					continue
				}
				gates := command.Gates
				if command.Destructive == "dependsOnInput" {
					gates = []string{"--apply"}
				}
				for _, gate := range gates {
					if !docHasWord(example.words, gate) {
						report(file, command.Name, example, command.Name+": missing "+gate+": "+example.text())
					}
				}
			}
		}
	}
	for key, reason := range allowed {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, "docApplyGateAllowed has no reason for "+key)
		}
		if !used[key] {
			problems = append(problems, "docApplyGateAllowed matches no example with a problem: "+key)
		}
	}
	slices.Sort(problems)
	return counts, problems
}

// docGateManifest reads the commands of the schema manifest golden.
func docGateManifest(t *testing.T) []docGateCommand {
	t.Helper()
	raw, err := os.ReadFile("internal/cli/testdata/schema.golden.json")
	require.NoError(t, err)
	var manifest struct {
		Commands []docGateCommand `json:"commands"`
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	withGates := 0
	for _, c := range manifest.Commands {
		if len(c.Gates) > 0 {
			withGates++
		}
	}
	require.Positive(t, withGates, "the schema manifest lists no command with gates")
	return manifest.Commands
}

// TestDocsApplyExamplesCarryTheirGates asserts that every command example in
// the Markdown files of docApplyGateFiles that has --apply also has every gate
// its command has in the schema manifest. An example copied as written is then
// not refused with CONFIRMATION_REQUIRED. Plain prose is not scanned; inline
// code spans, fenced blocks and indented blocks are.
func TestDocsApplyExamplesCarryTheirGates(t *testing.T) {
	commands := docGateManifest(t)
	texts := map[string]string{}
	for _, file := range docApplyGateFiles {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		texts[file] = string(raw)
	}
	counts, problems := docApplyScan(commands, docApplyGateFiles, texts, docApplyGateAllowed)
	assert.Empty(t, problems, "examples the CLI refuses when copied as written, or whose command is not in the schema manifest")

	total := 0
	for _, n := range counts {
		total += n
	}
	assert.GreaterOrEqual(t, total, 25, "the scan found too few examples with --apply")
	for _, file := range []string{"README.md", "MIGRATING.md", "REFERENCE.md", "skills/rancher/SKILL.md"} {
		assert.Positive(t, counts[file], "the scan found no example with --apply in "+file)
	}
}

// TestDocsApplyGateRows asserts how the docs --apply scan finds examples in
// Markdown and checks their gates, against a small manifest.
func TestDocsApplyGateRows(t *testing.T) {
	commands := []docGateCommand{
		{Name: "create kubeconfig", Gates: []string{"--apply", "--yes"}, Destructive: "true"},
		{Name: "patch setting", Aliases: []string{"settings"}, Gates: []string{"--apply", "--yes"}, Destructive: "true"},
		{Name: "create project", Gates: []string{"--apply"}, Destructive: "false"},
		{Name: "create resource", Gates: []string{"--apply"}, Destructive: "dependsOnInput"},
	}
	const kubeconfig = "rancher create kubeconfig --apply"
	fence := func(lines ...string) string { return "```sh\n" + strings.Join(lines, "\n") + "\n```\n" }
	missingYes := func(line int, example string) []string {
		return docMissingYes("create kubeconfig", line, example)
	}
	for _, tc := range []struct {
		row   string
		text  string
		count int
		want  []string
	}{
		{"wrappedSpan", "Intro\nNo binary? Run `rancher create\nkubeconfig --apply` now.\n", 1, missingYes(1, kubeconfig)},
		{"wrappedSpanWithGates", "Intro\n\nNo binary? Run `rancher create\nkubeconfig --apply --yes` now.\n", 1, nil},
		{"indentedBlockWithBackslash", "Steps:\n\n    rancher create kubeconfig \\\n      --apply\n", 1, missingYes(3, kubeconfig)},
		{"indentedBlockUnderListItem", "- item\n\n    " + kubeconfig + "\n", 1, missingYes(3, kubeconfig)},
		{"indentedParagraphUnderListItem", "- item\n\n    continue with `" + kubeconfig + "`\n", 1, missingYes(3, kubeconfig)},
		{"tildeFence", "~~~\n" + kubeconfig + "\n~~~\n", 1, missingYes(2, kubeconfig)},
		{"longerFenceContainsShorterFence", "````md\n```\n" + kubeconfig + "\n```\n````\n", 1, missingYes(3, kubeconfig)},
		{"fenceNestedInListItem", "1. step\n\n   ```sh\n   " + kubeconfig + "\n   ```\n", 1, missingYes(4, kubeconfig)},
		{"fencedCommentNamesYes", fence(kubeconfig + "   # also needs --yes"), 1, missingYes(2, kubeconfig)},
		{"fencedCommentNamesApply", fence("rancher create project a # --apply"), 0, nil},
		{"fencedCommentWithApostrophe", fence("# it's a comment", kubeconfig), 1, missingYes(3, kubeconfig)},
		{"fencedQuoteSpansLines", fence("rancher patch setting x --patch '[", "{}", "]' --apply"), 1,
			docMissingYes("patch setting", 2, "rancher patch setting x --patch '[ {} ] --apply")},
		{"applyIsLastWordOfUnit", "```sh\n" + kubeconfig, 1, missingYes(2, kubeconfig)},
		{"pipeIntoRancher", fence("printf x | " + kubeconfig), 1, missingYes(2, kubeconfig)},
		{"pipeOutOfRancher", fence("rancher create kubeconfig --apply --yes | jq ."), 1, nil},
		{"commandSubstitutionMissingGate", fence("KUBECONFIG=$(" + kubeconfig + ")"), 1, missingYes(2, kubeconfig)},
		{"commandSubstitutionWithGates", fence("echo $(rancher create kubeconfig --apply --yes)"), 1, nil},
		{"semicolonAttachedToFlag", fence(kubeconfig + "; echo --yes"), 1, missingYes(2, kubeconfig)},
		{"semicolonWithoutSpace", fence(kubeconfig + ";echo --yes"), 1, missingYes(2, kubeconfig)},
		{"substitutionClosedBeforeSemicolon", fence("echo $(" + kubeconfig + " ); echo --yes"), 1, missingYes(2, kubeconfig)},
		{"doubleQuotedCommand", "`bash -c \"" + kubeconfig + "\"`\n", 1, missingYes(1, kubeconfig)},
		{"singleQuotedCommand", "`bash -c '" + kubeconfig + "'`\n", 1, missingYes(1, kubeconfig)},
		{"globalFlagBeforeCommand", "`rancher --server s create kubeconfig --apply`\n", 1,
			[]string{"f.md:1: unknown command --server s create kubeconfig --apply"}},
		{"endWordPipe", fence(kubeconfig + " | echo --yes"), 1, missingYes(2, kubeconfig)},
		{"endWordOr", fence(kubeconfig + " || echo --yes"), 1, missingYes(2, kubeconfig)},
		{"endWordAnd", fence(kubeconfig + " && echo --yes"), 1, missingYes(2, kubeconfig)},
		{"endWordSemicolon", fence(kubeconfig + " ; echo --yes"), 1, missingYes(2, kubeconfig)},
		{"endWordRedirect", fence(kubeconfig + " > echo --yes"), 1, missingYes(2, kubeconfig)},
		{"endWordAppend", fence(kubeconfig + " >> echo --yes"), 1, missingYes(2, kubeconfig)},
		{"endWordComment", fence(kubeconfig + " # echo --yes"), 1, missingYes(2, kubeconfig)},
		{"escapedPipeInTableCell", "| a | b |\n|---|---|\n| x | `" + kubeconfig + " \\| echo --yes` |\n", 1, missingYes(3, kubeconfig)},
		{"escapedPipeInRowWithoutLeadingPipe", "a | b\n--|--\nx | `" + kubeconfig + " \\| echo --yes`\n", 1, missingYes(1, kubeconfig)},
		{"crlfLineEndings", "```sh\r\n" + kubeconfig + "\r\n```\r\n", 1, missingYes(2, kubeconfig)},
		{"applyPrefixDoesNotCount", "`rancher create kubeconfig --apply-foo`\n", 0, nil},
		{"applyFalseDoesNotCount", "`rancher create kubeconfig --apply=false`\n", 0, nil},
		{"applyTrueCounts", "`rancher create kubeconfig --apply=true --yes=true`\n", 1, nil},
		{"yesPrefixDoesNotCount", "`rancher create kubeconfig --apply --yes-foo`\n", 1, missingYes(1, "rancher create kubeconfig --apply --yes-foo")},
		{"aliasSpelling", "`rancher patch settings x --apply`\n", 1,
			docMissingYes("patch setting", 1, "rancher patch settings x --apply")},
		{"dependsOnInput", "`rancher create resource -f x.yaml --apply`\n", 1, nil},
		{"unknownCommand", "`rancher create kubeconfigz --apply`\n", 1,
			[]string{"f.md:1: unknown command create kubeconfigz --apply"}},
		{"twoExamplesInOneSpan", "`rancher create project a --apply && " + kubeconfig + "`\n", 2, missingYes(1, kubeconfig)},
		{"proseIsNotScanned", "Run rancher create kubeconfig --apply to get one.\n", 0, nil},
		{"prefixedWordIsNotAStart", "`xrancher create kubeconfig --apply`\n", 0, nil},
	} {
		t.Run(tc.row, func(t *testing.T) {
			counts, problems := docApplyScan(commands, []string{"f.md"}, map[string]string{"f.md": tc.text}, nil)
			assert.Equal(t, tc.count, counts["f.md"])
			assert.Equal(t, tc.want, problems)
		})
	}

	t.Run("allowlist", func(t *testing.T) {
		text := "`" + kubeconfig + "`\n"
		key := "f.md:create kubeconfig:" + kubeconfig
		texts := map[string]string{"f.md": text}
		_, problems := docApplyScan(commands, []string{"f.md"}, texts, map[string]string{key: "shows the refusal"})
		assert.Empty(t, problems)
		_, problems = docApplyScan(commands, []string{"f.md"}, texts, map[string]string{key: " "})
		assert.Equal(t, []string{"docApplyGateAllowed has no reason for " + key}, problems)
		other := key + " --yes"
		_, problems = docApplyScan(commands, []string{"f.md"}, texts, map[string]string{other: "x"})
		assert.Equal(t, []string{
			"docApplyGateAllowed matches no example with a problem: " + other,
			missingYes(1, kubeconfig)[0],
		}, problems)
	})
}

// docMissingYes returns the problem for an example of the named command that
// lacks --yes.
func docMissingYes(command string, line int, example string) []string {
	return []string{fmt.Sprintf("f.md:%d: %s: missing --yes: %s", line, command, example)}
}
