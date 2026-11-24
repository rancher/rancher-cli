package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
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

const (
	surfaceDoc       = "SURFACE.md"
	surfaceGuardFile = "internal/cli/surfaceconventions_test.go"
	surfaceMinRules  = 20
)

var (
	surfaceHeadingPattern = regexp.MustCompile(`^## ([0-9]+)\. `)
	surfaceBacktickSpan   = regexp.MustCompile("`([^`]*)`")
	surfaceTestName       = regexp.MustCompile("^Test[A-Za-z0-9]*$")
	surfaceParenthetical  = regexp.MustCompile(`\(([^()]*)\)`)
)

// surfaceDeclaredTests maps the name of every test function in the _test.go
// files git lists, outside testdata, to the slash paths of the files that
// declare it.
func surfaceDeclaredTests() (map[string][]string, error) {
	all, err := repositoryFiles()
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	tests := map[string][]string{}
	for _, p := range all {
		if !strings.HasSuffix(p, "_test.go") || inTestdata(p) {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && isTestFunction(fd) {
				tests[fd.Name.Name] = append(tests[fd.Name.Name], p)
			}
		}
	}
	return tests, nil
}

// surfaceRuleProblems reads the text of SURFACE.md and returns one problem for
// each rule it breaks: the rule headings run from 1 in order with no gap and
// number at least minRules; each rule has a "Checked by:" line, a "Not checked
// yet:" line or both; every test a "Checked by:" line names is declared in
// the file written beside it, the only text in parentheses on the line; and
// every test declared in guardFile is named on a "Checked by:" line. A line
// that starts "Checked by" without the colon and space, a backticked word that
// is not a test name and empty parentheses are problems too. tests maps a test
// name to the files that declare it.
func surfaceRuleProblems(doc string, tests map[string][]string, guardFile string, minRules int) []string {
	var problems []string
	named := map[string]bool{}
	next := 1
	number := 0
	var checked, notChecked bool
	closeRule := func() {
		if number > 0 && !checked && !notChecked {
			problems = append(problems, fmt.Sprintf("%s: rule %d has neither a \"Checked by:\" line nor a \"Not checked yet:\" line; add one", surfaceDoc, number))
		}
	}
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if m := surfaceHeadingPattern.FindStringSubmatch(line); m != nil {
			closeRule()
			n, _ := strconv.Atoi(m[1])
			if n != next {
				problems = append(problems, fmt.Sprintf("%s: rule %d comes where rule %d belongs; number the rules 1, 2, 3 in order with no gap", surfaceDoc, n, next))
			}
			next = n + 1
			number = n
			checked, notChecked = false, false
			continue
		}
		if number == 0 {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "Not checked yet: "); ok && strings.TrimSpace(rest) != "" {
			notChecked = true
		}
		if strings.HasPrefix(line, "Checked by") && !strings.HasPrefix(line, "Checked by: ") {
			problems = append(problems, fmt.Sprintf("%s: rule %d: a line starts with \"Checked by\" but not with \"Checked by: \"; write \"Checked by: \" and then the test names", surfaceDoc, number))
			continue
		}
		rest, ok := strings.CutPrefix(line, "Checked by: ")
		if !ok {
			continue
		}
		var names []string
		for _, m := range surfaceBacktickSpan.FindAllStringSubmatch(rest, -1) {
			if surfaceTestName.MatchString(m[1]) {
				names = append(names, m[1])
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: rule %d: the \"Checked by:\" line has %q in backticks, which is not a test name; write only test names in backticks", surfaceDoc, number, m[1]))
		}
		parens := surfaceParenthetical.FindAllStringSubmatch(rest, -1)
		if len(names) == 0 {
			problems = append(problems, fmt.Sprintf("%s: rule %d: the \"Checked by:\" line names no test in backticks; write each test name in backticks", surfaceDoc, number))
			continue
		}
		checked = true
		file := ""
		switch {
		case len(parens) == 1 && strings.TrimSpace(parens[0][1]) == "":
			problems = append(problems, fmt.Sprintf("%s: rule %d: the \"Checked by:\" line has empty parentheses; write the file of its tests in them", surfaceDoc, number))
		case len(parens) == 1:
			file = strings.TrimSpace(parens[0][1])
		default:
			problems = append(problems, fmt.Sprintf("%s: rule %d: the \"Checked by:\" line must have the file of its tests in parentheses, and no other text in parentheses", surfaceDoc, number))
		}
		for _, name := range names {
			named[name] = true
			files, found := tests[name]
			switch {
			case !found:
				problems = append(problems, fmt.Sprintf("%s: rule %d names the test %s, which no _test.go file declares", surfaceDoc, number, name))
			case file != "" && !slices.Contains(files, file):
				problems = append(problems, fmt.Sprintf("%s: rule %d names %s in %s, but it is declared in %s", surfaceDoc, number, name, file, strings.Join(files, ", ")))
			}
		}
	}
	closeRule()
	if next-1 < minRules {
		problems = append(problems, fmt.Sprintf("%s has %d rules; it must have at least %d. Restore the deleted rule, or lower surfaceMinRules in test/conformance/surfacerules_test.go in the change that removes it", surfaceDoc, next-1, minRules))
	}
	for _, name := range slices.Sorted(maps.Keys(tests)) {
		if slices.Contains(tests[name], guardFile) && !named[name] {
			problems = append(problems, fmt.Sprintf("%s declares the test %s, but no rule of %s names it; add a \"Checked by:\" line for it under the rule it checks", guardFile, name, surfaceDoc))
		}
	}
	return problems
}

// TestSurfaceRulesNameTestsThatExist asserts that every rule of SURFACE.md
// names the test that checks it or has a "Not checked yet:" line, that every
// test it names is declared in the file it names, and that every test of
// the conventions file is named by a rule.
func TestSurfaceRulesNameTestsThatExist(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(surfaceDoc)
	require.NoError(t, err)
	tests, err := surfaceDeclaredTests()
	require.NoError(t, err)
	require.NotEmpty(t, tests, "no _test.go file was read")
	guards := 0
	for _, files := range tests {
		if slices.Contains(files, surfaceGuardFile) {
			guards++
		}
	}
	assert.Positive(t, guards, "%s declares no test", surfaceGuardFile)
	for _, problem := range surfaceRuleProblems(string(doc), tests, surfaceGuardFile, surfaceMinRules) {
		assert.Fail(t, problem)
	}
}

// TestSurfaceRulesCheckFindsPlantedProblems asserts that the test of
// SURFACE.md reports exactly one problem for each of a missing test, a rule
// with no "Checked by:" or "Not checked yet:" line, a gap in the numbering, a
// test of the conventions file no rule names and a wrong file beside a test name, a line that starts
// "Checked by" without its colon, a backticked word that is not a test name,
// empty parentheses, a line with no test name and a line with two
// parentheses, and none for a clean text.
func TestSurfaceRulesCheckFindsPlantedProblems(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"TestFirst": {"pkg/first_test.go"},
		"TestGuard": {surfaceGuardFile},
	}
	doc := func(rules ...string) string {
		var b strings.Builder
		b.WriteString("# Title\n\nText.\n\n")
		for i, rule := range rules {
			fmt.Fprintf(&b, "## %d. Rule\n\nText.\n\n%s\n\n", i+1, rule)
		}
		return b.String()
	}
	clean := []string{
		"Checked by: `TestFirst` (pkg/first_test.go)",
		"Not checked yet: it comes later.",
		"Checked by: `TestGuard` (" + surfaceGuardFile + ")\n\nNot checked yet: the rest comes later.",
	}
	assert.Empty(t, surfaceRuleProblems(doc(clean...), tests, surfaceGuardFile, 3))

	check := func(text, want string, minRules int) {
		t.Helper()
		problems := surfaceRuleProblems(text, tests, surfaceGuardFile, minRules)
		require.Len(t, problems, 1, text)
		assert.Contains(t, problems[0], want)
	}

	check(doc(clean[0], clean[1], "Checked by: `TestGuard` ("+surfaceGuardFile+")\n\nChecked by: `TestMissing` (pkg/first_test.go)"), "TestMissing", 3)
	check(doc(clean[0], "No check line.", clean[2]), "rule 2 has neither", 3)
	check(strings.Replace(doc(clean...), "## 3. Rule", "## 4. Rule", 1), "no gap", 3)
	check(doc(clean[0], clean[1], "Not checked yet: later."), "TestGuard", 3)
	check(doc("Checked by: `TestFirst` (pkg/other_test.go)", clean[1], clean[2]), "pkg/other_test.go", 3)
	check(doc(clean...), "at least 4", 4)
	check(doc("Checked by `TestFirst` (pkg/first_test.go)\n\nNot checked yet: later.", clean[1], clean[2]), "not with", 3)
	check(doc("Checked by: `TestFirst` `notatest` (pkg/first_test.go)", clean[1], clean[2]), "not a test name", 3)
	check(doc("Checked by: `TestFirst` ()", clean[1], clean[2]), "empty parentheses", 3)
	check(doc("Checked by: TestFirst (pkg/first_test.go)\n\nNot checked yet: later.", clean[1], clean[2]), "names no test", 3)
	check(doc("Checked by: `TestFirst` (pkg/first_test.go) (other)", clean[1], clean[2]), "no other text in parentheses", 3)
}
